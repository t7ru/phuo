package installer

import (
	"bytes"
	"cmp"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/t7ru/phuo/internal/archive"
	"github.com/t7ru/phuo/internal/composer"
	"github.com/t7ru/phuo/internal/fetch"
	"github.com/t7ru/phuo/internal/localsettings"
	"github.com/t7ru/phuo/internal/manifest"
	"github.com/t7ru/phuo/internal/mw"
	"github.com/t7ru/phuo/internal/patch"
	"github.com/t7ru/phuo/internal/project"
	"github.com/t7ru/phuo/internal/registry"
	"github.com/t7ru/phuo/internal/source"
	"github.com/t7ru/phuo/internal/spec"
	"github.com/t7ru/phuo/internal/ui"
	"golang.org/x/sync/errgroup"
)

type OpKind int

const (
	OpAdd OpKind = iota
	OpUpdate
	OpRemove
	OpKeep
)

type Op struct {
	Key, From, To string
	Kind          OpKind
}

type Options struct {
	Force        bool
	DryRun       bool
	NoComposer   bool
	NoLoad       bool
	Disabled     bool
	LockfileOnly bool
	Frozen       bool
	Exact        bool
	Git          bool
	Full         bool
	Update       bool
	UpdateDB     bool
	L10n         bool // update translation-only tips too
	Revert       bool
	NoCache      bool
	Offline      bool
	Skin         bool
	NoSave       bool
	BaseURL      string
	Jobs         int
	// skip writing phuo.json/lock since the caller owns that
	// revert's cleanup must not rotate its own undo point
	SkipSave bool
	// yes/no for update.php as nil means never ask
	Ask func(title string) bool
}

type Plan struct {
	Ops    []Op
	p      *project.Project
	add    []spec.Spec
	remove []string
	opts   Options
}

type Summary struct {
	Added, Updated, Removed int
}

type workItem struct {
	spec          spec.Spec
	lockSpec      string
	direct        bool
	requirers     []string
	key           string
	res           source.Resolved
	man           manifest.Manifest
	integrity     string
	integrityWarn bool
	fallback404   bool
	fromSHA       string
	pkgRoot       string
	tmpDir        string
	fromLock      bool
	depConstraint string
	kind          OpKind
	keep          bool
}

func NewPlan(p *project.Project, add []spec.Spec, remove []string, opts Options) (*Plan, error) {
	for _, s := range add {
		if s.Name == "" {
			return nil, fmt.Errorf("package name required")
		}
	}
	return &Plan{p: p, add: add, remove: remove, opts: opts}, nil
}

func (pl *Plan) Apply(ctx context.Context, reporter *ui.Reporter) (Summary, error) {
	defer reporter.ClearProgress()
	if pl.p.Lock.Packages == nil {
		pl.p.Lock.Packages = map[string]project.Package{}
	}
	if pl.p.Lock.Version == 0 {
		pl.p.Lock.Version = 1
	}
	pl.p.Lock.MediaWiki = pl.p.MWVersion

	if len(pl.remove) > 0 {
		return pl.applyRemove(ctx, reporter)
	}
	return pl.applyInstall(ctx, reporter)
}

func (pl *Plan) applyRemove(ctx context.Context, reporter *ui.Reporter) (Summary, error) {
	var sum Summary
	var lines []string
	var exclude []string
	lsPath, ls, lsOK, err := pl.localSettings()
	if err != nil {
		return sum, err
	}
	// a leftover user wfLoad* for a deleted dir is fatal on every request
	// good thing phuo never edits those lines haha...
	userLoaded := func(key string) bool { return lsOK && ls.Outside.Has(key) }
	keys := make([]string, 0, len(pl.remove))
	for _, name := range pl.remove {
		key, err := pl.resolveKey(name)
		if err != nil {
			return sum, err
		}
		if pl.opts.Force {
			keys = append(keys, key)
			continue
		}
		if deps := pl.dependents(key); len(deps) > 0 {
			return sum, fmt.Errorf("%s is required by %s (use --force to remove)", keyName(key), joinNames(deps))
		}
		if userLoaded(key) {
			return sum, fmt.Errorf("%s is loaded by your own line at %s:%d; delete that line first (or use --force)",
				keyName(key), filepath.Base(lsPath), ls.Line[key])
		}
		keys = append(keys, key)
	}
	for _, key := range keys {
		exclude = append(exclude, pl.mergedInclude(key)...)
		if !pl.opts.DryRun {
			if err := os.RemoveAll(pl.destPath(key)); err != nil {
				return sum, err
			}
		}
		delete(pl.p.Lock.Packages, key)
		pl.dropManifest(key)
		sum.Removed++
		lines = append(lines, " - "+keyName(key))
		pl.Ops = append(pl.Ops, Op{Key: key, Kind: OpRemove})
	}
	pruned, pruneLines, prunedExcludes, err := pl.pruneDeps(pl.opts.DryRun, userLoaded)
	if err != nil {
		return sum, err
	}
	sum.Removed += pruned
	lines = append(lines, pruneLines...)
	exclude = append(exclude, prunedExcludes...)

	if !pl.opts.DryRun && !pl.opts.SkipSave {
		if err := pl.p.Save(); err != nil {
			return sum, err
		}
	}
	reporter.ClearProgress()
	for _, l := range lines {
		reporter.Info("%s", l)
	}
	if !pl.opts.DryRun {
		if err := pl.finishMW(ctx, reporter, composer.Plan{Exclude: exclude}, nil); err != nil {
			return sum, err
		}
	}
	if sum.Removed > 0 {
		reporter.Info("%s", countMsg(sum.Removed, 0)+" removed")
	}
	return sum, nil
}

func (pl *Plan) mergedInclude(key string) []string {
	lp, ok := pl.p.Lock.Packages[key]
	if !ok {
		return nil
	}
	mode := lp.Composer
	if mode == "" {
		man, _, err := manifest.Read(pl.destPath(key))
		if err == nil {
			if m, err := composer.Detect(pl.destPath(key), man); err == nil {
				mode = string(m)
			}
		}
	}
	if mode == string(composer.ModeMerged) {
		return []string{key + "/composer.json"}
	}
	return nil
}

func (pl *Plan) applyInstall(ctx context.Context, reporter *ui.Reporter) (Summary, error) {
	// .phuo-tmp only empties once every item has been swapped or cleaned up
	defer os.Remove(filepath.Join(pl.parentDir("extensions"), ".phuo-tmp"))
	defer os.Remove(filepath.Join(pl.parentDir("skins"), ".phuo-tmp"))
	if pl.opts.Frozen {
		if err := pl.checkFrozen(); err != nil {
			return Summary{}, err
		}
	}
	if os.Geteuid() == 0 && !pl.opts.DryRun && !pl.opts.LockfileOnly {
		reporter.Warn("running as root! phuo will keep existing packages' owners while new ones inherit the parent's")
	}

	reg := &registry.Client{
		HTTP:     fetch.Client(),
		BaseURL:  cmp.Or(pl.opts.BaseURL, pl.p.Manifest.Registry),
		CacheDir: pl.p.CacheDir,
		Offline:  pl.opts.Offline,
		NoCache:  pl.opts.NoCache,
	}
	resolver := source.New(reg, fetch.Client())
	ropts := source.ResolveOpts{
		Rel: pl.p.Rel, LTSRel: project.LTSRel(pl.p.MWVersion), MWVer: pl.p.MWVersion,
		Skin: pl.opts.Skin, Git: pl.opts.Git, Full: pl.opts.Full,
	}

	var queue []*workItem
	seen := map[string]bool{}

	for _, s := range pl.add {
		it := &workItem{spec: s, direct: true}
		if s.Kind == spec.Registry {
			it.lockSpec = registrySpec(s)
		} else {
			it.lockSpec = stripAlias(s)
		}
		key := guessKey(s)
		it.key = key
		if seen[key] {
			continue
		}
		seen[key] = true
		queue = append(queue, it)
	}

	var done []*workItem
	var hints []string
	edges := map[string][]string{}
	cacheDir := pl.p.CacheDir
	if pl.opts.NoCache {
		cacheDir = ""
	}

	for len(queue) > 0 {
		round := queue
		queue = nil
		reporter.Progress("resolving %d packages...", len(round))
		if err := pl.prefetch(ctx, reg, round); err != nil {
			return Summary{}, err
		}

		g, gctx := errgroup.WithContext(ctx)
		g.SetLimit(max(cmp.Or(pl.opts.Jobs, 16), 1))
		for _, it := range round {
			g.Go(func() error {
				return pl.prepareItem(gctx, it, resolver, ropts, cacheDir)
			})
		}
		if err := g.Wait(); err != nil {
			pl.cleanupTemps(round)
			return Summary{}, err
		}

		reporter.Progress("installing %d packages...", len(round))
		for _, it := range round {
			if it.keep {
				done = append(done, it)
				pl.Ops = append(pl.Ops, Op{Key: it.key, Kind: OpKeep, To: it.res.Ref})
				if lp, ok := pl.p.Lock.Packages[it.key]; ok {
					for _, dk := range lp.Dependencies {
						if seen[dk] {
							continue
						}
						if !pl.opts.Update && stampMatch(pl.destPath(dk), pl.p.Lock.Packages[dk]) {
							seen[dk] = true
							continue
						}
						name := keyName(dk)
						nit := &workItem{
							spec:     spec.Spec{Kind: spec.Registry, Name: name},
							lockSpec: "dep", key: dk, fromLock: !pl.opts.Update,
							requirers: []string{it.res.Name},
						}
						if keyType(dk) == "skins" {
							nit.spec.Skin = true
						}
						seen[dk] = true
						queue = append(queue, nit)
					}
				}
				continue
			}
			if it.fallback404 {
				reporter.Warn("%s: snapshot tarball unavailable, rebuilt from source at %s", it.res.Name, shortSHA(it.res.SHA))
			} else if it.integrityWarn {
				reporter.Warn("%s: integrity mismatch (updating lock)", it.res.Name)
			}
			if it.res.Hint != "" {
				hints = append(hints, it.res.Hint)
			}
			if mw := it.man.Requires.MediaWiki; mw != "" {
				ok, err := manifest.Satisfies(mw, pl.p.MWVersion)
				if err != nil {
					pl.cleanupTemps(round)
					return Summary{}, err
				}
				if !ok {
					msg := fmt.Sprintf("%s@%s requires MediaWiki %s (you have %s)", it.res.Name, it.res.Ref, mw, pl.p.MWVersion)
					if !pl.opts.Force {
						pl.cleanupTemps(round)
						return Summary{}, fmt.Errorf("%s", msg)
					}
					reporter.Warn("%s", msg)
				}
			}
			if it.depConstraint != "" {
				if err := checkDepVersion(reporter, it.res.Name, it.depConstraint, it.man.Version); err != nil {
					pl.cleanupTemps(round)
					return Summary{}, err
				}
			}

			for name := range it.man.Suggests.Extensions {
				reporter.Info("%s suggests: %s", it.res.Name, name)
			}
			for name := range it.man.Suggests.Skins {
				reporter.Info("%s suggests: %s", it.res.Name, name)
			}

			addDep := func(name, typ, constraint string) error {
				key := typ + "/" + name
				if reaches(key, it.key, edges) {
					return fmt.Errorf("dependency cycle: %s -> %s", it.key, key)
				}
				edges[it.key] = append(edges[it.key], key)
				if seen[key] {
					return nil
				}
				lp, locked := pl.p.Lock.Packages[key]
				if locked && !pl.opts.Update && stampMatch(pl.destPath(key), lp) {
					seen[key] = true
					return checkDepVersion(reporter, name, constraint, lp.Version)
				}
				depSpec := spec.Spec{Kind: spec.Registry, Name: name}
				if typ == "skins" {
					depSpec.Skin = true
				}
				nit := &workItem{
					spec: depSpec, lockSpec: "dep", direct: false,
					requirers: []string{it.res.Name}, key: key,
					fromLock: locked && !pl.opts.Update, depConstraint: constraint,
				}
				seen[key] = true
				queue = append(queue, nit)
				return nil
			}
			for name, c := range it.man.Requires.Extensions {
				if err := addDep(name, "extensions", c); err != nil {
					pl.cleanupTemps(round)
					return Summary{}, err
				}
			}
			for name, c := range it.man.Requires.Skins {
				if err := addDep(name, "skins", c); err != nil {
					pl.cleanupTemps(round)
					return Summary{}, err
				}
			}
			if it.fromLock {
				for _, dk := range pl.p.Lock.Packages[it.key].Dependencies {
					if seen[dk] {
						continue
					}
					name := keyName(dk)
					typ, _, _ := strings.Cut(dk, "/")
					nit := &workItem{
						spec:     spec.Spec{Kind: spec.Registry, Name: name},
						lockSpec: "dep", direct: false,
						requirers: []string{it.res.Name}, key: dk, fromLock: true,
					}
					if typ == "skins" {
						nit.spec.Skin = true
					}
					seen[dk] = true
					queue = append(queue, nit)
				}
			}

			it.kind = OpAdd
			if _, ok := pl.p.Lock.Packages[it.key]; ok {
				it.kind = OpUpdate
			}
			done = append(done, it)
		}
	}

	// ExtensionRegistry keys on the manifest name and refuses to load twice,
	// so two installed packages may never share one
	if err := pl.checkNames(done); err != nil {
		return Summary{}, err
	}

	depMap := map[string][]string{}
	for _, it := range done {
		if it.keep {
			continue
		}
		var deps []string
		for name := range it.man.Requires.Extensions {
			deps = append(deps, "extensions/"+name)
		}
		for name := range it.man.Requires.Skins {
			deps = append(deps, "skins/"+name)
		}
		depMap[it.key] = deps
	}

	var sum Summary
	var addLines []string
	extN, skinN := 0, 0

	for _, it := range done {
		if it.keep {
			continue
		}
		if it.direct && pl.opts.Exact {
			it.lockSpec = exactValue(it)
		}
		if !it.direct {
			it.lockSpec = "dep"
		}
		lockSpec := it.lockSpec

		dest := pl.destPath(it.key)
		if !pl.opts.DryRun && !pl.opts.LockfileOnly {
			uid, gid, had := statOwner(dest)
			if it.res.Local != "" {
				if err := linkLocal(it.res.Local, dest, pl.opts.Force); err != nil {
					pl.cleanupTemps(done)
					return sum, err
				}
			} else if it.pkgRoot != "" {
				if err := pl.applyPkgPatch(ctx, it, reporter); err != nil {
					pl.cleanupTemps(done)
					return sum, err
				}
				if err := archive.Swap(it.pkgRoot, dest); err != nil {
					pl.cleanupTemps(done)
					return sum, err
				}
				os.RemoveAll(it.tmpDir)
				it.tmpDir = ""
				if it.res.Clone == "" && source.IsFullSHA(it.res.SHA) {
					if err := manifest.WriteGitInfo(dest, it.res.SHA, it.res.Date, it.res.Source); err != nil {
						pl.cleanupTemps(done)
						return sum, err
					}
				}
			}
			if err := project.WriteStamp(dest, project.Stamp{SHA: it.res.SHA, Ref: it.res.Ref, Spec: lockSpec}); err != nil {
				return sum, err
			}
			if it.res.Local == "" {
				if err := keepOwner(dest, uid, gid, had); err != nil {
					return sum, err
				}
			}
		} else {
			os.RemoveAll(it.tmpDir)
			it.tmpDir = ""
		}

		date := ""
		if !it.res.Date.IsZero() {
			date = it.res.Date.UTC().Format(time.RFC3339)
		}
		pkg := project.Package{
			Spec: lockSpec, Ref: it.res.Ref, SHA: it.res.SHA, Date: date,
			Source: it.res.Source, Archive: it.res.Archive, Integrity: it.integrity,
			Version: it.man.Version, Policy: it.res.Policy,
			Dependencies: depMap[it.key],
		}
		pkg.Requires = project.Requires{
			MediaWiki:  it.man.Requires.MediaWiki,
			Extensions: it.man.Requires.Extensions,
			Skins:      it.man.Requires.Skins,
		}
		if old, ok := pl.p.Lock.Packages[it.key]; ok {
			pkg.Patch = old.Patch
		}
		pkg.Composer = string(composer.ModeNone)
		if !pl.opts.DryRun && !pl.opts.LockfileOnly {
			if mode, err := composer.Detect(dest, it.man); err == nil {
				pkg.Composer = string(mode)
			}
		}
		if !pl.opts.DryRun {
			pl.p.Lock.Packages[it.key] = pkg
			if it.direct && !pl.opts.NoSave {
				pl.setManifest(it.key, lockSpec)
			}
			if pl.opts.Disabled && (it.direct || it.kind == OpAdd) {
				pl.setDisabled(it.key, true)
			}
		}

		if it.kind == OpUpdate {
			sum.Updated++
		} else {
			sum.Added++
		}
		pl.Ops = append(pl.Ops, Op{Key: it.key, Kind: it.kind, From: it.fromSHA, To: it.res.SHA})
		line := formatAddLine(it)
		if it.kind == OpUpdate {
			line = formatUpdateLine(it)
		}
		addLines = append(addLines, line)
		if strings.HasPrefix(it.key, "skins/") {
			skinN++
		} else {
			extN++
		}
	}

	if !pl.opts.DryRun {
		if err := pl.p.Save(); err != nil {
			return sum, err
		}
	}

	reporter.ClearProgress()
	for _, h := range hints {
		reporter.Info("%s", h)
	}
	for _, l := range addLines {
		reporter.Info("%s", l)
	}
	if !pl.opts.DryRun {
		cplan := pl.composerPlan(done)
		if err := pl.finishMW(ctx, reporter, cplan, done); err != nil {
			return sum, err
		}
		if !pl.opts.LockfileOnly {
			pl.warnShadows(reporter, done)
		}
	}
	if n := extN + skinN; n > 0 && pl.opts.DryRun {
		reporter.Info("%s", countMsg(extN, skinN)+" would be installed")
	} else if n > 0 {
		verb := "installed"
		if sum.Added == 0 {
			verb = "updated"
		}
		reporter.Info("%s", countMsg(extN, skinN)+" "+verb)
	} else if len(pl.add) > 0 {
		reporter.Info("already up to date")
	}
	return sum, nil
}

func keepOwner(dest string, uid, gid int, had bool) error {
	if os.Geteuid() != 0 {
		return nil
	}
	if !had {
		var ok bool
		if uid, gid, ok = statOwner(filepath.Dir(dest)); !ok {
			return nil
		}
	}
	return chownTree(dest, uid, gid)
}

func (pl *Plan) warnShadows(reporter *ui.Reporter, done []*workItem) {
	// only this run's packages are checked
	// so pre-existing conflicts don't re-warn
	touched := make(map[string]bool, len(done))
	for _, it := range done {
		if !it.keep {
			touched[it.key] = true
		}
	}
	if len(touched) == 0 {
		return
	}
	dirs := make(map[string]string, len(pl.p.Lock.Packages))
	for key := range pl.p.Lock.Packages {
		dirs[key] = pl.destPath(key)
	}
	shadows, err := composer.DetectShadows(pl.p.Root, dirs)
	if err != nil {
		reporter.Warn("composer: %v", err)
		return
	}
	for _, s := range shadows {
		if slices.ContainsFunc(s.Copies, func(c composer.ShadowCopy) bool { return touched[c.Where] }) {
			reporter.Warn("%s (load order decides which copy wins)", s)
		}
	}
}

// one registry query per round
// Resolve then hits the memo and policy cache
func (pl *Plan) prefetch(ctx context.Context, reg *registry.Client, round []*workItem) error {
	var exts, skins []string
	for _, it := range round {
		_, locked := pl.p.Lock.Packages[it.key]
		if it.spec.Kind != spec.Registry || (locked && !pl.opts.Update && !pl.opts.Force) {
			continue
		}
		if it.spec.Skin {
			skins = append(skins, it.spec.Name)
		} else {
			exts = append(exts, it.spec.Name)
		}
	}
	if len(exts)+len(skins) == 0 {
		return nil
	}
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		_, _, err := reg.Branches(gctx, exts, skins)
		return err
	})
	g.Go(func() error {
		_, err := reg.Policies(gctx, exts, skins)
		return err
	})
	return g.Wait()
}

func (pl *Plan) prepareItem(ctx context.Context, it *workItem, resolver source.Resolver, ropts source.ResolveOpts, cacheDir string) error {
	if it.key == "" {
		it.key = guessKey(it.spec)
	}
	lp, haveLock := pl.p.Lock.Packages[it.key]
	if haveLock {
		it.fromSHA = lp.SHA
	}
	// revert replays the lock instead of resolving
	// the snapshot's sha is the whole point, so a moved ref must not win
	// local packages have no locked content, thus they take the normal path
	if pl.opts.Revert && haveLock && it.spec.Kind != spec.Local {
		if lp.Archive == "" && (lp.Source == "" || lp.SHA == "") {
			return fmt.Errorf("%s: nothing in the lock to revert to", it.key)
		}
		dest := pl.destPath(it.key)
		// lock is already the snapshot, hence the installed stamp is the only
		// record of what this revert is moving away from
		if s, ok := project.ReadStamp(dest); ok {
			it.fromSHA = s.SHA
		}
		if stampMatch(dest, lp) {
			it.keep = true
			it.res = source.Resolved{Name: it.spec.Name, Type: keyType(it.key), Ref: lp.Ref, SHA: lp.SHA}
			it.lockSpec = lp.Spec
			return nil
		}
		return pl.fetchFromLock(ctx, it, lp, cacheDir)
	}
	if haveLock && !pl.opts.Update && !pl.opts.Force {
		dest := pl.destPath(it.key)
		if stampMatch(dest, lp) {
			it.keep = true
			it.res = source.Resolved{Name: it.spec.Name, Type: keyType(it.key), Ref: lp.Ref, SHA: lp.SHA}
			it.lockSpec = lp.Spec
			return nil
		}
		if (it.fromLock || !it.direct) && lp.Archive != "" {
			return pl.fetchFromLock(ctx, it, lp, cacheDir)
		}
		if it.direct && lp.Archive != "" && lp.Spec == it.lockSpec {
			it.fromLock = true
			return pl.fetchFromLock(ctx, it, lp, cacheDir)
		}
	}

	res, err := resolver.Resolve(ctx, it.spec, ropts)
	if err != nil {
		return err
	}
	it.res = res
	if res.Type == "" {
		res.Type = "extensions"
		it.res.Type = "extensions"
	}
	it.key = res.Type + "/" + res.Name
	if lp2, ok := pl.p.Lock.Packages[it.key]; ok {
		it.fromSHA = lp2.SHA
		haveLock = true
		lp = lp2
	}

	if it.direct {
		if res.Spec.Kind == spec.Registry {
			it.lockSpec = registrySpec(it.spec)
		} else {
			it.lockSpec = stripAlias(it.spec)
		}
	} else {
		it.lockSpec = "dep"
	}

	if pl.opts.Update && haveLock && lp.SHA != "" && lp.SHA == res.SHA && !pl.opts.Force {
		it.keep = true
		return nil
	}
	// translation-only tips are not an update unless --l10n
	// a failed or missing log stays an update
	if pl.opts.Update && !pl.opts.L10n && haveLock && lp.SHA != "" && lp.SHA != res.SHA && source.CheapLog(res) {
		if logger, ok := resolver.(source.ChangeLogger); ok {
			commits, err := logger.Log(ctx, res, lp.SHA, res.SHA)
			if err == nil && source.L10nOnly(commits) {
				it.keep = true
				return nil
			}
		}
	}

	if res.Local != "" {
		it.man, _, err = manifest.Read(res.Local)
		return err
	}
	if res.Clone != "" && res.Archive == "" {
		parent := pl.parentDir(res.Type)
		it.tmpDir = filepath.Join(parent, ".phuo-tmp", res.Name)
		os.RemoveAll(it.tmpDir)
		if err := gitClone(ctx, res, it.tmpDir, pl.opts.Full); err != nil {
			return err
		}
		it.pkgRoot = it.tmpDir
		it.man, _, err = manifest.Read(it.pkgRoot)
		return err
	}
	if res.Archive == "" {
		return fmt.Errorf("%s: no archive URL", res.Name)
	}
	return pl.fetchArchive(ctx, it, cacheDir)
}

// no-archive lock entry (`github:`/`gitlab:`) always go https
// so an ssh origin in the lock still works
// and also lets the rewritten entry heals its source
func lockClone(lp project.Package) (source, clone string) {
	if sp, err := spec.Parse(lp.Spec); err == nil {
		switch sp.Kind {
		case spec.GitHub:
			return "https://github.com/" + sp.Repo, "https://github.com/" + sp.Repo + ".git"
		case spec.GitLab:
			return "https://gitlab.com/" + sp.Repo, "https://gitlab.com/" + sp.Repo + ".git"
		}
	}
	return lp.Source, lp.Source
}

func (pl *Plan) fetchFromLock(ctx context.Context, it *workItem, lp project.Package, cacheDir string) error {
	typ := keyType(it.key)
	name := keyName(it.key)
	it.res = source.Resolved{
		Name: name, Type: typ, Ref: lp.Ref, SHA: lp.SHA,
		Source: lp.Source, Archive: lp.Archive, Policy: lp.Policy,
		Spec: it.spec,
	}
	if lp.Date != "" {
		t, err := time.Parse(time.RFC3339, lp.Date)
		if err != nil {
			return fmt.Errorf("%s: lock date %q: %w", name, lp.Date, err)
		}
		it.res.Date = t
	}
	it.lockSpec = lp.Spec
	it.fromLock = true
	if lp.Archive == "" {
		if lp.Source == "" || lp.SHA == "" {
			return fmt.Errorf("%s: lock entry has no archive or source", name)
		}
		it.res.Source, it.res.Clone = lockClone(lp)
		it.res.Ref = lp.SHA
		it.tmpDir = filepath.Join(pl.parentDir(typ), ".phuo-tmp", name)
		os.RemoveAll(it.tmpDir)
		if err := gitClone(ctx, it.res, it.tmpDir, pl.opts.Full); err != nil {
			return err
		}
		it.res.Ref = lp.Ref
		it.pkgRoot = it.tmpDir
		man, _, err := manifest.Read(it.pkgRoot)
		it.man = man
		return err
	}
	if err := pl.fetchArchive(ctx, it, cacheDir); err != nil {
		return err
	}
	if lp.Integrity != "" && it.integrity != lp.Integrity && !it.fallback404 {
		if !pl.opts.Force {
			os.RemoveAll(it.tmpDir)
			return fmt.Errorf("%s: integrity mismatch", name)
		}
		it.integrityWarn = true
	}
	return nil
}

func isHTTP404(err error) bool {
	return err != nil && strings.Contains(err.Error(), "HTTP 404")
}

func (pl *Plan) fetchArchive(ctx context.Context, it *workItem, cacheDir string) error {
	parent := pl.parentDir(it.res.Type)
	if it.res.Type == "" {
		parent = pl.parentDir("extensions")
	}
	it.tmpDir = filepath.Join(parent, ".phuo-tmp", it.res.Name)
	os.RemoveAll(it.tmpDir)
	integrity, err := fetchInto(ctx, it.res, it.tmpDir, cacheDir)
	// extdist lists branch tips before their tarballs exist
	// therefore a snapshot URL can 404
	if isHTTP404(err) && it.res.SHA != "" && it.res.Source != "" {
		if fb, ok := source.ArchiveAt(it.res.Source, it.res.SHA); ok && fb != it.res.Archive {
			os.RemoveAll(it.tmpDir)
			it.res.Archive = fb
			it.fallback404 = true
			integrity, err = fetchInto(ctx, it.res, it.tmpDir, cacheDir)
		}
	}
	if err != nil {
		os.RemoveAll(it.tmpDir)
		return err
	}
	it.integrity = integrity
	pkgRoot, err := archive.PackageRoot(it.tmpDir)
	if err != nil {
		os.RemoveAll(it.tmpDir)
		return fmt.Errorf("%s: %w", it.res.Name, err)
	}
	it.pkgRoot = pkgRoot
	man, kind, err := manifest.Read(pkgRoot)
	if err != nil {
		os.RemoveAll(it.tmpDir)
		return err
	}
	it.man = man
	if kind != "" && kind != it.res.Type {
		it.res.Type = kind
		it.key = kind + "/" + it.res.Name
	}
	return nil
}

func fetchInto(ctx context.Context, res source.Resolved, tmp, cacheDir string) (string, error) {
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return "", err
	}
	root, err := os.OpenRoot(tmp)
	if err != nil {
		return "", err
	}
	defer root.Close()

	if strings.HasSuffix(strings.ToLower(res.Archive), ".zip") {
		zf := filepath.Join(tmp, ".archive.zip")
		f, err := os.Create(zf)
		if err != nil {
			return "", err
		}
		integrity, err := fetch.Download(ctx, res.Archive, cacheDir, f)
		f.Close()
		if err != nil {
			os.Remove(zf)
			return "", fmt.Errorf("%s: %w", res.Name, err)
		}
		err = archive.ExtractZip(zf, root)
		os.Remove(zf)
		if err != nil {
			return "", fmt.Errorf("%s: %w", res.Name, err)
		}
		return integrity, nil
	}

	pr, pw := io.Pipe()
	xerr := make(chan error, 1)
	go func() {
		err := archive.ExtractTarGz(pr, root)
		pr.CloseWithError(err)
		xerr <- err
	}()
	integrity, err := fetch.Download(ctx, res.Archive, cacheDir, pw)
	pw.CloseWithError(err)
	if e := <-xerr; e != nil {
		err = e
	}
	if err != nil {
		return "", fmt.Errorf("%s: %w", res.Name, err)
	}
	return integrity, nil
}

func gitClone(ctx context.Context, res source.Resolved, dest string, full bool) error {
	if _, err := exec.LookPath("git"); err != nil {
		return fmt.Errorf("git is required to clone %s", res.Name)
	}
	os.RemoveAll(dest)
	env := append(os.Environ(), fetch.GitEnv(res.Clone)...)
	// `--branch` is names only
	// an exact sha needs fetch + checkout
	ref := res.Ref
	if res.Spec.SHA != "" && source.IsFullSHA(res.SHA) {
		ref = res.SHA
	}
	if source.IsFullSHA(ref) {
		if err := os.MkdirAll(dest, 0o755); err != nil {
			return err
		}
		steps := [][]string{
			{"init", "--quiet"},
			{"remote", "add", "origin", res.Clone},
		}
		if full {
			steps = append(steps, []string{"fetch", "--quiet", "origin"})
		} else {
			steps = append(steps, []string{"fetch", "--quiet", "--depth", "1", "origin", ref})
		}
		steps = append(steps, []string{"checkout", "--quiet", ref})
		for _, args := range steps {
			cmd := exec.CommandContext(ctx, "git", args...)
			cmd.Dir = dest
			cmd.Env = env
			if out, err := cmd.CombinedOutput(); err != nil {
				os.RemoveAll(dest)
				return fmt.Errorf("git %s %s: %w\n%s", args[0], res.Name, err, out)
			}
		}
		return nil
	}
	args := []string{"clone"}
	if !full {
		args = append(args, "--depth", "1", "--single-branch")
	}
	if ref != "" {
		args = append(args, "--branch", ref)
	}
	args = append(args, res.Clone, dest)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Env = env
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("git clone %s: %w\n%s", res.Name, err, out)
	}
	return nil
}

func linkLocal(local, dest string, force bool) error {
	target, err := filepath.Abs(local)
	if err != nil {
		return err
	}
	if fi, err := os.Lstat(dest); err == nil {
		if fi.Mode()&os.ModeSymlink != 0 {
			cur, err := os.Readlink(dest)
			if err != nil {
				return err
			}
			same, err := samePath(cur, target)
			if err != nil {
				return err
			}
			if same {
				return nil
			}
			if !force {
				return fmt.Errorf("%s already exists and is not a symlink to %s", dest, target)
			}
			if err := os.Remove(dest); err != nil {
				return err
			}
		} else if force {
			if err := os.RemoveAll(dest); err != nil {
				return err
			}
		} else {
			return fmt.Errorf("%s already exists", dest)
		}
	}
	return os.Symlink(target, dest)
}

func samePath(a, b string) (bool, error) {
	var err error
	if a, err = filepath.Abs(a); err != nil {
		return false, err
	}
	if b, err = filepath.Abs(b); err != nil {
		return false, err
	}
	return filepath.Clean(a) == filepath.Clean(b), nil
}

func stampMatch(dest string, lp project.Package) bool {
	s, ok := project.ReadStamp(dest)
	return ok && s.SHA == lp.SHA && s.Ref == lp.Ref
}

func (pl *Plan) checkFrozen() error {
	for name, val := range pl.p.Manifest.Extensions {
		key := "extensions/" + name
		lp, ok := pl.p.Lock.Packages[key]
		if !ok || lp.Spec != val {
			return fmt.Errorf("frozen-lockfile: %s mismatch (phuo.json=%q lock=%q)", key, val, pkgSpec(lp, ok))
		}
	}
	for name, val := range pl.p.Manifest.Skins {
		key := "skins/" + name
		lp, ok := pl.p.Lock.Packages[key]
		if !ok || lp.Spec != val {
			return fmt.Errorf("frozen-lockfile: %s mismatch (phuo.json=%q lock=%q)", key, val, pkgSpec(lp, ok))
		}
	}
	for key, lp := range pl.p.Lock.Packages {
		if lp.Spec == "dep" {
			continue
		}
		name := keyName(key)
		var val string
		var ok bool
		if strings.HasPrefix(key, "skins/") {
			val, ok = pl.p.Manifest.Skins[name]
		} else {
			val, ok = pl.p.Manifest.Extensions[name]
		}
		if !ok {
			return fmt.Errorf("frozen-lockfile: %s in lock but not in phuo.json", key)
		}
		if val != lp.Spec {
			return fmt.Errorf("frozen-lockfile: %s mismatch (phuo.json=%q lock=%q)", key, val, lp.Spec)
		}
	}
	return nil
}

func pkgSpec(lp project.Package, ok bool) string {
	if !ok {
		return "<missing>"
	}
	return lp.Spec
}

func (pl *Plan) dependents(key string) []string {
	name := keyName(key)
	typ := keyType(key)
	var deps []string
	for k, pkg := range pl.p.Lock.Packages {
		if k == key {
			continue
		}
		m := pkg.Requires.Extensions
		if typ == "skins" {
			m = pkg.Requires.Skins
		}
		if _, ok := m[name]; ok {
			deps = append(deps, k)
		}
	}
	return deps
}

func joinNames(keys []string) string {
	names := make([]string, len(keys))
	for i, k := range keys {
		names[i] = keyName(k)
	}
	slices.Sort(names)
	return strings.Join(names, ", ")
}

func (pl *Plan) disabled(key string) bool { return slices.Contains(pl.p.Manifest.Disabled, key) }

func (pl *Plan) off(f localsettings.File, key string) bool {
	return !f.Outside.Has(key) && (pl.disabled(key) || f.Disabled.Has(key))
}

func (pl *Plan) setDisabled(key string, off bool) {
	d := &pl.p.Manifest.Disabled
	switch i := slices.Index(*d, key); {
	case off && i < 0:
		*d = append(*d, key)
		slices.Sort(*d)
	case !off && i >= 0:
		*d = slices.Delete(*d, i, i+1)
	}
}

func (pl *Plan) pruneDeps(dry bool, keep func(key string) bool) (int, []string, []string, error) {
	n := 0
	var lines, mergedKeys []string
	for {
		var drop []string
		for key, pkg := range pl.p.Lock.Packages {
			if pkg.Spec == "dep" && len(pl.dependents(key)) == 0 && !keep(key) {
				drop = append(drop, key)
			}
		}
		if len(drop) == 0 {
			break
		}
		for _, key := range drop {
			mergedKeys = append(mergedKeys, pl.mergedInclude(key)...)
			if !dry {
				if err := os.RemoveAll(pl.destPath(key)); err != nil {
					return n, lines, mergedKeys, err
				}
			}
			delete(pl.p.Lock.Packages, key)
			pl.setDisabled(key, false)
			n++
			lines = append(lines, " - "+keyName(key)+" (no longer required)")
			pl.Ops = append(pl.Ops, Op{Key: key, Kind: OpRemove})
		}
	}
	return n, lines, mergedKeys, nil
}

func (pl *Plan) resolveKey(name string) (string, error) {
	if strings.Contains(name, "/") {
		if _, ok := pl.p.Lock.Packages[name]; ok {
			return name, nil
		}
		return "", fmt.Errorf("%s is not installed", name)
	}
	if _, ok := pl.p.Lock.Packages["extensions/"+name]; ok {
		return "extensions/" + name, nil
	}
	if _, ok := pl.p.Lock.Packages["skins/"+name]; ok {
		return "skins/" + name, nil
	}
	return "", fmt.Errorf("%s is not installed", name)
}

func (pl *Plan) parentDir(typ string) string {
	if typ == "skins" {
		return filepath.Join(pl.p.Root, pl.p.Paths.Skins)
	}
	return filepath.Join(pl.p.Root, pl.p.Paths.Extensions)
}

func (pl *Plan) destPath(key string) string {
	typ, name, _ := strings.Cut(key, "/")
	return filepath.Join(pl.parentDir(typ), name)
}

func (pl *Plan) setManifest(key, val string) {
	name := keyName(key)
	if strings.HasPrefix(key, "skins/") {
		if pl.p.Manifest.Skins == nil {
			pl.p.Manifest.Skins = map[string]string{}
		}
		pl.p.Manifest.Skins[name] = val
		return
	}
	if pl.p.Manifest.Extensions == nil {
		pl.p.Manifest.Extensions = map[string]string{}
	}
	pl.p.Manifest.Extensions[name] = val
}

func (pl *Plan) dropManifest(key string) {
	pl.setDisabled(key, false)
	name := keyName(key)
	if strings.HasPrefix(key, "skins/") {
		delete(pl.p.Manifest.Skins, name)
		return
	}
	delete(pl.p.Manifest.Extensions, name)
}

func (pl *Plan) applyPkgPatch(ctx context.Context, it *workItem, reporter *ui.Reporter) error {
	rel, ok := pl.p.Manifest.PatchedDependencies[it.key]
	if !ok || rel == "" {
		return nil
	}
	path := rel
	if !filepath.IsAbs(path) {
		path = filepath.Join(pl.p.Root, rel)
	}
	err := patch.Apply(ctx, path, it.pkgRoot, true)
	if err != nil {
		if pl.opts.Force {
			reporter.Warn("patch %s failed for %s@%s; installing unpatched", rel, it.res.Name, shortSHA(it.res.SHA))
			return nil
		}
		return fmt.Errorf("patch %s does not apply to %s@%s: %w", rel, it.res.Name, shortSHA(it.res.SHA), err)
	}
	return nil
}

func (pl *Plan) checkNames(done []*workItem) error {
	var fresh []*workItem
	for _, it := range done {
		if !it.keep && it.man.Name != "" {
			fresh = append(fresh, it)
		}
	}
	if len(fresh) == 0 {
		return nil
	}
	names := make(map[string]string, len(pl.p.Lock.Packages)+len(fresh))
	for key := range pl.p.Lock.Packages {
		if man, _, err := manifest.Read(pl.destPath(key)); err == nil && man.Name != "" {
			names[man.Name] = key
		}
	}
	for _, it := range fresh {
		if prev, ok := names[it.man.Name]; ok && prev != it.key {
			return fmt.Errorf("%s: manifest name %q is already used by %s (MediaWiki cannot load both; drop one from phuo.json)",
				it.key, it.man.Name, prev)
		}
		names[it.man.Name] = it.key
	}
	return nil
}

func (pl *Plan) composerPlan(done []*workItem) composer.Plan {
	var p composer.Plan
	for _, it := range done {
		if it.keep {
			continue
		}
		lp := pl.p.Lock.Packages[it.key]
		switch composer.Mode(lp.Composer) {
		case composer.ModeMerged:
			p.Include = append(p.Include, it.key+"/composer.json")
		case composer.ModeVendored:
			if composer.NeedsInstall(pl.destPath(it.key), composer.ModeVendored) {
				p.InstallIn = append(p.InstallIn, pl.destPath(it.key))
			}
		}
	}
	return p
}

func (pl *Plan) finishMW(ctx context.Context, reporter *ui.Reporter, cplan composer.Plan, done []*workItem) error {
	if pl.opts.DryRun || pl.opts.LockfileOnly {
		return nil
	}
	path, f, ok, err := pl.localSettings()
	if err != nil {
		return err
	}
	if ok {
		if err := pl.writeLocalSettings(reporter, path, f); err != nil {
			return err
		}
	}
	pl.platformWarn(ctx, reporter, done)
	if err := pl.runComposer(ctx, reporter, cplan); err != nil {
		return err
	}
	var keys []string
	for _, it := range done {
		if !it.keep && !pl.disabled(it.key) {
			keys = append(keys, it.key)
		}
	}
	pl.schemaPrompt(ctx, reporter, keys)
	return nil
}

func (pl *Plan) SetLoad(ctx context.Context, reporter *ui.Reporter, names []string, on bool) error {
	path, f, ok, err := pl.localSettings()
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("phuo does not manage LocalSettings.php here (missing, \"localSettings\": false, or --no-load)")
	}
	off := func(key string) bool { return pl.off(f, key) }
	keys := make([]string, 0, len(names))
	for _, n := range names {
		key, err := pl.resolveKey(n)
		if err != nil {
			return err
		}
		keys = append(keys, key)
	}
	var changed []string
	if on {
		// requirements come along, otherwise mediawiki refuses to start
		for i := 0; i < len(keys); i++ {
			req := pl.p.Lock.Packages[keys[i]].Requires
			for typ, m := range map[string]map[string]string{"extensions/": req.Extensions, "skins/": req.Skins} {
				for n := range m {
					k := typ + n
					if _, ok := pl.p.Lock.Packages[k]; ok && off(k) && !slices.Contains(keys, k) {
						keys = append(keys, k)
					}
				}
			}
		}
		for _, key := range keys {
			if !off(key) {
				reporter.Info("%s is already enabled", keyName(key))
				continue
			}
			pl.setDisabled(key, false)
			changed = append(changed, key)
		}
		// an explicit enable beats a commented-out user line
		// once in the block, scans report it as loaded
		f.Disabled.Extensions = slices.DeleteFunc(f.Disabled.Extensions, func(n string) bool { return slices.Contains(changed, "extensions/"+n) })
		f.Disabled.Skins = slices.DeleteFunc(f.Disabled.Skins, func(n string) bool { return slices.Contains(changed, "skins/"+n) })
	} else {
		for _, key := range keys {
			if f.Outside.Has(key) {
				return fmt.Errorf("%s is loaded by your own line at %s:%d; comment that line out to disable it",
					keyName(key), filepath.Base(path), f.Line[key])
			}
			need := slices.DeleteFunc(pl.dependents(key), func(k string) bool { return off(k) || slices.Contains(keys, k) })
			if len(need) > 0 {
				return fmt.Errorf("%s is required by %s; disable those too", keyName(key), joinNames(need))
			}
		}
		for _, key := range keys {
			if off(key) {
				reporter.Info("%s is already disabled", keyName(key))
				continue
			}
			pl.setDisabled(key, true)
			changed = append(changed, key)
		}
	}
	if len(changed) == 0 {
		return nil
	}
	if err := pl.p.Save(); err != nil {
		return err
	}
	if err := pl.writeLocalSettings(reporter, path, f); err != nil {
		return err
	}
	if on {
		pl.schemaPrompt(ctx, reporter, changed)
	}
	return nil
}

// ok is false when phuo must leave LocalSettings.php alone
func (pl *Plan) localSettings() (path string, f localsettings.File, ok bool, err error) {
	if pl.opts.NoLoad || pl.p.Manifest.LocalSettings.Disabled() {
		return "", f, false, nil
	}
	path, _ = pl.p.Manifest.LocalSettings.Value("LocalSettings.php")
	if !filepath.IsAbs(path) {
		path = filepath.Join(pl.p.Root, path)
	}
	f, err = localsettings.Scan(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", f, false, nil
	}
	return path, f, err == nil, err
}

func (pl *Plan) writeLocalSettings(reporter *ui.Reporter, path string, f localsettings.File) error {
	var want localsettings.Loads
	var added, removed []string
	off := func(key string) bool { return pl.off(f, key) }
	for key := range pl.p.Lock.Packages {
		switch {
		case f.Outside.Has(key):
		case off(key):
			req := slices.DeleteFunc(pl.dependents(key), off)
			switch {
			case len(req) == 0:
			case pl.disabled(key):
				reporter.Warn("%s is disabled, but %s requires it; MediaWiki will refuse to start (phuo enable %s)",
					keyName(key), joinNames(req), keyName(key))
			default:
				reporter.Warn("%s is commented out at %s:%d, but %s requires it; MediaWiki will refuse to start",
					keyName(key), filepath.Base(path), f.Line[key], joinNames(req))
			}
		default:
			want.Add(key)
			if !f.InBlock.Has(key) {
				added = append(added, keyName(key))
			}
		}
	}
	for _, l := range []struct {
		typ   string
		names []string
	}{{"extensions/", f.InBlock.Extensions}, {"skins/", f.InBlock.Skins}} {
		for _, n := range l.names {
			if !want.Has(l.typ + n) {
				removed = append(removed, n)
			}
		}
	}
	changed, err := localsettings.Write(path, want)
	if err != nil || !changed {
		return err
	}
	slices.Sort(added)
	slices.Sort(removed)
	var parts []string
	if len(added) > 0 {
		parts = append(parts, "added "+strings.Join(added, ", "))
	}
	if len(removed) > 0 {
		parts = append(parts, "removed "+strings.Join(removed, ", "))
	}
	if len(parts) == 0 {
		parts = []string{"updated"}
	}
	reporter.Step("%s: %s", filepath.Base(path), strings.Join(parts, "; "))
	return nil
}

func (pl *Plan) runComposer(ctx context.Context, reporter *ui.Reporter, cplan composer.Plan) error {
	if pl.opts.NoComposer || pl.p.Manifest.Composer.Disabled() {
		return nil
	}
	bin, _ := pl.p.Manifest.Composer.Value("composer")
	idle := len(cplan.Include) == 0 && len(cplan.Exclude) == 0 && len(cplan.InstallIn) == 0
	if idle {
		reporter.Step("composer: nothing to do")
		return nil
	}
	var buf bytes.Buffer
	err := composer.Apply(ctx, pl.p.Root, bin, cplan, &buf)
	if err != nil {
		line := lastLine(buf.String())
		if line != "" {
			reporter.Error("%s", line)
		}
		return err
	}
	reporter.Step("composer: updated")
	return nil
}

func lastLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}
	if i := strings.LastIndex(s, "\n"); i >= 0 {
		return strings.TrimSpace(s[i+1:])
	}
	return s
}

func (pl *Plan) platformWarn(ctx context.Context, reporter *ui.Reporter, done []*workItem) {
	var fresh []*workItem
	for _, it := range done {
		if !it.keep && len(it.man.Requires.Platform) > 0 {
			fresh = append(fresh, it)
		}
	}
	if len(fresh) == 0 || pl.p.Manifest.PHP.Disabled() {
		return
	}
	bin, _ := pl.p.Manifest.PHP.Value("php")
	php := mw.PHP{Bin: bin, Root: pl.p.Root}
	if !php.Available() {
		return
	}
	plat, err := php.Platform(ctx)
	if err != nil {
		reporter.Warn("php: %v", err)
		return
	}
	for _, it := range fresh {
		for _, msg := range it.man.UnmetPlatform(plat.PHP, plat.Modules, plat.Abilities) {
			reporter.Warn("%s: %s", it.key, msg)
		}
	}
}

func (pl *Plan) schemaPrompt(ctx context.Context, reporter *ui.Reporter, keys []string) {
	var need []string
	for _, key := range keys {
		if ok, err := manifest.HasSchemaUpdates(pl.destPath(key)); err == nil && ok {
			need = append(need, keyName(key))
		}
	}
	if len(need) == 0 {
		return
	}
	if pl.opts.Revert {
		reporter.Warn("%s: database schema not reverted", strings.Join(need, ", "))
		return
	}
	hint := fmt.Sprintf("%s may change the database schema; run: php maintenance/run.php update --quick (or pass --update-db)", strings.Join(need, ", "))
	phpBin, phpOK := pl.p.Manifest.PHP.Value("php")
	p := mw.PHP{Bin: phpBin, Root: pl.p.Root}
	if phpOK && p.Available() {
		if pl.opts.UpdateDB || (pl.opts.Ask != nil && pl.opts.Ask(fmt.Sprintf("Run update.php now? (%s may have changed the schema)", strings.Join(need, ", ")))) {
			if err := p.UpdateDB(ctx, nil); err != nil {
				reporter.Warn("update.php: %v", err)
			}
			return
		}
	}
	reporter.Info("%s", hint)
}

func (pl *Plan) cleanupTemps(items []*workItem) {
	for _, it := range items {
		if it.tmpDir != "" {
			os.RemoveAll(it.tmpDir)
		}
	}
}

func reaches(start, target string, edges map[string][]string) bool {
	seen := map[string]bool{}
	var walk func(string) bool
	walk = func(n string) bool {
		if n == target {
			return true
		}
		if seen[n] {
			return false
		}
		seen[n] = true
		return slices.ContainsFunc(edges[n], walk)
	}
	return walk(start)
}

func guessKey(s spec.Spec) string {
	if s.Skin {
		return "skins/" + s.Name
	}
	return "extensions/" + s.Name
}

func keyName(key string) string {
	_, name, _ := strings.Cut(key, "/")
	return name
}

func keyType(key string) string {
	typ, _, _ := strings.Cut(key, "/")
	return typ
}

func stripAlias(s spec.Spec) string {
	str := s.String()
	if s.Name != "" {
		if rest, ok := strings.CutPrefix(str, s.Name+"@"); ok {
			return rest
		}
		if rest, ok := strings.CutPrefix(str, "skin:"+s.Name+"@"); ok {
			return "skin:" + rest
		}
	}
	return str
}

func registrySpec(s spec.Spec) string {
	s.Name, s.Skin = "", false
	return s.String()
}

func exactValue(it *workItem) string {
	if it.res.Spec.Kind == spec.Registry || it.spec.Kind == spec.Registry {
		ref := it.res.Ref
		if ref == "" {
			ref = "*"
		}
		return ref + "#" + it.res.SHA
	}
	base := stripAlias(it.spec)
	if it.res.SHA == "" {
		return base
	}
	if _, _, ok := strings.Cut(base, "#"); ok {
		return base
	}
	return base + "#" + it.res.SHA
}

func formatAddLine(it *workItem) string {
	sha7 := shortSHA(it.res.SHA)
	ref := it.res.Ref
	if ref == "" {
		ref = "*"
	}
	meta := sha7
	if !it.res.Date.IsZero() {
		meta += ", " + it.res.Date.UTC().Format("2006-01-02")
	}
	line := fmt.Sprintf(" + %s@%s (%s)", it.res.Name, ref, meta)
	if !it.direct && len(it.requirers) > 0 {
		line += "   <- required by " + strings.Join(it.requirers, ", ")
	}
	return line
}

func formatUpdateLine(it *workItem) string {
	return fmt.Sprintf(" ↑ %s %s -> %s", it.res.Name, cmp.Or(shortSHA(it.fromSHA), it.res.Ref), shortSHA(it.res.SHA))
}

func shortSHA(s string) string {
	if len(s) > 7 {
		return s[:7]
	}
	return s
}

func countMsg(exts, skins int) string {
	var parts []string
	if exts > 0 {
		if exts == 1 {
			parts = append(parts, "1 extension")
		} else {
			parts = append(parts, fmt.Sprintf("%d extensions", exts))
		}
	}
	if skins > 0 {
		if skins == 1 {
			parts = append(parts, "1 skin")
		} else {
			parts = append(parts, fmt.Sprintf("%d skins", skins))
		}
	}
	if len(parts) == 0 {
		return "0 packages"
	}
	return strings.Join(parts, ", ")
}

func checkDepVersion(reporter *ui.Reporter, name, constraint, version string) error {
	if constraint == "" || constraint == "*" {
		return nil
	}
	if version == "" {
		reporter.Warn("%s has no version; cannot check constraint %s", name, constraint)
		return nil
	}
	ok, err := manifest.Satisfies(constraint, version)
	if err != nil {
		return err
	}
	if !ok {
		reporter.Warn("%s@%s does not satisfy %s", name, version, constraint)
	}
	return nil
}
