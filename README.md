<div align="center">
<img alt="phuo wordmark" width="400" src="./wordmark.png">

# phuo

**phuo** (φύω), a dead simple MediaWiki extension and skin manager.

<img alt="A demo of phuo" width="800" src="./demo.gif/">

</div>

## Why?

No one likes to manage extension and skins in MediaWiki, there's extensions that have [Composer](https://getcomposer.org/) support which is great and help manage dependencies, but what about ones that don't?

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

And that's it! `init` will record the extensions and skins already installed, letting you carry over what you already have with no extra effort.

When you need to install something, do:

```bash
phuo add MobileFrontend
```

This will download the package from MediaWiki's 'registry' (the [Distributor](https://www.mediawiki.org/wiki/Special:ExtensionDistributor/) to be precise), installs what it requires, and loads it in your `LocalSettings.php`.

When you want to update, do:

```bash
phuo outdated
phuo update
```

And you can check what needs to be updated... then update all of them, because of course we want them at their latest!

Don't like the changes? do `phuo revert`, and the updates rolls back.

## Examples

```bash
phuo init                              # record extensions and skins already in the tree
phuo add MobileFrontend                # download, install requirements, and load it
phuo add MobileFrontend@REL1_43        # pin a branch, tag, master, github:owner/repo, or --skin
phuo add                               # search and pick
phuo remove Cite                       # uninstall
phuo enable Cite                       # load it from LocalSettings.php
phuo disable Cite                      # stays on disk, dropped from LocalSettings.php
phuo install                           # install what phuo.lock records
phuo install --frozen-lockfile         # stop if phuo.json and the lock disagree
phuo update                            # move everything to the newest ref
phuo update -i                         # pick what to update
phuo revert                            # undo the last change
phuo revert -i                         # pick what to undo
phuo prune                             # delete directories that aren't in the lock
phuo patch Echo                        # edit in place, then --commit or --remove
phuo diff Echo                         # show what an update would change
phuo licenses                          # group installed packages by license
phuo outdated                          # list packages that can move forward
phuo rel                               # show the release branch for this wiki
phuo rel REL1_43                       # override it and run phuo update afterward
phuo ls                                # what's installed
phuo info MobileFrontend               # registry page for one package
phuo search echo                       # search Extension and Skin pages
phuo why Echo                          # why a dependency is here
phuo changelog Echo                    # commits between the installed ref and the newest
phuo doctor                            # check the install
phuo cache                             # print the cache directory and size
phuo cache rm                          # delete it
phuo upgrade                           # update this phuo install
phuo completions bash                  # bash, zsh, fish, or powershell
phuo pm ls                             # ls, cache, and diff under a pm prefix
```

`phuo <command> --help` lists flags for that command.

## License

[MIT](./LICENSE)
