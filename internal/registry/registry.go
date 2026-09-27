package registry

import (
	"context"
	"fmt"
	"html"
	"iter"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/sahilm/fuzzy"
	"github.com/t7ru/phuo/internal/fetch"
	"golang.org/x/sync/errgroup"
)

const defaultBaseURL = "https://www.mediawiki.org/w/api.php"

type Client struct {
	HTTP     *http.Client
	BaseURL  string
	CacheDir string
	Offline  bool
	NoCache  bool

	mu   sync.Mutex
	memo map[string]*Branches // "extensions/Name" -> nil when the registry lacks it
}

type Branches struct {
	Refs   map[string]string
	Source string
}

type Policy struct {
	Kind           string
	Status         string
	Description    string
	NeedsUpdatePHP bool
}

type SearchHit struct {
	Name        string
	Snippet     string
	Installable bool
	Skin        bool
}

func (c *Client) httpClient() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return fetch.Client()
}

func (c *Client) base() string {
	if c.BaseURL != "" {
		return c.BaseURL
	}
	return defaultBaseURL
}

func (c *Client) Repos(ctx context.Context) (exts, skins []string, err error) {
	q := url.Values{
		"action":        {"query"},
		"list":          {"extdistrepos"},
		"format":        {"json"},
		"formatversion": {"2"},
	}
	var r struct {
		Query struct {
			Repos struct {
				Extensions []string `json:"extensions"`
				Skins      []string `json:"skins"`
			} `json:"extdistrepos"`
		} `json:"query"`
	}
	if err := c.getJSON(ctx, q, 24*time.Hour, &r); err != nil {
		return nil, nil, err
	}
	return r.Query.Repos.Extensions, r.Query.Repos.Skins, nil
}

func (c *Client) Branches(ctx context.Context, exts, skins []string) (map[string]Branches, map[string]Branches, error) {
	type resp struct {
		Query struct {
			EDB struct {
				Extensions map[string]map[string]string `json:"extensions"`
				Skins      map[string]map[string]string `json:"skins"`
			} `json:"extdistbranches"`
		} `json:"query"`
	}
	exOut, skOut := map[string]Branches{}, map[string]Branches{}
	c.mu.Lock()
	if c.memo == nil {
		c.memo = map[string]*Branches{}
	}
	miss := func(typ string, names []string, out map[string]Branches) (m []string) {
		for _, n := range names {
			b, ok := c.memo[typ+n]
			if !ok {
				m = append(m, n)
			} else if b != nil {
				out[n] = *b
			}
		}
		return m
	}
	exts, skins = miss("extensions/", exts, exOut), miss("skins/", skins, skOut)
	c.mu.Unlock()
	var reqs []url.Values
	for e, s := range zipChunks(exts, skins, 50) {
		v := url.Values{
			"action":        {"query"},
			"list":          {"extdistbranches"},
			"format":        {"json"},
			"formatversion": {"2"},
		}
		if len(e) > 0 {
			v.Set("edbexts", strings.Join(e, "|"))
		}
		if len(s) > 0 {
			v.Set("edbskins", strings.Join(s, "|"))
		}
		reqs = append(reqs, v)
	}
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for _, v := range reqs {
		g.Go(func() error {
			var r resp
			if err := c.getJSON(ctx, v, 0, &r); err != nil {
				return err
			}
			c.mu.Lock()
			defer c.mu.Unlock()
			for name, refs := range r.Query.EDB.Extensions {
				exOut[name] = toBranches(refs)
			}
			for name, refs := range r.Query.EDB.Skins {
				skOut[name] = toBranches(refs)
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, nil, err
	}
	c.mu.Lock()
	for _, n := range exts {
		c.memo["extensions/"+n] = ptr(exOut, n)
	}
	for _, n := range skins {
		c.memo["skins/"+n] = ptr(skOut, n)
	}
	c.mu.Unlock()
	return exOut, skOut, nil
}

func ptr(m map[string]Branches, n string) *Branches {
	if b, ok := m[n]; ok {
		return &b
	}
	return nil
}

func toBranches(m map[string]string) Branches {
	b := Branches{Refs: m, Source: m["source"]}
	delete(m, "source")
	return b
}

func zipChunks(exts, skins []string, n int) iter.Seq2[[]string, []string] {
	return func(yield func([]string, []string) bool) {
		for i := 0; i < len(exts) || i < len(skins); i += n {
			var e, s []string
			if i < len(exts) {
				e = exts[i:min(i+n, len(exts))]
			}
			if i < len(skins) {
				s = skins[i:min(i+n, len(skins))]
			}
			if !yield(e, s) {
				return
			}
		}
	}
}

func (c *Client) Snapshots(ctx context.Context) ([]string, error) {
	q := url.Values{
		"action":        {"query"},
		"meta":          {"siteinfo"},
		"siprop":        {"general"},
		"format":        {"json"},
		"formatversion": {"2"},
	}
	var r struct {
		Query struct {
			General struct {
				ExtensionDistributor struct {
					Snapshots []string `json:"snapshots"`
				} `json:"extensiondistributor"`
			} `json:"general"`
		} `json:"query"`
	}
	if err := c.getJSON(ctx, q, 24*time.Hour, &r); err != nil {
		return nil, err
	}
	return r.Query.General.ExtensionDistributor.Snapshots, nil
}

var (
	rePolicy = regexp.MustCompile(`(?i)\|\s*compatibility policy\s*=\s*(rel|master|ltsrel|main)`)
	reStatus = regexp.MustCompile(`(?i)\|\s*status\s*=\s*([^\n|{]+)`)
	reDesc   = regexp.MustCompile(`(?i)\|\s*description\s*=\s*([^\n|{]+)`)
	reUpdate = regexp.MustCompile(`(?i)\|\s*needs-updatephp\s*=\s*yes\b`)
	reTag    = regexp.MustCompile(`<[^>]*>`)
)

// descriptions carry <translate>/<!--T:n-->
// from that stupid i18n extension
// while search snippets carry HTML
func plain(s string) string {
	return strings.Join(strings.Fields(html.UnescapeString(reTag.ReplaceAllString(s, ""))), " ")
}

func (c *Client) Policies(ctx context.Context, exts, skins []string) (map[string]Policy, error) {
	out := make(map[string]Policy, len(exts)+len(skins))
	type want struct {
		key, title string
	}
	var miss []want
	for _, name := range exts {
		key := "extensions/" + name
		title := "Extension:" + name
		if p, ok := c.readPolicyCache(title); ok {
			out[key] = p
			continue
		}
		miss = append(miss, want{key, title})
	}
	for _, name := range skins {
		key := "skins/" + name
		title := "Skin:" + name
		if p, ok := c.readPolicyCache(title); ok {
			out[key] = p
			continue
		}
		miss = append(miss, want{key, title})
	}
	if len(miss) == 0 {
		return out, nil
	}
	if c.Offline {
		return nil, fmt.Errorf("offline: policy cache miss")
	}

	var mu sync.Mutex
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(4)
	for i := 0; i < len(miss); i += 50 {
		chunk := miss[i:min(i+50, len(miss))]
		g.Go(func() error {
			titles := make([]string, len(chunk))
			for j, w := range chunk {
				titles[j] = w.title
			}
			q := url.Values{
				"action":        {"query"},
				"prop":          {"revisions"},
				"titles":        {strings.Join(titles, "|")},
				"redirects":     {"1"},
				"rvslots":       {"main"},
				"rvprop":        {"content"},
				"format":        {"json"},
				"formatversion": {"2"},
			}
			var r struct {
				Query struct {
					Redirects []struct {
						From string `json:"from"`
						To   string `json:"to"`
					} `json:"redirects"`
					Pages []struct {
						Title     string `json:"title"`
						Missing   bool   `json:"missing"`
						Revisions []struct {
							Slots struct {
								Main struct {
									Content string `json:"content"`
								} `json:"main"`
							} `json:"slots"`
						} `json:"revisions"`
					} `json:"pages"`
				} `json:"query"`
			}
			if err := c.getJSON(ctx, q, 0, &r); err != nil {
				return err
			}
			redir := map[string]string{}
			for _, rd := range r.Query.Redirects {
				redir[rd.From] = rd.To
			}
			byTitle := map[string]Policy{}
			for _, pg := range r.Query.Pages {
				p := Policy{Kind: "rel"}
				if !pg.Missing && len(pg.Revisions) > 0 {
					p = parsePolicy(pg.Revisions[0].Slots.Main.Content)
				}
				byTitle[pg.Title] = p
			}
			mu.Lock()
			defer mu.Unlock()
			for _, w := range chunk {
				title := w.title
				if to, ok := redir[title]; ok {
					title = to
				}
				p, ok := byTitle[title]
				if !ok {
					p = Policy{Kind: "rel"}
				}
				out[w.key] = p
				c.writePolicyCache(w.title, p)
			}
			return nil
		})
	}
	return out, g.Wait()
}

func parsePolicy(wikitext string) Policy {
	p := Policy{Kind: "rel"}
	if m := rePolicy.FindStringSubmatch(wikitext); m != nil {
		p.Kind = strings.ToLower(m[1])
	}
	if m := reStatus.FindStringSubmatch(wikitext); m != nil {
		p.Status = strings.TrimSpace(m[1])
	}
	if m := reDesc.FindStringSubmatch(wikitext); m != nil {
		p.Description = plain(m[1])
	}
	p.NeedsUpdatePHP = reUpdate.MatchString(wikitext)
	return p
}

func (c *Client) Search(ctx context.Context, q string) ([]SearchHit, error) {
	v := url.Values{
		"action":        {"query"},
		"list":          {"search"},
		"srnamespace":   {"102|106"},
		"srsearch":      {q},
		"srprop":        {"snippet"},
		"srlimit":       {"50"},
		"format":        {"json"},
		"formatversion": {"2"},
	}
	var r struct {
		Query struct {
			Search []struct {
				Ns      int    `json:"ns"`
				Title   string `json:"title"`
				Snippet string `json:"snippet"`
			} `json:"search"`
		} `json:"query"`
	}
	if err := c.getJSON(ctx, v, time.Hour, &r); err != nil {
		return nil, err
	}
	exts, skins, err := c.Repos(ctx)
	if err != nil {
		return nil, err
	}
	inRepos := make(map[string]bool, len(exts)+len(skins))
	for _, n := range exts {
		inRepos[n] = true
	}
	for _, n := range skins {
		inRepos[n] = true
	}
	var hits []SearchHit
	for _, s := range r.Query.Search {
		if strings.Contains(s.Title, "/") {
			continue
		}
		name := s.Title
		if rest, ok := strings.CutPrefix(name, "Extension:"); ok {
			name = rest
		} else if rest, ok := strings.CutPrefix(name, "Skin:"); ok {
			name = rest
		}
		// page titles may be spaced ("Minerva Neue") where the repo is not ("MinervaNeue")
		if joined := strings.ReplaceAll(name, " ", ""); !inRepos[name] && inRepos[joined] {
			name = joined
		}
		hits = append(hits, SearchHit{
			Name:        name,
			Snippet:     plain(s.Snippet),
			Installable: inRepos[name],
			Skin:        s.Ns == 106,
		})
	}
	return hits, nil
}

func (c *Client) Suggest(name string) []string {
	exts, skins, ok := c.reposFromCache()
	if !ok {
		return nil
	}
	all := make([]string, 0, len(exts)+len(skins))
	all = append(all, exts...)
	all = append(all, skins...)
	lower := strings.ToLower(name)
	for _, n := range all {
		if strings.ToLower(n) == lower {
			return []string{n}
		}
	}
	matches := fuzzy.Find(name, all)
	var out []string
	for _, m := range matches {
		if levenshtein(lower, strings.ToLower(m.Str)) > 2 {
			continue
		}
		out = append(out, m.Str)
		if len(out) == 3 {
			break
		}
	}
	return out
}

func levenshtein(a, b string) int {
	if a == b {
		return 0
	}
	la, lb := len(a), len(b)
	if la == 0 {
		return lb
	}
	if lb == 0 {
		return la
	}
	if abs(la-lb) > 2 {
		return abs(la - lb)
	}
	prev := make([]int, lb+1)
	cur := make([]int, lb+1)
	for j := 0; j <= lb; j++ {
		prev[j] = j
	}
	for i := 1; i <= la; i++ {
		cur[0] = i
		rowMin := cur[0]
		for j := 1; j <= lb; j++ {
			cost := 1
			if a[i-1] == b[j-1] {
				cost = 0
			}
			del := prev[j] + 1
			ins := cur[j-1] + 1
			sub := prev[j-1] + cost
			cur[j] = min(del, ins, sub)
			if cur[j] < rowMin {
				rowMin = cur[j]
			}
		}
		if rowMin > 2 {
			return rowMin
		}
		prev, cur = cur, prev
	}
	return prev[lb]
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}
