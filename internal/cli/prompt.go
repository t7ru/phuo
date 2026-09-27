package cli

import (
	"errors"
	"fmt"
	"os"

	"github.com/charmbracelet/huh"
	"github.com/charmbracelet/lipgloss"
	"golang.org/x/term"

	"github.com/t7ru/phuo/internal/ui"
)

var theme = buildTheme()

func buildTheme() *huh.Theme {
	t := huh.ThemeCharm()
	c := lipgloss.Color(ui.Accent)
	for _, f := range []*huh.FieldStyles{&t.Focused, &t.Blurred} {
		f.Title = f.Title.Foreground(c)
		f.SelectSelector = f.SelectSelector.Foreground(c)
		f.NextIndicator = f.NextIndicator.Foreground(c)
		f.PrevIndicator = f.PrevIndicator.Foreground(c)
		f.MultiSelectSelector = f.MultiSelectSelector.Foreground(c)
		f.TextInput.Prompt = f.TextInput.Prompt.Foreground(c)
		f.FocusedButton = f.FocusedButton.Foreground(lipgloss.Color("235")).Background(c)
		f.Next = f.FocusedButton
	}
	return t
}

// matching huh.Run
func form(fields ...huh.Field) error {
	f := huh.NewForm(huh.NewGroup(fields...)).WithTheme(theme)
	if len(fields) == 1 {
		f = f.WithShowHelp(false)
	}
	return f.Run()
}

func promptOK(cli *CLI) bool {
	return !cli.JSON && !cli.Silent && !cli.Yes && term.IsTerminal(int(os.Stdin.Fd()))
}

func requireTTY(cli *CLI, what string) error {
	if cli.Yes {
		return userErr(fmt.Sprintf("%s needs input; -y is unattended, drop it to pick or pass the values on the command line", what))
	}
	if !promptOK(cli) {
		return userErr(fmt.Sprintf("%s needs a terminal (stdin is not a terminal); pass the values on the command line", what))
	}
	return nil
}

// huh's Ctrl-C becomes the CLI's interrupt exit
func abort(err error) error {
	if errors.Is(err, huh.ErrUserAborted) {
		return &ExitError{Code: 130, Msg: "aborted"}
	}
	return err
}

func asker(cli *CLI) func(string) bool {
	return func(title string) bool {
		if cli.Yes {
			return true
		}
		if !promptOK(cli) {
			return false
		}
		var ok bool
		return form(huh.NewConfirm().Title(title).Value(&ok)) == nil && ok
	}
}
