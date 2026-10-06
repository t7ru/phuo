package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/charmbracelet/huh"

	"github.com/t7ru/phuo/internal/installer"
	"github.com/t7ru/phuo/internal/project"
	"github.com/t7ru/phuo/internal/spec"
)

type RevertCmd struct {
	Name        string `arg:"" optional:"" name:"name" predictor:"installed" help:"Package to revert, or a snapshot index. Omit to revert the previous phuo.json and phuo.lock."`
	Interactive bool   `name:"interactive" short:"i" help:"Pick the packages to revert. Lists snapshots when more than one is kept."`
	DryRun      bool   `name:"dry-run" help:"Plan only; write nothing."`
}

// a bare number is a snapshot index
// extensions/2 still names a package
func (c *RevertCmd) slot() (n int, name string, indexed bool, err error) {
	if c.Name == "" {
		return 1, "", false, nil
	}
	i, nerr := strconv.Atoi(c.Name)
	if nerr != nil {
		return 1, c.Name, false, nil
	}
	if i < 1 {
		return 0, "", true, userErr("revert: snapshot index starts at 1")
	}
	return i, "", true, nil
}

// init-adopted entries (thus no sha or archive) record nothing to go back to
func replayable(lp project.Package) bool {
	return lp.Archive != "" || (lp.Source != "" && lp.SHA != "")
}

func (c *RevertCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	n, name, indexed, err := c.slot()
	if err != nil {
		return err
	}
	if c.Interactive && !indexed && p.Manifest.SnapshotCount() > 1 {
		if err := requireTTY(cli, "revert -i"); err != nil {
			return err
		}
		n, err = pickSnapshot(p)
		if err != nil || n == 0 {
			return err
		}
	}
	m, l, ok, err := p.Previous(n)
	if err != nil {
		return err
	}
	if !ok {
		return noSnapshot(n)
	}

	var specs []spec.Spec
	switch {
	case c.Interactive:
		if err := requireTTY(cli, "revert -i"); err != nil {
			return err
		}
		opts := make([]huh.Option[string], 0, len(p.Lock.Packages))
		for key, lp := range p.Lock.Packages {
			old, ok := l.Packages[key]
			if !ok || !replayable(old) || old.SHA == lp.SHA {
				continue
			}
			opts = append(opts, huh.NewOption(fmt.Sprintf("%-24s %s -> %s", keyName(key), shortSHA(lp.SHA), shortSHA(old.SHA)), key))
		}
		slices.SortFunc(opts, func(a, b huh.Option[string]) int { return cmp.Compare(a.Value, b.Value) })
		if len(opts) == 0 {
			rep.Info("nothing to revert")
			return nil
		}
		keys, err := pickMany("Packages to revert", opts)
		if err != nil || len(keys) == 0 {
			return err
		}
		specs = make([]spec.Spec, 0, len(keys))
		for _, key := range keys {
			p.Lock.Packages[key] = l.Packages[key]
			sp, err := specForKey(p, key)
			if err != nil {
				return err
			}
			specs = append(specs, sp)
		}
	case name == "":
		m.Snapshots = p.Manifest.Snapshots // the knob itself isn't history
		// extras added since the snapshot have to go too
		// like `remove`, this runs before the restore and must not write state
		// as a save would rotate the undo point away before the revert lands
		var extra []string
		for key := range p.Lock.Packages {
			if _, ok := l.Packages[key]; !ok {
				extra = append(extra, keyName(key))
			}
		}
		if len(extra) > 0 {
			rpl, err := installer.NewPlan(p, nil, extra, installer.Options{
				DryRun: c.DryRun, NoCache: cli.NoCache, Offline: cli.Offline, Jobs: cli.Jobs,
				SkipSave: true, Ask: asker(cli),
			})
			if err != nil {
				return err
			}
			if _, err := rpl.Apply(ctx, rep); err != nil {
				return err
			}
		}
		for key, cur := range p.Lock.Packages {
			if old, ok := l.Packages[key]; ok && !replayable(old) {
				l.Packages[key] = cur
				rep.Info("kept %s (the snapshot records no version for it)", keyName(key))
			}
		}
		p.SetState(m, l)
		specs, err = manifestSpecs(p, nil, false)
		if err != nil {
			return err
		}
		if n == 1 {
			rep.Info("reverting phuo.json and phuo.lock (%d packages)", len(p.Lock.Packages))
		} else {
			rep.Info("reverting snapshot %d (%d packages)", n, len(p.Lock.Packages))
		}
	default:
		key, err := resolveInstalledKey(p, name)
		if err != nil {
			return err
		}
		lp, ok := l.Packages[key]
		if !ok {
			return userErr(fmt.Sprintf("revert: %s is not in the previous lock", name))
		}
		if !replayable(lp) {
			return userErr(fmt.Sprintf("revert: %s has no version recorded in the snapshot (nothing to restore)", name))
		}
		sp, err := specForKey(p, key)
		if err != nil {
			return err
		}
		rep.Info("reverting %s %s -> %s", keyName(key), shortSHA(p.Lock.Packages[key].SHA), shortSHA(lp.SHA))
		p.Lock.Packages[key] = lp
		specs = []spec.Spec{sp}
	}

	pl, err := installer.NewPlan(p, specs, nil, installer.Options{
		Revert: true, DryRun: c.DryRun, NoCache: cli.NoCache, Offline: cli.Offline, Jobs: cli.Jobs,
		Ask: asker(cli),
	})
	if err != nil {
		return err
	}
	_, err = pl.Apply(ctx, rep)
	return err
}

func noSnapshot(n int) error {
	if n == 1 {
		return userErr(`revert: no snapshot yet (one is taken before each project write; "snapshots" in phuo.json sets how many are kept)`)
	}
	return userErr(fmt.Sprintf("revert: no snapshot %d (\"snapshots\" in phuo.json sets how many are kept)", n))
}

func pickSnapshot(p *project.Project) (int, error) {
	limit := p.Manifest.SnapshotCount()
	opts := make([]huh.Option[string], 0, limit)
	for i := 1; i <= limit; i++ {
		_, l, ok, err := p.Previous(i)
		if err != nil {
			return 0, err
		}
		if !ok {
			continue
		}
		opts = append(opts, huh.NewOption(fmt.Sprintf("%d  %d changes", i, snapshotChanges(p.Lock, l)), strconv.Itoa(i)))
	}
	if len(opts) == 0 {
		return 0, noSnapshot(1)
	}
	if len(opts) == 1 {
		return strconv.Atoi(opts[0].Value)
	}
	s, err := pickOne("Snapshot", opts)
	if err != nil || s == "" {
		return 0, err
	}
	return strconv.Atoi(s)
}

func snapshotChanges(cur, old project.Lock) int {
	n := 0
	for key, lp := range cur.Packages {
		prev, ok := old.Packages[key]
		if !ok || prev.SHA != lp.SHA {
			n++
		}
	}
	for key := range old.Packages {
		if _, ok := cur.Packages[key]; !ok {
			n++
		}
	}
	return n
}
