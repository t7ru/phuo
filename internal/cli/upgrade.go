package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	json "encoding/json/v2"

	"github.com/t7ru/phuo/internal/archive"
	"github.com/t7ru/phuo/internal/fetch"
	"github.com/t7ru/phuo/internal/ui"
	"github.com/t7ru/phuo/internal/version"
)

const repo = "t7ru/phuo"

func init() {
	exe, err := os.Executable()
	if err != nil {
		return
	}
	if resolved, err := filepath.EvalSymlinks(exe); err == nil {
		exe = resolved
	}
	matches, err := filepath.Glob(exe + ".old*")
	if err != nil {
		return
	}
	for _, m := range matches {
		os.Remove(m)
	}
	if runtime.GOOS == "windows" {
		os.Remove(exe + "~") // clean freak
	}
}

type UpgradeCmd struct{}

type upgradeResult struct {
	Current   string `json:"current"`
	Latest    string `json:"latest"`
	Updated   bool   `json:"updated"`
	Hint      string `json:"hint,omitzero"`
	Changelog string `json:"changelog,omitzero"`
}

func (c *UpgradeCmd) Run(ctx context.Context, cli *CLI) error {
	if cli.Offline {
		return userErr("upgrade needs the network")
	}
	rep := ui.New(ui.Options{
		JSON: cli.JSON, Silent: cli.Silent, Verbose: cli.Verbose,
		NoColor: cli.NoColor, NoProgress: cli.NoProgress,
	})
	rel, err := latestRelease(ctx)
	if err != nil {
		return err
	}
	cur := version.String()
	out := upgradeResult{Current: cur, Latest: rel.TagName}
	if version.Devel() {
		out.Hint = "development build"
		return out.finish(cli, rep,
			fmt.Sprintf("phuo %s is a development build", cur),
			"latest release: "+rel.TagName,
			"go install github.com/"+repo+"@latest",
			"https://github.com/"+repo+"/releases/latest",
		)
	}
	switch version.Compare(cur, rel.TagName) {
	case 0:
		return out.finish(cli, rep, fmt.Sprintf("phuo %s is up to date", cur))
	case 1:
		return out.finish(cli, rep, fmt.Sprintf("phuo %s is newer than %s", cur, rel.TagName))
	}
	if version.Module() != "" {
		if goExe, err := exec.LookPath("go"); err == nil {
			dest, derr := goBin(ctx, goExe)
			exe, eerr := currentExe()
			if derr == nil && eerr == nil && sameFile(exe, dest) {
				return goInstall(ctx, cli, rep, goExe, cur, rel.TagName)
			}
		}
	}
	return upgradeRelease(ctx, cli, rep, cur, rel)
}

func (r upgradeResult) finish(cli *CLI, rep *ui.Reporter, lines ...string) error {
	if cli.JSON {
		return json.MarshalWrite(os.Stdout, r)
	}
	for _, line := range lines {
		if r.Updated {
			rep.Step("%s", line)
		} else {
			rep.Info("%s", line)
		}
	}
	return nil
}

type ghAsset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

type ghRelease struct {
	TagName string    `json:"tag_name"`
	Assets  []ghAsset `json:"assets"`
}

func latestRelease(ctx context.Context) (ghRelease, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	if err != nil {
		return ghRelease{}, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	res, err := fetch.Client().Do(req)
	if err != nil {
		return ghRelease{}, err
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return ghRelease{}, userErr("no phuo releases published")
	}
	if res.StatusCode != http.StatusOK {
		return ghRelease{}, fmt.Errorf("GitHub releases: HTTP %d", res.StatusCode)
	}
	var rel ghRelease
	if err := json.UnmarshalRead(res.Body, &rel); err != nil {
		return ghRelease{}, err
	}
	if rel.TagName == "" {
		return ghRelease{}, fmt.Errorf("GitHub releases: missing tag")
	}
	return rel, nil
}

func (r ghRelease) asset(name string) (ghAsset, error) {
	for _, a := range r.Assets {
		if a.Name != name {
			continue
		}
		if a.URL == "" || a.Digest == "" {
			return ghAsset{}, fmt.Errorf("release %s: %s has no url or digest", r.TagName, name)
		}
		return a, nil
	}
	return ghAsset{}, userErr(fmt.Sprintf("release %s has no %s", r.TagName, name))
}

func goInstall(ctx context.Context, cli *CLI, rep *ui.Reporter, goExe, cur, latest string) error {
	rep.Progress("go install github.com/%s@latest", repo)
	cmd := exec.CommandContext(ctx, goExe, "install", "github.com/"+repo+"@latest")
	var buf strings.Builder
	cmd.Stdout = &buf
	cmd.Stderr = &buf
	err := cmd.Run()
	rep.ClearProgress()
	if err != nil {
		msg := strings.TrimSpace(buf.String())
		if msg != "" {
			return fmt.Errorf("go install github.com/%s@latest: %s: %w", repo, msg, err)
		}
		return fmt.Errorf("go install github.com/%s@latest: %w", repo, err)
	}
	return upgraded(cli, rep, cur, latest)
}

func upgradeRelease(ctx context.Context, cli *CLI, rep *ui.Reporter, cur string, rel ghRelease) error {
	name, err := assetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		return err
	}
	asset, err := rel.asset(name)
	if err != nil {
		return err
	}
	rep.Progress("downloading %s", rel.TagName)
	path, err := downloadVerified(ctx, asset.URL, asset.Digest)
	rep.ClearProgress()
	if err != nil {
		return err
	}
	defer os.Remove(path)

	dir, err := os.MkdirTemp("", "phuo-upgrade-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	bin, err := extractBin(path, name, dir)
	if err != nil {
		return err
	}
	exe, err := currentExe()
	if err != nil {
		return err
	}
	if err := replaceExe(bin, exe); err != nil {
		return fmt.Errorf("replace %s: %w", exe, err)
	}
	return upgraded(cli, rep, cur, rel.TagName)
}

func upgraded(cli *CLI, rep *ui.Reporter, cur, latest string) error {
	out := upgradeResult{Current: cur, Latest: latest, Updated: true, Changelog: compareURL(cur, latest)}
	msg := fmt.Sprintf("upgraded %s -> %s", cur, latest)
	if out.Changelog == "" {
		return out.finish(cli, rep, msg)
	}
	return out.finish(cli, rep, msg, out.Changelog)
}

func compareURL(from, to string) string {
	a, b := version.Tag(from), version.Tag(to)
	if a == "" || b == "" || a == b {
		return ""
	}
	return "https://github.com/" + repo + "/compare/" + a + "..." + b
}

func assetName(tag, goos, goarch string) (string, error) {
	var osName, ext string
	switch goos {
	case "linux":
		osName, ext = "linux", ".tar.gz"
	case "darwin":
		osName, ext = "macos", ".zip"
	case "windows":
		osName, ext = "windows", ".zip"
	default:
		return "", userErr(fmt.Sprintf("no release build for %s/%s", goos, goarch))
	}
	if goarch != "amd64" && !(goos == "darwin" && goarch == "arm64") {
		return "", userErr(fmt.Sprintf("no release build for %s/%s", goos, goarch))
	}
	return fmt.Sprintf("phuo-%s-%s-%s%s", tag, osName, goarch, ext), nil
}

func binName(goos string) string {
	if goos == "windows" {
		return "phuo.exe"
	}
	return "phuo"
}

func downloadVerified(ctx context.Context, url, digest string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	res, err := fetch.Client().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("download %s: HTTP %d", url, res.StatusCode)
	}
	f, err := os.CreateTemp("", "phuo-upgrade-*")
	if err != nil {
		return "", err
	}
	h := sha256.New()
	_, err = io.Copy(io.MultiWriter(f, h), res.Body)
	cerr := f.Close()
	if err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(f.Name())
		return "", err
	}
	if err := checkDigest(h.Sum(nil), digest); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}

func checkDigest(sum []byte, digest string) error {
	algo, want, ok := strings.Cut(digest, ":")
	got := hex.EncodeToString(sum)
	if !ok || algo != "sha256" || !strings.EqualFold(want, got) {
		return fmt.Errorf("checksum mismatch: got sha256:%s, release says %s", got, digest)
	}
	return nil
}

func extractBin(archivePath, asset, dir string) (string, error) {
	root, err := os.OpenRoot(dir)
	if err != nil {
		return "", err
	}
	defer root.Close()
	switch {
	case strings.HasSuffix(asset, ".tar.gz"):
		f, err := os.Open(archivePath)
		if err != nil {
			return "", err
		}
		err = archive.ExtractTarGz(f, root)
		cerr := f.Close()
		if err == nil {
			err = cerr
		}
		if err != nil {
			return "", err
		}
	case strings.HasSuffix(asset, ".zip"):
		if err := archive.ExtractZip(archivePath, root); err != nil {
			return "", err
		}
	default:
		return "", fmt.Errorf("unknown archive %s", asset)
	}
	name := binName(runtime.GOOS)
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("archive has no %s", name)
	}
	return path, nil
}

func goBin(ctx context.Context, goExe string) (string, error) {
	dir, err := goEnv(ctx, goExe, "GOBIN")
	if err != nil {
		return "", err
	}
	if dir == "" {
		gopath, err := goEnv(ctx, goExe, "GOPATH")
		if err != nil {
			return "", err
		}
		root, _, _ := strings.Cut(gopath, string(os.PathListSeparator))
		if root == "" {
			return "", errors.New("GOPATH is empty")
		}
		dir = filepath.Join(root, "bin")
	}
	return filepath.Join(dir, binName(runtime.GOOS)), nil
}

func goEnv(ctx context.Context, goExe, key string) (string, error) {
	out, err := exec.CommandContext(ctx, goExe, "env", key).Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func currentExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	return filepath.EvalSymlinks(exe)
}

func sameFile(a, b string) bool {
	ai, err1 := os.Stat(a)
	bi, err2 := os.Stat(b)
	return err1 == nil && err2 == nil && os.SameFile(ai, bi)
}

func replaceExe(src, dest string) error {
	info, err := os.Stat(dest)
	if err != nil {
		return err
	}
	mode := info.Mode().Perm()
	if mode&0o111 == 0 {
		mode = 0o755
	}
	if matches, err := filepath.Glob(dest + ".old*"); err == nil {
		for _, m := range matches {
			os.Remove(m)
		}
	}
	next := dest + ".new"
	if err := os.Remove(next); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := copyFile(src, next); err != nil {
		return err
	}
	if err := os.Chmod(next, mode); err != nil {
		os.Remove(next)
		return err
	}
	bak := dest + ".old"
	if err := os.Remove(bak); err != nil && !errors.Is(err, fs.ErrNotExist) {
		bak = dest + ".old-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	if err := os.Rename(dest, bak); err != nil {
		os.Remove(next)
		return err
	}
	if err := os.Rename(next, dest); err != nil {
		os.Rename(bak, dest)
		os.Remove(next)
		return err
	}
	// Windows keeps the running executable locked
	// thus this can fail until the process exits
	os.Remove(bak)
	return nil
}
