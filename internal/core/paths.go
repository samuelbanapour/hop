// Package core implements hop's package management engine: the recipe index,
// the content-addressed store, generation bookkeeping and the install
// transaction.
package core

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// Version is hop's own version, overridden at build time via -ldflags.
var Version = "0.1.0-dev"

// Revision is the build revision, overridden at build time.
var Revision = "unknown"

// Layout describes where hop keeps everything. Nothing outside Root is ever
// written — unless the installed set includes a GUI app (a Homebrew cask),
// which is what makes `hop uninstall-self` and `hop gc` trustworthy for
// everything else hop manages. A machine with no GUI app installed never
// triggers the exception at all: hop has no code path that touches anything
// outside Root unless one is actually present in the active generation. When
// one is, hop symlinks its .app bundle into ~/Applications so Spotlight and
// Finder can find it — see syncAppLinks in state.go for how that stays safe
// to reverse anyway.
//
//	<root>/store/<name>-<version>-<hash>/   immutable package trees
//	<root>/profiles/<n>/bin/<tool>          symlink farm for generation n
//	<root>/profiles/<n>/manifest.json       what generation n contains
//	<root>/current -> profiles/<n>          the single atomic activation point
//	<root>/cache/                           downloads and the index cache
//	<root>/lock                             advisory lock for mutations
type Layout struct {
	Root string
}

// DefaultRoot resolves the hop root: $HOP_ROOT, else ~/.hop.
func DefaultRoot() (string, error) {
	if r := os.Getenv("HOP_ROOT"); r != "" {
		abs, err := filepath.Abs(r)
		if err != nil {
			return "", fmt.Errorf("HOP_ROOT %q is not a usable path: %w", r, err)
		}
		return abs, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("cannot locate home directory (set HOP_ROOT to choose a prefix): %w", err)
	}
	return filepath.Join(home, ".hop"), nil
}

// NewLayout resolves the root and creates the directories hop needs.
func NewLayout(root string) (*Layout, error) {
	if root == "" {
		r, err := DefaultRoot()
		if err != nil {
			return nil, err
		}
		root = r
	}
	l := &Layout{Root: root}
	for _, d := range []string{l.Store(), l.Profiles(), l.Cache(), l.Downloads()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return nil, fmt.Errorf("cannot create %s: %w", d, err)
		}
	}
	return l, nil
}

// Store is the root of the content-addressed package store.
func (l *Layout) Store() string { return filepath.Join(l.Root, "store") }

// Profiles holds one directory per generation.
func (l *Layout) Profiles() string { return filepath.Join(l.Root, "profiles") }

// Profile is the directory for generation n.
func (l *Layout) Profile(n int) string {
	return filepath.Join(l.Profiles(), fmt.Sprintf("%d", n))
}

// ProfileBin is the symlink farm for generation n.
func (l *Layout) ProfileBin(n int) string { return filepath.Join(l.Profile(n), "bin") }

// ProfileManifest is the recorded contents of generation n.
func (l *Layout) ProfileManifest(n int) string {
	return filepath.Join(l.Profile(n), "manifest.json")
}

// Current is the symlink users put on PATH, via <root>/current/bin.
func (l *Layout) Current() string { return filepath.Join(l.Root, "current") }

// CurrentBin is the stable PATH entry. It never changes as generations do.
func (l *Layout) CurrentBin() string { return filepath.Join(l.Current(), "bin") }

// Cache holds the index cache and other regenerable data.
func (l *Layout) Cache() string { return filepath.Join(l.Root, "cache") }

// Downloads holds fetched artifacts, keyed by content hash.
func (l *Layout) Downloads() string { return filepath.Join(l.Cache(), "downloads") }

// IndexPath is the on-disk cached recipe index.
func (l *Layout) IndexPath() string { return filepath.Join(l.Cache(), "index.json") }

// IndexMetaPath stores the ETag and fetch time for conditional requests.
func (l *Layout) IndexMetaPath() string { return filepath.Join(l.Cache(), "index.meta.json") }

// LockPath is the advisory lock guarding all mutations.
func (l *Layout) LockPath() string { return filepath.Join(l.Root, "lock") }

// StorePath returns the store directory for a package build.
func (l *Layout) StorePath(name, version, hash string) string {
	return filepath.Join(l.Store(), StoreDirName(name, version, hash))
}

// StoreDirName builds the store directory name. The hash prefix makes the
// path content-addressed: two builds of the same version from different
// artifacts never collide, and an identical artifact is reused for free.
func StoreDirName(name, version, hash string) string {
	h := hash
	if len(h) > 12 {
		h = h[:12]
	}
	if h == "" {
		h = "nohash"
	}
	return fmt.Sprintf("%s-%s-%s", name, version, h)
}

// ---------------------------------------------------------------- platform ----

// Platform is a "<os>-<arch>" key used to select an artifact from a recipe.
type Platform string

// CurrentPlatform reports the running platform, honouring HOP_PLATFORM so the
// resolver and tests can be exercised against other targets.
func CurrentPlatform() Platform {
	if p := os.Getenv("HOP_PLATFORM"); p != "" {
		return Platform(p)
	}
	return Platform(runtime.GOOS + "-" + runtime.GOARCH)
}

// OS returns the operating system half of the platform key.
func (p Platform) OS() string {
	if i := strings.IndexByte(string(p), '-'); i > 0 {
		return string(p)[:i]
	}
	return string(p)
}

// Arch returns the architecture half of the platform key.
func (p Platform) Arch() string {
	if i := strings.IndexByte(string(p), '-'); i > 0 {
		return string(p)[i+1:]
	}
	return ""
}

// Pretty renders the platform the way people say it out loud.
func (p Platform) Pretty() string {
	os_, arch := p.OS(), p.Arch()
	names := map[string]string{"darwin": "macOS", "linux": "Linux", "windows": "Windows"}
	arches := map[string]string{"arm64": "Apple Silicon / ARM64", "amd64": "Intel / x86_64"}
	o, ok := names[os_]
	if !ok {
		o = os_
	}
	a, ok := arches[arch]
	if !ok {
		a = arch
	}
	if os_ == "linux" && arch == "arm64" {
		a = "ARM64"
	}
	return o + " (" + a + ")"
}

// Fallbacks lists platform keys to try in order. On Apple Silicon a native
// arm64 build is preferred, but an Intel build still runs under Rosetta 2 and
// beats telling the user hop has nothing for them.
func (p Platform) Fallbacks() []Platform {
	out := []Platform{p}
	if p.OS() == "darwin" && p.Arch() == "arm64" {
		out = append(out, "darwin-amd64")
	}
	return out
}

// IsRosettaFallback reports whether chosen is an emulated substitute for p.
func (p Platform) IsRosettaFallback(chosen Platform) bool {
	return p.OS() == "darwin" && p.Arch() == "arm64" && chosen == "darwin-amd64"
}
