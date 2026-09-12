# hop

A fast, atomic package manager for the terminal — like Homebrew, but every
install is a transaction and every mistake is one command away from undone.

```
$ hop install ripgrep fd bat jq
  install  ripgrep  15.2.0  1.7 MB
  install  fd       10.5.0  1.3 MB
  install  bat      0.26.1  3.1 MB
  install  jq       1.8.2   822 KB

  4 to install  ·  6.8 MB to download

==> Downloading 4 packages
  ✓ ripgrep  ✓ fd  ✓ bat  ✓ jq

✓ installed 4 in 0.8s  ·  6.8 MB downloaded
```

## Why not just use Homebrew?

Homebrew is what taught a generation of Mac and Linux users that a package
manager could be pleasant. hop keeps that spirit and fixes the parts that
still hurt:

| | Homebrew | hop |
|---|---|---|
| Update the index | clones/pulls a large git repo | one conditional HTTP request (usually `304 Not Modified`) |
| Install | serial, mutates the prefix in place | parallel downloads, atomic activation |
| A failed install | can leave a half-installed mess | changes nothing — the old state is untouched until the very last step |
| Undo a change | not really possible | `hop rollback` — instant, no download |
| Recipes | arbitrary Ruby, executed on your machine | pure JSON data, never executed |
| Checksums | often absent | every built-in recipe is SHA-256 pinned against the exact bytes hop tested |
| Project reproducibility | `Brewfile`, no lockfile | `hopfile.toml` + `hop.lock`, byte-for-byte reproducible |
| Startup cost | Ruby interpreter, ~100–300ms | single static binary, single-digit ms |
| Orphaned dependencies | accumulate; `brew autoremove` is a separate step | swept automatically on `hop remove` |

## Install

```bash
git clone https://github.com/sammybanapour/hop.git
cd hop
make build
./bin/hop --help
```

Or with Go installed:

```bash
go install github.com/sammybanapour/hop/cmd/hop@latest
```

Then put hop on your `PATH`:

```bash
eval "$(hop shellenv)"          # add this to ~/.zshrc, ~/.bashrc, etc.
```

hop ships with a recipe index compiled into the binary, so this works with no
network access and no separate "update" step on a fresh machine.

## The basics

```bash
hop search <query>          # find a package
hop install <pkg>...        # install, in parallel, atomically
hop list                    # what's installed
hop upgrade                 # upgrade everything (or name packages)
hop remove <pkg>...         # remove it and any now-orphaned dependencies
hop rollback                # undo the last change — instantly, no download
hop generations             # see every past state of your install
hop doctor                  # check PATH, disk, symlinks, integrity
```

## Coming from Homebrew

```bash
hop migrate --dry-run       # see what hop could take over from Homebrew
hop migrate                 # install those packages under hop
```

`hop migrate` reads Homebrew's Cellar directly — brew does not need to be
running, and nothing in your Homebrew installation is touched. It reports
formulae hop doesn't have a recipe for yet (so the two coexist fine on
whatever it can't cover), and it never runs `brew uninstall` for you: once
you're happy, it prints the exact command to remove the Homebrew copies
yourself.

## Reproducible project environments

```toml
# hopfile.toml
[packages]
ripgrep = "*"
fd      = "^10.0.0"
jq      = "1.8.2"
```

```bash
hop sync                    # make this machine match the hopfile exactly
hop lock                    # pin exact versions + checksums into hop.lock
hop sync --locked           # CI: fail rather than silently re-resolve
```

## How it works

Every package hop installs lands in a content-addressed store:

```
~/.hop/store/ripgrep-15.2.0-3750b2e93f37/
~/.hop/store/fd-10.5.0-b67e1836c468/
```

An install, upgrade or removal never touches these in place. Instead hop
builds a new **generation** — a complete, self-consistent snapshot of what
should be on your `PATH` — and only then flips one symlink:

```
~/.hop/current -> profiles/7
```

That symlink flip is the entire "installation" step, and it's atomic at the
filesystem level. Every previous generation stays on disk, so:

- **Rollback is instant.** `hop rollback` just points `current` at an older
  profile. No re-download, no rebuild — microseconds regardless of how big
  the change was.
- **A failed install changes nothing.** Downloads are verified against their
  SHA-256 digest before extraction; extraction happens in the store before
  linking; the generation is written before it's activated. If any step
  fails, the previous generation is still live.
- **`hop gc`** reclaims store paths that no generation references anymore —
  a real mark-and-sweep, not "delete anything old."

## Extending the recipe index

Recipes are plain JSON — no code runs from them, ever:

```json
{
  "name": "ripgrep",
  "version": "15.2.0",
  "artifacts": {
    "darwin-arm64": {
      "url": "https://github.com/BurntSushi/ripgrep/releases/download/15.2.0/ripgrep-15.2.0-aarch64-apple-darwin.tar.gz",
      "sha256": "…",
      "bin": ["rg"]
    }
  }
}
```

The built-in index (`internal/core/data/index.json`) is generated, not
hand-written: `go run ./tools/genindex` resolves each package's latest
GitHub release, downloads the real artifact, and derives the checksum, the
archive's strip depth, and the exact in-archive binary path by inspection —
nothing in it is guessed. Add a package by adding one entry to
`tools/recipes.json` and re-running the generator.

## Design notes

- **Zero third-party dependencies.** Everything — HTTP, tar/zip/gzip, JSON,
  a small TOML reader — is the Go standard library. `go build` never touches
  the network for anything but the packages you actually install.
- **JSON everywhere with `--json`.** Every command has a machine-readable
  form, so hop scripts as easily as it runs interactively.
- **Errors come with the next command to run.** A checksum mismatch, a
  missing platform build, a typo'd package name — each one explains itself
  and suggests the fix instead of a bare stack of text.

## License

MIT
