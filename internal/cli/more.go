package cli

import (
	"cmp"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	json "encoding/json/v2"

	"github.com/t7ru/phuo/internal/archive"
	"github.com/t7ru/phuo/internal/fetch"
	"github.com/t7ru/phuo/internal/localsettings"
	"github.com/t7ru/phuo/internal/manifest"
	"github.com/t7ru/phuo/internal/project"
	"github.com/t7ru/phuo/internal/registry"
	"github.com/t7ru/phuo/internal/source"
	"github.com/t7ru/phuo/internal/spec"
	"github.com/t7ru/phuo/internal/ui"
	"golang.org/x/sync/errgroup"
)

type LicensesCmd struct{}

func (c *LicensesCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	groups := map[string][]string{}
	for key := range p.Lock.Packages {
		dir := packageDir(p, key)
		lic := "Unknown"
		if man, _, err := manifest.Read(dir); err == nil && man.License != "" {
			lic = man.License
		}
		groups[lic] = append(groups[lic], keyName(key))
	}
	for _, names := range groups {
		slices.Sort(names)
	}
	keys := make([]string, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	if cli.JSON {
		return json.MarshalWrite(os.Stdout, groups, json.Deterministic(true))
	}
	for _, lic := range keys {
		rep.Info("%s", lic)
		for _, name := range groups[lic] {
			rep.Info("  %s", name)
		}
	}
	return nil
}

type OutdatedCmd struct {
	Patterns []string `arg:"" optional:"" name:"pattern" predictor:"installed" help:"Glob patterns; ! negates."`
	L10n     bool     `name:"l10n" help:"Include packages whose only new commits are translations."`
}

type outdatedRow struct {
	Package    string `json:"package"`
	Ref        string `json:"ref"`
	Current    string `json:"current"`
	Latest     string `json:"latest"`
	Behind     string `json:"behind,omitzero"`
	Flag       string `json:"flag,omitzero"`
	Key        string `json:"-"`
	CurrentSHA string `json:"-"`
	TargetSHA  string `json:"-"`
	L10nOnly   bool   `json:"-"`
}

func (c *OutdatedCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	rows, err := collectOutdated(ctx, cli, p, c.Patterns, rep)
	if err != nil {
		return err
	}
	rows = slices.DeleteFunc(rows, func(r outdatedRow) bool { return r.Flag == "" && r.CurrentSHA == r.TargetSHA })
	rows = withoutL10n(rows, c.L10n)
	if cli.JSON {
		return json.MarshalWrite(os.Stdout, rows)
	}
	if len(rows) == 0 {
		rep.Info("all packages up to date")
		return nil
	}
	t := ui.Table{
		Header:  []string{"Package", "Ref", "Current", "Latest", "Behind"},
		Unicode: true,
	}
	for _, r := range rows {
		pkg := r.Package
		if r.Flag != "" {
			pkg += " !"
		}
		t.Rows = append(t.Rows, []string{pkg, r.Ref, r.Current, r.Latest, r.Behind})
	}
	return t.Render(os.Stdout)
}

func collectOutdated(ctx context.Context, cli *CLI, p *project.Project, patterns []string, rep *ui.Reporter) ([]outdatedRow, error) {
	reg := registryClient(cli, p)
	resolver := source.New(reg, fetch.Client())
	logger, _ := resolver.(source.ChangeLogger)
	ropts := source.ResolveOpts{Rel: p.Rel, LTSRel: project.LTSRel(p.MWVersion), MWVer: p.MWVersion}

	keys := make([]string, 0, len(p.Lock.Packages))
	for key := range p.Lock.Packages {
		if nameMatches(keyName(key), patterns) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)

	specs := make([]spec.Spec, len(keys))
	var exts, skins []string
	for i, key := range keys {
		sp, err := specForKey(p, key)
		if err != nil {
			return nil, err
		}
		specs[i] = sp
		if sp.Kind == spec.Registry {
			if keyType(key) == "skins" {
				skins = append(skins, sp.Name)
			} else {
				exts = append(exts, sp.Name)
			}
		}
	}
	if len(keys) == 0 {
		return nil, nil
	}
	if rep != nil {
		defer rep.ClearProgress()
	}
	if len(exts)+len(skins) > 0 {
		if rep != nil {
			rep.Progress("fetching registry data for %d packages...", len(keys))
		}
		if _, _, err := reg.Branches(ctx, exts, skins); err != nil {
			return nil, err
		}
		if _, err := reg.Policies(ctx, exts, skins); err != nil {
			return nil, err
		}
	}

	rows := make([]outdatedRow, len(keys))
	var warnOnce sync.Once
	var done atomic.Int64
	if rep != nil {
		rep.Progress("resolving %d packages...", len(keys))
	}
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	for i, key := range keys {
		g.Go(func() error {
			defer func() {
				if rep != nil {
					rep.Progress("resolved %d/%d packages...", done.Add(1), len(keys))
				}
			}()
			lp := p.Lock.Packages[key]
			res, err := resolver.Resolve(ctx, specs[i], ropts)
			if err != nil {
				return err
			}
			row := &rows[i]
			*row = outdatedRow{
				Package:    keyName(key),
				Ref:        lp.Ref,
				Current:    formatSHADate(lp.SHA, lp.Date),
				Latest:     formatSHADate(res.SHA, dateString(res.Date)),
				Key:        key,
				CurrentSHA: lp.SHA,
				TargetSHA:  res.SHA,
			}
			if lp.Requires.MediaWiki != "" {
				if ok, err := manifest.Satisfies(lp.Requires.MediaWiki, p.MWVersion); err == nil && !ok {
					row.Flag = "mediawiki"
				}
			}
			if strings.HasPrefix(lp.Ref, "REL") && relOlderThan(lp.Ref, p.Rel) {
				if row.Flag != "" {
					row.Flag += ",rel"
				} else {
					row.Flag = "rel"
				}
			}
			if logger == nil || !source.CheapLog(res) || lp.SHA == "" || res.SHA == "" || lp.SHA == res.SHA {
				return nil
			}
			commits, err := logger.Log(ctx, res, lp.SHA, res.SHA)
			if err != nil {
				if rep != nil && !errors.Is(err, context.Canceled) {
					warnOnce.Do(func() { rep.Warn("changelog: %v", err) })
				}
				return nil
			}
			row.Behind, row.L10nOnly = source.Behind(commits)
			return nil
		})
	}
	return rows, g.Wait()
}

type RelCmd struct {
	Value string `arg:"" optional:"" name:"rel" help:"REL1_xx, master, or empty to show."`
	Clear bool   `name:"clear" help:"Clear the rel override."`
}

func (c *RelCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	if c.Clear {
		p.Manifest.Rel = ""
		if err := p.Save(); err != nil {
			return err
		}
		rep.Info("rel override cleared")
		rep.Info("run phuo update to apply")
		return nil
	}
	if c.Value != "" {
		p.Manifest.Rel = c.Value
		if err := p.Save(); err != nil {
			return err
		}
		rep.Info("rel set to %s", c.Value)
		rep.Info("run phuo update to apply")
		return nil
	}
	reg := registryClient(cli, p)
	snaps, err := reg.Snapshots(ctx)
	if err != nil {
		return err
	}
	derived := project.Rel(p.MWVersion)
	override := p.Manifest.Rel
	if override == "" {
		override = "(none)"
	}
	rep.Info("MW_VERSION: %s", p.MWVersion)
	rep.Info("derived rel: %s", derived)
	rep.Info("manifest override: %s", override)
	rep.Info("effective: %s", p.Rel)
	rep.Info("snapshots: %s", strings.Join(snaps, ", "))
	return nil
}

type LsCmd struct {
	All bool `name:"all" help:"Flatten dep packages."`
}

func (c *LsCmd) Run(ctx context.Context, cli *CLI) error {
	p, _, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	if cli.JSON {
		return json.MarshalWrite(os.Stdout, p.Lock.Packages, json.Deterministic(true))
	}
	var ls *localsettings.File
	if lsPath, ok := p.Manifest.LocalSettings.Value("LocalSettings.php"); ok {
		if !filepath.IsAbs(lsPath) {
			lsPath = filepath.Join(p.Root, lsPath)
		}
		if f, err := localsettings.Scan(lsPath); err == nil {
			ls = &f
		}
	}
	t := ui.Table{
		Header:  []string{"Name", "Type", "Ref", "SHA", "Version", "Policy", "Spec", "Enabled"},
		Unicode: true,
		Rows:    lsRows(p, c.All, ls),
	}
	return t.Render(os.Stdout)
}

func lsRows(p *project.Project, all bool, ls *localsettings.File) [][]string {
	enabled := func(key string) string {
		switch {
		case ls == nil:
			return "?"
		case ls.Outside.Has(key), ls.InBlock.Has(key):
			return "yes"
		case ls.Disabled.Has(key):
			return "commented"
		case slices.Contains(p.Manifest.Disabled, key):
			return "disabled"
		}
		return "no"
	}
	keys := make([]string, 0, len(p.Lock.Packages))
	for key, pkg := range p.Lock.Packages {
		if all || pkg.Spec != "dep" {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	var rows [][]string
	seen := map[string]bool{}
	var addRow func(key string, indent string)
	addRow = func(key string, indent string) {
		if seen[key] {
			return
		}
		seen[key] = true
		pkg := p.Lock.Packages[key]
		rows = append(rows, []string{
			indent + keyName(key), keyType(key), pkg.Ref, shortSHA(pkg.SHA),
			pkg.Version, pkg.Policy, pkg.Spec, enabled(key),
		})
		if all {
			return
		}
		var deps []string
		for _, d := range pkg.Dependencies {
			// a package you declared yourself stays at top level
			// `phuo why` shows who needs it
			if dp, ok := p.Lock.Packages[d]; ok && dp.Spec == "dep" {
				deps = append(deps, d)
			}
		}
		slices.Sort(deps)
		for _, d := range deps {
			addRow(d, indent+"  ")
		}
	}
	for _, key := range keys {
		addRow(key, "")
	}
	return rows
}

type InfoCmd struct {
	Name string `arg:"" name:"name" help:"Package name."`
}

func (c *InfoCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	reg := registryClient(cli, p)
	name := c.Name
	skin := false
	if rest, ok := strings.CutPrefix(name, "skin:"); ok {
		name, skin = rest, true
	}
	var exts, skins []string
	if skin {
		skins = []string{name}
	} else {
		exts = []string{name}
	}
	exBr, skBr, err := reg.Branches(ctx, exts, skins)
	if err != nil {
		return err
	}
	br, ok := exBr[name]
	typ := "extensions"
	if !ok {
		br, ok = skBr[name]
		typ = "skins"
	}
	if !ok && !skin {
		exBr2, skBr2, err := reg.Branches(ctx, nil, []string{name})
		if err != nil {
			return err
		}
		_ = exBr2
		if b, ok2 := skBr2[name]; ok2 {
			br, ok, typ = b, true, "skins"
		}
	}
	if !ok {
		return userErr(fmt.Sprintf("Unknown extension/skin %q", c.Name))
	}
	pols, err := reg.Policies(ctx, pickNames(typ, "extensions", name), pickNames(typ, "skins", name))
	if err != nil {
		return err
	}
	pol := pols[typ+"/"+name]
	kind := pol.Kind
	if kind == "" {
		kind = "rel"
	}
	type infoRequires struct {
		MediaWiki  string            `json:"MediaWiki,omitzero"`
		Extensions map[string]string `json:"extensions,omitzero"`
		Skins      map[string]string `json:"skins,omitzero"`
	}
	type infoOut struct {
		Name        string            `json:"name"`
		Type        string            `json:"type"`
		Description string            `json:"description,omitzero"`
		Status      string            `json:"status,omitzero"`
		Policy      string            `json:"policy"`
		URL         string            `json:"url,omitzero"`
		Version     string            `json:"version,omitzero"`
		License     string            `json:"license,omitzero"`
		Requires    *infoRequires     `json:"requires,omitzero"`
		Installed   bool              `json:"installed"`
		SHA         string            `json:"sha,omitzero"`
		Refs        map[string]string `json:"refs"`
	}
	out := infoOut{
		Name: name, Type: typ, Description: pol.Description, Status: pol.Status,
		Policy: kind, URL: br.Source, Refs: map[string]string{},
	}
	if lp, ok := p.Lock.Packages[typ+"/"+name]; ok {
		out.Installed = true
		out.Version = lp.Version
		out.SHA = shortSHA(lp.SHA)
		dir := packageDir(p, typ+"/"+name)
		if man, _, err := manifest.Read(dir); err == nil {
			out.License = man.License
			if man.Requires.MediaWiki != "" || len(man.Requires.Extensions) > 0 || len(man.Requires.Skins) > 0 {
				out.Requires = &infoRequires{
					MediaWiki:  man.Requires.MediaWiki,
					Extensions: man.Requires.Extensions,
					Skins:      man.Requires.Skins,
				}
			}
		}
	}
	refs := make([]string, 0, len(br.Refs))
	for ref, url := range br.Refs {
		refs = append(refs, ref)
		out.Refs[ref] = source.ArchiveSHA(url)
	}
	slices.Sort(refs)
	if cli.JSON {
		return json.MarshalWrite(os.Stdout, out, json.Deterministic(true))
	}
	rep.Info("name: %s", out.Name)
	rep.Info("type: %s", out.Type)
	if out.Description != "" {
		rep.Info("description: %s", out.Description)
	}
	if out.Status != "" {
		rep.Info("status: %s", out.Status)
	}
	rep.Info("policy: %s", out.Policy)
	if out.URL != "" {
		rep.Info("url: %s", out.URL)
	}
	if out.Installed {
		if out.Version != "" {
			rep.Info("version: %s", out.Version)
		}
		if out.License != "" {
			rep.Info("license: %s", out.License)
		}
		if out.Requires != nil {
			rep.Info("requires:")
			if out.Requires.MediaWiki != "" {
				rep.Info("  MediaWiki: %s", out.Requires.MediaWiki)
			}
			for n, c := range out.Requires.Extensions {
				rep.Info("  extensions/%s: %s", n, c)
			}
			for n, c := range out.Requires.Skins {
				rep.Info("  skins/%s: %s", n, c)
			}
		}
		rep.Info("installed: yes (%s)", out.SHA)
	} else {
		rep.Info("installed: no")
	}
	rep.Info("refs:")
	width := 0
	for _, ref := range refs {
		if out.Refs[ref] != "" && utf8.RuneCountInString(ref) > width {
			width = utf8.RuneCountInString(ref)
		}
	}
	for _, ref := range refs {
		if sha := out.Refs[ref]; sha != "" {
			rep.Info("  %-*s  %s", width, ref, sha)
		} else {
			rep.Info("  %s", ref)
		}
	}
	return nil
}

func pickNames(typ, want, name string) []string {
	if typ == want {
		return []string{name}
	}
	return nil
}

type SearchCmd struct {
	Query string `arg:"" name:"query" help:"Search query."`
}

func (c *SearchCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		cache, cerr := project.CacheDir(cli.CacheDir, os.Getenv("PHUO_CACHE_DIR"))
		if cerr != nil {
			return err
		}
		p = &project.Project{CacheDir: cache}
		rep = ui.New(ui.Options{
			JSON: cli.JSON, Silent: cli.Silent, Verbose: cli.Verbose,
			NoColor: cli.NoColor, NoProgress: cli.NoProgress,
		})
	}
	reg := registryClient(cli, p)
	hits, err := reg.Search(ctx, c.Query)
	if err != nil {
		return err
	}
	for _, h := range hits {
		name := h.Name
		if h.Skin {
			name = "skin:" + name
		}
		if !h.Installable {
			name += " (not on ExtensionDistributor)"
		}
		rep.Info("%s", name)
		if h.Snippet != "" {
			rep.Info("    %s", h.Snippet)
		}
	}
	return nil
}

type WhyCmd struct {
	Name string `arg:"" name:"name" predictor:"installed" help:"Package name."`
}

func (c *WhyCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	key, err := resolveInstalledKey(p, c.Name)
	if err != nil {
		return err
	}
	lp := p.Lock.Packages[key]
	if lp.Spec != "dep" {
		rep.Info("direct dependency (phuo.json)")
		return nil
	}
	name := keyName(key)
	typ := keyType(key)
	found := false
	for k, pkg := range p.Lock.Packages {
		var m map[string]string
		if typ == "skins" {
			m = pkg.Requires.Skins
		} else {
			m = pkg.Requires.Extensions
		}
		if cstr, ok := m[name]; ok {
			rep.Info("%s requires %s (%s)", keyName(k), name, cstr)
			found = true
		}
	}
	if !found {
		rep.Info("%s is a dep with no recorded requirers", name)
	}
	return nil
}

type ChangelogCmd struct {
	Name string `arg:"" name:"name" predictor:"installed" help:"Package name."`
	All  bool   `name:"all" help:"Include l10n-bot commits."`
	From string `name:"from" help:"From ref/sha."`
	To   string `name:"to" help:"To ref/sha."`
}

func (c *ChangelogCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	key, err := resolveInstalledKey(p, c.Name)
	if err != nil {
		return err
	}
	lp := p.Lock.Packages[key]
	sp, err := specForKey(p, key)
	if err != nil {
		return err
	}
	reg := registryClient(cli, p)
	resolver := source.New(reg, fetch.Client())
	logger, ok := resolver.(source.ChangeLogger)
	if !ok {
		return userErr("changelog: resolver does not support ChangeLogger")
	}
	ropts := source.ResolveOpts{Rel: p.Rel, LTSRel: project.LTSRel(p.MWVersion), MWVer: p.MWVersion}
	res, err := resolver.Resolve(ctx, sp, ropts)
	if err != nil {
		return err
	}
	from, to := c.From, c.To
	if from == "" {
		from = lp.SHA
	}
	if to == "" {
		to = res.SHA
	}
	if from == "" || to == "" {
		return userErr("changelog: need from and to shas")
	}
	commits, err := logger.Log(ctx, res, from, to)
	if err != nil {
		return err
	}
	for _, cmt := range commits {
		if !c.All && cmt.Email == source.L10nBot {
			continue
		}
		rep.Info("%s", cmt.Subject)
	}
	dir := packageDir(p, key)
	for _, pat := range []string{"RELEASE-NOTES*", "CHANGELOG*", "HISTORY*"} {
		matches, _ := filepath.Glob(filepath.Join(dir, pat))
		for _, m := range matches {
			rep.Info("also: %s", filepath.Base(m))
		}
	}
	return nil
}

type DoctorCmd struct {
	PHP  bool `name:"php" help:"Run PHP-backed checks."`
	Wiki bool `name:"wiki" help:"Query the remote wiki."`
}

type CacheCmd struct {
	Action string `arg:"" optional:"" name:"action" help:"dir (default) or rm."`
}

func (c *CacheCmd) Run(ctx context.Context, cli *CLI) error {
	dir, err := project.CacheDir(cli.CacheDir, os.Getenv("PHUO_CACHE_DIR"))
	if err != nil {
		return envErr(err.Error())
	}
	switch c.Action {
	case "dir":
		fmt.Println(dir)
		return nil
	case "rm":
		if err := os.RemoveAll(dir); err != nil {
			return err
		}
		fmt.Println("removed", dir)
		return nil
	case "":
		fmt.Println(dir)
		var size int64
		_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
			if err != nil || d.IsDir() {
				return nil
			}
			fi, err := d.Info()
			if err != nil {
				return nil
			}
			size += fi.Size()
			return nil
		})
		fmt.Printf("%s\n", formatSize(size))
		return nil
	default:
		return userErr(fmt.Sprintf("cache: unknown action %q (want dir or rm)", c.Action))
	}
}

type PmCmd struct {
	Ls    LsCmd    `cmd:"" help:"List installed packages."`
	Cache CacheCmd `cmd:"" help:"Show or clear the global cache."`
	Diff  DiffCmd  `cmd:"" help:"Show what updating a package would change."`
}

func registryClient(cli *CLI, p *project.Project) *registry.Client {
	return &registry.Client{
		HTTP:     fetch.Client(),
		BaseURL:  p.Manifest.Registry,
		CacheDir: p.CacheDir,
		Offline:  cli.Offline,
		NoCache:  cli.NoCache,
	}
}

func manifestSpecs(p *project.Project, patterns []string, latest bool) ([]spec.Spec, error) {
	var specs []spec.Spec
	add := func(name, val string, skin bool) error {
		if !nameMatches(name, patterns) {
			return nil
		}
		sp, err := spec.Parse(val)
		if err != nil {
			return err
		}
		sp.Name = name
		if skin {
			sp.Skin = true
		}
		if latest {
			key := "extensions/" + name
			if skin {
				key = "skins/" + name
			}
			moveRel(p, key, &sp)
		}
		specs = append(specs, sp)
		return nil
	}
	for name, val := range p.Manifest.Extensions {
		if err := add(name, val, false); err != nil {
			return nil, err
		}
	}
	for name, val := range p.Manifest.Skins {
		if err := add(name, val, true); err != nil {
			return nil, err
		}
	}
	return specs, nil
}

// drop a lagging REL pin so the spec re-resolves to p.Rel
func moveRel(p *project.Project, key string, sp *spec.Spec) {
	if lp, ok := p.Lock.Packages[key]; ok && sp.Kind == spec.Registry && strings.HasPrefix(lp.Ref, "REL") && relOlderThan(lp.Ref, p.Rel) {
		sp.Ref = ""
	}
}

func nameMatches(name string, patterns []string) bool {
	if len(patterns) == 0 {
		return true
	}
	var pos, neg []string
	for _, p := range patterns {
		if rest, ok := strings.CutPrefix(p, "!"); ok {
			neg = append(neg, rest)
		} else {
			pos = append(pos, p)
		}
	}
	ok := len(pos) == 0
	for _, p := range pos {
		if m, err := path.Match(p, name); err == nil && m {
			ok = true
			break
		}
	}
	if !ok {
		return false
	}
	for _, p := range neg {
		if m, err := path.Match(p, name); err == nil && m {
			return false
		}
	}
	return true
}

func withoutL10n(rows []outdatedRow, include bool) []outdatedRow {
	if include {
		return rows
	}
	return slices.DeleteFunc(rows, func(r outdatedRow) bool {
		return r.L10nOnly && r.Flag == ""
	})
}

func relOlderThan(ref, cur string) bool {
	a, ok1 := relNum(ref)
	b, ok2 := relNum(cur)
	return ok1 && ok2 && a < b
}

func relNum(s string) (int, bool) {
	rest, ok := strings.CutPrefix(s, "REL")
	if !ok {
		return 0, false
	}
	maj, min, ok := strings.Cut(rest, "_")
	if !ok {
		return 0, false
	}
	major, err1 := strconv.Atoi(maj)
	minor, err2 := strconv.Atoi(min)
	if err1 != nil || err2 != nil {
		return 0, false
	}
	return major*1000 + minor, true
}

func resolveInstalledKey(p *project.Project, name string) (string, error) {
	if strings.Contains(name, "/") {
		if _, ok := p.Lock.Packages[name]; ok {
			return name, nil
		}
		return "", userErr(fmt.Sprintf("%s is not installed", name))
	}
	if _, ok := p.Lock.Packages["extensions/"+name]; ok {
		return "extensions/" + name, nil
	}
	if _, ok := p.Lock.Packages["skins/"+name]; ok {
		return "skins/" + name, nil
	}
	return "", userErr(fmt.Sprintf("%s is not installed", name))
}

func specForKey(p *project.Project, key string) (spec.Spec, error) {
	name := keyName(key)
	lp := p.Lock.Packages[key]
	if lp.Spec != "dep" {
		var val string
		var ok bool
		if keyType(key) == "skins" {
			val, ok = p.Manifest.Skins[name]
		} else {
			val, ok = p.Manifest.Extensions[name]
		}
		if ok {
			sp, err := spec.Parse(val)
			if err != nil {
				return spec.Spec{}, err
			}
			sp.Name = name
			if keyType(key) == "skins" {
				sp.Skin = true
			}
			return sp, nil
		}
	}
	sp := spec.Spec{Kind: spec.Registry, Name: name}
	if keyType(key) == "skins" {
		sp.Skin = true
	}
	return sp, nil
}

func packageDir(p *project.Project, key string) string {
	typ, name, _ := strings.Cut(key, "/")
	if typ == "skins" {
		return filepath.Join(p.Root, p.Paths.Skins, name)
	}
	return filepath.Join(p.Root, p.Paths.Extensions, name)
}

func keyName(key string) string {
	_, name, _ := strings.Cut(key, "/")
	return name
}

func keyType(key string) string {
	typ, _, _ := strings.Cut(key, "/")
	return typ
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func formatSHADate(sha, date string) string {
	s := cmp.Or(shortSHA(sha), "unknown")
	if date == "" {
		return s
	}
	if t, err := time.Parse(time.RFC3339, date); err == nil {
		return s + " (" + t.UTC().Format("2006-01-02") + ")"
	}
	return s
}

func dateString(t time.Time) string {
	if t.IsZero() {
		return ""
	}
	return t.UTC().Format(time.RFC3339)
}

func formatSize(n int64) string {
	const (
		kb = 1024
		mb = kb * 1024
		gb = mb * 1024
	)
	switch {
	case n >= gb:
		return fmt.Sprintf("%.1f GB", float64(n)/float64(gb))
	case n >= mb:
		return fmt.Sprintf("%.1f MB", float64(n)/float64(mb))
	case n >= kb:
		return fmt.Sprintf("%.1f KB", float64(n)/float64(kb))
	default:
		return fmt.Sprintf("%d B", n)
	}
}

func extractArchive(ctx context.Context, url, tmp, cacheDir string) error {
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return err
	}
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return err
	}
	defer root.Close()
	pr, pw := io.Pipe()
	xerr := make(chan error, 1)
	go func() {
		err := archive.ExtractTarGz(pr, root)
		pr.CloseWithError(err)
		xerr <- err
	}()
	_, err = fetch.Download(ctx, url, cacheDir, pw)
	pw.CloseWithError(err)
	if e := <-xerr; e != nil {
		err = e
	}
	return err
}

var diffExclude = map[string]bool{
	"vendor": true, ".phuo.json": true, "gitinfo.json": true, "version": true, "i18n": true,
}

func copyTreeFiltered(src, dst string, all bool) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return os.MkdirAll(dst, 0o755)
		}
		if !all {
			base := filepath.Base(rel)
			top, _, _ := strings.Cut(filepath.ToSlash(rel), "/")
			if diffExclude[base] || diffExclude[top] {
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0o755)
		}
		if d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		return copyFile(path, target)
	})
}

func copyFile(src, dst string) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(dst)
	if err != nil {
		return err
	}
	_, err = io.Copy(out, in)
	return errors.Join(err, out.Close())
}

func nameOnlyDiff(left, right string) ([]string, error) {
	leftHashes, err := hashTree(left)
	if err != nil {
		return nil, err
	}
	rightHashes, err := hashTree(right)
	if err != nil {
		return nil, err
	}
	var changed []string
	seen := map[string]bool{}
	for path, h := range leftHashes {
		seen[path] = true
		if rightHashes[path] != h {
			changed = append(changed, path)
		}
	}
	for path := range rightHashes {
		if !seen[path] {
			changed = append(changed, path)
		}
	}
	slices.Sort(changed)
	return changed, nil
}

func hashTree(root string) (map[string]string, error) {
	out := map[string]string{}
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		f, err := os.Open(path)
		if err != nil {
			return err
		}
		h := sha256.New()
		_, err = io.Copy(h, f)
		f.Close()
		if err != nil {
			return err
		}
		out[filepath.ToSlash(rel)] = hex.EncodeToString(h.Sum(nil))
		return nil
	})
	return out, err
}
