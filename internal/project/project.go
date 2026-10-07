package project

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strconv"
	"strings"

	"encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/t7ru/phuo/internal/mw"
)

var (
	ErrNoProject    = errors.New("no phuo.json found; run phuo init")
	ErrNoMediaWiki  = errors.New("not a MediaWiki root (missing includes/Defines.php or LocalSettings.php)")
	ErrNoLock       = errors.New("no phuo.lock found; run phuo init")
	ErrNotInstalled = errors.New("not installed")
)

type Project struct {
	Root, MWVersion, Rel string
	Manifest             Manifest
	Lock                 Lock
	Paths                struct{ Extensions, Skins string }
	CacheDir             string
}

type Manifest struct {
	Version             int               `json:"version,omitzero"`
	Extensions          map[string]string `json:"extensions,omitempty"`
	Skins               map[string]string `json:"skins,omitempty"`
	Disabled            []string          `json:"disabled,omitempty"` // lock keys, e.g. "extensions/Nuke"
	Rel                 string            `json:"rel,omitzero"`
	Paths               Paths             `json:"paths,omitzero"`
	LocalSettings       OptString         `json:"localSettings,omitzero"`
	Composer            OptString         `json:"composer,omitzero"`
	PHP                 OptString         `json:"php,omitzero"`
	Wiki                string            `json:"wiki,omitzero"`
	PatchedDependencies map[string]string `json:"patchedDependencies,omitzero"`
	Registry            string            `json:"registry,omitzero"`
	Snapshots           *int              `json:"snapshots,omitzero"`
}

// how many previous states Save keeps
func (m Manifest) SnapshotCount() int {
	if m.Snapshots == nil {
		return 1
	}
	return *m.Snapshots
}

type Paths struct {
	Extensions string `json:"extensions,omitzero"`
	Skins      string `json:"skins,omitzero"`
}

func (p Paths) IsZero() bool {
	return p.Extensions == "" && p.Skins == ""
}

// string, JSON false, or omitted
type OptString struct {
	off  bool
	path string
	set  bool
}

func (o OptString) IsZero() bool { return !o.set }

func (o OptString) Disabled() bool { return o.set && o.off }

func (o OptString) Value(def string) (string, bool) {
	if !o.set {
		return def, true
	}
	if o.off {
		return "", false
	}
	if o.path == "" {
		return def, true
	}
	return o.path, true
}

func (o *OptString) UnmarshalJSON(b []byte) error {
	o.set = true
	if string(b) == "false" {
		o.off = true
		return nil
	}
	return json.Unmarshal(b, &o.path)
}

func (o OptString) MarshalJSON() ([]byte, error) {
	if o.off {
		return []byte("false"), nil
	}
	return json.Marshal(o.path)
}

type Lock struct {
	Version   int                `json:"version"`
	MediaWiki string             `json:"mediawiki"`
	Packages  map[string]Package `json:"packages,omitzero"`
}

func (l Lock) Lookup(name string) (string, error) {
	slash := strings.Contains(name, "/")
	if slash {
		if _, ok := l.Packages[name]; ok {
			return name, nil
		}
	} else if _, ok := l.Packages["extensions/"+name]; ok {
		return "extensions/" + name, nil
	} else if _, ok := l.Packages["skins/"+name]; ok {
		return "skins/" + name, nil
	}
	var hits []string
	for key := range l.Packages {
		got := key
		if !slash {
			_, got, _ = strings.Cut(key, "/")
		}
		if strings.EqualFold(got, name) {
			hits = append(hits, key)
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return "", fmt.Errorf("%s is %w", name, ErrNotInstalled)
	}
	slices.Sort(hits)
	return "", fmt.Errorf("%s matches %s", name, strings.Join(hits, ", "))
}

type Package struct {
	Spec         string   `json:"spec"`
	Ref          string   `json:"ref,omitzero"`
	SHA          string   `json:"sha,omitzero"`
	Date         string   `json:"date,omitzero"`
	Source       string   `json:"source,omitzero"`
	Archive      string   `json:"archive,omitzero"`
	Integrity    string   `json:"integrity,omitzero"`
	Version      string   `json:"version,omitzero"`
	Requires     Requires `json:"requires,omitzero"`
	Policy       string   `json:"policy,omitzero"`
	Composer     string   `json:"composer,omitzero"`
	Dependencies []string `json:"dependencies,omitzero"`
	Patch        string   `json:"patch,omitzero"`
}

type Requires struct {
	MediaWiki  string            `json:"MediaWiki,omitzero"`
	Extensions map[string]string `json:"extensions,omitempty"`
	Skins      map[string]string `json:"skins,omitempty"`
}

func (r Requires) IsZero() bool {
	return r.MediaWiki == "" && len(r.Extensions) == 0 && len(r.Skins) == 0
}

func CacheDir(flag, env string) (string, error) {
	if flag != "" {
		return flag, nil
	}
	if env != "" {
		return env, nil
	}
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "phuo"), nil
}

func Find(cwd string) (root string, hasPhuo bool, err error) {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return "", false, err
	}
	if root, ok := walkUp(dir, func(d string) bool { return fileExists(filepath.Join(d, "phuo.json")) }); ok {
		return root, true, nil
	}
	if root, ok := walkUp(dir, isMediaWiki); ok {
		return root, false, nil
	}
	return "", false, ErrNoMediaWiki
}

func FindLock(cwd string) (string, error) {
	dir, err := filepath.Abs(cwd)
	if err != nil {
		return "", err
	}
	d, ok := walkUp(dir, func(p string) bool { return fileExists(filepath.Join(p, "phuo.lock")) })
	if !ok {
		return "", ErrNoLock
	}
	return filepath.Join(d, "phuo.lock"), nil
}

func walkUp(dir string, match func(string) bool) (string, bool) {
	for d := dir; ; {
		if match(d) {
			return d, true
		}
		parent := filepath.Dir(d)
		if parent == d {
			return "", false
		}
		d = parent
	}
}

func Load(cwd string) (*Project, error) {
	root, hasPhuo, err := Find(cwd)
	if err != nil {
		return nil, err
	}
	if !hasPhuo {
		return nil, ErrNoProject
	}
	mwVer, err := mw.Version(root)
	if err != nil {
		return nil, err
	}
	var m Manifest
	mf, err := os.Open(filepath.Join(root, "phuo.json"))
	if err != nil {
		return nil, err
	}
	defer mf.Close()
	if err := json.UnmarshalRead(mf, &m); err != nil {
		return nil, fmt.Errorf("phuo.json: %w", err)
	}
	lock, err := ReadLock(filepath.Join(root, "phuo.lock"))
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	cache, err := CacheDir("", os.Getenv("PHUO_CACHE_DIR"))
	if err != nil {
		return nil, err
	}
	p := &Project{Root: root, MWVersion: mwVer, CacheDir: cache}
	p.SetState(m, lock)
	return p, nil
}

func ReadLock(path string) (Lock, error) {
	f, err := os.Open(path)
	if err != nil {
		return Lock{}, err
	}
	defer f.Close()
	var lock Lock
	if err := json.UnmarshalRead(f, &lock); err != nil {
		return Lock{}, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return lock, nil
}

func (p *Project) SetState(m Manifest, l Lock) {
	p.Manifest, p.Lock = m, l
	p.Rel = Rel(p.MWVersion)
	if m.Rel != "" {
		p.Rel = m.Rel
	}
	p.Paths.Extensions, p.Paths.Skins = "extensions", "skins"
	if m.Paths.Extensions != "" {
		p.Paths.Extensions = m.Paths.Extensions
	}
	if m.Paths.Skins != "" {
		p.Paths.Skins = m.Paths.Skins
	}
}

// a save that changes nothing keeps no snapshot
// so repeated no-op installs don't rotate the undo point away
func (p *Project) Save() error {
	jsonPath := filepath.Join(p.Root, "phuo.json")
	lockPath := filepath.Join(p.Root, "phuo.lock")
	jsonB, err := encodeJSON(p.Manifest)
	if err != nil {
		return err
	}
	lockB, err := encodeJSON(p.Lock)
	if err != nil {
		return err
	}
	oldJSON, err := readOptional(jsonPath)
	if err != nil {
		return err
	}
	oldLock, err := readOptional(lockPath)
	if err != nil {
		return err
	}
	if bytes.Equal(jsonB, oldJSON) && bytes.Equal(lockB, oldLock) {
		return nil
	}
	if n := p.Manifest.SnapshotCount(); n > 0 && p.CacheDir != "" && oldJSON != nil && oldLock != nil {
		if err := p.snapshot(n, oldJSON, oldLock); err != nil {
			return err
		}
	}
	if err := writeJSON(lockPath, lockB); err != nil {
		return err
	}
	return writeJSON(jsonPath, jsonB)
}

func (p *Project) Previous(n int) (Manifest, Lock, bool, error) {
	if p.CacheDir == "" {
		return Manifest{}, Lock{}, false, nil
	}
	dir := filepath.Join(p.CacheDir, "snapshots", rootKey(p.Root))
	jsonB, err := readOptional(slot(dir, "phuo.json", n))
	if err != nil {
		return Manifest{}, Lock{}, false, err
	}
	lockB, err := readOptional(slot(dir, "phuo.lock", n))
	if err != nil {
		return Manifest{}, Lock{}, false, err
	}
	if jsonB == nil || lockB == nil {
		return Manifest{}, Lock{}, false, nil
	}
	var m Manifest
	if err := json.Unmarshal(jsonB, &m); err != nil {
		return Manifest{}, Lock{}, false, fmt.Errorf("snapshot phuo.json: %w", err)
	}
	var l Lock
	if err := json.Unmarshal(lockB, &l); err != nil {
		return Manifest{}, Lock{}, false, fmt.Errorf("snapshot phuo.lock: %w", err)
	}
	return m, l, true, nil
}

func encodeJSON(v any) ([]byte, error) {
	return json.Marshal(v, json.Deterministic(true), jsontext.Multiline(true), jsontext.WithIndent("\t"))
}

// missing file is nil
// any other error is real
func readOptional(path string) ([]byte, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	return b, err
}

type Stamp struct {
	SHA  string `json:"sha"`
	Ref  string `json:"ref"`
	Spec string `json:"spec"`
}

func ReadStamp(dir string) (Stamp, bool) {
	b, err := os.ReadFile(filepath.Join(dir, ".phuo.json"))
	if err != nil {
		return Stamp{}, false
	}
	var s Stamp
	if json.Unmarshal(b, &s) != nil {
		return Stamp{}, false
	}
	return s, true
}

func WriteStamp(dir string, s Stamp) error {
	b, err := json.Marshal(s)
	if err != nil {
		return err
	}
	return writeJSON(filepath.Join(dir, ".phuo.json"), append(b, '\n'))
}

func writeJSON(path string, data []byte) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".phuo-*")
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		os.Remove(f.Name())
		return err
	}
	if err := errors.Join(f.Chmod(0o644), f.Close()); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

func (p *Project) snapshot(n int, oldJSON, oldLock []byte) error {
	dir := filepath.Join(p.CacheDir, "snapshots", rootKey(p.Root))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	os.Remove(slot(dir, "phuo.lock", n))
	os.Remove(slot(dir, "phuo.json", n))
	for i := n - 1; i >= 1; i-- {
		if err := rotate(slot(dir, "phuo.lock", i), slot(dir, "phuo.lock", i+1)); err != nil {
			return err
		}
		if err := rotate(slot(dir, "phuo.json", i), slot(dir, "phuo.json", i+1)); err != nil {
			return err
		}
	}
	return errors.Join(writeJSON(slot(dir, "phuo.lock", 1), oldLock), writeJSON(slot(dir, "phuo.json", 1), oldJSON))
}

func slot(dir, name string, n int) string {
	return filepath.Join(dir, name+"."+strconv.Itoa(n))
}

func rotate(from, to string) error {
	if err := os.Rename(from, to); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	return nil
}

func rootKey(root string) string {
	key := filepath.Clean(root)
	if runtime.GOOS == "windows" {
		key = strings.ToLower(key)
	}
	sum := sha256.Sum256([]byte(key))
	return hex.EncodeToString(sum[:])
}

func Rel(v string) string {
	major, rest, _ := strings.Cut(v, ".")
	minor, _, _ := strings.Cut(rest, ".")
	return "REL" + major + "_" + minor
}

func LTSRel(v string) string {
	major, rest, _ := strings.Cut(v, ".")
	minor, _, _ := strings.Cut(rest, ".")
	m, _ := strconv.Atoi(minor)
	return fmt.Sprintf("REL%s_%d", major, m-((m-3)%4+4)%4)
}

func isMediaWiki(dir string) bool {
	return fileExists(filepath.Join(dir, "includes", "Defines.php")) &&
		fileExists(filepath.Join(dir, "LocalSettings.php"))
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}
