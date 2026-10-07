package cli

import (
	"context"
	"fmt"
	"strings"

	"github.com/t7ru/phuo/internal/project"
	"github.com/t7ru/phuo/internal/spec"
)

type PinCmd struct {
	Names []string `arg:"" name:"name" predictor:"installed" help:"Packages to pin at their current sha."`
}

type UnpinCmd struct {
	Names []string `arg:"" name:"name" predictor:"installed" help:"Packages to unpin."`
}

func (c *PinCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	var rows [][2]string
	for _, name := range c.Names {
		key, lp, cur, err := directPkg(p, name, "pin")
		if err != nil {
			return err
		}
		if lp.SHA == "" {
			return userErr(fmt.Sprintf("pin: %s has no sha", keyName(key)))
		}
		next, err := pinValue(cur, lp)
		if err != nil {
			return err
		}
		if cur == next && lp.Spec == next {
			rep.Info("%s is already pinned", keyName(key))
			continue
		}
		rows = append(rows, [2]string{key, next})
	}
	if err := writeSpecs(p, rows); err != nil {
		return err
	}
	for _, row := range rows {
		rep.Info("pinned %s %s", keyName(row[0]), row[1])
	}
	return nil
}

func (c *UnpinCmd) Run(ctx context.Context, cli *CLI) error {
	p, rep, err := loadProject(ctx, cli)
	if err != nil {
		return err
	}
	var rows [][2]string
	for _, name := range c.Names {
		key, lp, cur, err := directPkg(p, name, "unpin")
		if err != nil {
			return err
		}
		next, ok, err := unpinValue(cur, lp)
		if err != nil {
			return err
		}
		if !ok {
			rep.Info("%s is not pinned", keyName(key))
			continue
		}
		rows = append(rows, [2]string{key, next})
	}
	if err := writeSpecs(p, rows); err != nil {
		return err
	}
	for _, row := range rows {
		rep.Info("unpinned %s", keyName(row[0]))
	}
	return nil
}

func directPkg(p *project.Project, name, cmd string) (string, project.Package, string, error) {
	key, err := p.Lock.Lookup(name)
	if err != nil {
		return "", project.Package{}, "", err
	}
	lp := p.Lock.Packages[key]
	if lp.Spec == "dep" {
		return "", project.Package{}, "", userErr(fmt.Sprintf("%s: %s is a dependency", cmd, keyName(key)))
	}
	return key, lp, readSpec(p, key), nil
}

func readSpec(p *project.Project, key string) string {
	name := keyName(key)
	if keyType(key) == "skins" {
		if v, ok := p.Manifest.Skins[name]; ok {
			return v
		}
	} else if v, ok := p.Manifest.Extensions[name]; ok {
		return v
	}
	return p.Lock.Packages[key].Spec
}

func writeSpecs(p *project.Project, rows [][2]string) error {
	if len(rows) == 0 {
		return nil
	}
	for _, row := range rows {
		key, val := row[0], row[1]
		name := keyName(key)
		if keyType(key) == "skins" {
			if p.Manifest.Skins == nil {
				p.Manifest.Skins = map[string]string{}
			}
			p.Manifest.Skins[name] = val
		} else {
			if p.Manifest.Extensions == nil {
				p.Manifest.Extensions = map[string]string{}
			}
			p.Manifest.Extensions[name] = val
		}
		lp := p.Lock.Packages[key]
		lp.Spec = val
		p.Lock.Packages[key] = lp
	}
	return p.Save()
}

// registry pins keep the manifest ref (else the locked branch) and the lock sha
// other kinds put the sha in the ref, which is how github/git specs already select a commit
func pinValue(val string, lp project.Package) (string, error) {
	sp, err := spec.Parse(val)
	if err != nil {
		return "", err
	}
	sp.Name, sp.Skin = "", false
	if sp.Kind == spec.Registry {
		if sp.Ref == "" {
			sp.Ref = lp.Ref
		}
		if sp.Ref == "" {
			sp.Ref = "*"
		}
		sp.SHA = lp.SHA
		return sp.String(), nil
	}
	sp.Ref, sp.SHA = lp.SHA, ""
	return sp.String(), nil
}

func unpinValue(val string, lp project.Package) (string, bool, error) {
	if lp.SHA == "" || !strings.HasSuffix(val, "#"+lp.SHA) {
		return "", false, nil
	}
	sp, err := spec.Parse(val)
	if err != nil {
		return "", false, err
	}
	sp.Name, sp.Skin = "", false
	if sp.Kind == spec.Registry {
		return "*", true, nil
	}
	if sp.Ref == lp.SHA {
		sp.Ref = lp.Ref
		if sp.Ref == lp.SHA {
			sp.Ref = ""
		}
	}
	sp.SHA = ""
	return sp.String(), true, nil
}
