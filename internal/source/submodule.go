package source

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	json "encoding/json/v2"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/t7ru/phuo/internal/fetch"
)

func Gitlink(ctx context.Context, src, sha, name, cacheDir string) (string, error) {
	repo, err := url.Parse(src)
	if err != nil || !IsFullSHA(sha) {
		return "", fmt.Errorf("no repository and full commit SHA for submodule %s", name)
	}
	var cached string
	if cacheDir != "" {
		sum := sha256.Sum256([]byte(src + "\x00" + sha + "\x00" + name))
		cached = filepath.Join(cacheDir, "gitlinks", hex.EncodeToString(sum[:]))
		if b, err := os.ReadFile(cached); err == nil && IsFullSHA(string(b)) {
			return string(b), nil
		}
	}
	dir, base := path.Split(name)
	var id string
	switch repo.Hostname() {
	case "gerrit.wikimedia.org":
		u := gitiles(src) + "/+/" + sha + "/" + (&url.URL{Path: dir}).EscapedPath() + "?format=JSON"
		var tree struct {
			Entries []struct {
				Name string `json:"name"`
				Type string `json:"type"`
				ID   string `json:"id"`
			} `json:"entries"`
		}
		if err := readTree(ctx, u, true, &tree); err != nil {
			return "", err
		}
		for _, e := range tree.Entries {
			if e.Name == base && e.Type == "commit" {
				id = e.ID
				break
			}
		}
	case "github.com":
		u := "https://api.github.com/repos/" + GitHubRepo(src) + "/contents/" + (&url.URL{Path: name}).EscapedPath() + "?ref=" + sha
		var entry struct {
			SHA string `json:"sha"`
			URL string `json:"submodule_git_url"`
		}
		if err := readTree(ctx, u, false, &entry); err != nil {
			return "", err
		}
		if entry.URL != "" {
			id = entry.SHA
		}
	case "gitlab.com":
		project := strings.TrimPrefix(strings.TrimSuffix(strings.TrimSuffix(repo.Path, "/"), ".git"), "/")
		query := url.Values{"ref": {sha}, "path": {strings.TrimSuffix(dir, "/")}, "per_page": {"100"}}
		for page := 1; ; page++ {
			query.Set("page", strconv.Itoa(page))
			u := "https://gitlab.com/api/v4/projects/" + url.PathEscape(project) + "/repository/tree?" + query.Encode()
			var entries []struct {
				Path string `json:"path"`
				Mode string `json:"mode"`
				ID   string `json:"id"`
			}
			if err := readTree(ctx, u, false, &entries); err != nil {
				return "", err
			}
			for _, e := range entries {
				if e.Path == name && e.Mode == "160000" {
					id = e.ID
					break
				}
			}
			if id != "" || len(entries) < 100 {
				break
			}
		}
	default:
		return "", fmt.Errorf("gitlink lookup is unavailable for %s; use --git with a Git repository spec", repo.Hostname())
	}
	if !IsFullSHA(id) {
		return "", fmt.Errorf("no gitlink for %s in %s at %s", name, src, sha)
	}
	if cached != "" {
		if err := os.MkdirAll(filepath.Dir(cached), 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(cached, []byte(id), 0o600); err != nil {
			return "", err
		}
	}
	return id, nil
}

func SubmoduleSource(parent, raw string) (string, error) {
	if host, repo, ok := strings.Cut(strings.TrimPrefix(raw, "git@"), ":"); ok && strings.HasPrefix(raw, "git@") {
		raw = "ssh://" + host + "/" + repo
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", err
	}
	if strings.HasPrefix(raw, "../") || strings.HasPrefix(raw, "./") {
		base, err := url.Parse(strings.TrimSuffix(parent, "/") + "/")
		if err != nil {
			return "", err
		}
		u = base.ResolveReference(u)
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "github.com", "gitlab.com", "gerrit.wikimedia.org":
		if u.Scheme == "http" || u.Scheme == "https" || u.Scheme == "ssh" || u.Scheme == "git" {
			repo := strings.Trim(u.EscapedPath(), "/")
			if host == "gerrit.wikimedia.org" {
				repo = strings.TrimPrefix(strings.TrimPrefix(repo, "r/"), "g/")
				repo = "r/" + strings.TrimPrefix(repo, "plugins/gitiles/")
			}
			return "https://" + host + "/" + repo, nil
		}
	}
	return "", fmt.Errorf("archive is unavailable for submodule host %q; use --git with a Git repository spec", host)
}

func readTree(ctx context.Context, u string, xssi bool, v any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	if req.URL.Hostname() == "gitlab.com" {
		if token := os.Getenv("GITLAB_TOKEN"); token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	}
	res, err := fetch.Client().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return &fetch.HTTPError{URL: u, Status: res.StatusCode}
	}
	if xssi {
		return decodeGitiles(res.Body, v)
	}
	return json.UnmarshalRead(res.Body, v)
}
