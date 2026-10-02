package spec

import (
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

type Kind uint8

const (
	Registry Kind = iota
	GitHub
	GitLab
	Git
	Archive
	Local
)

type Spec struct {
	Name string // install dir name; empty for registry ref-only values
	Kind Kind
	Repo string
	Ref  string
	Skin bool
}

var (
	nameRe    = regexp.MustCompile(`^[A-Za-z0-9_.\-]+$`)
	refOnlyRe = regexp.MustCompile(`^(REL\d+_\d+|master|main)$`)
)

func Parse(s string) (Spec, error) {
	var p Spec
	if rest, ok := strings.CutPrefix(s, "skin:"); ok {
		p.Skin, s = true, rest
	}
	if norm, skin, ok, wiki := normalizeMediaWiki(s); ok || wiki {
		if !ok {
			return p, fmt.Errorf("unrecognised spec %q", s)
		}
		p.Skin = p.Skin || skin
		s = norm
	}
	// "Alias@<source>" — but not "git@host:..." and not a credentialed URL ("https://u:p@host")
	if !strings.HasPrefix(s, "git@") && !strings.Contains(s, "://") {
		if name, src, ok := strings.Cut(s, "@"); ok && nameRe.MatchString(name) {
			p.Name, s = name, src
		}
	}
	s, p.Ref, _ = strings.Cut(s, "#")
	switch {
	case s == "" || s == "*":
		p.Kind = Registry
	case strings.HasPrefix(s, "github:"):
		p.Kind, p.Repo = GitHub, s[7:]
	case strings.HasPrefix(s, "gitlab:"):
		p.Kind, p.Repo = GitLab, s[7:]
	case strings.HasPrefix(s, "git+"), strings.HasPrefix(s, "git@"), strings.HasSuffix(s, ".git"):
		p.Kind, p.Repo = Git, s // keep git+ so String round-trips
	case strings.HasPrefix(s, "file:"):
		p.Kind, p.Repo = Local, s[5:]
	case strings.Contains(s, "://"):
		p.Kind, p.Repo = Archive, s
	case nameRe.MatchString(s) && p.Name == "":
		// bare REL*/master/main are phuo.json ref values
		if refOnlyRe.MatchString(s) {
			p.Kind, p.Ref = Registry, s
		} else {
			p.Kind, p.Name = Registry, s
		}
	case nameRe.MatchString(s):
		p.Kind, p.Ref = Registry, s
	default:
		return p, fmt.Errorf("unrecognised spec %q", s)
	}
	if p.Kind == GitHub || p.Kind == GitLab {
		o, r, ok := strings.Cut(p.Repo, "/")
		if !ok || o == "" || r == "" || strings.Contains(r, "/") {
			kind := "github"
			if p.Kind == GitLab {
				kind = "gitlab"
			}
			return p, fmt.Errorf("%s spec must be owner/repo, got %q", kind, p.Repo)
		}
	}
	if p.Name == "" && p.Kind != Registry {
		p.Name = deriveName(p.Repo)
	}
	return p, nil
}

func (s Spec) String() string {
	body := s.body()
	if s.Skin {
		return "skin:" + body
	}
	return body
}

func (s Spec) body() string {
	switch s.Kind {
	case Registry:
		switch {
		case s.Name != "" && s.Ref != "":
			return s.Name + "@" + s.Ref
		case s.Name != "":
			return s.Name
		case s.Ref != "":
			return s.Ref
		default:
			return "*"
		}
	case GitHub:
		return s.alias("github:" + s.Repo)
	case GitLab:
		return s.alias("gitlab:" + s.Repo)
	case Git:
		return s.alias(s.Repo)
	case Archive:
		return s.alias(s.Repo)
	case Local:
		return s.alias("file:" + s.Repo)
	default:
		return "*"
	}
}

func (s Spec) alias(src string) string {
	if s.Ref != "" {
		src += "#" + s.Ref
	}
	if s.Name != "" && s.Name != deriveName(s.Repo) {
		return s.Name + "@" + src
	}
	return src
}

// Extension:Foo, Skin:Foo, and mediawiki.org page URLs are registry names.
// wiki is true for a mediawiki.org URL even when the title is not a package.
func normalizeMediaWiki(s string) (norm string, skin, ok, wiki bool) {
	raw := s
	if strings.Contains(s, "://") {
		u, err := url.Parse(s)
		if err != nil || !strings.EqualFold(strings.TrimPrefix(u.Hostname(), "www."), "mediawiki.org") {
			return "", false, false, false
		}
		wiki = true
		raw = strings.Trim(strings.TrimPrefix(u.EscapedPath(), "/wiki/"), "/")
		if title := u.Query().Get("title"); title != "" && strings.HasSuffix(u.Path, "/index.php") {
			raw = title
		}
		if decoded, err := url.PathUnescape(raw); err == nil {
			raw = decoded
		}
	}
	switch {
	case len(raw) > len("Extension:") && strings.EqualFold(raw[:len("Extension:")], "Extension:"):
		raw = raw[len("Extension:"):]
	case len(raw) > len("Skin:") && strings.EqualFold(raw[:len("Skin:")], "Skin:"):
		raw = raw[len("Skin:"):]
		skin = true
	default:
		return "", false, false, wiki
	}
	name, _, _ := strings.Cut(raw, "@")
	name, _, _ = strings.Cut(name, "#")
	if !nameRe.MatchString(name) {
		return "", false, false, wiki
	}
	return raw, skin, true, wiki
}

func deriveName(repo string) string {
	s := strings.TrimPrefix(repo, "git+")
	s = strings.TrimSuffix(s, "/")
	if i := strings.LastIndexAny(s, "/:"); i >= 0 {
		s = s[i+1:]
	}
	s = strings.TrimSuffix(s, ".git")
	for _, p := range []string{"mediawiki-extensions-", "mediawiki-skins-", "mediawiki-extension-"} {
		if rest, ok := strings.CutPrefix(s, p); ok {
			return rest
		}
	}
	return s
}
