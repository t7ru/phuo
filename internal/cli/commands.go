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

	"github.com/charmbracelet/huh"

	"github.com/t7ru/phuo/internal/archive"
	"github.com/t7ru/phuo/internal/fetch"
	"github.com/t7ru/phuo/internal/installer"
	"github.com/t7ru/phuo/internal/project"
	"github.com/t7ru/phuo/internal/source"
	"github.com/t7ru/phuo/internal/spec"
	"github.com/t7ru/phuo/internal/ui"
)

type InitCmd struct {
	ManageLoads bool `name:"manage-loads" help:"Move wfLoad* lines for adopted packages into the phuo block."`
}

type AddCmd struct {
	Specs        []string `arg:"" optional:"" name:"spec" help:"Package specs to add; omit to search and pick."`
	Skin         bool     `name:"skin" help:"Force skin type."`
	Git          bool     `name:"git" help:"Clone via git instead of tarball."`
	Full         bool     `name:"full" help:"Full git history (with --git)."`
	NoComposer   bool     `name:"no-composer" help:"Skip running composer."`
	NoLoad       bool     `name:"no-load" help:"Skip LocalSettings.php edits."`
	Disabled     bool     `name:"disabled" help:"Install without loading (see phuo enable)."`
	UpdateDB     bool     `name:"update-db" help:"Run update.php after install."`
	Force        bool     `name:"force" short:"f" help:"Force despite requirement mismatches."`
	DryRun       bool     `name:"dry-run" help:"Plan only; write nothing."`
	NoSave       bool     `name:"no-save" help:"Install without updating phuo.json."`
	LockfileOnly bool     `name:"lockfile-only" help:"Update the lock without touching dirs."`
	Exact        bool     `name:"exact" short:"E" help:"Pin #sha into phuo.json."`
}

func (c *AddCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	specs := make([]spec.Spec, 0, len(c.Specs))
	for _, s := range c.Specs {
		sp, err := spec.Parse(s)
		if err != nil {
			return err
		}
		if c.Skin {
			sp.Skin = true
		}
		specs = append(specs, sp)
	}
	if len(specs) == 0 {
		if err := requireTTY(cli, "add"); err != nil {
			return err
		}
		if specs, err = pickAdd(ctx, cli, p); err != nil || len(specs) == 0 {
			return err
		}
	}
	pl, err := installer.NewPlan(p, specs, nil, installer.Options{
		Force: c.Force, DryRun: c.DryRun, NoComposer: c.NoComposer, NoLoad: c.NoLoad, Disabled: c.Disabled,
		LockfileOnly: c.LockfileOnly, Exact: c.Exact, Git: c.Git, Full: c.Full,
		UpdateDB: c.UpdateDB, NoCache: cli.NoCache, Offline: cli.Offline, Skin: c.Skin, NoSave: c.NoSave,
		Jobs: cli.Jobs,
		Ask:  asker(cli),
	})
	if err != nil {
		return err
	}
	_, err = pl.Apply(ctx, rep)
	return err
}

type RemoveCmd struct {
	Names      []string `arg:"" optional:"" name:"name" predictor:"installed" help:"Packages to remove; omit to pick."`
	NoComposer bool     `name:"no-composer" help:"Skip running composer."`
	NoLoad     bool     `name:"no-load" help:"Skip LocalSettings.php edits."`
	Force      bool     `name:"force" short:"f" help:"Remove even if required by others."`
}

func (c *RemoveCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	names := c.Names
	if len(names) == 0 {
		if err := requireTTY(cli, "remove"); err != nil {
			return err
		}
		opts := installedOpts(p, nil)
		if len(opts) == 0 {
			return userErr("nothing installed")
		}
		if names, err = pickMany("Packages to remove", opts); err != nil || len(names) == 0 {
			return err
		}
	}
	pl, err := installer.NewPlan(p, nil, names, installer.Options{
		Force: c.Force, NoComposer: c.NoComposer, NoLoad: c.NoLoad,
		NoCache: cli.NoCache, Offline: cli.Offline,
		Ask: asker(cli),
	})
	if err != nil {
		return err
	}
	_, err = pl.Apply(ctx, rep)
	return err
}

type EnableCmd struct {
	Names []string `arg:"" optional:"" name:"name" predictor:"installed" help:"Packages or glob patterns to enable (requirements come along); omit to pick."`
}

func (c *EnableCmd) Run(ctx context.Context, cli *CLI) error {
	return setLoad(ctx, cli, "enable", c.Names, true)
}

type DisableCmd struct {
	Names []string `arg:"" optional:"" name:"name" predictor:"installed" help:"Packages or glob patterns to disable; omit to pick."`
}

func (c *DisableCmd) Run(ctx context.Context, cli *CLI) error {
	return setLoad(ctx, cli, "disable", c.Names, false)
}

func setLoad(ctx context.Context, cli *CLI, cmd string, names []string, on bool) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	if len(names) == 0 {
		if err := requireTTY(cli, cmd); err != nil {
			return err
		}
		opts := installedOpts(p, func(key string) bool { return slices.Contains(p.Manifest.Disabled, key) == on })
		if len(opts) == 0 {
			return userErr(fmt.Sprintf("nothing to %s", cmd))
		}
		title := "Packages to " + cmd
		if names, err = pickMany(title, opts); err != nil || len(names) == 0 {
			return err
		}
	}
	if names, err = expandNames(p, names); err != nil {
		return err
	}
	pl, err := installer.NewPlan(p, nil, nil, installer.Options{Ask: asker(cli)})
	if err != nil {
		return err
	}
	return pl.SetLoad(ctx, rep, names, on)
}

func expandNames(p *project.Project, names []string) ([]string, error) {
	var out []string
	for _, n := range names {
		if !strings.ContainsAny(n, "*?[") {
			out = append(out, n)
			continue
		}
		before := len(out)
		pat := []string{n}
		for key := range p.Lock.Packages {
			if nameMatches(keyName(key), pat) {
				out = append(out, key)
			}
		}
		if len(out) == before {
			return nil, userErr(fmt.Sprintf("no packages match %q", n))
		}
	}
	slices.Sort(out)
	return slices.Compact(out), nil
}

type InstallCmd struct {
	FrozenLockfile bool `name:"frozen-lockfile" help:"Error if phuo.json and lock disagree."`
	LockfileOnly   bool `name:"lockfile-only" help:"Resolve and write lock only."`
	Force          bool `name:"force" short:"f" help:"Force reinstall."`
	DryRun         bool `name:"dry-run" help:"Plan only; write nothing."`
	UpdateDB       bool `name:"update-db" help:"Run update.php after install."`
}

func (c *InstallCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	specs, err := manifestSpecs(p, nil, false)
	if err != nil {
		return err
	}
	pl, err := installer.NewPlan(p, specs, nil, installer.Options{
		Force: c.Force, DryRun: c.DryRun, LockfileOnly: c.LockfileOnly, Frozen: c.FrozenLockfile,
		UpdateDB: c.UpdateDB, NoCache: cli.NoCache, Offline: cli.Offline, Jobs: cli.Jobs,
		Ask: asker(cli),
	})
	if err != nil {
		return err
	}
	_, err = pl.Apply(ctx, rep)
	return err
}

func loadProject(ctx context.Context, cli *CLI) (*project.Project, *ui.Reporter, error) {
	cwd := cli.Cwd
	if cwd == "" {
		cwd = "."
	}
	p, err := project.Load(cwd)
	if err != nil {
		return nil, nil, err
	}
	if cli.CacheDir != "" {
		p.CacheDir = cli.CacheDir
	}
	rep := ui.New(ui.Options{
		JSON: cli.JSON, Silent: cli.Silent, Verbose: cli.Verbose,
		NoColor: cli.NoColor, NoProgress: cli.NoProgress,
	})
	return p, rep, nil
}

type UpdateCmd struct {
	Names       []string `arg:"" optional:"" name:"name" predictor:"installed" help:"Names or patterns to update."`
	Interactive bool     `name:"interactive" short:"i" help:"Interactive multi-select."`
	DryRun      bool     `name:"dry-run" help:"Plan only; write nothing."`
	Latest      bool     `name:"latest" help:"Move REL pins up to the derived rel."`
	L10n        bool     `name:"l10n" help:"Include packages whose only new commits are translations."`
	UpdateDB    bool     `name:"update-db" help:"Run update.php after update."`
}

func (c *UpdateCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	var specs []spec.Spec
	reg := registryClient(cli, p)
	if c.Interactive {
		rows, err := collectOutdated(ctx, p, c.Names, rep, reg, cli.Jobs)
		if err != nil {
			return err
		}
		rows = slices.DeleteFunc(rows, func(r outdatedRow) bool {
			return r.CurrentSHA == r.TargetSHA && !strings.Contains(r.Flag, "rel")
		})
		rows = withoutL10n(rows, c.L10n)
		keys, latest, err := pickOutdated(cli, rows, p.Rel)
		if err != nil {
			return err
		}
		if len(keys) == 0 {
			return nil
		}
		for _, key := range keys {
			sp, err := specForKey(p, key)
			if err != nil {
				return err
			}
			if latest || c.Latest {
				moveRel(p, key, &sp)
			}
			specs = append(specs, sp)
		}
	} else {
		specs, err = manifestSpecs(p, c.Names, c.Latest)
		if err != nil {
			return err
		}
		if len(specs) == 0 {
			return userErr("no packages to update")
		}
	}
	pl, err := installer.NewPlan(p, specs, nil, installer.Options{
		DryRun: c.DryRun, Update: true, L10n: c.L10n, UpdateDB: c.UpdateDB,
		NoCache: cli.NoCache, Offline: cli.Offline, Registry: reg, Jobs: cli.Jobs,
		Ask: asker(cli),
	})
	if err != nil {
		return err
	}
	_, err = pl.Apply(ctx, rep)
	return err
}

type PruneCmd struct {
	DryRun bool `name:"dry-run" help:"List only; delete nothing."`
}

func (c *PruneCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	var orphans []string
	scan := func(rel, prefix string) error {
		dir := filepath.Join(p.Root, rel)
		entries, err := os.ReadDir(dir)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			return err
		}
		for _, e := range entries {
			name := e.Name()
			if strings.HasPrefix(name, ".") {
				continue
			}
			full := filepath.Join(dir, name)
			fi, err := os.Lstat(full)
			if err != nil {
				return err
			}
			if fi.Mode()&os.ModeSymlink != 0 || !fi.IsDir() {
				continue
			}
			key := prefix + name
			if _, ok := p.Lock.Packages[key]; ok {
				continue
			}
			orphans = append(orphans, key)
		}
		return nil
	}
	if err := scan(p.Paths.Extensions, "extensions/"); err != nil {
		return err
	}
	if err := scan(p.Paths.Skins, "skins/"); err != nil {
		return err
	}
	if len(orphans) == 0 {
		rep.Info("nothing to prune")
		return nil
	}
	for _, key := range orphans {
		rep.Info(" %s", key)
	}
	if c.DryRun {
		return nil
	}
	if !cli.Yes {
		if !promptOK(cli) {
			return userErr("pass -y to delete")
		}
		del := false
		q := huh.NewConfirm().
			Title(fmt.Sprintf("Delete %d unmanaged directories?", len(orphans))).
			Affirmative("delete").Negative("keep").
			Value(&del)
		if err := abort(form(q)); err != nil {
			return err
		}
		if !del {
			rep.Info("kept")
			return nil
		}
	}
	for _, key := range orphans {
		typ, name, _ := strings.Cut(key, "/")
		parent := filepath.Join(p.Root, p.Paths.Extensions)
		if typ == "skins" {
			parent = filepath.Join(p.Root, p.Paths.Skins)
		}
		if err := os.RemoveAll(filepath.Join(parent, name)); err != nil {
			return err
		}
		rep.Info(" - %s", name)
	}
	return nil
}

type PatchCmd struct {
	Name       string `arg:"" optional:"" name:"name" predictor:"installed" help:"Package to patch."`
	Commit     bool   `name:"commit" help:"Write the patch file from local edits."`
	Remove     bool   `name:"remove" help:"Remove the patch and reinstall."`
	PatchesDir string `name:"patches-dir" default:"patches" help:"Directory for patch files."`
}

type DiffCmd struct {
	Name     string `arg:"" name:"name" predictor:"installed" help:"Package to diff."`
	Stat     bool   `name:"stat" help:"Show diffstat."`
	NameOnly bool   `name:"name-only" help:"List changed paths only."`
	All      bool   `name:"all" help:"Include vendor/i18n/stamp files."`
}

func (c *DiffCmd) Run(ctx context.Context, cli *CLI) error {
	p, _, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	key, err := p.Lock.Lookup(c.Name)
	if err != nil {
		return err
	}
	sp, err := specForKey(p, key)
	if err != nil {
		return err
	}
	reg := registryClient(cli, p)
	resolver := source.New(reg, fetch.Client())
	ropts := source.ResolveOpts{Rel: p.Rel, LTSRel: project.LTSRel(p.MWVersion), MWVer: p.MWVersion}
	res, err := resolver.Resolve(ctx, sp, ropts)
	if err != nil {
		return err
	}
	if res.Archive == "" {
		return userErr(fmt.Sprintf("%s: no archive to diff against", c.Name))
	}
	tmp, err := os.MkdirTemp("", "phuo-diff-*")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp)
	cacheDir := p.CacheDir
	if cli.NoCache {
		cacheDir = ""
	}
	if err := extractArchive(ctx, res.Archive, tmp, cacheDir); err != nil {
		return err
	}
	pkgRoot, err := archive.PackageRoot(tmp)
	if err != nil {
		return err
	}
	installed := packageDir(p, key)
	if _, err := os.Stat(installed); err != nil {
		return userErr(fmt.Sprintf("%s is not installed", c.Name))
	}

	left := installed
	right := pkgRoot
	if !c.All {
		leftCopy := filepath.Join(tmp, ".left")
		rightCopy := filepath.Join(tmp, ".right")
		if err := copyTreeFiltered(installed, leftCopy, false); err != nil {
			return err
		}
		if err := copyTreeFiltered(pkgRoot, rightCopy, false); err != nil {
			return err
		}
		left, right = leftCopy, rightCopy
	}

	if c.NameOnly {
		changed, err := nameOnlyDiff(left, right)
		if err != nil {
			return err
		}
		if cli.JSON {
			return json.MarshalWrite(os.Stdout, changed)
		}
		for _, path := range changed {
			fmt.Println(path)
		}
		return nil
	}
	if _, err := exec.LookPath("git"); err != nil {
		return envErr("git is required for phuo diff (or use --name-only)")
	}
	args := []string{"diff", "--no-index"}
	if c.Stat {
		args = append(args, "--stat")
	}
	args = append(args, left, right)
	cmd := exec.CommandContext(ctx, "git", args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	err = cmd.Run()
	if err != nil {
		if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
			return nil // diff found
		}
		return err
	}
	return nil
}
