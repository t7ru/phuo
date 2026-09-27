package cli

import (
	"github.com/alecthomas/kong"
)

type ExitError struct {
	Code int
	Msg  string
}

func (e *ExitError) Error() string { return e.Msg }

func envErr(msg string) error  { return &ExitError{Code: 2, Msg: msg} }
func userErr(msg string) error { return &ExitError{Code: 1, Msg: msg} }

type CLI struct {
	Cwd        string           `name:"cwd" help:"Run as if phuo was started in this directory." type:"path"`
	JSON       bool             `name:"json" help:"Machine-readable output."`
	Silent     bool             `name:"silent" short:"q" help:"Suppress non-error output."`
	Verbose    bool             `name:"verbose" short:"v" help:"Verbose output."`
	NoColor    bool             `name:"no-color" help:"Disable ANSI colors (also NO_COLOR)."`
	NoProgress bool             `name:"no-progress" help:"Disable the progress indicator."`
	NoCache    bool             `name:"no-cache" help:"Bypass cache reads."`
	CacheDir   string           `name:"cache-dir" help:"Override the global cache directory." type:"path"`
	Yes        bool             `name:"yes" short:"y" help:"Assume yes for prompts."`
	Offline    bool             `name:"offline" help:"Fail instead of hitting the network."`
	Version    kong.VersionFlag `name:"version" help:"Print version and exit."`

	Init        InitCmd        `cmd:"" help:"Create phuo.json in a MediaWiki root."`
	Add         AddCmd         `cmd:"" aliases:"a" help:"Add extensions or skins."`
	Remove      RemoveCmd      `cmd:"" aliases:"rm,uninstall" help:"Remove extensions or skins."`
	Enable      EnableCmd      `cmd:"" help:"Load installed packages via LocalSettings.php."`
	Disable     DisableCmd     `cmd:"" help:"Keep installed packages out of LocalSettings.php."`
	Install     InstallCmd     `cmd:"" aliases:"i" help:"Install from phuo.lock."`
	Update      UpdateCmd      `cmd:"" aliases:"up" help:"Update packages to newest refs."`
	Revert      RevertCmd      `cmd:"" help:"Undo the last project change from the local snapshot."`
	Prune       PruneCmd       `cmd:"" help:"Remove directories not in the lock."`
	Patch       PatchCmd       `cmd:"" help:"Manage local patches for a package."`
	Diff        DiffCmd        `cmd:"" help:"Show what updating a package would change."`
	Licenses    LicensesCmd    `cmd:"" help:"Group installed packages by license."`
	Outdated    OutdatedCmd    `cmd:"" help:"List outdated packages."`
	Rel         RelCmd         `cmd:"" help:"Show or set the default REL override."`
	Ls          LsCmd          `cmd:"" aliases:"list" help:"List installed packages."`
	Info        InfoCmd        `cmd:"" help:"Show registry metadata for a package."`
	Search      SearchCmd      `cmd:"" help:"Search Extension:/Skin: pages."`
	Why         WhyCmd         `cmd:"" help:"Explain why a package is installed."`
	Changelog   ChangelogCmd   `cmd:"" aliases:"log" help:"Show commits between refs."`
	Doctor      DoctorCmd      `cmd:"" help:"Run health checks on the install."`
	Cache       CacheCmd       `cmd:"" help:"Show or clear the global cache."`
	Completions CompletionsCmd `cmd:"" help:"Print shell completion scripts."`
	Pm          PmCmd          `cmd:"" help:"Package-manager aliases (ls, cache, diff)."`
}
