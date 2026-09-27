package cli

import (
	"fmt"
	"io"
	"os"
	"slices"
	"strings"

	"github.com/alecthomas/kong"
	"github.com/willabides/kongplete"
)

type CompletionsCmd struct {
	Shell string `arg:"" name:"shell" help:"bash, zsh, fish, or powershell."`
}

func (c *CompletionsCmd) Run() error {
	script, err := completionScript(c.Shell)
	if err != nil {
		return err
	}
	_, err = io.WriteString(os.Stdout, script)
	return err
}

func completionScript(shell string) (string, error) {
	var root CLI
	parser, err := kong.New(&root,
		kong.Name("phuo"),
		kong.Description("MediaWiki extension and skin manager."),
	)
	if err != nil {
		return "", err
	}
	if _, err := kongplete.Command(parser); err != nil {
		return "", err
	}
	cmds, flags := collectCompletions(parser.Model.Node)
	switch strings.ToLower(shell) {
	case "bash":
		return bashCompletion(cmds, flags), nil
	case "zsh":
		return zshCompletion(cmds, flags), nil
	case "fish":
		return fishCompletion(cmds, flags), nil
	case "powershell", "pwsh":
		return powershellCompletion(cmds, flags), nil
	default:
		return "", userErr(fmt.Sprintf("unsupported shell %q (want bash, zsh, fish, or powershell)", shell))
	}
}

func collectCompletions(node *kong.Node) (cmds, flags []string) {
	if node == nil {
		return nil, nil
	}
	for _, f := range node.Flags {
		if f == nil || f.Hidden {
			continue
		}
		flags = append(flags, "--"+f.Name)
		if f.Short != 0 {
			flags = append(flags, "-"+string(f.Short))
		}
	}
	for _, child := range node.Children {
		if child == nil || child.Hidden || child.Type != kong.CommandNode {
			continue
		}
		cmds = append(cmds, child.Name)
		for _, f := range child.Flags {
			if f == nil || f.Hidden {
				continue
			}
			flags = append(flags, "--"+f.Name)
			if f.Short != 0 {
				flags = append(flags, "-"+string(f.Short))
			}
		}
	}
	slices.Sort(cmds)
	slices.Sort(flags)
	flags = slices.Compact(flags)
	return cmds, flags
}

func bashCompletion(cmds, flags []string) string {
	words := strings.Join(append(append([]string{}, cmds...), flags...), " ")
	return fmt.Sprintf(`# phuo bash completion
_phuo() {
	local cur="${COMP_WORDS[COMP_CWORD]}"
	COMPREPLY=($(compgen -W %q -- "$cur"))
}
complete -F _phuo phuo
`, words)
}

func zshCompletion(cmds, flags []string) string {
	words := strings.Join(append(append([]string{}, cmds...), flags...), " ")
	return fmt.Sprintf(`#compdef phuo
_phuo() {
	local -a opts
	opts=(%s)
	_describe 'phuo' opts
}
compdef _phuo phuo
`, words)
}

func fishCompletion(cmds, flags []string) string {
	var b strings.Builder
	b.WriteString("# phuo fish completion\n")
	for _, c := range cmds {
		fmt.Fprintf(&b, "complete -c phuo -f -a %q\n", c)
	}
	for _, f := range flags {
		switch {
		case strings.HasPrefix(f, "--"):
			fmt.Fprintf(&b, "complete -c phuo -f -l %q\n", strings.TrimPrefix(f, "--"))
		case strings.HasPrefix(f, "-") && len(f) == 2:
			fmt.Fprintf(&b, "complete -c phuo -f -s %q\n", f[1:])
		}
	}
	return b.String()
}

func powershellCompletion(cmds, flags []string) string {
	words := append(append([]string{}, cmds...), flags...)
	quoted := make([]string, len(words))
	for i, w := range words {
		quoted[i] = "'" + strings.ReplaceAll(w, "'", "''") + "'"
	}
	return fmt.Sprintf(`# phuo powershell completion
Register-ArgumentCompleter -Native -CommandName phuo -ScriptBlock {
	param($wordToComplete, $commandAst, $cursorPosition)
	$cmds = @(%s)
	$cmds | Where-Object { $_ -like "$wordToComplete*" } | ForEach-Object {
		[System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
	}
}
`, strings.Join(quoted, ", "))
}
