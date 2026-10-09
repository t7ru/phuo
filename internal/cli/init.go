package cli

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/t7ru/phuo/internal/composer"
	"github.com/t7ru/phuo/internal/localsettings"
	"github.com/t7ru/phuo/internal/manifest"
	"github.com/t7ru/phuo/internal/mw"
	"github.com/t7ru/phuo/internal/project"
	"github.com/t7ru/phuo/internal/source"
	"github.com/t7ru/phuo/internal/ui"
)

func (c *InitCmd) Run(ctx context.Context, cli *CLI) error {
	cwd := cli.Cwd
	if cwd == "" {
		cwd = "."
	}
	root, hasPhuo, err := project.Find(cwd)
	if err != nil {
		return err
	}
	if hasPhuo {
		return userErr("phuo.json already exists")
	}
	ver, err := mw.Version(root)
	if err != nil {
		return err
	}
	rep := ui.New(ui.Options{
		JSON: cli.JSON, Silent: cli.Silent, Verbose: cli.Verbose,
		NoColor: cli.NoColor, NoProgress: cli.NoProgress,
	})

	m := project.Manifest{
		Version:    1,
		Extensions: map[string]string{},
		Skins:      map[string]string{},
	}
	lock := project.Lock{
		Version:   1,
		MediaWiki: ver,
		Packages:  map[string]project.Package{},
	}

	// release tarball is like ~40 bundled packages
	// unloaded ones are adopted as disabled so
	// the managed block doesn't switch them all on
	ls, err := localsettings.Scan(filepath.Join(root, "LocalSettings.php"))
	if err != nil {
		return err
	}
	var adopted, unknown []string
	scan := func(typ string) error {
		entries, err := os.ReadDir(filepath.Join(root, typ))
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		for _, e := range entries {
			name := e.Name()
			key := typ + "/" + name
			if !e.IsDir() || strings.HasPrefix(name, ".") {
				continue
			}
			pkg, manVal, ok, err := adoptDir(filepath.Join(root, typ, name), project.Rel(ver))
			if err != nil {
				return err
			}
			if !ok {
				unknown = append(unknown, key)
				continue
			}
			if typ == "skins" {
				m.Skins[name] = manVal
			} else {
				m.Extensions[name] = manVal
			}
			lock.Packages[key] = pkg
			if !ls.Outside.Has(key) && !ls.Disabled.Has(key) {
				m.Disabled = append(m.Disabled, key)
				key += " (disabled)"
			}
			adopted = append(adopted, key)
		}
		return nil
	}
	if err := scan("extensions"); err != nil {
		return err
	}
	if err := scan("skins"); err != nil {
		return err
	}

	p := &project.Project{Root: root, MWVersion: ver}
	p.SetState(m, lock)
	if err := p.Save(); err != nil {
		return err
	}
	rep.Info("created phuo.json (MediaWiki %s)", ver)
	for _, k := range adopted {
		rep.Info(" + %s", k)
	}
	for _, k := range unknown {
		rep.Warn("unmanaged: %s (no extension.json or skin.json)", k)
	}
	if len(m.Disabled) > 0 {
		rep.Info("disabled packages were not loaded by LocalSettings.php; phuo enable <name> to load one")
	}
	if c.ManageLoads {
		return adoptLoads(p, rep)
	}
	return nil
}

func adoptDir(dir, rel string) (pkg project.Package, manVal string, ok bool, err error) {
	_, gitinfoErr := os.Stat(filepath.Join(dir, "gitinfo.json"))
	_, versionErr := os.Stat(filepath.Join(dir, "version"))
	if gitinfoErr == nil && versionErr == nil {
		sha, date, remote, err := manifest.ReadGitInfo(dir)
		if err != nil {
			return pkg, "", false, err
		}
		_, ref, err := manifest.ReadVersionFile(dir)
		if err != nil {
			return pkg, "", false, err
		}
		pkg = project.Package{Spec: "*", Ref: ref, SHA: sha, Source: remote}
		if !date.IsZero() {
			pkg.Date = date.UTC().Format(time.RFC3339)
		}
		if err := project.WriteStamp(dir, project.Stamp{SHA: sha, Ref: ref, Spec: "*"}); err != nil {
			return pkg, "", false, err
		}
		return finishAdopt(dir, pkg, "*")
	}

	if fi, err := os.Stat(filepath.Join(dir, ".git")); err == nil && fi.IsDir() {
		ref, sha, err := readGitHEAD(dir)
		if err != nil {
			return pkg, "", false, err
		}
		url, err := readGitOrigin(dir)
		if err != nil {
			return pkg, "", false, err
		}
		manVal = gitSpec(url, ref)
		src := url
		if ownerRepo := source.GitHubRepo(url); ownerRepo != "" {
			// a `github:` spec is https by definition
			// an ssh origin would make the lock need the op's keys
			src = "https://github.com/" + ownerRepo
		}
		pkg = project.Package{Spec: manVal, Ref: ref, SHA: sha, Source: src}
		if err := project.WriteStamp(dir, project.Stamp{SHA: sha, Ref: ref, Spec: manVal}); err != nil {
			return pkg, "", false, err
		}
		return finishAdopt(dir, pkg, manVal)
	}

	if _, _, err := manifest.Read(dir); err != nil {
		return pkg, "", false, nil
	}
	// tarball install has neither gitinfo.json nor .git
	// phuo's own stamp is the only record of the spec and sha it installed there
	if s, ok := project.ReadStamp(dir); ok && s.Spec != "" {
		manVal = s.Spec
		if manVal == "dep" {
			manVal = "*" // `dep` is a lock-only marker and adoption makes it direct
		}
		return finishAdopt(dir, project.Package{Spec: manVal, Ref: s.Ref, SHA: s.SHA}, manVal)
	}
	// bundled at core's REL with no gitinfo.json
	// stamp so `install` doesn't replace them
	// `update` will move them to the branch head
	if err := project.WriteStamp(dir, project.Stamp{Ref: rel, Spec: "*"}); err != nil {
		return pkg, "", false, err
	}
	return finishAdopt(dir, project.Package{Spec: "*", Ref: rel}, "*")
}

func finishAdopt(dir string, pkg project.Package, manVal string) (project.Package, string, bool, error) {
	man, _, err := manifest.Read(dir)
	if err != nil {
		if _, extErr := os.Stat(filepath.Join(dir, "extension.json")); errors.Is(extErr, fs.ErrNotExist) {
			if _, skinErr := os.Stat(filepath.Join(dir, "skin.json")); errors.Is(skinErr, fs.ErrNotExist) {
				return pkg, manVal, true, nil
			}
		}
		return pkg, "", false, err
	}
	pkg.Version = man.Version
	pkg.Requires = project.Requires{
		MediaWiki:  man.Requires.MediaWiki,
		Extensions: man.Requires.Extensions,
		Skins:      man.Requires.Skins,
	}
	for name := range man.Requires.Extensions {
		pkg.Dependencies = append(pkg.Dependencies, "extensions/"+name)
	}
	for name := range man.Requires.Skins {
		pkg.Dependencies = append(pkg.Dependencies, "skins/"+name)
	}
	slices.Sort(pkg.Dependencies)
	mode, err := composer.Detect(dir, man)
	if err != nil {
		return pkg, "", false, err
	}
	pkg.Composer = string(mode)
	return pkg, manVal, true, nil
}

func gitSpec(url, ref string) string {
	if ownerRepo := source.GitHubRepo(url); ownerRepo != "" {
		s := "github:" + ownerRepo
		if ref != "" {
			s += "#" + ref
		}
		return s
	}
	u := url
	if !strings.HasPrefix(u, "git+") {
		u = "git+" + u
	}
	if ref != "" {
		u += "#" + ref
	}
	return u
}

func readGitHEAD(dir string) (ref, sha string, err error) {
	b, err := os.ReadFile(filepath.Join(dir, ".git", "HEAD"))
	if err != nil {
		return "", "", err
	}
	line := strings.TrimSpace(string(b))
	if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
		ref = rest
		if sb, err := os.ReadFile(filepath.Join(dir, ".git", "refs", "heads", ref)); err == nil {
			sha = strings.TrimSpace(string(sb))
		}
		return ref, sha, nil
	}
	return "", line, nil
}

func readGitOrigin(dir string) (string, error) {
	b, err := os.ReadFile(filepath.Join(dir, ".git", "config"))
	if err != nil {
		return "", err
	}
	inOrigin := false
	for line := range strings.SplitSeq(string(b), "\n") {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "[") {
			inOrigin = line == `[remote "origin"]`
			continue
		}
		if !inOrigin {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if ok && strings.TrimSpace(key) == "url" {
			return strings.TrimSpace(val), nil
		}
	}
	return "", fmt.Errorf("%s: no origin remote", dir)
}
