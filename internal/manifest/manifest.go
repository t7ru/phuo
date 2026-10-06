package manifest

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"time"

	json "encoding/json/v2"
)

type Authors []string

func (a *Authors) UnmarshalJSON(b []byte) error {
	if err := json.Unmarshal(b, (*[]string)(a)); err == nil {
		return nil
	}
	var one string
	if err := json.Unmarshal(b, &one); err != nil {
		return err
	}
	*a = Authors{one}
	return nil
}

type Manifest struct {
	Name            string  `json:"name"`
	Version         string  `json:"version"`
	Type            string  `json:"type"`
	URL             string  `json:"url"`
	License         string  `json:"license-name"`
	Author          Authors `json:"author"`
	ManifestVersion int     `json:"manifest_version"`
	Requires        struct {
		MediaWiki  string            `json:"MediaWiki"`
		Platform   map[string]any    `json:"platform"`
		Extensions map[string]string `json:"extensions"`
		Skins      map[string]string `json:"skins"`
	} `json:"requires"`
	Suggests struct {
		Extensions map[string]string `json:"extensions"`
		Skins      map[string]string `json:"skins"`
	} `json:"suggests"`
	LoadComposerAutoloader bool `json:"load_composer_autoloader"`
}

func Read(dir string) (Manifest, string, error) {
	path, kind, err := findManifest(dir)
	if err != nil {
		return Manifest{}, "", err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, "", err
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return Manifest{}, "", err
	}
	return m, kind, nil
}

func findManifest(dir string) (path, kind string, err error) {
	ext := filepath.Join(dir, "extension.json")
	if _, err := os.Stat(ext); err == nil {
		return ext, "extensions", nil
	}
	skin := filepath.Join(dir, "skin.json")
	if _, err := os.Stat(skin); err == nil {
		return skin, "skins", nil
	}
	return "", "", fmt.Errorf("no extension.json or skin.json in %s", dir)
}

type ver [4]int

func parseVer(s string) (v ver, ok bool) {
	s = strings.TrimPrefix(s, "v")
	if i := strings.IndexAny(s, "-+ "); i >= 0 {
		s = s[:i]
	}
	parts := strings.Split(s, ".")
	if len(parts) > 4 {
		return v, false
	}
	for i, p := range parts {
		if p == "*" || p == "x" {
			break
		}
		n, err := strconv.Atoi(p)
		if err != nil {
			return v, false
		}
		v[i] = n
	}
	return v, true
}

func (a ver) cmp(b ver) int { return slices.Compare(a[:], b[:]) }

func Satisfies(constraint, version string) (bool, error) {
	if strings.TrimSpace(constraint) == "" {
		return false, fmt.Errorf("empty constraint")
	}
	have, ok := parseVer(version)
	if !ok {
		return false, fmt.Errorf("invalid version %q", version)
	}
	for or := range strings.SplitSeq(constraint, "||") {
		match := true
		saw := false
		var pending string
		for atom := range strings.FieldsFuncSeq(or, func(r rune) bool { return r == ',' || r == ' ' }) {
			if pending != "" {
				atom = pending + atom
				pending = ""
			} else if isBareOp(atom) {
				pending = atom
				continue
			}
			saw = true
			ok, err := atomSatisfies(atom, have)
			if err != nil {
				return false, err
			}
			match = match && ok
		}
		if pending != "" {
			return false, fmt.Errorf("invalid constraint %q", pending)
		}
		if saw && match {
			return true, nil
		}
	}
	return false, nil
}

func isBareOp(s string) bool {
	switch s {
	case ">=", "<=", "!=", "==", ">", "<", "=", "^", "~":
		return true
	}
	return false
}

func atomSatisfies(atom string, have ver) (bool, error) {
	if atom == "*" {
		return true, nil
	}
	op := ""
	for _, o := range []string{">=", "<=", "!=", "==", ">", "<", "=", "^", "~"} {
		if rest, ok := strings.CutPrefix(atom, o); ok {
			op, atom = o, rest
			break
		}
	}
	want, ok := parseVer(atom)
	if !ok {
		return false, fmt.Errorf("invalid constraint %q", op+atom)
	}
	c := have.cmp(want)
	switch op {
	case ">=":
		return c >= 0, nil
	case ">":
		return c > 0, nil
	case "<=":
		return c <= 0, nil
	case "<":
		return c < 0, nil
	case "!=":
		return c != 0, nil
	}
	n := strings.Count(atom, ".") + 1
	var upper ver
	switch {
	case op == "^" && want[0] > 0:
		upper = ver{want[0] + 1}
	case op == "^":
		upper = ver{0, want[1] + 1}
	case op == "~" && n >= 2:
		upper = want
		upper[n-2]++
		clear(upper[n-1:])
	case strings.HasSuffix(atom, ".*") || strings.HasSuffix(atom, ".x"):
		upper = want
		upper[n-2]++
		clear(upper[n-1:])
	default:
		return c == 0, nil
	}
	return c >= 0 && have.cmp(upper) < 0, nil
}

// mediawiki writes this as a JSON number (ExtensionDistributor tarballs)
// or a quoted one (createGitInfo.php)
type epoch int64

func (e *epoch) UnmarshalJSON(b []byte) error {
	s := strings.Trim(strings.TrimSpace(string(b)), `"`)
	if s == "" || s == "null" {
		return nil
	}
	v, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return fmt.Errorf("headCommitDate %s: not a unix timestamp", b)
	}
	*e = epoch(v)
	return nil
}

type gitInfo struct {
	HeadSHA1       string `json:"headSHA1"`
	HeadCommitDate epoch  `json:"headCommitDate,omitzero"`
	RemoteURL      string `json:"remoteURL"`
}

func ReadGitInfo(dir string) (sha string, date time.Time, remote string, err error) {
	b, err := os.ReadFile(filepath.Join(dir, "gitinfo.json"))
	if err != nil {
		return "", time.Time{}, "", err
	}
	var g gitInfo
	if err := json.Unmarshal(b, &g); err != nil {
		return "", time.Time{}, "", err
	}
	return strings.TrimSpace(g.HeadSHA1), time.Unix(int64(g.HeadCommitDate), 0).UTC(), g.RemoteURL, nil
}

func WriteGitInfo(dir, sha string, date time.Time, remote string) error {
	g := gitInfo{HeadSHA1: sha, RemoteURL: remote}
	if !date.IsZero() {
		g.HeadCommitDate = epoch(date.Unix())
	}
	b, err := json.Marshal(g)
	if err != nil {
		return err
	}
	f, err := os.OpenFile(filepath.Join(dir, "gitinfo.json"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if errors.Is(err, fs.ErrExist) {
		return nil
	}
	if err != nil {
		return err
	}
	_, err = f.Write(append(b, '\n'))
	return errors.Join(err, f.Close())
}

func ReadVersionFile(dir string) (name, ref string, err error) {
	b, err := os.ReadFile(filepath.Join(dir, "version"))
	if err != nil {
		return "", "", err
	}
	line, _, _ := strings.Cut(string(b), "\n")
	name, ref, ok := strings.Cut(line, ":")
	if !ok {
		return "", "", fmt.Errorf("invalid version file: %q", line)
	}
	return strings.TrimSpace(name), strings.TrimSpace(ref), nil
}

type schemaProbe struct {
	Hooks          map[string]any `json:"Hooks"`
	HookHandlers   map[string]any `json:"HookHandlers"`
	InstallerTasks []any          `json:"InstallerTasks"`
}

func HasSchemaUpdates(dir string) (bool, error) {
	path, _, err := findManifest(dir)
	if err != nil {
		return false, err
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	var p schemaProbe
	if err := json.Unmarshal(b, &p); err != nil {
		return false, err
	}
	if _, ok := p.Hooks["LoadExtensionSchemaUpdates"]; ok {
		return true, nil
	}
	for _, h := range p.HookHandlers {
		m, _ := h.(map[string]any)
		hooks, _ := m["hooks"].(map[string]any)
		if _, ok := hooks["LoadExtensionSchemaUpdates"]; ok {
			return true, nil
		}
	}
	return len(p.InstallerTasks) > 0, nil
}
