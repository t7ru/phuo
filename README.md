<div align="center">
<img alt="phuo wordmark" width="400" src="./wordmark.png">

# phuo

**phuo** (φύω), a dead simple MediaWiki extension and skin manager.

<img alt="A demo of phuo" width="800" src="./demo.gif/">

</div>

## Why?

No one likes to manage extensions and skins in MediaWiki. Some have [Composer](https://getcomposer.org/) support, which is great for dependencies, but what about the ones that don't?

phuo will manage everything for you, the external binaries needed, the compatibility policy, and more.

## Usage

phuo requires [Go 1.27](https://go.dev/doc/install) or later.

```bash
go install github.com/t7ru/phuo@latest
```

You can also get a prebuilt binary from the [releases page](https://github.com/t7ru/phuo/releases).

From your MediaWiki root:

```bash
phuo init
```

And that's it! `init` records the extensions and skins already installed, so you can carry over what you already have with no extra effort.

When you need to install something, do:

```bash
phuo add MobileFrontend
phuo add skin:Liberty
phuo add CodeMirror@REL1_45
```

This will download the package from MediaWiki's 'registry' (the [Distributor](https://www.mediawiki.org/wiki/Special:ExtensionDistributor/) to be precise), installs what it requires, and loads it in your `LocalSettings.php`.

When you want to update, do:

```bash
phuo outdated
phuo update
```

And you can check what needs to be updated... then update all of them, because of course we want them at their latest!

Don't like the changes? Use `phuo revert` and it rolls the last change back!

## Commands

`phuo --help` lists everything, while `phuo <command> --help` lists flags. Commands that take names will open a picker if you were to you omit them.

|                              |                                                        |
| ---------------------------- | ------------------------------------------------------ |
| `init`                       | Create `phuo.json` and record what's already installed |
| `adopt`                      | Move your `wfLoad*` lines into the phuo block          |
| `add` (`a`)                  | Add extensions or skins                                |
| `remove` (`rm`)              | Remove packages and unused requirements                |
| `install` (`i`)              | Install from `phuo.lock`                               |
| `enable` / `disable`         | Load or stop loading packages in `LocalSettings.php`   |
| `outdated`                   | List packages with newer commits                       |
| `update` (`up`)              | Update to the latest commit of the branch              |
| `pin` / `unpin`              | Pin or unpin at the current commit                     |
| `revert`                     | Undo the last change                                   |
| `rel`                        | Show or set the release branch                         |
| `diff` / `changelog` (`log`) | What an update would change                            |
| `patch`                      | Edit a package in place, then `--commit` or `--remove` |
| `ls` (`list`)                | What's installed                                       |
| `info`                       | Registry and local metadata                            |
| `search`                     | Search Extension and Skin pages                        |
| `why`                        | Why a package is installed                             |
| `licenses`                   | Group by license                                       |
| `doctor`                     | Health checks                                          |
| `prune`                      | Delete directories not in the lock                     |
| `fetch`                      | Download locked archives into the cache                |
| `cache`                      | Show the cache, or `cache rm` to delete it             |
| `upgrade`                    | Update this phuo binary                                |
| `completions`                | bash, zsh, fish, or powershell                         |

For a more comprehensive list, see [Phuo#Commands](https://www.mediawiki.org/wiki/Phuo#Commands) on its MediaWiki page.

## License

[MIT](./LICENSE)
