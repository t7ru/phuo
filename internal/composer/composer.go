package composer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"

	"encoding/json/jsontext"
	json "encoding/json/v2"

	"github.com/t7ru/phuo/internal/manifest"
)

type Mode string

const (
	ModeNone     Mode = "none"
	ModeVendored Mode = "vendored"
	ModeMerged   Mode = "merged"
)

type composerJSON struct {
	Require map[string]string `json:"require"`
}

func Detect(dir string, m manifest.Manifest) (Mode, error) {
	b, err := os.ReadFile(filepath.Join(dir, "composer.json"))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return ModeNone, nil
		}
		return "", err
	}
	var cj composerJSON
	if err := json.Unmarshal(b, &cj); err != nil {
		return "", err
	}
	if onlyPlatformRequires(cj.Require) {
		return ModeNone, nil
	}
	if m.LoadComposerAutoloader {
		return ModeVendored, nil
	}
	return ModeMerged, nil
}

func onlyPlatformRequires(req map[string]string) bool {
	for k := range req {
		switch {
		case k == "php", k == "composer/installers", k == "composer-plugin-api":
		case strings.HasPrefix(k, "ext-"):
		default:
			return false
		}
	}
	return true
}

func NeedsInstall(dir string, mode Mode) bool {
	if mode != ModeVendored {
		return false
	}
	_, err := os.Stat(filepath.Join(dir, "vendor", "autoload.php"))
	return err != nil
}

type Plan struct {
	Include, Exclude []string
	InstallIn        []string
}

func Apply(ctx context.Context, root, bin string, p Plan, out io.Writer) error {
	if bin == "" {
		bin = "composer"
	}
	needUpdate := false
	if len(p.Include) > 0 || len(p.Exclude) > 0 {
		path := filepath.Join(root, "composer.local.json")
		doc, err := readLocal(path)
		if err != nil {
			return err
		}
		include, err := getInclude(doc)
		if err != nil {
			return err
		}
		orig := slices.Clone(include)
		include = applyIncludes(include, p.Include, p.Exclude)
		changed := !slices.Equal(orig, include)
		needUpdate = changed || globTouched(orig, p.Include, p.Exclude)
		if changed {
			setInclude(doc, include)
			if err := writeJSON(path, doc); err != nil {
				return err
			}
		}
	}
	return runPlan(ctx, root, bin, p, out, needUpdate)
}

func runPlan(ctx context.Context, root, bin string, p Plan, out io.Writer, needUpdate bool) error {
	if !needUpdate && len(p.InstallIn) == 0 {
		return nil
	}

	var cmds []string
	if needUpdate {
		cmds = append(cmds, bin+" update --no-dev")
	}
	for range p.InstallIn {
		cmds = append(cmds, bin+" install --no-dev")
	}
	if _, err := exec.LookPath(bin); err != nil {
		return fmt.Errorf("%s not found; run: %s", bin, strings.Join(cmds, "; "))
	}

	if needUpdate {
		if err := runComposer(ctx, bin, root, []string{"update", "--no-dev"}, out); err != nil {
			return err
		}
	}
	for _, d := range p.InstallIn {
		dir := d
		if !filepath.IsAbs(dir) {
			dir = filepath.Join(root, dir)
		}
		if err := runComposer(ctx, bin, dir, []string{"install", "--no-dev"}, out); err != nil {
			return err
		}
	}
	return nil
}

func globTouched(include, adds, removes []string) bool {
	for _, p := range adds {
		if coveredByGlob(p, include) {
			return true
		}
	}
	for _, p := range removes {
		if coveredByGlob(p, include) {
			return true
		}
	}
	return false
}

func applyIncludes(include, adds, removes []string) []string {
	out := slices.Clone(include)
	for _, r := range removes {
		out = slices.DeleteFunc(out, func(s string) bool { return s == r })
	}
	for _, a := range adds {
		if coveredByGlob(a, out) || slices.Contains(out, a) {
			continue
		}
		out = append(out, a)
	}
	return out
}

func coveredByGlob(path string, include []string) bool {
	for _, g := range include {
		switch g {
		case "extensions/*/composer.json":
			if matchStar(path, "extensions") {
				return true
			}
		case "skins/*/composer.json":
			if matchStar(path, "skins") {
				return true
			}
		}
	}
	return false
}

func matchStar(path, tree string) bool {
	rest, ok := strings.CutPrefix(path, tree+"/")
	if !ok {
		return false
	}
	name, rest, ok := strings.Cut(rest, "/")
	return ok && rest == "composer.json" && name != "" && !strings.Contains(name, "/")
}

func readLocal(path string) (map[string]any, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return map[string]any{}, nil
		}
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}

func getInclude(doc map[string]any) ([]string, error) {
	extra, _ := doc["extra"].(map[string]any)
	if extra == nil {
		return nil, nil
	}
	mp, _ := extra["merge-plugin"].(map[string]any)
	if mp == nil {
		return nil, nil
	}
	return asStrings(mp["include"])
}

func setInclude(doc map[string]any, include []string) {
	extra, _ := doc["extra"].(map[string]any)
	if extra == nil {
		extra = map[string]any{}
		doc["extra"] = extra
	}
	mp, _ := extra["merge-plugin"].(map[string]any)
	if mp == nil {
		mp = map[string]any{}
		extra["merge-plugin"] = mp
	}
	arr := make([]any, len(include))
	for i, s := range include {
		arr[i] = s
	}
	mp["include"] = arr
}

func asStrings(v any) ([]string, error) {
	switch x := v.(type) {
	case nil:
		return nil, nil
	case []any:
		out := make([]string, 0, len(x))
		for _, e := range x {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("composer.local.json: include entry is not a string")
			}
			out = append(out, s)
		}
		return out, nil
	case []string:
		return slices.Clone(x), nil
	default:
		return nil, fmt.Errorf("composer.local.json: include is not an array")
	}
}

func writeJSON(path string, v any) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".phuo-composer-*")
	if err != nil {
		return err
	}
	err = json.MarshalWrite(f, v, json.Deterministic(true), jsontext.Multiline(true), jsontext.WithIndent("\t"))
	if err = errors.Join(err, f.Close()); err != nil {
		os.Remove(f.Name())
		return err
	}
	return os.Rename(f.Name(), path)
}

func runComposer(ctx context.Context, bin, dir string, args []string, out io.Writer) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	if out != nil {
		cmd.Stdout = out
		cmd.Stderr = out
	}
	return cmd.Run()
}
