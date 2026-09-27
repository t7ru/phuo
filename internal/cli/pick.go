package cli

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/charmbracelet/huh"

	"github.com/t7ru/phuo/internal/project"
	"github.com/t7ru/phuo/internal/registry"
	"github.com/t7ru/phuo/internal/spec"
)

func pickMany(title string, opts []huh.Option[string]) ([]string, error) {
	var out []string
	if err := abort(form(huh.NewMultiSelect[string]().Title(title).Options(opts...).Filtering(true).Value(&out))); err != nil {
		return nil, err
	}
	slices.Sort(out)
	return out, nil
}

func installedOpts(p *project.Project, keep func(key string) bool) []huh.Option[string] {
	keys := make([]string, 0, len(p.Lock.Packages))
	for key := range p.Lock.Packages {
		if keep == nil || keep(key) {
			keys = append(keys, key)
		}
	}
	slices.Sort(keys)
	opts := make([]huh.Option[string], 0, len(keys))
	for _, key := range keys {
		opts = append(opts, huh.NewOption(fmt.Sprintf("%-28s %s", keyName(key), shortSHA(p.Lock.Packages[key].SHA)), key))
	}
	return opts
}

func pickAdd(ctx context.Context, cli *CLI, p *project.Project) ([]spec.Spec, error) {
	reg := registryClient(cli, p)
	var q string
	search := huh.NewInput().Title("Search extensions and skins").Validate(huh.ValidateNotEmpty()).Value(&q)
	if err := abort(form(search)); err != nil {
		return nil, err
	}
	hits, err := reg.Search(ctx, q)
	if err != nil {
		return nil, err
	}
	hits = slices.DeleteFunc(hits, func(h registry.SearchHit) bool { return !h.Installable })
	if len(hits) == 0 {
		return nil, userErr(fmt.Sprintf("nothing installable matches %q", q))
	}
	opts := make([]huh.Option[string], 0, len(hits))
	for _, h := range hits {
		key := "extensions/" + h.Name
		if h.Skin {
			key = "skins/" + h.Name
		}
		opts = append(opts, huh.NewOption(fmt.Sprintf("%-28s %s", h.Name, h.Snippet), key))
	}
	keys, err := pickMany("Packages to add", opts)
	if err != nil || len(keys) == 0 {
		return nil, err
	}

	var exts, skins []string
	for _, key := range keys {
		if keyType(key) == "skins" {
			skins = append(skins, keyName(key))
		} else {
			exts = append(exts, keyName(key))
		}
	}
	branches, skinBranches, err := reg.Branches(ctx, exts, skins)
	if err != nil {
		return nil, err
	}
	refs := make([]string, len(keys))
	fields := make([]huh.Field, 0, len(keys))
	for i, key := range keys {
		b := branches[keyName(key)]
		if keyType(key) == "skins" {
			b = skinBranches[keyName(key)]
		}
		refs[i] = "*"
		fields = append(fields, huh.NewSelect[string]().
			Title("Ref for "+keyName(key)).
			Options(refOptions(b)...).
			Value(&refs[i]))
	}
	if err := abort(form(fields...)); err != nil {
		return nil, err
	}
	yes := false
	if err := abort(form(huh.NewConfirm().Title(fmt.Sprintf("Install %d packages?", len(keys))).Value(&yes))); err != nil {
		return nil, err
	}
	if !yes {
		return nil, nil
	}
	specs := make([]spec.Spec, 0, len(keys))
	for i, key := range keys {
		sp := spec.Spec{Kind: spec.Registry, Name: keyName(key), Skin: keyType(key) == "skins"}
		if refs[i] != "*" {
			sp.Ref = refs[i]
		}
		specs = append(specs, sp)
	}
	return specs, nil
}

func refOptions(b registry.Branches) []huh.Option[string] {
	opts := []huh.Option[string]{huh.NewOption("* (follow policy)", "*")}
	for _, r := range slices.Sorted(maps.Keys(b.Refs)) {
		if r = strings.TrimSpace(r); r != "" {
			opts = append(opts, huh.NewOption(r, r))
		}
	}
	return opts
}
