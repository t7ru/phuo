package registry

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"encoding/json/jsontext"
	json "encoding/json/v2"
)

type cacheEntry struct {
	Expires      time.Time      `json:"expires"`
	ETag         string         `json:"etag,omitzero"`
	LastModified string         `json:"lastModified,omitzero"`
	Body         jsontext.Value `json:"body"`
}

func (c *Client) metaPath(key string) string {
	sum := sha256.Sum256([]byte(key))
	return filepath.Join(c.CacheDir, "meta", hex.EncodeToString(sum[:]))
}

func (c *Client) readCache(key string) (e cacheEntry, hit bool) {
	if c.CacheDir == "" || c.NoCache {
		return e, false
	}
	return c.readCacheFile(key)
}

func (c *Client) readCacheFile(key string) (e cacheEntry, hit bool) {
	if c.CacheDir == "" {
		return e, false
	}
	b, err := os.ReadFile(c.metaPath(key))
	if err != nil {
		return e, false
	}
	if err := json.Unmarshal(b, &e); err != nil {
		return e, false
	}
	return e, true
}

func (c *Client) writeCache(key string, e cacheEntry) {
	if c.CacheDir == "" {
		return
	}
	dir := filepath.Join(c.CacheDir, "meta")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	path := c.metaPath(key)
	tmp, err := os.CreateTemp(dir, ".phuo-*")
	if err != nil {
		return
	}
	tmpName := tmp.Name()
	if err := json.MarshalWrite(tmp, e); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		return
	}
	if err := tmp.Close(); err != nil {
		os.Remove(tmpName)
		return
	}
	os.Rename(tmpName, path)
}

func (c *Client) getJSON(ctx context.Context, q url.Values, ttl time.Duration, dest any) error {
	u := c.base() + "?" + q.Encode()
	body, err := c.getBody(ctx, u, ttl)
	if err != nil {
		return err
	}
	if err := checkAPIError(body); err != nil {
		return err
	}
	return json.Unmarshal(body, dest)
}

func (c *Client) getBody(ctx context.Context, u string, ttl time.Duration) ([]byte, error) {
	if ttl > 0 {
		if e, hit := c.readCache(u); hit && time.Now().Before(e.Expires) {
			return e.Body, nil
		}
		if c.Offline {
			if !c.NoCache {
				if e, hit := c.readCacheFile(u); hit {
					return e.Body, nil
				}
			}
			return nil, fmt.Errorf("offline: cache miss")
		}
		prior, _ := c.readCacheFile(u)
		if c.NoCache {
			prior = cacheEntry{}
		}
		return c.fetch(ctx, u, prior, ttl)
	}
	if c.Offline {
		return nil, fmt.Errorf("offline: cache miss")
	}
	return c.fetch(ctx, u, cacheEntry{}, 0)
}

func (c *Client) fetch(ctx context.Context, u string, prior cacheEntry, ttl time.Duration) ([]byte, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	if prior.ETag != "" {
		req.Header.Set("If-None-Match", prior.ETag)
	}
	if prior.LastModified != "" {
		req.Header.Set("If-Modified-Since", prior.LastModified)
	}
	res, err := c.httpClient().Do(req)
	if err != nil {
		return nil, err
	}
	defer res.Body.Close()

	switch res.StatusCode {
	case http.StatusNotModified:
		if len(prior.Body) == 0 {
			return nil, fmt.Errorf("%s: HTTP 304 without cached body", u)
		}
		if ttl > 0 {
			prior.Expires = time.Now().Add(ttl)
			c.writeCache(u, prior)
		}
		return prior.Body, nil
	case http.StatusOK:
		body, err := io.ReadAll(res.Body)
		if err != nil {
			return nil, err
		}
		if ttl > 0 {
			c.writeCache(u, cacheEntry{
				Expires:      time.Now().Add(ttl),
				ETag:         res.Header.Get("ETag"),
				LastModified: res.Header.Get("Last-Modified"),
				Body:         body,
			})
		}
		return body, nil
	default:
		return nil, fmt.Errorf("%s: HTTP %d", u, res.StatusCode)
	}
}

func checkAPIError(body []byte) error {
	var e struct {
		Error *struct {
			Code string `json:"code"`
			Info string `json:"info"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err != nil {
		return err
	}
	if e.Error != nil {
		return fmt.Errorf("api %s: %s", e.Error.Code, e.Error.Info)
	}
	return nil
}

func (c *Client) readPolicyCache(title string) (Policy, bool) {
	key := "policy:" + title
	e, hit := c.readCache(key)
	if hit && time.Now().Before(e.Expires) {
		var p Policy
		if err := json.Unmarshal(e.Body, &p); err == nil {
			return p, true
		}
		return Policy{}, false
	}
	if c.Offline && !c.NoCache {
		e, hit = c.readCacheFile(key)
		if !hit {
			return Policy{}, false
		}
		var p Policy
		if err := json.Unmarshal(e.Body, &p); err == nil {
			return p, true
		}
	}
	return Policy{}, false
}

func (c *Client) writePolicyCache(title string, p Policy) {
	body, err := json.Marshal(p)
	if err != nil {
		return
	}
	c.writeCache("policy:"+title, cacheEntry{
		Expires: time.Now().Add(7 * 24 * time.Hour),
		Body:    body,
	})
}

func (c *Client) reposFromCache() (exts, skins []string, ok bool) {
	q := url.Values{
		"action":        {"query"},
		"list":          {"extdistrepos"},
		"format":        {"json"},
		"formatversion": {"2"},
	}
	u := c.base() + "?" + q.Encode()
	e, hit := c.readCacheFile(u)
	if !hit {
		return nil, nil, false
	}
	var r struct {
		Query struct {
			Repos struct {
				Extensions []string `json:"extensions"`
				Skins      []string `json:"skins"`
			} `json:"extdistrepos"`
		} `json:"query"`
	}
	if err := json.Unmarshal(e.Body, &r); err != nil {
		return nil, nil, false
	}
	return r.Query.Repos.Extensions, r.Query.Repos.Skins, true
}
