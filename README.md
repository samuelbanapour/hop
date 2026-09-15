# HopCLI

A fast, atomic package manager for the terminal — like Homebrew, but every
install is a transaction and every mistake is one command away from undone.

Ships as a single binary called `hop`.

[![Site](https://img.shields.io/badge/site-hopcli--site.onrender.com-A9631E)](https://hopcli-site.onrender.com)

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
manager could be pleasant. HopCLI keeps that spirit and fixes the parts that
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
| GUI apps | casks — a separate mechanism from formulae | same `hop install`, same store; the `.app` is symlinked into `~/Applications` |

## Install

```bash
curl -fsSL https://raw.githubusercontent.com/samuelbanapour/hopcli/master/scripts/install.sh | sh
```

Downloads the right release for your OS/arch, verifies it against the
release's SHA256SUMS, and drops it into `~/.hop/bin` — nothing outside that
directory is touched. Or build it yourself:

```bash
git clone https://github.com/samuelbanapour/hopcli.git
cd hop
make build
./bin/hop --help
```

Or with Go installed:

```bash
go install github.com/samuelbanapour/hopcli/cmd/hop@latest
```

Then put hop on your `PATH`:

```bash
eval "$(hop shellenv)"          # add this to ~/.zshrc, ~/.bashrc, etc.
```

hop ships with a recipe index compiled into the binary, so this works with no
network access and no separate "update" step on a fresh machine. That index
currently carries **517 recipes and 1,867 verified artifacts** across
`darwin-arm64`, `darwin-amd64`, `linux-amd64` and `linux-arm64` — standalone
CLI tools, well over 300 Homebrew formulae with their real dependency graphs,
28 OS/VM images spanning Linux, BSD, and macOS from Lion through Tahoe, and a
handful of GUI apps (Homebrew casks) that land in `~/Applications`.

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
hop self-update             # update hop itself (not your packages — see below)
```

`hop upgrade` and `hop self-update` are deliberately separate: the first
moves installed *packages* to their newest indexed version, the second
replaces the `hop` binary itself with the newest GitHub release. Neither
touches what the other manages. hop also checks for a newer release of
itself in the background at most once a day — never blocking a command,
never installing anything on its own — and prints a one-line hint when one
exists.

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

## OS and VM images

hop doesn't just manage CLI tools — it can pull OS disk images and container
rootfs tarballs too, and keep them current the same way it keeps everything
else current:

```bash
hop search iso              # fedora-workstation, archlinux-iso, …
hop install ubuntu-cloud    # download + verify, never extracted
hop info ubuntu-cloud       # → exact path to the .img/.qcow2 file
hop upgrade ubuntu-cloud    # re-resolves when a new point release ships
```

An image recipe (`"kind": "image"` in the index) behaves differently from a
CLI tool on purpose: the artifact is verified against its checksum and
content-addressed in the store exactly like a binary is, but it is **never
extracted and never put on `PATH`** — a disk image or rootfs tarball is
meant to be handed to `qemu`, `docker import`, or a hypervisor exactly as
downloaded, not unpacked. `hop info <name>` prints the file's real path once
it's installed.

It isn't limited to Linux cloud images, either — `freebsd-vm` is a real,
non-Linux BSD image, and `raspios-lite` is a flash-to-SD-card OS with no
cloud-init in sight (and, honestly, no `amd64` build at all: Raspberry Pi
hardware is arm64-only, so hop says so rather than pretending otherwise).

| Package | What it is |
|---|---|
| `ubuntu-cloud` | Ubuntu 24.04 LTS server cloud image |
| `debian-cloud` | Debian 12 (bookworm) generic cloud image |
| `alpine-minirootfs` | Alpine Linux minimal root filesystem, for containers |
| `freebsd-vm` | FreeBSD's general-purpose VM image — not Linux, not cloud-init |
| `openbsd-vm` | OpenBSD install ISO — a second, independent BSD |
| `netbsd-iso` | NetBSD install ISO — the BSD built to run on almost anything |
| `raspios-lite` | Raspberry Pi OS Lite, arm64 only, for real SD-card hardware |
| `fedora-workstation` | Fedora Workstation Live ISO — an installer, not a cloud image |
| `archlinux-iso` | Arch Linux install ISO, x86_64 only |
| `opensuse-tumbleweed` | openSUSE Tumbleweed DVD installer — the rolling-release openSUSE |
| `rocky-linux` | Rocky Linux minimal installer ISO — a free, community-rebuilt RHEL |
| `void-linux` | Void Linux live ISO — its own xbps package manager, no systemd |
| `macos-recovery` | macOS full restore image for Apple Silicon VMs |

Each is resolved straight from its own distro's official checksum manifest —
Ubuntu, Alpine and Fedora publish SHA-256, Debian only publishes SHA-512
(which is why hop's artifact format accepts either), FreeBSD and OpenBSD use
`SHA256 (file) = digest` rather than the GNU `sha256sum` convention, and
Void's and Kali's combined manifests need picking the right line out of
several. All of it is verified with exactly the same rigor as everything
else in the index — nothing here is trusted just because it "looks
official."

### Specialized Linux, for when "general-purpose" isn't the point

A handful of distros exist specifically for security testing, privacy, or
virtualization rather than everyday use — hop treats them the same as any
other image, just verified against manifests with their own quirks:

| Package | What it is |
|---|---|
| `kali-linux` | Debian-derived, for security testing and penetration testing |
| `parrot-security` | Debian-derived, for security testing (x86_64 only — arm64 ships as a UTM tarball, not an ISO) |
| `proxmox-ve` | A Debian-derived type-1 hypervisor, for running VMs directly on hardware |
| `tails` | An amnesic live OS that routes all traffic through Tor (x86_64 only) |

`tails` is the one exception to "every image is independently re-hashed":
the Tails project publishes only a PGP signature for its image, no plain
checksum manifest at all, so hop trusts it on first use like it does for
some legacy macOS DMGs — the same limitation, not a lowered bar applied
selectively.

`macos-recovery` deserves its own note. It's resolved from
[api.ipsw.me](https://ipsw.me), a long-standing public aggregator of
Apple's own signed firmware metadata — the URL and SHA-256 it reports both
point straight back to `updates.cdn-apple.com`, the same channel Apple
Configurator and open-source tools like Tart and UTM already use to
provision macOS VMs under Apple's own Virtualization framework. It's Apple
Silicon–only (Intel Macs restore over the network, not from a downloadable
image), the file runs 15–20+ GB, and running it is governed by Apple's own
software license agreement. Given the size, hop's other image recipes are
verified by downloading the full artifact and re-hashing it during
generation; doing that for an 18GB file on every index rebuild wasn't
practical, so this one recipe's checksum is taken from Apple's own
manifest rather than independently re-derived — the same trust a package
manager places in any signed upstream repository index.

### Every macOS release Apple's own infrastructure still serves

Beyond the current-release recovery/installer images, hop's index carries
one recipe per macOS version reachable at all — `macos-lion` (10.7.5, 2012)
through `macos-tahoe` (26, current), eleven versions spanning fourteen
years:

```bash
hop search macos             # every version hop can resolve
hop install macos-sequoia    # a specific historical release
```

Two sources, both Apple's own: the live software-update catalog for recent
generations (using the same technique the open-source tool
[mist-cli](https://github.com/ninxsoft/mist-cli) pioneered — fetch each
product's own installer script and read its embedded version string, since
the catalog itself doesn't list versions inline), and a short list of
specific historical Apple CDN URLs for Lion through Sierra, which the live
catalog no longer carries but mist-cli's maintainers have spent years
confirming still resolve — genindex re-verifies each one live (a real HEAD
request) before ever including it, never trusting a URL just because it
worked once.

**Nothing older than Lion 10.7.5 exists in this index, and none ever
will.** Mac OS X Server 10.1 through Snow Leopard 10.6 predate the Mac App
Store/catalog system entirely, shipped only on physical CD/DVD, and Apple
has never re-hosted them in any digital form since. Even mist-cli — a
project entirely dedicated to hunting down every Apple-hosted macOS URL
still alive — has never found one from that era. That absence, from the
project most likely to have found it, is itself the evidence there isn't
one to find.

**hop does not, and will not, pull Windows install images**, at any
version. This was tested concretely, not assumed: walking the actual
multi-step flow tools like Fido and CrystalFetch use (session init → SKU
lookup → download link), the final step — the one that actually matters —
comes back rejected by Microsoft's own named bot-detection system:
`"Sentinel marked this request as rejected"`. A second channel, Microsoft's
own [Windows Dev Virtual
Machines](https://developer.microsoft.com/en-us/windows/downloads/virtual-machines/)
page, loads Arkose Labs' CAPTCHA challenge script in its own security
policy. Two separate official channels, both deliberately gated against
exactly this kind of automated access — working around either would mean
defeating bot-detection Microsoft put there on purpose, which is outside
what hop does, regardless of what any other tool attempts. If you need a
Windows VM, get one from that Windows Dev VMs page yourself; hop can't
automate the parts Microsoft has explicitly fenced off.

## Homebrew bottles, relocated properly

hop can also pull formulae straight from Homebrew's own bottle
infrastructure — the same prebuilt binaries `brew install` itself
downloads, fetched directly from `ghcr.io/homebrew/core` as OCI registry
blobs (the real Docker-registry token-then-blob protocol: an anonymous pull
token, then the blob, verified against the exact SHA-256 Homebrew's own API
publishes). Close to 200 formulae are in the index this way — networking
and diagnostic tools (`nmap`, `iperf3`, `mtr`, `rclone`), media and document
tooling (`ffmpeg`, `imagemagick`, `pandoc`, `exiftool`), dev-environment
version managers (`pyenv`, `rbenv`, `direnv`, `git-lfs`), GNU utilities for
scripts that expect them (`gawk`, `gnu-sed`, `findutils`), and enough
terminal fun (`cowsay`, `figlet`, `cmatrix`) that a package index doesn't
have to take itself too seriously:

```bash
hop install htop     # pulls htop + its real dependency, ncurses
hop install nmap     # openssl@3, libssh2, pcre2, lua — resolved transitively
```

A Homebrew bottle is compiled with placeholder tokens
(`@@HOMEBREW_PREFIX@@`/`@@HOMEBREW_CELLAR@@`) standing in for wherever it
ends up installed — `brew install` itself patches these at pour time, which
is exactly why a bottle simply extracted elsewhere doesn't run. hop performs
the same patch into its own content-addressed store paths instead of a
shared Homebrew prefix. This turned out to mean four distinct kinds of
patching, each found by actually running the installed binary rather than
trusting a clean install:

- **Mach-O load commands** (`install_name_tool`, then an ad-hoc re-sign,
  since patching invalidates the existing signature) — the case most tools
  in this space handle.
- **Plain-text wrapper scripts** carrying the same placeholder tokens — a
  Ruby or Python gem's `bin/<name>` launcher, notably.
- **`#!/usr/bin/env <interpreter>` shebangs**, which carry no placeholder at
  all. hop deliberately keeps a dependency-only package like `python@3.14`
  off the user's `PATH`, so an unrewritten shebang would silently fall
  through to whatever interpreter (if any) happens to be ambient on the
  machine — these get pinned to the exact dependency hop resolved, the same
  thing `brew`'s own installer does for gem-based formulae.
- **Symlinks that climb out to Homebrew's shared `opt/<formula>` farm** — a
  Python-based CLI's bundled venv interpreter link, notably. These need
  patching in the extractor itself, since a naive path-traversal guard
  would otherwise silently drop the symlink before relocation ever sees it.

This was verified for real at every step, not assumed: `htop` crashed with
`Symbol not found: _COLORS` before any of this existed, `tig` — three real
dependencies deep (`ncurses`, `pcre2`, `readline`) — now runs with every
linked library confirmed pointing at its actual hop store path, and `nmap`,
`yt-dlp` and `csvkit` each surfaced one of the less obvious cases above by
actually failing at runtime until it was fixed.

One formula doesn't fit this scheme at all: **Ruby bakes literal, real
absolute paths (`/opt/homebrew/Cellar/ruby/…`) directly into `libruby.dylib`
for its own VM bootstrap**, confirmed with `strings` on the extracted
library and by running hop's own copy of `ruby` standalone — not a
placeholder token hop's relocation can patch, and not a bug in the sense
everything above was. hop doesn't carry `ruby` (or its one Homebrew
dependent) rather than ship something that fails at runtime.

A pure-library dependency (`readline`, `openssl@3`, and most of what a real
formula's dependency graph pulls in) still becomes a recipe — it just puts
nothing on `PATH`, which is exactly correct for something only ever loaded
as a shared library.

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
- **`hop gc`** (aliases: `prune`, `clean` — same command, for anyone coming
  from Homebrew or npm) reclaims store paths that no generation references
  anymore — a real mark-and-sweep, not "delete anything old."

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
