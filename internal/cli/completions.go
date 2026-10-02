package cli

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	json "encoding/json/v2"

	"github.com/alecthomas/kong"
	"github.com/posener/complete"
	"github.com/willabides/kongplete"

	"github.com/t7ru/phuo/internal/project"
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
	switch strings.ToLower(shell) {
	case "bash":
		return "complete -C phuo phuo\n", nil
	case "zsh":
		return "#compdef phuo\nautoload -Uz bashcompinit\nbashcompinit\ncomplete -C phuo phuo\n", nil
	case "fish":
		return `function __complete_phuo
    set -lx COMP_LINE (commandline -cp)
    test -z (commandline -ct)
    and set COMP_LINE "$COMP_LINE "
    phuo
end
complete -f -c phuo -a "(__complete_phuo)"
`, nil
	case "powershell", "pwsh":
		return `Register-ArgumentCompleter -Native -CommandName phuo -ScriptBlock {
	param($wordToComplete, $commandAst, $cursorPosition)
	$line = $commandAst.Extent.Text
	if ([string]::IsNullOrEmpty($wordToComplete) -and -not $line.EndsWith(' ')) { $line += ' ' }
	$prevLine, $prevPoint = $env:COMP_LINE, $env:COMP_POINT
	try {
		$env:COMP_LINE = $line
		$env:COMP_POINT = "$($line.Length)"
		& phuo | ForEach-Object {
			[System.Management.Automation.CompletionResult]::new($_, $_, 'ParameterValue', $_)
		}
	} finally {
		$env:COMP_LINE = $prevLine
		$env:COMP_POINT = $prevPoint
	}
}
`, nil
	default:
		return "", userErr(fmt.Sprintf("unsupported shell %q (want bash, zsh, fish, or powershell)", shell))
	}
}

// Complete handles shell completion and returns when this is a normal run.
// aliases share the command they point at, which kongplete does not register
func Complete(parser *kong.Kong) {
	if parser == nil {
		return
	}
	cmd, err := kongplete.Command(parser, kongplete.WithPredictor("installed", complete.PredictFunc(InstalledNames)))
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	wireAliases(parser.Model.Node, cmd.Sub)
	cmp := complete.New(parser.Model.Name, cmd)
	cmp.Out = parser.Stdout
	if cmp.Complete() {
		os.Exit(0)
	}
}

func wireAliases(node *kong.Node, sub complete.Commands) {
	if node == nil || sub == nil {
		return
	}
	for _, child := range node.Children {
		if child == nil || child.Hidden {
			continue
		}
		cmd, ok := sub[child.Name]
		if !ok {
			continue
		}
		for _, alias := range child.Aliases {
			if _, exists := sub[alias]; !exists {
				sub[alias] = cmd
			}
		}
		wireAliases(child, cmd.Sub)
	}
}

func InstalledNames(a complete.Args) []string {
	root, has, err := project.Find(completionCwd(a))
	if err != nil || !has {
		return nil
	}
	f, err := os.Open(filepath.Join(root, "phuo.lock"))
	if err != nil {
		return nil
	}
	defer f.Close()
	var lock project.Lock
	if json.UnmarshalRead(f, &lock) != nil || len(lock.Packages) == 0 {
		return nil
	}
	names := make([]string, 0, len(lock.Packages))
	seen := make(map[string]struct{}, len(lock.Packages))
	for key := range lock.Packages {
		name := key
		if _, n, ok := strings.Cut(key, "/"); ok {
			name = n
		}
		if _, ok := seen[name]; ok {
			continue
		}
		seen[name] = struct{}{}
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

func completionCwd(a complete.Args) string {
	words := a.Completed
	for i := 0; i < len(words); i++ {
		if rest, ok := strings.CutPrefix(words[i], "--cwd="); ok && rest != "" {
			return rest
		}
		if words[i] == "--cwd" && i+1 < len(words) {
			return words[i+1]
		}
	}
	return "."
}
