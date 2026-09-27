package fetch

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/t7ru/phuo/internal/version"
)

var (
	clientOnce sync.Once
	shared     *http.Client
)

func Client() *http.Client {
	clientOnce.Do(func() {
		shared = &http.Client{
			Transport: &uaTransport{
				base: &http.Transport{
					ForceAttemptHTTP2:     true,
					MaxIdleConnsPerHost:   16,
					IdleConnTimeout:       90 * time.Second,
					ResponseHeaderTimeout: 30 * time.Second,
				},
				auth: hostAuth(),
			},
		}
	})
	return shared
}

// exact host match so tokens never travel to other origins
// git smart-HTTP needs Basic auth
// GitHub's API and codeload accept it too (x-access-token)
func hostAuth() map[string]string {
	auth := map[string]string{}
	if tok := cmp.Or(os.Getenv("GITHUB_TOKEN"), os.Getenv("GH_TOKEN")); tok != "" {
		v := "Basic " + base64.StdEncoding.EncodeToString([]byte("x-access-token:"+tok))
		for _, h := range []string{"github.com", "codeload.github.com", "api.github.com"} {
			auth[h] = v
		}
	}
	if tok := os.Getenv("GITLAB_TOKEN"); tok != "" {
		auth["gitlab.com"] = "Basic " + base64.StdEncoding.EncodeToString([]byte("oauth2:"+tok))
	}
	// Gitea want the token as the basic-auth username (`https://token@host`)
	// self-hosted gets comma-separated FORGEJO_HOSTS because I give up
	if tok := os.Getenv("FORGEJO_TOKEN"); tok != "" {
		v := "Basic " + base64.StdEncoding.EncodeToString([]byte(tok+":"))
		for h := range strings.SplitSeq(cmp.Or(os.Getenv("FORGEJO_HOSTS"), "codeberg.org"), ",") {
			if h = strings.TrimSpace(h); h != "" {
				auth[h] = v
			}
		}
	}
	return auth
}

func AuthFor(host string) string { return hostAuth()[host] }

// send Authorization via http.extraHeader so the token isn't in argv or the clone URL
func GitEnv(rawURL string) []string {
	u, err := url.Parse(rawURL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return nil
	}
	h := AuthFor(u.Hostname())
	if h == "" {
		return nil
	}
	return []string{
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: " + h,
	}
}

type uaTransport struct {
	base http.RoundTripper
	auth map[string]string
}

func (t *uaTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req = req.Clone(req.Context())
	req.Header.Set("User-Agent", version.UserAgent())
	if v, ok := t.auth[req.URL.Hostname()]; ok && req.Header.Get("Authorization") == "" {
		req.Header.Set("Authorization", v)
	}
	return t.base.RoundTrip(req)
}

// empty cacheDir skips cache reads and writes (--no-cache, tests)
func Download(ctx context.Context, url, cacheDir string, w io.Writer) (string, error) {
	if cacheDir != "" {
		if integrity, ok := serveCached(cacheDir, url, w); ok {
			return integrity, nil
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	res, err := Client().Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s: HTTP %d", url, res.StatusCode)
	}

	h := sha256.New()
	writers := []io.Writer{w, h}
	var partPath string
	var part *os.File
	if cacheDir != "" {
		dir := filepath.Join(cacheDir, "tarballs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		partPath = filepath.Join(dir, urlKey(url)+".part")
		part, err = os.Create(partPath)
		if err != nil {
			return "", err
		}
		writers = append(writers, part)
	}
	mw := io.MultiWriter(writers...)
	_, copyErr := io.Copy(mw, res.Body)
	if part != nil {
		closeErr := part.Close()
		if copyErr != nil {
			os.Remove(partPath)
			return "", copyErr
		}
		if closeErr != nil {
			os.Remove(partPath)
			return "", closeErr
		}
	} else if copyErr != nil {
		return "", copyErr
	}

	sum := h.Sum(nil)
	integrity := "sha256-" + base64.StdEncoding.EncodeToString(sum)
	if cacheDir != "" {
		blob := filepath.Join(cacheDir, "tarballs", hex.EncodeToString(sum)+".tar.gz")
		if err := os.Rename(partPath, blob); err != nil {
			os.Remove(partPath)
			return "", err
		}
		idxDir := filepath.Join(cacheDir, "tarballs", "by-url")
		if err := os.MkdirAll(idxDir, 0o755); err != nil {
			return "", err
		}
		if err := os.WriteFile(filepath.Join(idxDir, urlKey(url)), []byte(integrity+"\n"), 0o644); err != nil {
			return "", err
		}
	}
	return integrity, nil
}

func serveCached(cacheDir, url string, w io.Writer) (string, bool) {
	idx := filepath.Join(cacheDir, "tarballs", "by-url", urlKey(url))
	b, err := os.ReadFile(idx)
	if err != nil {
		return "", false
	}
	integrity := string(trimNL(b))
	raw, err := base64.StdEncoding.DecodeString(integrity[len("sha256-"):])
	if err != nil || len(raw) != sha256.Size {
		return "", false
	}
	blob := filepath.Join(cacheDir, "tarballs", hex.EncodeToString(raw)+".tar.gz")
	f, err := os.Open(blob)
	if err != nil {
		return "", false
	}
	defer f.Close()
	if _, err := io.Copy(w, f); err != nil {
		return "", false
	}
	return integrity, true
}

func urlKey(url string) string {
	sum := sha256.Sum256([]byte(url))
	return hex.EncodeToString(sum[:])
}

func trimNL(b []byte) []byte {
	for len(b) > 0 && (b[len(b)-1] == '\n' || b[len(b)-1] == '\r') {
		b = b[:len(b)-1]
	}
	return b
}
