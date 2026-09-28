package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	json "encoding/json/v2"

	"github.com/t7ru/phuo/internal/composer"
	"github.com/t7ru/phuo/internal/fetch"
	"github.com/t7ru/phuo/internal/localsettings"
	"github.com/t7ru/phuo/internal/manifest"
	"github.com/t7ru/phuo/internal/mw"
	"github.com/t7ru/phuo/internal/patch"
	"github.com/t7ru/phuo/internal/project"
)

func (c *DoctorCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	var problems []string
	problem := func(format string, args ...any) {
		msg := fmt.Sprintf(format, args...)
		problems = append(problems, msg)
		rep.Info("%s", msg)
	}

	dirInstalled := func(typ, name string) bool {
		key := typ + "/" + name
		if _, ok := p.Lock.Packages[key]; ok {
			return true
		}
		_, err := os.Stat(packageDir(p, key))
		return err == nil
	}

	plat, platOK := mw.Platform{}, false
	if !p.Manifest.PHP.Disabled() {
		bin, _ := p.Manifest.PHP.Value("php")
		php := mw.PHP{Bin: bin, Root: p.Root}
		if php.Available() {
			plat, err = php.Platform(ctx)
			if err != nil {
				rep.Warn("php: %s", firstLine(err.Error()))
			} else {
				platOK = true
			}
		} else {
			rep.Warn("php not found; platform requirements not checked")
		}
	}

	for key := range p.Lock.Packages {
		dir := packageDir(p, key)
		man, _, err := manifest.Read(dir)
		if err != nil {
			continue
		}
		if mwReq := man.Requires.MediaWiki; mwReq != "" {
			ok, err := manifest.Satisfies(mwReq, p.MWVersion)
			if err != nil {
				problem("%s: requires.MediaWiki %s: %v", key, mwReq, err)
			} else if !ok {
				problem("%s: requires MediaWiki %s (have %s)", key, mwReq, p.MWVersion)
			}
		}
		for name := range man.Requires.Extensions {
			if !dirInstalled("extensions", name) {
				problem("%s: requires extensions/%s (not installed)", key, name)
			}
		}
		for name := range man.Requires.Skins {
			if !dirInstalled("skins", name) {
				problem("%s: requires skins/%s (not installed)", key, name)
			}
		}
		if platOK {
			for _, msg := range man.UnmetPlatform(plat.PHP, plat.Modules, plat.Abilities) {
				problem("%s: %s", key, msg)
			}
		}
	}

	if !p.Manifest.LocalSettings.Disabled() {
		lsPath, ok := p.Manifest.LocalSettings.Value("LocalSettings.php")
		if ok {
			if !filepath.IsAbs(lsPath) {
				lsPath = filepath.Join(p.Root, lsPath)
			}
			if _, err := os.Stat(lsPath); err == nil {
				ls, err := localsettings.Scan(lsPath)
				if err != nil {
					problem("%v", err)
				} else {
					for _, l := range []localsettings.Loads{ls.Outside, ls.InBlock} {
						for _, key := range l.Keys() {
							if _, err := os.Stat(packageDir(p, key)); err != nil {
								problem("%s: loaded but missing", key)
							}
						}
					}
					off := func(k string) bool {
						return !ls.Outside.Has(k) && !ls.InBlock.Has(k) && (ls.Disabled.Has(k) || slices.Contains(p.Manifest.Disabled, k))
					}
					for key := range p.Lock.Packages {
						if ls.Outside.Has(key) || ls.InBlock.Has(key) {
							continue
						}
						if !off(key) {
							problem("%s: installed but not loaded", key)
							continue
						}
						typ, name, _ := strings.Cut(key, "/")
						var by []string
						for k, pkg := range p.Lock.Packages {
							m := pkg.Requires.Extensions
							if typ == "skins" {
								m = pkg.Requires.Skins
							}
							if _, ok := m[name]; ok && !off(k) {
								by = append(by, k)
							}
						}
						where := "disabled"
						if ls.Disabled.Has(key) {
							where = fmt.Sprintf("commented out at LocalSettings.php:%d", ls.Line[key])
						}
						if len(by) > 0 {
							slices.Sort(by)
							problem("%s: %s, but required by %s", key, where, strings.Join(by, ", "))
						}
					}
				}
			}
		}
	}

	if err := doctorComposer(p, problem, rep.Warn); err != nil {
		return err
	}

	for _, pair := range [][2]string{{p.Paths.Extensions, "extensions/"}, {p.Paths.Skins, "skins/"}} {
		dir := filepath.Join(p.Root, pair[0])
		entries, err := os.ReadDir(dir)
		if err != nil {
			continue
		}
		for _, e := range entries {
			name := e.Name()
			if name == ".phuo-tmp" || strings.HasSuffix(name, ".phuo-old") {
				problem("%s/%s: leftover", pair[0], name)
				continue
			}
			if strings.HasPrefix(name, ".") {
				continue
			}
			full := filepath.Join(dir, name)
			fi, err := os.Lstat(full)
			if err != nil || !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
				continue
			}
			key := pair[1] + name
			if _, ok := p.Lock.Packages[key]; !ok {
				rep.Warn("%s: unmanaged directory", key)
			}
		}
	}

	gitWarned := false
	for key, rel := range p.Manifest.PatchedDependencies {
		if _, err := exec.LookPath("git"); err != nil {
			if !gitWarned {
				rep.Warn("git not found; skipping patch checks")
				gitWarned = true
			}
			break
		}
		path := rel
		if !filepath.IsAbs(path) {
			path = filepath.Join(p.Root, rel)
		}
		if err := patch.Check(ctx, path, packageDir(p, key)); err != nil {
			problem("%s: patch %s does not apply: %v", key, rel, err)
		}
	}

	if c.PHP && !p.Manifest.PHP.Disabled() {
		bin, _ := p.Manifest.PHP.Value("php")
		php := mw.PHP{Bin: bin, Root: p.Root}
		if php.Available() {
			doctorPHP(ctx, p, php, problem, rep.Warn)
		}
	}

	if c.Wiki {
		if p.Manifest.Wiki == "" {
			return userErr(`set "wiki" in phuo.json, e.g. {"wiki":"https://wiki.example.org/w/api.php"}`)
		}
		ver, _, _, err := mw.Remote(ctx, fetch.Client(), p.Manifest.Wiki)
		if err != nil {
			problem("wiki: %v", err)
		} else if ver != "" && ver != p.MWVersion {
			problem("wiki: remote MediaWiki %s (local %s)", ver, p.MWVersion)
		}
	}

	if cli.JSON {
		if problems == nil {
			problems = []string{}
		}
		if err := json.MarshalWrite(os.Stdout, problems); err != nil {
			return err
		}
	}
	if len(problems) > 0 {
		return userErr(fmt.Sprintf("%d problem(s)", len(problems)))
	}
	rep.Step("ok")
	return nil
}

func doctorComposer(p *project.Project, problem func(string, ...any), warn func(string, ...any)) error {
	path := filepath.Join(p.Root, "composer.local.json")
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			for key, pkg := range p.Lock.Packages {
				if pkg.Composer == string(composer.ModeMerged) {
					problem("%s: composer merged but composer.local.json missing", key)
				}
			}
			return nil
		}
		return err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return err
	}
	include := composerIncludes(doc)
	hasExtGlob := slices.Contains(include, "extensions/*/composer.json")
	hasSkinGlob := slices.Contains(include, "skins/*/composer.json")
	for key, pkg := range p.Lock.Packages {
		if pkg.Composer != string(composer.ModeMerged) {
			continue
		}
		entry := key + "/composer.json"
		if slices.Contains(include, entry) {
			continue
		}
		if strings.HasPrefix(key, "skins/") && hasSkinGlob {
			continue
		}
		if strings.HasPrefix(key, "extensions/") && hasExtGlob {
			continue
		}
		problem("%s: missing from composer.local.json include", key)
	}
	localInfo, err1 := os.Stat(path)
	instInfo, err2 := os.Stat(filepath.Join(p.Root, "vendor", "composer", "installed.json"))
	if err1 == nil && err2 == nil && instInfo.ModTime().Before(localInfo.ModTime()) {
		warn("composer.lock stale: vendor/composer/installed.json older than composer.local.json")
	}
	return nil
}

func composerIncludes(doc map[string]any) []string {
	extra, _ := doc["extra"].(map[string]any)
	if extra == nil {
		return nil
	}
	mp, _ := extra["merge-plugin"].(map[string]any)
	if mp == nil {
		return nil
	}
	arr, _ := mp["include"].([]any)
	var out []string
	for _, e := range arr {
		if s, ok := e.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func doctorPHP(ctx context.Context, p *project.Project, php mw.PHP, problem, warn func(string, ...any)) {
	loaded, err := php.Registry(ctx)
	if err != nil {
		warn("php: %s", firstLine(err.Error()))
		return
	}
	regSet := map[string]bool{}
	for _, l := range loaded {
		typ := "extensions"
		if strings.EqualFold(l.Type, "skin") || strings.EqualFold(l.Type, "skins") {
			typ = "skins"
		}
		regSet[typ+"/"+l.Name] = true
	}
	for key := range p.Lock.Packages {
		if !regSet[key] {
			problem("%s: in lock but not in PHP ExtensionRegistry", key)
		}
	}
	cfg, err := php.Config(ctx, "wgExtensionDirectory", "wgStyleDirectory")
	if err != nil {
		warn("php: %s", firstLine(err.Error()))
	} else {
		if v, ok := cfg["wgExtensionDirectory"].(string); ok && v != "" {
			want := filepath.Join(p.Root, p.Paths.Extensions)
			if filepath.Clean(v) != filepath.Clean(want) {
				problem("wgExtensionDirectory=%s (expected %s)", v, want)
			}
		}
		if v, ok := cfg["wgStyleDirectory"].(string); ok && v != "" {
			want := filepath.Join(p.Root, p.Paths.Skins)
			if filepath.Clean(v) != filepath.Clean(want) {
				problem("wgStyleDirectory=%s (expected %s)", v, want)
			}
		}
	}
	ok, msg, err := php.ComposerLockUpToDate(ctx)
	if err != nil {
		warn("php: %s", firstLine(err.Error()))
	} else if !ok {
		problem("composer.lock not up to date: %s", firstLine(msg))
	}
}

func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return strings.TrimSpace(s[:i])
	}
	return s
}
