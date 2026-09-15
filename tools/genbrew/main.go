// Command genbrew adds real Homebrew formulae to hop's built-in index,
// fetched as OCI registry blobs directly from ghcr.io/homebrew/core — the
// exact same bottles `brew install` itself downloads.
//
// Homebrew's own JSON API (formulae.brew.sh/api/formula.json) is a single
// public, unauthenticated manifest covering every formula in homebrew/core:
// stable version, per-platform bottle URL, per-platform SHA-256, declared
// executables, and the runtime dependency graph. genbrew reads it once and
// builds recipes straight from that data — no re-download-and-rehash step,
// since Homebrew's own manifest already is the authoritative, signed record
// (the same trust a system package manager places in its own repository
// index).
//
// A Homebrew bottle is a tar.gz of a Cellar/<formula>/<version>/ tree, not a
// single flat binary, so hop's ordinary bin-discovery (searching the
// extracted tree by name) does the rest — genbrew supplies the exact
// executable names from Homebrew's own "executables" field rather than
// guessing.
//
// Because a bottle can depend on other bottles at runtime (shared
// libraries), genbrew computes the full transitive dependency closure of
// whatever formulae are requested and includes all of it — a partial
// dependency graph would produce an installable-looking recipe that
// actually fails to run.
//
//	go run ./tools/genbrew                      # the built-in curated seed list
//	go run ./tools/genbrew -formula ripgrep,jq  # just these (+ their closure)
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"

	"github.com/samuelbanapour/hopcli/tools/internal/indexio"
)

const bulkURL = "https://formulae.brew.sh/api/formula.json"

// brewFormula is the slice of Homebrew's JSON API this tool needs.
type brewFormula struct {
	Name              string   `json:"name"`
	Desc              string   `json:"desc"`
	License           string   `json:"license"`
	Homepage          string   `json:"homepage"`
	Executables       []string `json:"executables"`
	Deprecated        bool     `json:"deprecated"`
	DeprecationReason string   `json:"deprecation_reason,omitempty"`
	Disabled          bool     `json:"disabled"`
	Revision          int      `json:"revision"`
	Versions          struct {
		Stable string `json:"stable"`
		Bottle bool   `json:"bottle"`
	} `json:"versions"`
	Dependencies []string `json:"dependencies"`
	Bottle       struct {
		Stable struct {
			Files map[string]struct {
				URL    string `json:"url"`
				SHA256 string `json:"sha256"`
			} `json:"files"`
		} `json:"stable"`
	} `json:"bottle"`

	// Variations carries per-platform overrides to Homebrew's default
	// dependency list. Dependencies above reflects only the formula's
	// default (effectively macOS) dependency set — a formula whose Ruby
	// definition declares an extra dependency inside `on_linux do ... end`
	// doesn't appear there at all, only under variations["arm64_linux"]/
	// ["x86_64_linux"]. Verified for real: nmap's Linux bottle needs
	// zlib-ng-compat (its own libz.so.1) and its top-level Dependencies
	// omits it entirely — found by actually running a relocated nmap on
	// Linux and hitting a missing shared library no declared dependency
	// provided.
	Variations map[string]struct {
		Dependencies []string `json:"dependencies"`
	} `json:"variations"`
}

// allDependencies returns f's declared dependencies plus anything its
// Linux-specific bottle variations add on top, deduplicated. See the
// Variations field doc for why this needs to exist as well as
// Dependencies alone.
func (f *brewFormula) allDependencies() []string {
	seen := make(map[string]bool, len(f.Dependencies))
	out := make([]string, 0, len(f.Dependencies))
	add := func(name string) {
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
	}
	for _, d := range f.Dependencies {
		add(d)
	}
	for _, key := range [...]string{"arm64_linux", "x86_64_linux"} {
		if v, ok := f.Variations[key]; ok {
			for _, d := range v.Dependencies {
				add(d)
			}
		}
	}
	return out
}

// seedFormulae are hand-picked, well-known standalone CLI tools not already
// covered by hop's GitHub-release-based recipes. Their transitive runtime
// dependencies are pulled in automatically alongside them.
var seedFormulae = []string{
	"htop", "tmux", "neovim", "tree", "wget", "pandoc", "graphviz", "gnupg",
	"moreutils", "parallel", "ncdu", "tig", "glances", "watch",
	"the_silver_searcher", "universal-ctags", "multitail", "byobu",
	"entr", "fswatch", "pv", "fzy", "peco", "most",
	"colordiff", "highlight", "shellharden", "vale", "dos2unix",
	"jless", "miller", "csvkit",

	// networking and system diagnostics
	"nmap", "iperf3", "mtr", "speedtest-cli", "rclone", "httpie",

	// media and document tooling
	"yt-dlp", "exiftool", "ghostscript", "mediainfo", "imagemagick", "ffmpeg",

	// dev environment and version managers
	"direnv", "asdf", "pyenv", "rbenv", "git-lfs", "shfmt", "watchman",

	// GNU utilities, for the version many scripts actually expect
	"gawk", "gnu-sed", "findutils", "grep", "curl", "git",

	// terminal fun, because a package index shouldn't take itself too seriously.
	// lolcat is deliberately not here: it's a Ruby gem, and Homebrew's own
	// ruby bottle bakes literal (non-placeholder) absolute paths like
	// "/opt/homebrew/Cellar/ruby/4.0.6_1" directly into libruby.dylib for
	// its VM bootstrap — verified via `strings` on the extracted dylib —
	// rather than using the @@HOMEBREW_...@@ tokens every other relocatable
	// bottle uses. Those aren't placeholders hop's relocation can patch:
	// hop's store path is a different length than Homebrew's short fixed
	// prefix, so even a same-length in-place binary patch is impossible.
	// Confirmed by actually running hop's own extracted ruby standalone
	// (`ruby -e 'puts RUBY_VERSION'`), which fails with the exact same
	// LoadError lolcat did. python@3.14, by contrast, computes its prefix
	// relative to its own executable at runtime and was verified to work
	// correctly (sys.prefix, ssl, sqlite3 all resolve inside hop's store).
	"cowsay", "figlet", "sl", "cmatrix", "fastfetch", "asciinema",

	// glibc: not something a hop user installs directly, but every Linux
	// Homebrew bottle's executable has its ELF interpreter (PT_INTERP)
	// pointing at Homebrew's own bundled glibc rather than the host's —
	// confirmed by actually running a relocated bottle's binary on real
	// Linux, which fails with "cannot execute: required file not found"
	// without this. Pulled in transparently as an implicit dependency of
	// every Homebrew+Linux install by resolve.go, not something users
	// request themselves.
	"glibc",
}

// artifact/recipe/index mirror internal/core's JSON shape exactly, field for
// field — every field any of the three generators (genindex, genbrew,
// genimages) can set, even ones this particular generator never populates
// itself. Each generator's own run reads the *entire* existing index.json,
// decodes it into its own local structs, and writes the whole thing back
// out; a field missing from one generator's struct is silently dropped from
// every other generator's recipes the moment this one runs. This happened
// for real: an earlier version of this struct without Kind/SHA512/SHA1
// stripped "kind":"image" (and SHA-512/SHA-1 digests) from every OS-image
// recipe the first time this generator ran after genimages added them. Keep
// this struct pair a complete superset in lockstep with
// internal/core/index.go's Artifact and Recipe types, not just the subset
// this generator happens to write.
type artifact struct {
	URL           string   `json:"url"`
	SHA256        string   `json:"sha256,omitempty"`
	SHA512        string   `json:"sha512,omitempty"`
	SHA1          string   `json:"sha1,omitempty"`
	OCITokenURL   string   `json:"oci_token_url,omitempty"`
	Size          int64    `json:"size,omitempty"`
	Format        string   `json:"format,omitempty"`
	Strip         int      `json:"strip,omitempty"`
	Bin           []string `json:"bin,omitempty"`
	NoExecutables bool     `json:"no_executables,omitempty"`
	Man           []string `json:"man,omitempty"`
}

type recipe struct {
	Name        string               `json:"name"`
	Version     string               `json:"version"`
	Description string               `json:"description,omitempty"`
	Homepage    string               `json:"homepage,omitempty"`
	License     string               `json:"license,omitempty"`
	Keywords    []string             `json:"keywords,omitempty"`
	Kind        string               `json:"kind,omitempty"`
	Aliases     []string             `json:"aliases,omitempty"`
	Deps        []string             `json:"deps,omitempty"`
	Artifacts   map[string]*artifact `json:"artifacts"`
	Caveats     string               `json:"caveats,omitempty"`
}

func main() {
	var only string
	for i, a := range os.Args {
		if a == "-formula" && i+1 < len(os.Args) {
			only = os.Args[i+1]
		}
	}
	seeds := seedFormulae
	if only != "" {
		seeds = splitList(only)
	}

	logf("fetching %s ...", bulkURL)
	all, err := fetchAllFormulae()
	if err != nil {
		die("%v", err)
	}
	logf("loaded %s", plural(len(all), "formula", "formulae"))

	byName := map[string]*brewFormula{}
	for i := range all {
		byName[all[i].Name] = &all[i]
	}

	closure, missing, err := transitiveClosure(byName, seeds)
	if err != nil {
		die("%v", err)
	}
	if len(missing) > 0 {
		warn("not found in homebrew/core: %s", strings.Join(missing, ", "))
	}

	var built []*recipe
	var skipped []string
	for _, name := range closure {
		f := byName[name]
		r, err := buildRecipe(f)
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s (%v)", name, err))
			continue
		}
		built = append(built, r)
	}
	sort.Slice(built, func(i, j int) bool { return built[i].Name < built[j].Name })

	for _, r := range built {
		logf("%-20s %-10s %d %s, deps: %s", r.Name, r.Version, len(r.Artifacts),
			plural(len(r.Artifacts), "platform", "platforms"), depsOrNone(r.Deps))
	}
	if len(skipped) > 0 {
		warn("skipped: %s", strings.Join(skipped, "; "))
	}

	const outPath = "internal/core/data/index.json"
	total, err := indexio.Merge(outPath, built)
	if err != nil {
		die("%v", err)
	}

	arts := 0
	for _, r := range built {
		arts += len(r.Artifacts)
	}
	logf("")
	logf("wrote %s: %d recipes total, %d from homebrew/core (%d artifacts)", outPath, total, len(built), arts)
}

func fetchAllFormulae() ([]brewFormula, error) {
	req, err := http.NewRequest(http.MethodGet, bulkURL, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "hop-genbrew")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", bulkURL, resp.Status)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 128<<20))
	if err != nil {
		return nil, err
	}
	var all []brewFormula
	if err := json.Unmarshal(body, &all); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", bulkURL, err)
	}
	return all, nil
}

// transitiveClosure walks each seed's runtime dependency graph (never
// build/test-only dependencies, which the installed bottle doesn't need at
// run time) and returns every formula reached, dependencies before their
// dependents so the resulting recipe list is already in a safe install
// order for anyone reading it by eye.
func transitiveClosure(byName map[string]*brewFormula, seeds []string) (order, missing []string, err error) {
	seen := map[string]bool{}
	seedSet := map[string]bool{}
	for _, s := range seeds {
		seedSet[s] = true
	}
	var visiting []string // cycle detection path, for a clear error rather than infinite recursion

	var visit func(name string) error
	visit = func(name string) error {
		if seen[name] {
			return nil
		}
		f, ok := byName[name]
		if !ok {
			missing = append(missing, name)
			return nil
		}
		for _, v := range visiting {
			if v == name {
				return fmt.Errorf("dependency cycle: %s -> %s", strings.Join(visiting, " -> "), name)
			}
		}
		if f.Disabled {
			// Disabled means Homebrew itself refuses to build this at all —
			// often a license or safety reason (fdk-aac: patent-encumbered,
			// "does not meet the license policy"), never something to
			// carry forward regardless of intent.
			return nil
		}
		if f.Deprecated && !seedSet[name] {
			// A deprecated *dependency*, pulled in only because something
			// else needs it, would break everything relying on it the
			// moment Homebrew actually removes it — skip. A deprecated
			// formula named directly is a deliberate "give me the legacy
			// version" request and is let through: hop keeping an old
			// version installable after Homebrew itself retires it is
			// exactly the kind of thing hop's own architecture is good at.
			return nil
		}
		visiting = append(visiting, name)
		for _, dep := range f.allDependencies() {
			if err := visit(dep); err != nil {
				return err
			}
		}
		visiting = visiting[:len(visiting)-1]

		seen[name] = true
		order = append(order, name)
		return nil
	}

	for _, s := range seeds {
		if err := visit(s); err != nil {
			return nil, nil, err
		}
	}
	return order, missing, nil
}

// platformBottle picks the best bottle key for a hop platform. Homebrew
// tags macOS bottles by OS codename (newest first here) and a formula with
// no compiled, platform-specific content at all ships a single "all" key
// instead — colordiff and parallel, notably.
var macArmKeys = []string{"arm64_golden_gate", "arm64_tahoe", "arm64_sequoia", "arm64_sonoma", "arm64_ventura", "arm64_monterey", "arm64_big_sur"}
var macIntelKeys = []string{"golden_gate", "tahoe", "sequoia", "sonoma", "ventura", "monterey", "big_sur", "catalina"}

func platformBottle(f *brewFormula, plat string) (url, sha256 string, ok bool) {
	files := f.Bottle.Stable.Files
	if all, ok := files["all"]; ok {
		return all.URL, all.SHA256, true
	}
	var keys []string
	switch plat {
	case "darwin-arm64":
		keys = macArmKeys
	case "darwin-amd64":
		keys = macIntelKeys
	case "linux-arm64":
		keys = []string{"arm64_linux"}
	case "linux-amd64":
		keys = []string{"x86_64_linux"}
	}
	for _, k := range keys {
		if f, ok := files[k]; ok {
			return f.URL, f.SHA256, true
		}
	}
	return "", "", false
}

func buildRecipe(f *brewFormula) (*recipe, error) {
	if !f.Versions.Bottle || len(f.Bottle.Stable.Files) == 0 {
		return nil, fmt.Errorf("no bottle published")
	}
	// A formula with no declared executables is a pure library — most of
	// homebrew/core, and most of what ends up in any real dependency
	// closure. It still has to become a recipe: leaving it out would mean
	// every formula that depends on it fails to resolve at all. It just
	// puts nothing on PATH.
	//
	// glibc is a narrow, explicit exception even though it does declare
	// executables (ld.so, ldconfig, nscd, ...): real Homebrew marks it
	// keg_only specifically because linking it onto PATH risks shadowing
	// the system's own glibc, and hop pulls it in purely as an implicit
	// runtime dependency for other Linux bottles' ELF interpreter (see
	// relocateHomebrewBottle) — a user was never going to `hop install
	// glibc` themselves. This is deliberately not a general "respect
	// Homebrew's keg_only flag" rule: several already-shipped formulae
	// (curl, sqlite, readline, libxml2) are also keg_only in real Homebrew
	// but have executables a hop user plausibly does want on PATH, and
	// changing that is a separate decision, not a side effect of this one.
	isLibrary := len(f.Executables) == 0 || f.Name == "glibc"

	arts := map[string]*artifact{}
	for _, plat := range []string{"darwin-arm64", "darwin-amd64", "linux-arm64", "linux-amd64"} {
		url, sha, ok := platformBottle(f, plat)
		if !ok {
			continue
		}
		arts[plat] = &artifact{
			URL:           url,
			SHA256:        sha,
			OCITokenURL:   ociTokenURL(f.Name),
			Format:        "tar.gz",
			Bin:           f.Executables,
			NoExecutables: isLibrary,
		}
	}
	if len(arts) == 0 {
		return nil, fmt.Errorf("no usable bottle for any hop platform")
	}

	keywords := []string{"homebrew"}
	desc := f.Desc
	if isLibrary {
		keywords = append(keywords, "library")
		desc += " (library — pulled in only as a dependency; nothing on PATH)"
	}

	caveats := "Installed from Homebrew's own bottle (the same binary `brew install` " +
		f.Name + " would fetch), via ghcr.io/homebrew/core — not built or verified by hop itself beyond the checksum Homebrew publishes."
	if f.Deprecated {
		keywords = append(keywords, "legacy", "deprecated")
		reason := f.DeprecationReason
		if reason == "" {
			reason = "no longer supported"
		}
		caveats += fmt.Sprintf(" Homebrew itself has deprecated this formula (%s) and will eventually remove it; hop keeps it installable as a legacy version regardless, since an old version staying available after upstream retires it is exactly what hop's store is for.", reason)
	}

	return &recipe{
		Name:        f.Name,
		Version:     pkgVersion(f),
		Description: desc,
		Homepage:    f.Homepage,
		License:     f.License,
		Keywords:    keywords,
		Deps:        f.allDependencies(),
		Artifacts:   arts,
		Caveats:     caveats,
	}, nil
}

// pkgVersion is the version string a Homebrew bottle's own Cellar tree is
// laid out under internally — "1.11.1" normally, but "1.11.1_4" whenever the
// formula carries a nonzero revision (a bottle rebuilt without a version
// bump, e.g. after a dependency's ABI changed). hop's recipe Version must
// match this exactly: it becomes both the store path this package is
// installed under and, via depLoc in apply.go, the path relocateHomebrewBottle
// substitutes into every other bottle's placeholder references to this
// formula. Using the bare stable version here silently mismatches every
// revisioned formula's actual on-disk directory, producing an
// install-succeeds-but-binary-won't-run failure that only surfaces when the
// binary actually runs.
func pkgVersion(f *brewFormula) string {
	if f.Revision > 0 {
		return fmt.Sprintf("%s_%d", f.Versions.Stable, f.Revision)
	}
	return f.Versions.Stable
}

// ociTokenURL builds ghcr.io's anonymous pull-token endpoint for a
// homebrew/core repository. Requesting it needs no credentials: anyone can
// obtain a read-only token for a public repository, which is what lets hop
// fetch a public bottle without an account of its own.
func ociTokenURL(formula string) string {
	// A versioned formula name like "openssl@3" is not the real ghcr.io
	// repository path — Homebrew publishes it as "homebrew/core/openssl/3"
	// (the "@" becomes a path separator), since "@" isn't valid in an OCI
	// repository name at all. Requesting a token for the literal formula
	// name gets a 400 from the registry; this has to match the actual
	// artifact URL's own repository path.
	repo := strings.Replace(formula, "@", "/", 1)
	return fmt.Sprintf("https://ghcr.io/token?scope=repository:homebrew/core/%s:pull&service=ghcr.io", repo)
}

func depsOrNone(deps []string) string {
	if len(deps) == 0 {
		return "none"
	}
	return strings.Join(deps, ", ")
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

func logf(f string, a ...any) { fmt.Fprintf(os.Stdout, f+"\n", a...) }
func warn(f string, a ...any) { fmt.Fprintf(os.Stderr, "  ! "+f+"\n", a...) }
func die(f string, a ...any)  { fmt.Fprintf(os.Stderr, "error: "+f+"\n", a...); os.Exit(1) }
