package cli

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/huh"
)

func pickOutdated(cli *CLI, rows []outdatedRow, rel string) ([]string, bool, error) {
	if err := requireTTY(cli, "update -i"); err != nil {
		return nil, false, err
	}
	if len(rows) == 0 {
		return nil, false, nil
	}
	lags := map[string]bool{}
	opts := make([]huh.Option[string], 0, len(rows))
	for _, r := range rows {
		label := fmt.Sprintf("%s  %s  %s -> %s", r.Package, r.Ref, r.Current, r.Latest)
		if strings.Contains(r.Flag, "rel") {
			lags[r.Key] = true
			label += "  (" + rel + " available)"
		}
		opts = append(opts, huh.NewOption(label, r.Key).Selected(true))
	}
	selected, err := pickMany("Packages to update", opts)
	if err != nil || len(selected) == 0 {
		return selected, false, err
	}
	for _, k := range selected {
		if lags[k] {
			latest := false
			err := abort(form(huh.NewConfirm().Title(fmt.Sprintf("Move REL pins to %s?", rel)).Value(&latest)))
			return selected, latest, err
		}
	}
	return selected, false, nil
}
