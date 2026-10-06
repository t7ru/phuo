package fetch

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
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
			Transport: &retryTransport{base: &uaTransport{
				base: &http.Transport{
					ForceAttemptHTTP2:     true,
					MaxIdleConnsPerHost:   16,
					IdleConnTimeout:       90 * time.Second,
					ResponseHeaderTimeout: 30 * time.Second,
				},
				auth: hostAuth(),
			}},
		}
	})
	return shared
}

const (
	retryAttempts = 3
	retryBackoff  = 250 * time.Millisecond
	retryMaxWait  = 5 * time.Second
)

type retryTransport struct{ base http.RoundTripper }

func (t *retryTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.Body != nil && req.GetBody == nil {
		return t.base.RoundTrip(req)
	}
	for attempt := 0; ; attempt++ {
		res, err := t.base.RoundTrip(req)
		if attempt >= retryAttempts-1 || !retryable(res, err) {
			return res, err
		}
		if res != nil {
			io.Copy(io.Discard, io.LimitReader(res.Body, 1<<16))
			res.Body.Close()
		}
		if !sleepCtx(req.Context(), retryDelay(res, attempt)) {
			return nil, req.Context().Err()
		}
		if req.GetBody != nil {
			body, err := req.GetBody()
			if err != nil {
				return nil, err
			}
			req = req.Clone(req.Context())
			req.Body = body
		}
	}
}

func retryable(res *http.Response, err error) bool {
	if err != nil {
		return !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded)
	}
	switch res.StatusCode {
	case http.StatusRequestTimeout, http.StatusTooManyRequests,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

func retryDelay(res *http.Response, attempt int) time.Duration {
	if res != nil {
		if v := res.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(v); err == nil {
				return min(time.Duration(secs)*time.Second, retryMaxWait)
			}
			if at, err := http.ParseTime(v); err == nil {
				return min(max(time.Until(at), 0), retryMaxWait)
			}
		}
	}
	d := retryBackoff << attempt
	return d + rand.N(d/2)
}

func sleepCtx(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-t.C:
		return true
	case <-ctx.Done():
		return false
	}
}

// exact host match so tokens never travel to other origins
// git smart-HTTP needs Basic auth
// GitHub's API and codeload accept it too (x-access-token)
func hostAuth() map[string]string {
	auth := map[string]string{}
	if tok := cmp.Or(os.Getenv("GH_TOKEN"), os.Getenv("GITHUB_TOKEN")); tok != "" {
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

	h := sha256.New()
	writers := []io.Writer{h, w}
	var partPath string
	var part *os.File
	if cacheDir != "" {
		dir := filepath.Join(cacheDir, "tarballs")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return "", err
		}
		partPath = filepath.Join(dir, urlKey(url)+".part")
		f, err := os.Create(partPath)
		if err != nil {
			return "", err
		}
		part = f
		writers = append([]io.Writer{part}, writers...)
	}
	mw := io.MultiWriter(writers...)

	// a dropped body resumes where it stopped
	// with hash and part keep accumulating
	var received int64
	for attempt := 0; ; attempt++ {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return "", failDownload(part, partPath, err)
		}
		if received > 0 {
			req.Header.Set("Range", "bytes="+strconv.FormatInt(received, 10)+"-")
		}
		res, err := Client().Do(req)
		if err != nil {
			if received == 0 || attempt >= retryAttempts-1 || ctx.Err() != nil {
				return "", failDownload(part, partPath, err)
			}
			if !sleepCtx(ctx, retryDelay(nil, attempt)) {
				return "", failDownload(part, partPath, ctx.Err())
			}
			continue
		}
		if received > 0 && res.StatusCode == http.StatusOK {
			res.Body.Close()
			return "", failDownload(part, partPath, fmt.Errorf("%s: server ignored Range on retry", url))
		}
		if res.StatusCode != http.StatusOK && res.StatusCode != http.StatusPartialContent {
			res.Body.Close()
			return "", failDownload(part, partPath, fmt.Errorf("%s: HTTP %d", url, res.StatusCode))
		}
		n, err := io.Copy(mw, res.Body)
		res.Body.Close()
		received += n
		if err == nil {
			break
		}
		// the sink died but not the network
		if n == 0 || attempt >= retryAttempts-1 || ctx.Err() != nil {
			return "", failDownload(part, partPath, err)
		}
		if !sleepCtx(ctx, retryDelay(res, attempt)) {
			return "", failDownload(part, partPath, ctx.Err())
		}
	}
	if part != nil {
		if err := part.Close(); err != nil {
			os.Remove(partPath)
			return "", err
		}
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

func failDownload(part *os.File, partPath string, err error) error {
	if part != nil {
		part.Close()
		os.Remove(partPath)
	}
	return err
}

// same as Download except the blob read
func Cached(cacheDir, url string) (string, bool) {
	blob, integrity, ok := cacheEntry(cacheDir, url)
	if !ok {
		return "", false
	}
	if _, err := os.Stat(blob); err != nil {
		return "", false
	}
	return integrity, true
}

// the content-addressed blob may be shared
func Uncache(cacheDir, url string) error {
	err := os.Remove(filepath.Join(cacheDir, "tarballs", "by-url", urlKey(url)))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func serveCached(cacheDir, url string, w io.Writer) (string, bool) {
	blob, integrity, ok := cacheEntry(cacheDir, url)
	if !ok {
		return "", false
	}
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

func cacheEntry(cacheDir, url string) (blob, integrity string, ok bool) {
	if cacheDir == "" {
		return "", "", false
	}
	b, err := os.ReadFile(filepath.Join(cacheDir, "tarballs", "by-url", urlKey(url)))
	if err != nil {
		return "", "", false
	}
	integrity = string(trimNL(b))
	raw, err := base64.StdEncoding.DecodeString(strings.TrimPrefix(integrity, "sha256-"))
	if err != nil || len(raw) != sha256.Size {
		return "", "", false
	}
	return filepath.Join(cacheDir, "tarballs", hex.EncodeToString(raw)+".tar.gz"), integrity, true
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
