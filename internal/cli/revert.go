package cli

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/charmbracelet/huh"

	"github.com/t7ru/phuo/internal/installer"
	"github.com/t7ru/phuo/internal/project"
	"github.com/t7ru/phuo/internal/spec"
)

type RevertCmd struct {
	Name        string `arg:"" optional:"" name:"name" help:"Package to revert; omit to revert phuo.json and phuo.lock."`
	Interactive bool   `name:"interactive" short:"i" help:"Pick the packages to revert."`
	DryRun      bool   `name:"dry-run" help:"Plan only; write nothing."`
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
	m, l, ok, err := p.Previous(1)
	if err != nil {
		return err
	}
	if !ok {
		return userErr(`revert: no snapshot yet (one is taken before each project write; "snapshots" in phuo.json sets how many are kept)`)
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
	case c.Name == "":
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
				DryRun: c.DryRun, NoCache: cli.NoCache, Offline: cli.Offline,
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
		rep.Info("reverting phuo.json and phuo.lock (%d packages)", len(p.Lock.Packages))
	default:
		key, err := resolveInstalledKey(p, c.Name)
		if err != nil {
			return err
		}
		lp, ok := l.Packages[key]
		if !ok {
			return userErr(fmt.Sprintf("revert: %s is not in the previous lock", c.Name))
		}
		if !replayable(lp) {
			return userErr(fmt.Sprintf("revert: %s has no version recorded in the snapshot (nothing to restore)", c.Name))
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
		Revert: true, DryRun: c.DryRun, NoCache: cli.NoCache, Offline: cli.Offline,
		Ask: asker(cli),
	})
	if err != nil {
		return err
	}
	_, err = pl.Apply(ctx, rep)
	return err
}
