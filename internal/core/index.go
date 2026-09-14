package core

import (
	"embed"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// SchemaVersion is the recipe index format hop understands. A newer index is
// refused with a clear message rather than misparsed.
const SchemaVersion = 1

//go:embed all:data
var builtinFS embed.FS

// Artifact is one prebuilt download for one platform. Recipes are pure data:
// hop never executes anything from an index, which is the single biggest
// security difference from a Homebrew tap full of arbitrary Ruby.
type Artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
	// SHA512 verifies an artifact whose upstream publishes only a SHA-512
	// manifest (Debian's cloud images, notably).
	SHA512 string `json:"sha512,omitempty"`
	// SHA1 verifies an artifact whose upstream manifest predates SHA-256
	// becoming standard (Apple's software update catalog, notably, still
	// publishes only a SHA-1 "Digest" for every package). Weak as a defence
	// against a determined adversary, but it still catches a corrupted or
	// substituted download — the actual threat model here — and it is what
	// the publisher itself signs, so hop pins what they actually published.
	// Exactly one of SHA256/SHA512/SHA1 is expected to be set, checked in
	// that order of preference if a recipe somehow sets more than one.
	SHA1 string `json:"sha1,omitempty"`
	Size int64  `json:"size,omitempty"`

	// OCITokenURL marks URL as an OCI Distribution API blob (the format
	// Homebrew's own bottles are hosted in, on ghcr.io) rather than a plain
	// HTTP download. Fetching one is a two-step protocol, not a GET: hop
	// first fetches this URL, which returns anonymously (no credentials) a
	// short-lived JSON {"token": "..."}, then sends that token as a Bearer
	// Authorization header on the request to URL itself. The registry
	// responds with a redirect to a separately pre-signed, short-lived CDN
	// URL — which is exactly why a plain static URL can't be pinned here the
	// way every other artifact in the index is: only the registry blob
	// address and its token endpoint stay stable long enough to compile into
	// a released binary; the actual signed download link is minutes-lived.
	OCITokenURL string `json:"oci_token_url,omitempty"`

	// Format overrides detection from the URL: tar.gz, tar.xz, tar.bz2, tar,
	// zip, gz or raw.
	Format string `json:"format,omitempty"`

	// Strip removes N leading path components on extraction, so a tarball that
	// wraps everything in ripgrep-14.1.1/ lands flat in the store.
	Strip int `json:"strip,omitempty"`

	// Bin lists paths inside the extracted tree to expose on PATH. An entry
	// may be written "name=path" when the command should appear under a
	// different name than the file has, which upstreams that ship
	// yq_darwin_arm64 and friends require. A bare name is searched for if the
	// path does not exist, tolerating upstreams that move their layout.
	Bin []string `json:"bin,omitempty"`

	// NoExecutables marks a bin-kind artifact that legitimately puts nothing
	// on PATH at all — a pure shared library, pulled in only because
	// something else depends on it at runtime (Homebrew's dependency graph
	// is full of these: readline, gettext, openssl and the like). Without
	// this, an empty Bin list is indistinguishable from "not declared,
	// please auto-discover", and auto-discovery correctly errors when a
	// library archive contains no executable at all.
	NoExecutables bool `json:"no_executables,omitempty"`

	// Man lists manpages to link into <profile>/share/man.
	Man []string `json:"man,omitempty"`

	// AppPath names the .app bundle inside a KindApp artifact: the
	// mount-relative path inside a .dmg's volume, or the path inside a .zip.
	// Empty means "the one top-level *.app directory", which is how almost
	// every Homebrew cask actually ships.
	AppPath string `json:"app_path,omitempty"`
}

// Kind distinguishes what an installed generation actually does with a
// recipe's artifact.
type Kind string

const (
	// KindBin is a command-line tool: its artifact is extracted, its binaries
	// are discovered and linked onto PATH. The default; recipes never need to
	// write it out explicitly.
	KindBin Kind = ""

	// KindImage is a large single-file artifact — a cloud VM disk image, a
	// container rootfs tarball — that is verified and content-addressed like
	// any other package, but is never extracted and never touches PATH. It is
	// stored as-is and located afterward with `hop info`.
	KindImage Kind = "image"

	// KindApp is a macOS GUI application — what Homebrew calls a cask. Its
	// artifact is a .dmg or .zip containing a .app bundle; hop verifies the
	// download, copies the bundle into its store like anything else, and
	// exposes it by symlinking into ~/Applications rather than onto PATH.
	// Darwin-only: a recipe with this kind simply has no artifact for any
	// other platform, so install fails with the normal "unsupported
	// platform" error everywhere else.
	KindApp Kind = "app"
)

// Recipe describes one installable package.
type Recipe struct {
	Name        string   `json:"name"`
	Version     string   `json:"version"`
	Description string   `json:"description,omitempty"`
	Homepage    string   `json:"homepage,omitempty"`
	License     string   `json:"license,omitempty"`
	Keywords    []string `json:"keywords,omitempty"`

	// Kind selects how an artifact is handled after download. Empty (KindBin)
	// for ordinary CLI tools; KindImage for OS/VM images and rootfs archives;
	// KindApp for a macOS GUI application (a Homebrew cask).
	Kind Kind `json:"kind,omitempty"`

	// Aliases are other names this package answers to, including the name
	// Homebrew uses when it differs (brew calls delta "git-delta"). They make
	// `hop install <brew name>` and `hop migrate` work without a lookup table.
	Aliases []string `json:"aliases,omitempty"`

	// Deps are runtime dependencies, resolved transitively before install.
	Deps []string `json:"deps,omitempty"`

	// Artifacts is keyed by "<os>-<arch>".
	Artifacts map[Platform]*Artifact `json:"artifacts"`

	// Caveats is shown after install, for packages needing a shell hook.
	Caveats string `json:"caveats,omitempty"`
}

// Artifact selects the best artifact for p, reporting which platform matched
// so callers can warn about emulated fallbacks.
func (r *Recipe) Artifact(p Platform) (*Artifact, Platform, bool) {
	for _, cand := range p.Fallbacks() {
		if a, ok := r.Artifacts[cand]; ok && a != nil && a.URL != "" {
			return a, cand, true
		}
	}
	return nil, "", false
}

// Platforms lists the platforms this recipe supports, sorted for display.
func (r *Recipe) Platforms() []string {
	out := make([]string, 0, len(r.Artifacts))
	for p := range r.Artifacts {
		out = append(out, string(p))
	}
	sort.Strings(out)
	return out
}

// BinNames returns the command names this recipe puts on PATH for p.
func (r *Recipe) BinNames(p Platform) []string {
	a, _, ok := r.Artifact(p)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(a.Bin))
	for _, b := range a.Bin {
		name, _ := SplitBin(b)
		out = append(out, name)
	}
	return out
}

// Index is a collection of recipes from one source.
type Index struct {
	Schema    int       `json:"schema"`
	Source    string    `json:"source,omitempty"`
	Generated time.Time `json:"generated,omitempty"`
	Recipes   []*Recipe `json:"recipes"`

	byName  map[string]*Recipe
	fetched time.Time // when this copy reached disk
	builtin bool
}

// Len reports the recipe count.
func (ix *Index) Len() int { return len(ix.Recipes) }

// IsBuiltin reports whether this index is the one compiled into the binary.
func (ix *Index) IsBuiltin() bool { return ix.builtin }

// Age reports how long ago the index was fetched.
func (ix *Index) Age() time.Time { return ix.fetched }

// index builds the name lookup and validates invariants once at load time,
// so every later lookup is a map hit and cannot fail in surprising ways.
func (ix *Index) build() error {
	if ix.Schema > SchemaVersion {
		return fmt.Errorf("index needs schema v%d but this hop understands v%d; upgrade hop", ix.Schema, SchemaVersion)
	}
	ix.byName = make(map[string]*Recipe, len(ix.Recipes))
	for _, r := range ix.Recipes {
		if r.Name == "" {
			return fmt.Errorf("index contains a recipe with no name")
		}
		if r.Kind != KindBin && r.Kind != KindImage && r.Kind != KindApp {
			return fmt.Errorf("%s: unknown kind %q", r.Name, r.Kind)
		}
		key := strings.ToLower(r.Name)
		if _, dup := ix.byName[key]; dup {
			return fmt.Errorf("index defines %q twice", r.Name)
		}
		ix.byName[key] = r
	}
	// Aliases are registered second so a real package name always wins over
	// another package's alias.
	for _, r := range ix.Recipes {
		for _, al := range r.Aliases {
			k := strings.ToLower(al)
			if k == "" {
				continue
			}
			if _, taken := ix.byName[k]; taken {
				continue
			}
			ix.byName[k] = r
		}
	}
	sort.Slice(ix.Recipes, func(i, j int) bool { return ix.Recipes[i].Name < ix.Recipes[j].Name })
	return nil
}

// Lookup finds a recipe by name or alias, case-insensitively.
func (ix *Index) Lookup(name string) (*Recipe, bool) {
	r, ok := ix.byName[strings.ToLower(name)]
	return r, ok
}

// Match is a scored search result.
type Match struct {
	Recipe *Recipe
	Score  int
}

// Search ranks recipes against a query. Exact and prefix name matches come
// first, then name substrings, then keywords, then descriptions; below that,
// a typo-tolerant edit-distance pass catches "ripgrpe".
func (ix *Index) Search(query string) []Match {
	q := strings.ToLower(strings.TrimSpace(query))
	if q == "" {
		out := make([]Match, 0, len(ix.Recipes))
		for _, r := range ix.Recipes {
			out = append(out, Match{r, 1})
		}
		return out
	}

	var out []Match
	for _, r := range ix.Recipes {
		name := strings.ToLower(r.Name)
		desc := strings.ToLower(r.Description)

		score := 0
		switch {
		case name == q:
			score = 1000
		case strings.HasPrefix(name, q):
			score = 800 - len(name)
		case strings.Contains(name, q):
			score = 600 - len(name)
		}
		for _, k := range r.Keywords {
			if strings.ToLower(k) == q {
				score = maxInt(score, 500)
			} else if strings.Contains(strings.ToLower(k), q) {
				score = maxInt(score, 300)
			}
		}
		if score == 0 && strings.Contains(desc, q) {
			score = 200
			if strings.HasPrefix(desc, q) {
				score = 250
			}
		}
		if score == 0 && len(q) >= 4 {
			if d := editDistance(q, name); d <= 2 {
				score = 100 - d*10 // typo tolerance
			}
		}
		if score > 0 {
			out = append(out, Match{r, score})
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Score != out[j].Score {
			return out[i].Score > out[j].Score
		}
		return out[i].Recipe.Name < out[j].Recipe.Name
	})
	return out
}

// Suggest returns close names for an unknown package, powering the
// "did you mean" line that makes a typo a one-second fix.
func (ix *Index) Suggest(name string, limit int) []string {
	type cand struct {
		name string
		d    int
	}
	q := strings.ToLower(name)
	var cs []cand
	for _, r := range ix.Recipes {
		n := strings.ToLower(r.Name)
		d := editDistance(q, n)
		// Accept near-misses, plus any name that contains the query.
		if d <= 3 || strings.Contains(n, q) || strings.Contains(q, n) {
			if strings.Contains(n, q) && d > 3 {
				d = 3
			}
			cs = append(cs, cand{r.Name, d})
		}
	}
	sort.Slice(cs, func(i, j int) bool {
		if cs[i].d != cs[j].d {
			return cs[i].d < cs[j].d
		}
		return cs[i].name < cs[j].name
	})
	out := make([]string, 0, limit)
	for _, c := range cs {
		if len(out) >= limit {
			break
		}
		out = append(out, c.name)
	}
	return out
}

// ------------------------------------------------------------------ loading ----

// BuiltinIndex returns the index compiled into the binary. hop therefore works
// on a fresh machine with no network at all, which `brew` cannot do.
func BuiltinIndex() (*Index, error) {
	b, err := builtinFS.ReadFile("data/index.json")
	if err != nil {
		return nil, fmt.Errorf("built-in index missing from binary: %w", err)
	}
	ix := &Index{}
	if err := json.Unmarshal(b, ix); err != nil {
		return nil, fmt.Errorf("built-in index is corrupt: %w", err)
	}
	ix.builtin = true
	if err := ix.build(); err != nil {
		return nil, err
	}
	return ix, nil
}

// LoadIndex returns the cached index if present and usable, else the built-in
// one. It never fails merely because the cache is stale or damaged: a broken
// cache falls back with a warning so hop stays usable.
func LoadIndex(l *Layout) (*Index, error) {
	b, err := os.ReadFile(l.IndexPath())
	if err != nil {
		return BuiltinIndex()
	}
	ix := &Index{}
	if err := json.Unmarshal(b, ix); err != nil {
		return BuiltinIndex()
	}
	if err := ix.build(); err != nil {
		return BuiltinIndex()
	}
	if st, err := os.Stat(l.IndexPath()); err == nil {
		ix.fetched = st.ModTime()
	}
	// A cache with fewer recipes than the built-in index is a downgrade; take
	// whichever knows more so `hop install` does not mysteriously regress.
	bi, berr := BuiltinIndex()
	if berr == nil && bi.Len() > ix.Len() && ix.Source == "" {
		return bi, nil
	}
	return ix, nil
}

// indexMeta records validators for conditional index requests.
type indexMeta struct {
	ETag      string    `json:"etag,omitempty"`
	Modified  string    `json:"last_modified,omitempty"`
	URL       string    `json:"url,omitempty"`
	FetchedAt time.Time `json:"fetched_at,omitempty"`
}

// IndexURL reports the configured remote index, if any.
func IndexURL() string { return os.Getenv("HOP_INDEX_URL") }

// UpdateResult describes what `hop update` did.
type UpdateResult struct {
	Changed   bool
	NotModif  bool
	Recipes   int
	Source    string
	FetchedAt time.Time
}

// UpdateIndex refreshes the cached index from HOP_INDEX_URL with a single
// conditional HTTP request. This is the whole of `hop update`: no git clone,
// no thousands of files, one round trip that usually returns 304.
func UpdateIndex(l *Layout, client *http.Client) (*UpdateResult, error) {
	url := IndexURL()
	if url == "" {
		return nil, fmt.Errorf("no remote index configured")
	}

	var meta indexMeta
	if b, err := os.ReadFile(l.IndexMetaPath()); err == nil {
		_ = json.Unmarshal(b, &meta)
	}

	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("bad index URL %q: %w", url, err)
	}
	req.Header.Set("User-Agent", userAgent())
	req.Header.Set("Accept-Encoding", "gzip")
	// Only trust validators recorded for this same URL.
	if meta.URL == url {
		if meta.ETag != "" {
			req.Header.Set("If-None-Match", meta.ETag)
		}
		if meta.Modified != "" {
			req.Header.Set("If-Modified-Since", meta.Modified)
		}
	}

	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("fetching index: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotModified {
		cur, _ := LoadIndex(l)
		n := 0
		if cur != nil {
			n = cur.Len()
		}
		return &UpdateResult{NotModif: true, Recipes: n, Source: url, FetchedAt: meta.FetchedAt}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("index server returned %s", resp.Status)
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		return nil, fmt.Errorf("reading index: %w", err)
	}

	// Validate before overwriting the cache: a malformed download must never
	// cost the user their working index.
	cand := &Index{}
	if err := json.Unmarshal(body, cand); err != nil {
		return nil, fmt.Errorf("index is not valid JSON: %w", err)
	}
	if err := cand.build(); err != nil {
		return nil, err
	}
	if cand.Source == "" {
		cand.Source = url
		body, _ = json.Marshal(cand)
	}

	if err := writeFileAtomic(l.IndexPath(), body, 0o644); err != nil {
		return nil, fmt.Errorf("caching index: %w", err)
	}
	now := time.Now()
	mb, _ := json.MarshalIndent(indexMeta{
		ETag:      resp.Header.Get("ETag"),
		Modified:  resp.Header.Get("Last-Modified"),
		URL:       url,
		FetchedAt: now,
	}, "", "  ")
	_ = writeFileAtomic(l.IndexMetaPath(), mb, 0o644)

	return &UpdateResult{Changed: true, Recipes: cand.Len(), Source: url, FetchedAt: now}, nil
}

func userAgent() string { return "hop/" + Version + " (+https://github.com/samuelbanapour/hopcli)" }

// ----------------------------------------------------------------- helpers ----

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// editDistance is Levenshtein distance with a two-row buffer, used only for
// suggestions so the naive implementation is fine.
func editDistance(a, b string) int {
	ar, br := []rune(a), []rune(b)
	if len(ar) == 0 {
		return len(br)
	}
	if len(br) == 0 {
		return len(ar)
	}
	prev := make([]int, len(br)+1)
	cur := make([]int, len(br)+1)
	for j := range prev {
		prev[j] = j
	}
	for i := 1; i <= len(ar); i++ {
		cur[0] = i
		for j := 1; j <= len(br); j++ {
			cost := 1
			if ar[i-1] == br[j-1] {
				cost = 0
			}
			cur[j] = minInt(minInt(cur[j-1]+1, prev[j]+1), prev[j-1]+cost)
		}
		prev, cur = cur, prev
	}
	return prev[len(br)]
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// SplitBin parses a recipe bin declaration into the command name to expose
// and the path inside the extracted tree. "rg" and "bin/rg" both yield the
// name "rg"; "yq=yq_darwin_arm64" renames on the way out.
func SplitBin(spec string) (name, rel string) {
	if i := strings.IndexByte(spec, '='); i > 0 {
		return spec[:i], spec[i+1:]
	}
	return baseName(spec), spec
}

func baseName(p string) string {
	p = strings.TrimSuffix(p, "/")
	if i := strings.LastIndexByte(p, '/'); i >= 0 {
		return p[i+1:]
	}
	return p
}
