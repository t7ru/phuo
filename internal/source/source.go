package source

import (
	"bufio"
	"bytes"
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	json "encoding/json/v2"

	"github.com/t7ru/phuo/internal/fetch"
	"github.com/t7ru/phuo/internal/gitproto"
	"github.com/t7ru/phuo/internal/registry"
	"github.com/t7ru/phuo/internal/spec"
)

type Resolved struct {
	Name, Type string // Type: "extensions" | "skins"
	Spec       spec.Spec
	Ref, SHA   string
	Date       time.Time
	Source     string
	Archive    string
	Clone      string
	Local      string
	Policy     string
	Hint       string
}

type ResolveOpts struct {
	Rel    string
	LTSRel string
	MWVer  string
	Skin   bool
	Git    bool
	Full   bool
}

type Resolver interface {
	Resolve(ctx context.Context, s spec.Spec, opts ResolveOpts) (Resolved, error)
}

type Commit struct {
	SHA, Author, Email, Subject string
	Date                        time.Time
}

type ChangeLogger interface {
	Log(ctx context.Context, res Resolved, from, to string) ([]Commit, error)
}

type registryAPI interface {
	Branches(ctx context.Context, exts, skins []string) (map[string]registry.Branches, map[string]registry.Branches, error)
	Policies(ctx context.Context, exts, skins []string) (map[string]registry.Policy, error)
	Suggest(name string) []string
}

func New(reg *registry.Client, c *http.Client) Resolver {
	r := &resolver{reg: reg, http: c}
	if !reg.NoCache {
		r.cacheDir = reg.CacheDir
	}
	return r
}

type resolver struct {
	reg      registryAPI
	http     *http.Client
	cacheDir string
}

func (r *resolver) Resolve(ctx context.Context, s spec.Spec, opts ResolveOpts) (Resolved, error) {
	switch s.Kind {
	case spec.Registry:
		return r.resolveRegistry(ctx, s, opts)
	case spec.GitHub:
		return r.resolveGitHub(ctx, s, opts)
	case spec.GitLab:
		return r.resolveGitLab(ctx, s, opts)
	case spec.Git:
		return r.resolveGit(ctx, s)
	case spec.Archive:
		return Resolved{Name: s.Name, Spec: s, Archive: s.Repo, Source: s.Repo}, nil
	case spec.Local:
		return Resolved{Name: s.Name, Spec: s, Local: s.Repo}, nil
	default:
		return Resolved{}, fmt.Errorf("unknown spec kind %d", s.Kind)
	}
}

func (r *resolver) resolveRegistry(ctx context.Context, s spec.Spec, opts ResolveOpts) (Resolved, error) {
	name := s.Name
	skinFirst := opts.Skin || s.Skin
	var typ string
	var br registry.Branches
	var found bool

	try := func(asSkin bool) error {
		var exts, skins []string
		kind := "extensions"
		if asSkin {
			skins = []string{name}
			kind = "skins"
		} else {
			exts = []string{name}
		}
		ex, sk, err := r.reg.Branches(ctx, exts, skins)
		if err != nil {
			return err
		}
		set := ex
		if asSkin {
			set = sk
		}
		if b, ok := set[name]; ok {
			br, found, typ = b, true, kind
		}
		return nil
	}
	for _, asSkin := range []bool{skinFirst, !skinFirst} {
		if err := try(asSkin); err != nil {
			return Resolved{}, err
		}
		if found {
			break
		}
	}
	if !found {
		msg := fmt.Sprintf("Unknown extension/skin %q", name)
		if sug := r.reg.Suggest(name); len(sug) > 0 {
			msg += fmt.Sprintf(". Did you mean %s?", sug[0])
		}
		return Resolved{}, fmt.Errorf("%s", msg)
	}

	var exts, skins []string
	if typ == "skins" {
		skins = []string{name}
	} else {
		exts = []string{name}
	}
	pols, err := r.reg.Policies(ctx, exts, skins)
	if err != nil {
		return Resolved{}, err
	}
	pol := pols[typ+"/"+name]
	policyKind := pol.Kind
	if policyKind == "" {
		policyKind = "rel"
	}

	var shas map[string]string
	if len(br.Refs) == 0 && strings.Contains(br.Source, "gerrit.wikimedia.org") {
		// sometimes ExtensionDistributor fucks up and don't have snapshots of some repos
		// gerrit should still hopefully have then
		heads, err := gitproto.LsRefs(ctx, r.http, br.Source, "refs/heads/")
		if err != nil {
			return Resolved{}, err
		}
		br.Refs, shas = map[string]string{}, map[string]string{}
		for head, sha := range heads {
			if ref, ok := strings.CutPrefix(head, "refs/heads/"); ok && (ref == "master" || strings.HasPrefix(ref, "REL")) {
				br.Refs[ref], _ = ArchiveAt(br.Source, sha)
				shas[ref] = sha
			}
		}
	}
	ref, hint, err := selectRef(name, s.Ref, opts, policyKind, br.Refs)
	if err != nil {
		return Resolved{}, err
	}
	archive := br.Refs[ref]
	sha := cmp.Or(shas[ref], ArchiveSHA(archive))

	out := Resolved{
		Name: name, Type: typ, Spec: s,
		Ref: ref, SHA: sha, Source: br.Source,
		Archive: archive, Policy: policyKind, Hint: hint,
	}

	if opts.Git {
		out.Clone = br.Source
		out.Archive = ""
		_, sha, err = r.resolveHostRef(ctx, br.Source, ref)
		if err != nil {
			return Resolved{}, err
		}
		out.SHA = sha
		return out, nil
	}

	if sha != "" && strings.Contains(br.Source, "gerrit.wikimedia.org") {
		full, date, err := r.gitilesCommit(ctx, br.Source, sha)
		if err != nil {
			return Resolved{}, err
		}
		out.SHA = full
		out.Date = date
	}
	return out, nil
}

func selectRef(name, pinned string, opts ResolveOpts, policy string, refs map[string]string) (ref, hint string, err error) {
	avail := sortedRefs(refs)
	list := strings.Join(avail, ", ")
	if pinned != "" {
		if _, ok := refs[pinned]; !ok {
			return "", "", fmt.Errorf("%s@%s: ref not available (available: %s)", name, pinned, list)
		}
		return pinned, "", nil
	}
	if opts.Rel != "" {
		if _, ok := refs[opts.Rel]; ok {
			if policy == "master" || policy == "main" || policy == "ltsrel" {
				rec := policy
				if policy == "ltsrel" {
					rec = opts.LTSRel
				}
				hint = fmt.Sprintf("%s follows the %s policy; its maintainer recommends %s (phuo add %s@%s)",
					name, policy, rec, name, rec)
			}
			return opts.Rel, hint, nil
		}
	}
	switch policy {
	case "master", "main":
		if _, ok := refs["master"]; ok {
			return "master", "", nil
		}
		if policy == "main" {
			if _, ok := refs["main"]; ok {
				return "main", "", nil
			}
		}
	case "ltsrel":
		if opts.LTSRel != "" {
			if _, ok := refs[opts.LTSRel]; ok {
				return opts.LTSRel, "", nil
			}
		}
	}
	return "", "", fmt.Errorf("no suitable ref for %s (available: %s); try phuo add %s@REF", name, list, name)
}

func sortedRefs(refs map[string]string) []string {
	keys := make([]string, 0, len(refs))
	for k := range refs {
		if k != "" {
			keys = append(keys, k)
		}
	}
	slices.Sort(keys)
	return keys
}

func ArchiveSHA(url string) string {
	base, ok := strings.CutSuffix(url, ".tar.gz")
	if !ok {
		return ""
	}
	_, sha, ok := strings.CutLast(base, "-")
	if !ok || len(sha) < 7 {
		return ""
	}
	return sha
}

// commits are immutable, so the sha7 -> (full sha, date) lookup is cached forever
func (r *resolver) gitilesCommit(ctx context.Context, source, sha7 string) (string, time.Time, error) {
	var cf string
	if r.cacheDir != "" {
		sum := sha256.Sum256([]byte(source + "@" + sha7))
		cf = filepath.Join(r.cacheDir, "commits", hex.EncodeToString(sum[:]))
		if b, err := os.ReadFile(cf); err == nil {
			full, ts, _ := strings.Cut(string(b), " ")
			if t, err := time.Parse(time.RFC3339, ts); err == nil {
				return full, t, nil
			}
		}
	}
	full, t, err := r.gitilesCommitFetch(ctx, source, sha7)
	if err != nil || cf == "" {
		return full, t, err
	}
	if err := os.MkdirAll(filepath.Dir(cf), 0o755); err != nil {
		return "", time.Time{}, err
	}
	return full, t, os.WriteFile(cf, []byte(full+" "+t.Format(time.RFC3339)), 0o644)
}

func (r *resolver) gitilesCommitFetch(ctx context.Context, source, sha7 string) (string, time.Time, error) {
	u := gitiles(source) + "/+log/" + sha7 + "?format=JSON&n=1"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", time.Time{}, err
	}
	res, err := r.http.Do(req)
	if err != nil {
		return "", time.Time{}, err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", time.Time{}, fmt.Errorf("%s: gitiles HTTP %d", u, res.StatusCode)
	}
	var log gitilesLog
	if err := decodeGitiles(res.Body, &log); err != nil {
		return "", time.Time{}, err
	}
	if len(log.Log) == 0 {
		return "", time.Time{}, fmt.Errorf("%s: empty gitiles log", u)
	}
	e := log.Log[0]
	t, err := time.Parse("Mon Jan 02 15:04:05 2006 -0700", e.Author.Time)
	if err != nil {
		return "", time.Time{}, err
	}
	return e.Commit, t, nil
}

type gitilesLog struct {
	Log  []gitilesEntry `json:"log"`
	Next string         `json:"next"`
}

type gitilesEntry struct {
	Commit  string `json:"commit"`
	Message string `json:"message"`
	Author  struct {
		Name  string `json:"name"`
		Email string `json:"email"`
		Time  string `json:"time"`
	} `json:"author"`
}

// )]}'\n XSSI prefix on every gitiles JSON response
func decodeGitiles(r io.Reader, v any) error {
	br := bufio.NewReader(r)
	if _, err := br.Discard(5); err != nil {
		return err
	}
	return json.UnmarshalRead(br, v)
}

func (r *resolver) resolveGitHub(ctx context.Context, s spec.Spec, opts ResolveOpts) (Resolved, error) {
	repo := "https://github.com/" + s.Repo + ".git"
	ref, sha, err := r.resolveHostRef(ctx, repo, s.Ref)
	if err != nil {
		return Resolved{}, err
	}
	out := Resolved{
		Name: s.Name, Spec: s, Ref: ref, SHA: sha,
		Source:  "https://github.com/" + s.Repo,
		Archive: "https://codeload.github.com/" + s.Repo + "/tar.gz/" + sha,
	}
	if opts.Git {
		out.Clone, out.Archive = repo, ""
	}
	return out, nil
}

func (r *resolver) resolveGitLab(ctx context.Context, s spec.Spec, opts ResolveOpts) (Resolved, error) {
	repo := "https://gitlab.com/" + s.Repo + ".git"
	ref, sha, err := r.resolveHostRef(ctx, repo, s.Ref)
	if err != nil {
		return Resolved{}, err
	}
	_, short, _ := strings.CutLast(s.Repo, "/")
	out := Resolved{
		Name: s.Name, Spec: s, Ref: ref, SHA: sha,
		Source:  "https://gitlab.com/" + s.Repo,
		Archive: "https://gitlab.com/" + s.Repo + "/-/archive/" + sha + "/" + short + "-" + sha + ".tar.gz",
	}
	if opts.Git {
		out.Clone, out.Archive = repo, ""
	}
	return out, nil
}

func (r *resolver) resolveGit(ctx context.Context, s spec.Spec) (Resolved, error) {
	url := strings.TrimPrefix(s.Repo, "git+")
	out := Resolved{Name: s.Name, Spec: s, Ref: s.Ref, Clone: url, Source: url}
	httpURL := strings.HasPrefix(url, "https://") || strings.HasPrefix(url, "http://")
	if httpURL {
		ref, sha, err := r.resolveHostRef(ctx, url, s.Ref)
		if err != nil {
			return Resolved{}, err
		}
		out.Ref, out.SHA = ref, sha
		return out, nil
	}
	sha, ref, err := gitLSRemote(ctx, url, s.Ref)
	if err != nil {
		return Resolved{}, err
	}
	out.SHA, out.Ref = sha, ref
	return out, nil
}

func (r *resolver) resolveHostRef(ctx context.Context, repo, ref string) (string, string, error) {
	if IsFullSHA(ref) {
		return ref, ref, nil
	}
	if ref == "" {
		refs, err := gitproto.LsRefs(ctx, r.http, repo, "HEAD")
		if err != nil {
			return "", "", authHint(repo, err)
		}
		sha := refs["HEAD"]
		if sha == "" {
			return "", "", fmt.Errorf("%s: HEAD not found", repo)
		}
		for name, s := range refs {
			if name != "HEAD" && s == sha {
				if rest, ok := strings.CutPrefix(name, "refs/heads/"); ok {
					return rest, sha, nil
				}
			}
		}
		return "HEAD", sha, nil
	}
	refs, err := gitproto.LsRefs(ctx, r.http, repo, "refs/heads/"+ref, "refs/tags/"+ref)
	if err != nil {
		return "", "", authHint(repo, err)
	}
	if sha, ok := refs["refs/heads/"+ref]; ok {
		return ref, sha, nil
	}
	if sha, ok := refs["refs/tags/"+ref]; ok {
		return ref, sha, nil
	}
	return "", "", fmt.Errorf("%s: ref %q not found", repo, ref)
}

// say whether a token was even attached, so a 401 doesn't send the user
// hunting for keys when it's actually PAT scope
func authHint(repo string, err error) error {
	if !errors.Is(err, gitproto.ErrAuth) {
		return err
	}
	host, _, _ := strings.Cut(strings.TrimPrefix(repo, "https://"), "/")
	if fetch.AuthFor(host) != "" {
		return fmt.Errorf("%w; the token was sent but rejected: check it is valid and has access to this repository", err)
	}
	return fmt.Errorf("%w; no token for %s: set GITHUB_TOKEN/GH_TOKEN (or GITLAB_TOKEN), or use an SSH URL", err, host)
}

func IsFullSHA(s string) bool {
	if len(s) != 40 {
		return false
	}
	for _, c := range s {
		if c >= '0' && c <= '9' || c >= 'a' && c <= 'f' || c >= 'A' && c <= 'F' {
			continue
		}
		return false
	}
	return true
}

func gitLSRemote(ctx context.Context, url, ref string) (sha, resolved string, err error) {
	if _, err := exec.LookPath("git"); err != nil {
		return "", "", fmt.Errorf("git not found in PATH (required for ssh/git remotes)")
	}
	arg := ref
	if arg == "" {
		arg = "HEAD"
	}
	cmd := exec.CommandContext(ctx, "git", "ls-remote", url, arg)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", "", fmt.Errorf("git ls-remote %s: %w\n%s", url, err, stderr.Bytes())
	}
	line, _, _ := strings.Cut(string(out), "\n")
	sha, name, ok := strings.Cut(line, "\t")
	if !ok || sha == "" {
		return "", "", fmt.Errorf("git ls-remote %s: ref %q not found", url, arg)
	}
	resolved = ref
	if resolved == "" {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(name), "refs/heads/"); ok {
			resolved = rest
		} else {
			resolved = "HEAD"
		}
	}
	return strings.TrimSpace(sha), resolved, nil
}

func (r *resolver) Log(ctx context.Context, res Resolved, from, to string) ([]Commit, error) {
	switch {
	case strings.Contains(res.Source, "gerrit.wikimedia.org") || strings.Contains(res.Clone, "gerrit.wikimedia.org"):
		src := res.Source
		if src == "" {
			src = res.Clone
		}
		return r.gitilesLog(ctx, src, from, to)
	case res.Spec.Kind == spec.GitHub || strings.Contains(res.Source, "github.com") || strings.Contains(res.Archive, "github.com"):
		repo := res.Spec.Repo
		if res.Spec.Kind != spec.GitHub {
			repo = GitHubRepo(cmp.Or(res.Source, res.Archive))
		}
		return r.githubCompare(ctx, repo, from, to)
	case res.Clone != "" || res.Local != "":
		return gitLog(ctx, res, from, to)
	default:
		return nil, fmt.Errorf("%s: no changelog source", res.Name)
	}
}

// Log over HTTP (gitiles, GitHub compare) rather than a bare clone
// only cheap kinds belong in `outdated`
func CheapLog(res Resolved) bool {
	return strings.Contains(res.Source, "gerrit.wikimedia.org") ||
		strings.Contains(res.Clone, "gerrit.wikimedia.org") ||
		res.Spec.Kind == spec.GitHub ||
		strings.Contains(res.Source, "github.com") ||
		strings.Contains(res.Archive, "github.com")
}

func GitHubRepo(u string) string {
	u = strings.TrimSuffix(strings.TrimSuffix(u, "/"), ".git")
	_, rest, ok := strings.Cut(u, "github.com/")
	if !ok {
		if _, rest, ok = strings.Cut(u, "github.com:"); !ok {
			return ""
		}
	}
	owner, repo, _ := strings.Cut(rest, "/")
	repo, _, _ = strings.Cut(repo, "/")
	if owner == "" || repo == "" {
		return ""
	}
	return owner + "/" + repo
}

// ExtensionDistributor clone URLs are ".../r/.../Name.git"
// gitiles is /r/plugins/gitiles/ without .git
func gitiles(source string) string {
	return strings.Replace(strings.TrimSuffix(strings.TrimSuffix(source, "/"), ".git"), "gerrit.wikimedia.org/r/", "gerrit.wikimedia.org/r/plugins/gitiles/", 1)
}

func ArchiveAt(source, sha string) (string, bool) {
	switch {
	case strings.Contains(source, "gerrit.wikimedia.org"):
		return gitiles(source) + "/+archive/" + sha + ".tar.gz", true
	case strings.Contains(source, "github.com"):
		if repo := GitHubRepo(source); repo != "" {
			return "https://codeload.github.com/" + repo + "/tar.gz/" + sha, true
		}
	case strings.Contains(source, "gitlab.com"):
		_, path, _ := strings.Cut(strings.TrimSuffix(strings.TrimSuffix(source, "/"), ".git"), "gitlab.com/")
		if path != "" {
			_, last, ok := strings.CutLast(path, "/")
			if !ok {
				last = path
			}
			return "https://gitlab.com/" + path + "/-/archive/" + sha + "/" + last + "-" + sha + ".tar.gz", true
		}
	}
	return "", false
}

func (r *resolver) gitilesLog(ctx context.Context, source, from, to string) ([]Commit, error) {
	base := gitiles(source) + "/+log/" + from + ".." + to + "?format=JSON&n=100"
	var all []Commit
	u := base
	seen := map[string]struct{}{}
	for {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			return nil, err
		}
		res, err := r.http.Do(req)
		if err != nil {
			return nil, err
		}
		if res.StatusCode != http.StatusOK {
			res.Body.Close()
			return nil, fmt.Errorf("%s: gitiles HTTP %d", u, res.StatusCode)
		}
		var log gitilesLog
		err = decodeGitiles(res.Body, &log)
		res.Body.Close()
		if err != nil {
			return nil, err
		}
		for _, e := range log.Log {
			t, err := time.Parse("Mon Jan 02 15:04:05 2006 -0700", e.Author.Time)
			if err != nil {
				return nil, err
			}
			subj, _, _ := strings.Cut(e.Message, "\n")
			all = append(all, Commit{
				SHA: e.Commit, Author: e.Author.Name, Email: e.Author.Email,
				Date: t, Subject: subj,
			})
		}
		if log.Next == "" {
			return all, nil
		}
		if _, ok := seen[log.Next]; ok {
			return all, nil
		}
		seen[log.Next] = struct{}{}
		u = base + "&s=" + log.Next
	}
}

func (r *resolver) githubCompare(ctx context.Context, repo, from, to string) ([]Commit, error) {
	u := "https://api.github.com/repos/" + repo + "/compare/" + from + "..." + to
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	res, err := r.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusForbidden {
		return nil, fmt.Errorf("GitHub API rate limited; set GITHUB_TOKEN")
	}
	if res.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s: HTTP %d", u, res.StatusCode)
	}
	var cmp struct {
		Commits []struct {
			SHA    string `json:"sha"`
			Commit struct {
				Message string `json:"message"`
				Author  struct {
					Name  string    `json:"name"`
					Email string    `json:"email"`
					Date  time.Time `json:"date"`
				} `json:"author"`
			} `json:"commit"`
		} `json:"commits"`
	}
	if err := json.UnmarshalRead(res.Body, &cmp); err != nil {
		return nil, err
	}
	out := make([]Commit, 0, len(cmp.Commits))
	for _, c := range slices.Backward(cmp.Commits) { // newest first
		subj, _, _ := strings.Cut(c.Commit.Message, "\n")
		out = append(out, Commit{
			SHA: c.SHA, Author: c.Commit.Author.Name, Email: c.Commit.Author.Email,
			Date: c.Commit.Author.Date, Subject: subj,
		})
	}
	return out, nil
}

func gitLog(ctx context.Context, res Resolved, from, to string) ([]Commit, error) {
	if _, err := exec.LookPath("git"); err != nil {
		return nil, fmt.Errorf("git not found in PATH")
	}
	dir := res.Local
	args := []string{"log", "--format=%H%x09%an%x09%ae%x09%aI%x09%s", from + ".." + to}
	var cmd *exec.Cmd
	if dir != "" {
		cmd = exec.CommandContext(ctx, "git", args...)
		cmd.Dir = dir
	} else if res.Clone != "" {
		tmp, err := os.MkdirTemp("", "phuo-log-*")
		if err != nil {
			return nil, err
		}
		defer os.RemoveAll(tmp)
		clone := exec.CommandContext(ctx, "git", "clone", "--bare", "--quiet", res.Clone, tmp)
		clone.Env = append(os.Environ(), fetch.GitEnv(res.Clone)...)
		if out, err := clone.CombinedOutput(); err != nil {
			return nil, fmt.Errorf("git clone: %w\n%s", err, out)
		}
		cmd = exec.CommandContext(ctx, "git", append([]string{"-C", tmp}, args...)...)
	} else {
		return nil, fmt.Errorf("%s: git changelog requires a local checkout", res.Name)
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git log: %w\n%s", err, stderr.Bytes())
	}
	var commits []Commit
	for line := range strings.SplitSeq(strings.TrimSuffix(string(out), "\n"), "\n") {
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, "\t", 5)
		if len(parts) < 5 {
			continue
		}
		t, err := time.Parse(time.RFC3339, parts[3])
		if err != nil {
			return nil, err
		}
		commits = append(commits, Commit{
			SHA: parts[0], Author: parts[1], Email: parts[2], Date: t, Subject: parts[4],
		})
	}
	return commits, nil
}
