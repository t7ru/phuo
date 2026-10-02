package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"slices"

	"github.com/t7ru/phuo/internal/localsettings"
	"github.com/t7ru/phuo/internal/project"
	"github.com/t7ru/phuo/internal/ui"
)

type AdoptCmd struct{}

func (c *AdoptCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	return adoptLoads(p, rep)
}

func adoptLoads(p *project.Project, rep *ui.Reporter) error {
	if p.Manifest.LocalSettings.Disabled() {
		return userErr(`adopt: "localSettings": false`)
	}
	path, ok := p.Manifest.LocalSettings.Value("LocalSettings.php")
	if !ok {
		return userErr(`adopt: "localSettings": false`)
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(p.Root, path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	f, err := localsettings.ScanBytes(b)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var keys []string
	for _, key := range f.Outside.Keys() {
		if _, ok := p.Lock.Packages[key]; ok {
			keys = append(keys, key)
		}
	}
	if len(keys) == 0 {
		rep.Info("nothing to adopt")
		return nil
	}
	out, err := localsettings.Drop(b, keys)
	if err != nil {
		return err
	}
	f, err = localsettings.ScanBytes(out)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	var want localsettings.Loads
	for key := range p.Lock.Packages {
		if f.Outside.Has(key) || slices.Contains(p.Manifest.Disabled, key) || f.Disabled.Has(key) {
			continue
		}
		want.Add(key)
	}
	out, err = localsettings.Render(out, want)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !bytes.Equal(out, b) {
		if err := os.WriteFile(path, out, 0o644); err != nil {
			return err
		}
	}
	slices.Sort(keys)
	for _, key := range keys {
		rep.Info("adopted %s", keyName(key))
	}
	return nil
}
