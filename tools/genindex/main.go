// Command genindex builds hop's built-in recipe index from live upstream
// releases.
//
// For every package in tools/recipes.json it resolves the latest GitHub
// release, picks the right asset per platform, then downloads and *inspects*
// each archive to derive the exact SHA-256, strip depth, binary path and
// manpages. Nothing about the resulting index is guessed, which is what lets
// hop enforce checksums rather than merely record them.
//
//	go run ./tools/genindex                 # refresh every package
//	go run ./tools/genindex -only fd,bat    # refresh a subset
//	go run ./tools/genindex -platforms darwin-arm64
package main

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// spec is the hand-maintained description of a package.
type spec struct {
	Name        string   `json:"name"`
	Repo        string   `json:"repo"`
	Bin         []string `json:"bin"`
	Description string   `json:"description"`
	License     string   `json:"license"`
	Keywords    []string `json:"keywords"`
	Aliases     []string `json:"aliases"`
	Deps        []string `json:"deps"`
	Caveats     string   `json:"caveats"`
}

type specFile struct {
	Packages []spec `json:"packages"`
}

// Index output types mirror internal/core. They are duplicated rather than
// imported so the generator can never be broken by an engine refactor.
type artifact struct {
	URL    string   `json:"url"`
	SHA256 string   `json:"sha256,omitempty"`
	Size   int64    `json:"size,omitempty"`
	Format string   `json:"format,omitempty"`
	Strip  int      `json:"strip,omitempty"`
	Bin    []string `json:"bin,omitempty"`
	Man    []string `json:"man,omitempty"`
}

type recipe struct {
	Name        string               `json:"name"`
	Version     string               `json:"version"`
	Description string               `json:"description,omitempty"`
	Homepage    string               `json:"homepage,omitempty"`
	License     string               `json:"license,omitempty"`
	Keywords    []string             `json:"keywords,omitempty"`
	Aliases     []string             `json:"aliases,omitempty"`
	Deps        []string             `json:"deps,omitempty"`
	Artifacts   map[string]*artifact `json:"artifacts"`
	Caveats     string               `json:"caveats,omitempty"`
}

type index struct {
	Schema    int       `json:"schema"`
	Source    string    `json:"source"`
	Generated time.Time `json:"generated"`
	Recipes   []*recipe `json:"recipes"`
}

// ghRelease is the slice of the GitHub API response we need.
type ghRelease struct {
	TagName    string `json:"tag_name"`
	Prerelease bool   `json:"prerelease"`
	Draft      bool   `json:"draft"`
	Assets     []struct {
		Name string `json:"name"`
		URL  string `json:"browser_download_url"`
		Size int64  `json:"size"`
	} `json:"assets"`
}

var allPlatforms = []string{"darwin-arm64", "darwin-amd64", "linux-amd64", "linux-arm64"}

func main() {
	var (
		specPath  = flag.String("spec", "tools/recipes.json", "package spec file")
		outPath   = flag.String("out", "internal/core/data/index.json", "index output path")
		only      = flag.String("only", "", "comma-separated package names to refresh")
		platforms = flag.String("platforms", strings.Join(allPlatforms, ","), "comma-separated platforms")
		jobs      = flag.Int("j", 5, "concurrent packages")
		noHash    = flag.Bool("no-hash", false, "skip downloads; emit URLs without digests (trust on first use)")
	)
	flag.Parse()

	sf, err := loadSpec(*specPath)
	if err != nil {
		die("reading %s: %v", *specPath, err)
	}

	wanted := map[string]bool{}
	for _, n := range splitList(*only) {
		wanted[strings.ToLower(n)] = true
	}
	plats := splitList(*platforms)

	client := &http.Client{Timeout: 20 * time.Minute}
	token := os.Getenv("GITHUB_TOKEN") // optional; only raises the rate limit

	var (
		mu      sync.Mutex
		out     []*recipe
		skipped []string
	)
	sem := make(chan struct{}, *jobs)
	var wg sync.WaitGroup

	for _, s := range sf.Packages {
		if len(wanted) > 0 && !wanted[strings.ToLower(s.Name)] {
			continue
		}
		wg.Add(1)
		go func(s spec) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			r, err := build(client, token, s, plats, *noHash)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				warn("%-12s skipped: %v", s.Name, err)
				skipped = append(skipped, s.Name)
				return
			}
			logf("%-12s %-10s %d %s", r.Name, r.Version, len(r.Artifacts), plural(len(r.Artifacts), "artifact", "artifacts"))
			out = append(out, r)
		}(s)
	}
	wg.Wait()

	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })

	// Merge with any existing index so a partial run (-only) is additive
	// rather than destructive.
	if len(wanted) > 0 {
		if prev, err := loadIndex(*outPath); err == nil {
			have := map[string]bool{}
			for _, r := range out {
				have[r.Name] = true
			}
			for _, r := range prev.Recipes {
				if !have[r.Name] {
					out = append(out, r)
				}
			}
			sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
		}
	}

	ix := &index{
		Schema:    1,
		Source:    "builtin",
		Generated: time.Now().UTC().Truncate(time.Second),
		Recipes:   out,
	}
	b, err := json.MarshalIndent(ix, "", "  ")
	if err != nil {
		die("encoding index: %v", err)
	}
	if err := os.MkdirAll(filepath.Dir(*outPath), 0o755); err != nil {
		die("%v", err)
	}
	if err := os.WriteFile(*outPath, append(b, '\n'), 0o644); err != nil {
		die("writing %s: %v", *outPath, err)
	}

	arts := 0
	for _, r := range out {
		arts += len(r.Artifacts)
	}
	logf("")
	logf("wrote %s: %d recipes, %d artifacts", *outPath, len(out), arts)
	if len(skipped) > 0 {
		sort.Strings(skipped)
		warn("skipped: %s", strings.Join(skipped, ", "))
	}
}

// build resolves one package into a recipe.
func build(client *http.Client, token string, s spec, plats []string, noHash bool) (*recipe, error) {
	rel, err := latestRelease(client, token, s.Repo)
	if err != nil {
		return nil, err
	}
	version := normaliseTag(rel.TagName, s.Name)
	if version == "" {
		return nil, fmt.Errorf("release has no usable tag (%q)", rel.TagName)
	}

	r := &recipe{
		Name:        s.Name,
		Version:     version,
		Description: s.Description,
		Homepage:    "https://github.com/" + s.Repo,
		License:     s.License,
		Keywords:    s.Keywords,
		Aliases:     s.Aliases,
		Deps:        s.Deps,
		Caveats:     s.Caveats,
		Artifacts:   map[string]*artifact{},
	}

	cmds := s.Bin
	if len(cmds) == 0 {
		cmds = []string{s.Name}
	}

	for _, plat := range plats {
		asset, ok := pickAsset(rel, plat)
		if !ok {
			continue
		}
		a := &artifact{URL: asset.URL, Size: asset.Size, Format: formatOf(asset.Name)}

		if noHash {
			a.Bin = cmds
			r.Artifacts[plat] = a
			continue
		}

		digest, layout, err := inspect(client, asset.URL, a.Format, cmds)
		if err != nil {
			warn("%-12s %-13s %v", s.Name, plat, err)
			continue
		}
		a.SHA256 = digest
		a.Strip = layout.strip
		a.Bin = layout.bins
		a.Man = layout.mans
		r.Artifacts[plat] = a
	}

	if len(r.Artifacts) == 0 {
		return nil, fmt.Errorf("no usable assets for %s", strings.Join(plats, ", "))
	}
	return r, nil
}

func latestRelease(client *http.Client, token, repo string) (*ghRelease, error) {
	req, _ := http.NewRequest(http.MethodGet, "https://api.github.com/repos/"+repo+"/releases/latest", nil)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", "hop-genindex")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusForbidden && resp.Header.Get("X-RateLimit-Remaining") == "0" {
		return nil, fmt.Errorf("GitHub API rate limit reached; set GITHUB_TOKEN and retry")
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GitHub API returned %s", resp.Status)
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return nil, err
	}
	if rel.Draft {
		return nil, fmt.Errorf("latest release is a draft")
	}
	return &rel, nil
}

// normaliseTag turns an upstream tag into a bare version. Projects tag
// releases as "v1.2.3", "1.2.3", "jq-1.8.2" or "release-1.2.3"; hop wants the
// version alone so comparisons and store paths stay consistent.
func normaliseTag(tag, pkg string) string {
	v := strings.TrimSpace(tag)
	for _, prefix := range []string{pkg + "-", pkg + "_", pkg + "/", "release-", "releases/", "rel-"} {
		if len(prefix) > 1 && strings.HasPrefix(strings.ToLower(v), strings.ToLower(prefix)) {
			v = v[len(prefix):]
		}
	}
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	// Anything left must start with a digit to be a version.
	if v == "" || v[0] < '0' || v[0] > '9' {
		return ""
	}
	return v
}

// ------------------------------------------------------------ asset picking ----

// osTokens and archTokens describe how upstreams spell platforms. Matching on
// both halves independently handles almost every naming scheme in the wild
// without per-package patterns.
var osTokens = map[string][]string{
	"darwin": {"darwin", "apple", "macos", "osx", "mac"},
	"linux":  {"linux"},
}

var archTokens = map[string][]string{
	"arm64": {"aarch64", "arm64"},
	"amd64": {"x86_64", "amd64", "x64"},
}

// rejectTokens are never the artifact we want: signatures, checksums, distro
// packages, other architectures, and variant builds.
var rejectTokens = []string{
	".sha256", ".sha256sum", ".sha512", ".md5", ".asc", ".sig", ".sbom", ".pem", ".json", ".txt",
	".deb", ".rpm", ".pkg", ".msi", ".apk", ".dmg", ".exe", ".nupkg",
	"i686", "i386", "armv7", "armv6", "armhf", "arm-unknown", "s390x", "ppc64", "riscv", "mips",
	"android", "windows", "freebsd", "netbsd", "illumos", "wasm",
	"no_libgit", "-debug", "sources", "source-",
}

type pickedAsset struct {
	Name string
	URL  string
	Size int64
}

// pickAsset selects the best asset for a platform, preferring tarballs and,
// on Linux, statically linked musl builds for portability.
func pickAsset(rel *ghRelease, plat string) (pickedAsset, bool) {
	parts := strings.SplitN(plat, "-", 2)
	if len(parts) != 2 {
		return pickedAsset{}, false
	}
	osT, archT := osTokens[parts[0]], archTokens[parts[1]]
	otherArch := "arm64"
	if parts[1] == "arm64" {
		otherArch = "amd64"
	}

	type scored struct {
		a     pickedAsset
		score int
	}
	var cands []scored

	for _, as := range rel.Assets {
		lower := strings.ToLower(as.Name)

		if !containsAny(lower, osT) || !containsAny(lower, archT) {
			continue
		}
		// An asset naming the other architecture is a universal/fat build at
		// best and a mismatch at worst; skip it.
		if containsAny(lower, archTokens[otherArch]) {
			continue
		}
		if containsAny(lower, rejectTokens) {
			continue
		}

		score := 0
		switch {
		case strings.HasSuffix(lower, ".tar.gz"), strings.HasSuffix(lower, ".tgz"):
			score += 100
		case strings.HasSuffix(lower, ".tar.xz"):
			score += 80
		case strings.HasSuffix(lower, ".zip"):
			score += 60
		case strings.HasSuffix(lower, ".tar.bz2"):
			score += 50
		default:
			score += 40 // a bare binary, e.g. jq-macos-arm64
		}
		if parts[0] == "linux" {
			if strings.Contains(lower, "musl") {
				score += 20 // static: runs on any distro
			} else if strings.Contains(lower, "gnu") {
				score += 10
			}
		}
		cands = append(cands, scored{pickedAsset{as.Name, as.URL, as.Size}, score})
	}

	if len(cands) == 0 {
		return pickedAsset{}, false
	}
	sort.SliceStable(cands, func(i, j int) bool {
		if cands[i].score != cands[j].score {
			return cands[i].score > cands[j].score
		}
		return len(cands[i].a.Name) < len(cands[j].a.Name) // prefer the plainer name
	})
	return cands[0].a, true
}

func formatOf(name string) string {
	l := strings.ToLower(name)
	switch {
	case strings.HasSuffix(l, ".tar.gz"), strings.HasSuffix(l, ".tgz"):
		return "tar.gz"
	case strings.HasSuffix(l, ".tar.xz"), strings.HasSuffix(l, ".txz"):
		return "tar.xz"
	case strings.HasSuffix(l, ".tar.bz2"):
		return "tar.bz2"
	case strings.HasSuffix(l, ".tar"):
		return "tar"
	case strings.HasSuffix(l, ".zip"):
		return "zip"
	case strings.HasSuffix(l, ".gz"):
		return "gz"
	default:
		return "raw"
	}
}

// ------------------------------------------------------------- inspection ----

// layout is what inspecting an archive taught us about its shape.
type layout struct {
	strip int
	bins  []string
	mans  []string
}

// inspect downloads an artifact once, computing its digest while listing its
// contents, then deletes it. The file is streamed to a temp path and removed
// immediately, so refreshing the whole index costs no lasting disk.
func inspect(client *http.Client, url, format string, cmds []string) (string, layout, error) {
	tmp, err := os.CreateTemp("", "hop-genindex-*")
	if err != nil {
		return "", layout{}, err
	}
	name := tmp.Name()
	defer func() { tmp.Close(); os.Remove(name) }()

	req, _ := http.NewRequest(http.MethodGet, url, nil)
	req.Header.Set("User-Agent", "hop-genindex")
	resp, err := client.Do(req)
	if err != nil {
		return "", layout{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", layout{}, fmt.Errorf("download returned %s", resp.Status)
	}

	h := sha256.New()
	if _, err := io.Copy(io.MultiWriter(tmp, h), resp.Body); err != nil {
		return "", layout{}, err
	}
	if err := tmp.Sync(); err != nil {
		return "", layout{}, err
	}
	digest := hex.EncodeToString(h.Sum(nil))

	names, err := listArchive(name, format)
	if err != nil {
		return "", layout{}, err
	}
	lay, err := deriveLayout(names, cmds, format)
	if err != nil {
		return "", layout{}, err
	}
	return digest, lay, nil
}

// listArchive returns the member paths of an archive. A raw single-file
// artifact reports no members.
func listArchive(path, format string) ([]string, error) {
	switch format {
	case "raw", "gz":
		return nil, nil
	case "zip":
		zr, err := zip.OpenReader(path)
		if err != nil {
			return nil, fmt.Errorf("not a zip: %w", err)
		}
		defer zr.Close()
		var out []string
		for _, f := range zr.File {
			if f.FileInfo().IsDir() {
				continue
			}
			out = append(out, f.Name)
		}
		return out, nil
	case "tar.gz":
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		zr, err := gzip.NewReader(f)
		if err != nil {
			return nil, fmt.Errorf("not gzip: %w", err)
		}
		defer zr.Close()
		return listTar(zr)
	case "tar":
		f, err := os.Open(path)
		if err != nil {
			return nil, err
		}
		defer f.Close()
		return listTar(f)
	default:
		// tar.xz and tar.bz2 are installable but not introspectable here;
		// fall back to declaring the command names without a path.
		return nil, nil
	}
}

func listTar(r io.Reader) ([]string, error) {
	tr := tar.NewReader(r)
	var out []string
	for {
		h, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, err
		}
		if h.Typeflag == tar.TypeDir {
			continue
		}
		out = append(out, strings.TrimPrefix(h.Name, "./"))
	}
	return out, nil
}

// deriveLayout works out the strip depth and the in-archive paths of the
// commands and manpages, so the recipe records facts instead of assumptions.
func deriveLayout(names, cmds []string, format string) (layout, error) {
	if len(names) == 0 {
		// Single-file artifact: hop writes it to bin/<cmd> itself.
		return layout{bins: cmds}, nil
	}

	// Strip a shared top-level directory, as nearly every release tarball has.
	strip := 0
	if root, ok := commonRoot(names); ok && root != "" {
		strip = 1
		for i := range names {
			names[i] = strings.TrimPrefix(names[i], root+"/")
		}
	}

	var bins, mans []string
	for _, cmd := range cmds {
		hit, ok := findMember(names, cmd)
		if !ok {
			return layout{}, fmt.Errorf("archive does not contain a %q binary", cmd)
		}
		// Record an explicit rename when the file is not named after the
		// command, so hop links yq_darwin_arm64 onto PATH as "yq".
		if path.Base(hit) != cmd {
			bins = append(bins, cmd+"="+hit)
		} else {
			bins = append(bins, hit)
		}
	}
	for _, n := range names {
		base := path.Base(n)
		if strings.HasSuffix(base, ".1") && !strings.Contains(n, "completions") {
			mans = append(mans, n)
		}
	}
	sort.Strings(mans)
	if len(mans) > 6 {
		mans = mans[:6]
	}
	return layout{strip: strip, bins: bins, mans: mans}, nil
}

// commonRoot reports the single shared first path segment, if there is one.
func commonRoot(names []string) (string, bool) {
	root := ""
	for _, n := range names {
		n = strings.TrimPrefix(filepath.ToSlash(n), "./")
		i := strings.IndexByte(n, '/')
		if i < 0 {
			return "", false // a top-level file: nothing to strip
		}
		seg := n[:i]
		if root == "" {
			root = seg
			continue
		}
		if seg != root {
			return "", false
		}
	}
	return root, root != ""
}

// findMember locates a command inside an archive listing, preferring a
// top-level or bin/ location over something buried in completions.
func findMember(names []string, cmd string) (string, bool) {
	var hits []string
	for _, n := range names {
		if path.Base(n) == cmd {
			hits = append(hits, n)
		}
	}
	if len(hits) == 0 {
		// Many Go projects ship the binary named after its platform, e.g.
		// yq_darwin_arm64. Accept <cmd> followed by a separator.
		for _, n := range names {
			b := path.Base(n)
			if strings.HasPrefix(b, cmd+"_") || strings.HasPrefix(b, cmd+"-") {
				hits = append(hits, n)
			}
		}
	}
	if len(hits) == 0 {
		return "", false
	}
	sort.Slice(hits, func(i, j int) bool {
		di := strings.Count(hits[i], "/")
		dj := strings.Count(hits[j], "/")
		bi := strings.HasPrefix(hits[i], "bin/")
		bj := strings.HasPrefix(hits[j], "bin/")
		if bi != bj {
			return bi
		}
		if di != dj {
			return di < dj
		}
		return hits[i] < hits[j]
	})
	return hits[0], true
}

// ---------------------------------------------------------------- helpers ----

func loadSpec(p string) (*specFile, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var sf specFile
	if err := json.Unmarshal(b, &sf); err != nil {
		return nil, err
	}
	if len(sf.Packages) == 0 {
		return nil, fmt.Errorf("spec declares no packages")
	}
	return &sf, nil
}

func loadIndex(p string) (*index, error) {
	b, err := os.ReadFile(p)
	if err != nil {
		return nil, err
	}
	var ix index
	if err := json.Unmarshal(b, &ix); err != nil {
		return nil, err
	}
	return &ix, nil
}

func containsAny(s string, toks []string) bool {
	for _, t := range toks {
		if strings.Contains(s, t) {
			return true
		}
	}
	return false
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
