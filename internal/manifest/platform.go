package manifest

import (
	"fmt"
	"slices"
	"strings"
)

func (m Manifest) UnmetPlatform(php string, modules []string, abilities map[string]bool) []string {
	loaded := make(map[string]bool, len(modules))
	for _, mod := range modules {
		loaded[strings.ToLower(mod)] = true
	}
	var out []string
	for key, val := range m.Requires.Platform {
		if key == "php" {
			constraint, isStr := val.(string)
			if !isStr {
				out = append(out, "platform.php is not a string")
				continue
			}
			satisfied, err := Satisfies(constraint, php)
			switch {
			case err != nil:
				out = append(out, fmt.Sprintf("platform.php %q: %v", constraint, err))
			case !satisfied:
				out = append(out, fmt.Sprintf("requires PHP %s (CLI is %s)", constraint, php))
			}
			continue
		}
		if name, ok := strings.CutPrefix(key, "ext-"); ok {
			if !loaded[strings.ToLower(name)] {
				out = append(out, "requires the "+name+" PHP extension")
			}
			continue
		}
		if name, ok := strings.CutPrefix(key, "ability-"); ok {
			need, isBool := val.(bool)
			if !isBool {
				out = append(out, fmt.Sprintf("platform.%s is not a boolean", key))
			} else if need && !abilities[name] {
				out = append(out, "requires the "+name+" ability")
			}
			continue
		}
		out = append(out, fmt.Sprintf("unknown platform requirement %q", key))
	}
	slices.Sort(out)
	return out
}
