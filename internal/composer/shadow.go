package composer

import (
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	json "encoding/json/v2"
)

type ShadowCopy struct {
	Where   string
	Version string
}

type Shadow struct {
	Name   string
	Copies []ShadowCopy
}

func (s Shadow) String() string {
	parts := make([]string, len(s.Copies))
	for i, c := range s.Copies {
		parts[i] = c.Where + "=" + c.Version
	}
	return s.Name + ": " + strings.Join(parts, " ")
}

func DetectShadows(root string, dirs map[string]string) ([]Shadow, error) {
	type source struct {
		where, path string
	}
	sources := make([]source, 0, len(dirs)+1)
	sources = append(sources, source{"vendor", filepath.Join(root, "vendor", "composer", "installed.json")})
	for _, key := range slices.Sorted(maps.Keys(dirs)) {
		sources = append(sources, source{key, filepath.Join(dirs[key], "vendor", "composer", "installed.json")})
	}
	byName := map[string][]ShadowCopy{}
	for _, s := range sources {
		pkgs, err := readInstalled(s.path)
		if err != nil {
			return nil, err
		}
		for name, version := range pkgs {
			byName[name] = append(byName[name], ShadowCopy{Where: s.where, Version: version})
		}
	}
	var out []Shadow
	for name, copies := range byName {
		if len(copies) > 1 && differ(copies) {
			out = append(out, Shadow{Name: name, Copies: copies})
		}
	}
	slices.SortFunc(out, func(a, b Shadow) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

func differ(copies []ShadowCopy) bool {
	compat := compatVersion(copies[0].Version)
	return slices.ContainsFunc(copies[1:], func(c ShadowCopy) bool { return compatVersion(c.Version) != compat })
}

func compatVersion(v string) string {
	v = strings.TrimPrefix(v, "v")
	major, rest, ok := strings.Cut(v, ".")
	if major != "0" || !ok {
		return major
	}
	minor, _, _ := strings.Cut(rest, ".")
	return "0." + minor
}

func readInstalled(path string) (map[string]string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, nil
		}
		return nil, err
	}
	var doc struct {
		Packages []struct {
			Name    string `json:"name"`
			Version string `json:"version"`
		} `json:"packages"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	out := make(map[string]string, len(doc.Packages))
	for _, pkg := range doc.Packages {
		out[pkg.Name] = pkg.Version
	}
	return out, nil
}
