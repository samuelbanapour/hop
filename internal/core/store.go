package core

import (
	"archive/tar"
	"archive/zip"
	"compress/bzip2"
	"compress/gzip"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// maxArchiveBytes caps total *extracted* size, so a zip bomb inside a CLI
// tool's archive cannot fill a disk that is already close to full. No
// legitimate CLI tool needs anywhere near this much unpacked; this stays
// conservative on purpose. It is unrelated to maxDownloadBytes in fetch.go,
// which bounds the raw network transfer and has to accommodate a real
// multi-gigabyte OS image that is never extracted at all.
const maxArchiveBytes = 4 << 30 // 4 GiB

// maxArchiveEntries caps member count for the same reason.
const maxArchiveEntries = 200_000

// Format identifies an artifact's container.
type Format string

// Supported artifact formats.
const (
	FormatTarGz  Format = "tar.gz"
	FormatTarXz  Format = "tar.xz"
	FormatTarBz2 Format = "tar.bz2"
	FormatTar    Format = "tar"
	FormatZip    Format = "zip"
	FormatGz     Format = "gz"
	FormatRaw    Format = "raw"
)

// DetectFormat resolves an artifact's format, preferring the explicit field
// and falling back to the URL's extension.
func DetectFormat(a *Artifact) Format {
	if a.Format != "" {
		return Format(strings.TrimPrefix(strings.ToLower(a.Format), "."))
	}
	u := strings.ToLower(a.URL)
	if i := strings.IndexAny(u, "?#"); i >= 0 {
		u = u[:i] // ignore query strings on CDN URLs
	}
	switch {
	case strings.HasSuffix(u, ".tar.gz"), strings.HasSuffix(u, ".tgz"):
		return FormatTarGz
	case strings.HasSuffix(u, ".tar.xz"), strings.HasSuffix(u, ".txz"):
		return FormatTarXz
	case strings.HasSuffix(u, ".tar.bz2"), strings.HasSuffix(u, ".tbz"), strings.HasSuffix(u, ".tbz2"):
		return FormatTarBz2
	case strings.HasSuffix(u, ".tar"):
		return FormatTar
	case strings.HasSuffix(u, ".zip"):
		return FormatZip
	case strings.HasSuffix(u, ".gz"):
		return FormatGz
	default:
		return FormatRaw
	}
}

// BinLink is one command to put on PATH: the name it is exposed under, and
// where the file lives. Keeping these separate lets a recipe expose an
// upstream's yq_darwin_arm64 as plain "yq".
type BinLink struct {
	Name string `json:"name"`
	Path string `json:"path"` // absolute while discovering, store-relative in manifests
}

// StoreEntry is a materialised package tree in the store.
type StoreEntry struct {
	Path     string    // absolute store directory
	Bins     []BinLink // executables to expose
	Mans     []string  // absolute paths to manpages
	Size     int64
	Reused   bool // already present; extraction was skipped
	Platform Platform
}

// HaveStorePath reports whether a store path already holds a complete package.
// Completeness is marked by a .hop-complete sentinel written last, so an
// interrupted extraction is never mistaken for a finished one.
func HaveStorePath(dir string) bool {
	return exists(filepath.Join(dir, ".hop-complete"))
}

// Materialise extracts artifact into the store for the given recipe, or reuses
// an existing identical tree. The write is staged in a sibling temp directory
// and renamed into place, so the store only ever contains complete packages.
//
// deps carries the store location and version of every already-materialised
// dependency of r, keyed by formula/package name. It exists for one reason:
// a Homebrew bottle's binaries reference their runtime dependencies by an
// unresolved build-time placeholder (@@HOMEBREW_PREFIX@@/opt/<name>/...)
// that has to be rewritten to hop's own store path for that dependency
// before the binary will actually run — see relocateHomebrewBottle. Every
// other artifact kind ignores deps entirely; it costs them nothing since
// the relocation step itself no-ops the instant it finds no such
// placeholder in the extracted tree.
func Materialise(l *Layout, r *Recipe, a *Artifact, plat Platform, archivePath, contentHash string, deps map[string]depLocation) (*StoreEntry, error) {
	dir := l.StorePath(r.Name, r.Version, contentHash)
	isImage := r.Kind == KindImage

	// resolve fills in Bins/Mans for a bin-kind package; an image-kind package
	// carries neither, since it is never extracted and never touches PATH.
	resolve := func(e *StoreEntry) error {
		if isImage {
			return nil
		}
		return e.discover(a)
	}

	if HaveStorePath(dir) {
		e := &StoreEntry{Path: dir, Reused: true, Platform: plat}
		if err := resolve(e); err != nil {
			// A damaged reuse is worse than a slow re-extract: rebuild it.
			_ = removeTree(dir)
		} else {
			e.Size, _ = dirSize(dir)
			return e, nil
		}
	}

	// Stage into a temp sibling so a failure leaves no partial store path.
	if err := os.MkdirAll(l.Store(), 0o755); err != nil {
		return nil, err
	}
	stage, err := os.MkdirTemp(l.Store(), ".stage-"+r.Name+"-")
	if err != nil {
		return nil, fmt.Errorf("creating staging directory: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = removeTree(stage)
		}
	}()

	if isImage {
		// An OS/VM image or rootfs tarball is verified and content-addressed
		// like anything else hop installs, but it is never unpacked: the
		// whole point is to hand the exact bytes to qemu, docker import, or
		// whatever else expects them intact.
		if err := placeImageFile(archivePath, stage, a); err != nil {
			return nil, fmt.Errorf("storing %s: %w", r.Name, err)
		}
	} else if err := extract(archivePath, stage, DetectFormat(a), a.Strip, defaultRawName(r, a)); err != nil {
		return nil, fmt.Errorf("extracting %s: %w", r.Name, err)
	}

	if !isImage {
		// dir, not stage, is deliberate: a relocated binary's load commands
		// must name the path the tree will actually live at once renamed
		// into place below, not the temporary staging path it's sitting in
		// while being patched.
		if err := relocateHomebrewBottle(stage, r.Name, r.Version, dir, deps); err != nil {
			return nil, fmt.Errorf("relocating %s: %w", r.Name, err)
		}
	}

	e := &StoreEntry{Path: stage, Platform: plat}
	if err := resolve(e); err != nil {
		return nil, err
	}

	// Sentinel last: its presence means "this tree is whole".
	if err := os.WriteFile(filepath.Join(stage, ".hop-complete"), []byte(contentHash+"\n"), 0o644); err != nil {
		return nil, err
	}

	_ = removeTree(dir) // clear an interrupted prior attempt
	if err := os.Rename(stage, dir); err != nil {
		// A concurrent hop may have won the race; its result is equivalent.
		if HaveStorePath(dir) {
			committed = true
			e2 := &StoreEntry{Path: dir, Reused: true, Platform: plat}
			if err2 := resolve(e2); err2 == nil {
				e2.Size, _ = dirSize(dir)
				return e2, nil
			}
		}
		return nil, fmt.Errorf("committing %s to the store: %w", r.Name, err)
	}
	committed = true

	// Re-point discovered paths at the committed location.
	final := &StoreEntry{Path: dir, Platform: plat}
	if err := resolve(final); err != nil {
		return nil, err
	}
	final.Size, _ = dirSize(dir)
	return final, nil
}

// placeImageFile puts the verified download-cache file into the store as-is,
// named for the artifact's URL (query strings stripped). A hard link is
// tried first so a multi-hundred-megabyte image is never copied twice on the
// same filesystem; io.Copy is the portable fallback.
func placeImageFile(archivePath, stage string, a *Artifact) error {
	name := baseName(strings.SplitN(a.URL, "?", 2)[0])
	if name == "" {
		name = "image"
	}
	dst := filepath.Join(stage, name)

	if err := os.Link(archivePath, dst); err == nil {
		return nil
	}
	src, err := os.Open(archivePath)
	if err != nil {
		return err
	}
	defer src.Close()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o644)
	if err != nil {
		return err
	}
	defer out.Close()
	_, err = io.Copy(out, src)
	return err
}

// discover locates the executables and manpages named by the artifact. A
// recipe may name a path ("bin/rg") or a bare command ("rg"); bare names, and
// paths that upstream has since moved, are searched for across the tree.
func (e *StoreEntry) discover(a *Artifact) error {
	e.Bins, e.Mans = nil, nil

	if a.NoExecutables {
		return nil // a pure library, present only to satisfy a dependent's runtime linking
	}

	want := a.Bin
	if len(want) == 0 {
		// No declaration: adopt every executable in a conventional location.
		found, err := scanExecutables(e.Path)
		if err != nil {
			return err
		}
		if len(found) == 0 {
			return fmt.Errorf("no executable found in the extracted tree; the recipe needs an explicit \"bin\" list")
		}
		e.Bins = found
		return nil
	}

	var missing []string
	for _, spec := range want {
		name, rel := SplitBin(spec)
		p := filepath.Join(e.Path, filepath.Clean(rel))
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			_ = os.Chmod(p, 0o755)
			e.Bins = append(e.Bins, BinLink{Name: name, Path: p})
			continue
		}
		// Fall back to searching by file name, then by the exposed command
		// name: a single-file artifact is written to bin/<name>, so the
		// recipe's in-archive path never exists on disk.
		probes := []string{baseName(rel), name}
		// Homebrew renames a handful of GNU tools with a leading "g" (gsed,
		// ggrep, gfind, ...) specifically to avoid shadowing macOS's own
		// BSD-flavored versions — a rename its Linux bottles don't need or
		// make, since GNU tools are already the Linux default, and don't
		// perform: confirmed for real, findutils'/gnu-sed's/grep's own
		// Linux bottles ship plain find/sed/grep, not gfind/gsed/ggrep,
		// while Homebrew's bulk API's "executables" field (genbrew's only
		// source for Bin) reports the same g-prefixed names for every
		// platform regardless. The name actually exposed on PATH still
		// matches what was declared (BinLink below uses name, not probe) —
		// this only widens where hop looks for the underlying file, so a
		// Linux user gets the same predictable `gfind`/`gsed`/`ggrep`
		// hop already gives a macOS user, rather than silently shadowing
		// their system's own find/sed/grep under the bare name.
		if stripped, ok := strings.CutPrefix(name, "g"); ok && stripped != "" {
			probes = append(probes, stripped)
		}
		found := ""
		for _, probe := range probes {
			if hit, err := findByName(e.Path, probe); err == nil && hit != "" {
				found = hit
				break
			}
		}
		if found != "" {
			_ = os.Chmod(found, 0o755)
			e.Bins = append(e.Bins, BinLink{Name: name, Path: found})
			continue
		}
		missing = append(missing, rel)
	}
	if len(missing) > 0 && len(e.Bins) == 0 {
		// Every declared bin missing points at something fundamentally
		// wrong (a bad recipe, a corrupted download) — worth failing loudly.
		return fmt.Errorf("recipe declares %s but the archive does not contain %s",
			strings.Join(want, ", "), strings.Join(missing, ", "))
	}
	// A partial miss, by contrast, is tolerated: Homebrew's own formula
	// metadata declares one executable list across every platform a bottle
	// ships for, with no per-platform breakdown available at all — a
	// formula can legitimately omit an architecture-specific tool on one
	// platform (util-linux's x86 personality-switching aliases i386/
	// x86_64, which simply don't exist in its arm64 bottle, notably) while
	// still shipping the rest. Failing the whole install over one such gap
	// would be exactly the "layout differs, tolerate it" case Bin's own
	// doc comment already describes, just not honored until now.

	for _, rel := range a.Man {
		p := filepath.Join(e.Path, filepath.Clean(rel))
		if exists(p) {
			e.Mans = append(e.Mans, p)
		} else if hit, err := findByName(e.Path, baseName(rel)); err == nil && hit != "" {
			e.Mans = append(e.Mans, hit)
		}
	}
	return nil
}

// findByName locates a file by base name, preferring shallow matches and
// conventional bin/ directories.
func findByName(root, name string) (string, error) {
	var hits []string
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if isAppleDouble(p) {
			return nil
		}
		if d.Name() == name {
			hits = append(hits, p)
		}
		return nil
	})
	if err != nil || len(hits) == 0 {
		return "", err
	}
	sort.Slice(hits, func(i, j int) bool {
		di := strings.Count(hits[i], string(filepath.Separator))
		dj := strings.Count(hits[j], string(filepath.Separator))
		bi := strings.Contains(hits[i], string(filepath.Separator)+"bin"+string(filepath.Separator))
		bj := strings.Contains(hits[j], string(filepath.Separator)+"bin"+string(filepath.Separator))
		if bi != bj {
			return bi
		}
		if di != dj {
			return di < dj
		}
		return hits[i] < hits[j]
	})
	return hits[0], nil
}

// scanExecutables finds plausible commands when a recipe declares no bins.
func scanExecutables(root string) ([]BinLink, error) {
	var out []BinLink
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() || isAppleDouble(p) {
			return nil
		}
		fi, err := d.Info()
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		// An executable bit, or living in a bin/ directory, qualifies.
		inBin := filepath.Base(filepath.Dir(p)) == "bin"
		if fi.Mode()&0o111 != 0 || inBin {
			if looksLikeDoc(d.Name()) {
				return nil
			}
			_ = os.Chmod(p, 0o755)
			out = append(out, BinLink{Name: d.Name(), Path: p})
		}
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return out[i].Path < out[j].Path })
	return out, err
}

func looksLikeDoc(name string) bool {
	l := strings.ToLower(name)
	for _, s := range []string{".md", ".txt", ".1", ".json", ".toml", ".yml", ".yaml", ".ps1", ".bash", ".zsh", ".fish", ".complete"} {
		if strings.HasSuffix(l, s) {
			return true
		}
	}
	switch l {
	case "readme", "license", "licence", "changelog", "copying", "authors", "notice":
		return true
	}
	return false
}

// defaultRawName picks the filename for a single-file artifact.
func defaultRawName(r *Recipe, a *Artifact) string {
	if len(a.Bin) > 0 {
		name, _ := SplitBin(a.Bin[0])
		return name
	}
	return r.Name
}

// ---------------------------------------------------------------- extract ----

// extract unpacks archive into dest. rawName is used for single-file formats.
func extract(archive, dest string, f Format, strip int, rawName string) error {
	switch f {
	case FormatTarGz:
		return withFile(archive, func(r io.Reader) error {
			zr, err := gzip.NewReader(r)
			if err != nil {
				return fmt.Errorf("not a gzip stream: %w", err)
			}
			defer zr.Close()
			return untar(zr, dest, strip)
		})
	case FormatTarBz2:
		return withFile(archive, func(r io.Reader) error {
			return untar(bzip2.NewReader(r), dest, strip)
		})
	case FormatTar:
		return withFile(archive, func(r io.Reader) error { return untar(r, dest, strip) })
	case FormatTarXz:
		return untarXz(archive, dest, strip)
	case FormatZip:
		return unzip(archive, dest, strip)
	case FormatGz:
		return withFile(archive, func(r io.Reader) error {
			zr, err := gzip.NewReader(r)
			if err != nil {
				return fmt.Errorf("not a gzip stream: %w", err)
			}
			defer zr.Close()
			return writeSingle(zr, dest, rawName)
		})
	case FormatRaw:
		return withFile(archive, func(r io.Reader) error { return writeSingle(r, dest, rawName) })
	default:
		return fmt.Errorf("unsupported artifact format %q", f)
	}
}

func withFile(path string, fn func(io.Reader) error) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return fn(f)
}

// writeSingle materialises a single-file artifact as an executable.
func writeSingle(r io.Reader, dest, name string) error {
	binDir := filepath.Join(dest, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return err
	}
	if name == "" {
		name = "program"
	}
	out := filepath.Join(binDir, baseName(name))
	f, err := os.OpenFile(out, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return err
	}
	defer f.Close()
	if _, err := io.Copy(f, io.LimitReader(r, maxArchiveBytes)); err != nil {
		return err
	}
	return nil
}

// stripComponents removes the first n path segments, returning "" if the
// member lies entirely within the stripped prefix.
func stripComponents(name string, n int) string {
	if n <= 0 {
		return name
	}
	name = strings.TrimPrefix(filepath.ToSlash(name), "./")
	parts := strings.Split(name, "/")
	if len(parts) <= n {
		return ""
	}
	return strings.Join(parts[n:], "/")
}

// homebrewSiblingSymlinkPattern matches a relative symlink target that
// climbs out of a formula's own version directory to reach Homebrew's
// shared "opt/<formula>" symlink-farm entry, or another formula's Cellar
// entry directly — the one recognized shape of "escapes dest" symlink that
// untar's path-traversal guard below lets through rather than silently
// dropping. It's declared here rather than alongside the platform-specific
// relocation code in relocate_darwin.go/relocate_linux.go because untar
// itself has no build tag and needs it on every platform, including the
// relocate_other.go no-op stub's.
var homebrewSiblingSymlinkPattern = regexp.MustCompile(`^(?:\.\./)+(?:opt|Cellar)/[^/]+/`)

// untar extracts a tar stream. It rejects path traversal, skips device nodes
// and AppleDouble sidecars, and defers symlinks until all regular files exist
// so link targets resolve regardless of member order.
func untar(r io.Reader, dest string, strip int) error {
	tr := tar.NewReader(r)
	var total int64
	var entries int

	type pending struct {
		path, name, target string
		hard               bool
	}
	var links []pending

	for {
		h, err := tr.Next()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return fmt.Errorf("reading tar: %w", err)
		}
		if entries++; entries > maxArchiveEntries {
			return fmt.Errorf("archive has more than %d entries; refusing to extract", maxArchiveEntries)
		}

		name := stripComponents(h.Name, strip)
		if name == "" || name == "." || isAppleDouble(name) || strings.Contains(name, "/._") {
			continue
		}
		path, err := safeJoin(dest, name)
		if err != nil {
			return err
		}

		switch h.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
				return err
			}
			total += h.Size
			if total > maxArchiveBytes {
				return fmt.Errorf("archive expands beyond %s; refusing to extract", humanBytes(maxArchiveBytes))
			}
			mode := os.FileMode(h.Mode).Perm()
			if mode == 0 {
				mode = 0o644
			}
			f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
			if err != nil {
				return err
			}
			if _, err := io.Copy(f, io.LimitReader(tr, maxArchiveBytes)); err != nil {
				f.Close()
				return err
			}
			if err := f.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			links = append(links, pending{path, name, h.Linkname, false})
		case tar.TypeLink:
			links = append(links, pending{path, name, h.Linkname, true})
		default:
			// Skip FIFOs, char/block devices: no legitimate CLI tarball needs them.
			continue
		}
	}

	for _, ln := range links {
		if err := os.MkdirAll(filepath.Dir(ln.path), 0o755); err != nil {
			return err
		}
		_ = os.Remove(ln.path)
		if ln.hard {
			src, err := safeJoin(dest, stripComponents(ln.target, strip))
			if err != nil {
				continue
			}
			if err := os.Link(src, ln.path); err != nil {
				// Fall back to a symlink; some filesystems refuse hard links.
				_ = os.Symlink(src, ln.path)
			}
			continue
		}
		// A relative symlink is fine; an absolute one would escape the store.
		if filepath.IsAbs(ln.target) {
			continue
		}
		// The target is resolved against dest — the overall extraction
		// root — not against the symlink's own containing directory.
		// Climbing "../" out of a symlink's own directory into a sibling
		// that's still inside dest (Python's own "bin/python3.14 ->
		// ../Frameworks/.../bin/python3.14", notably) is completely normal
		// and must not be rejected; only a target that escapes dest itself
		// is unsafe. Resolving against the symlink's own directory instead
		// (as opposed to dest) would flag that entirely ordinary case as an
		// escape merely for climbing out of its own immediate directory.
		resolvedRel := filepath.Join(filepath.Dir(ln.name), ln.target)
		if _, err := safeJoin(dest, resolvedRel); err != nil {
			if !homebrewSiblingSymlinkPattern.MatchString(ln.target) {
				continue // link points outside the store and isn't a recognized, relocatable reference; drop it
			}
			// A Homebrew bottle's own bundled-venv interpreter symlink
			// (yt-dlp's libexec/bin/python3.14, notably) legitimately
			// climbs out to "../../../opt/<formula>/..." — Homebrew's
			// shared symlink farm, which doesn't exist once this formula
			// is extracted alone into its own store directory. It escapes
			// dest exactly like a malicious tar-slip target would, but
			// relocateHomebrewBottle runs immediately after extraction for
			// every non-image artifact and rewrites this exact pattern
			// into a safe absolute path — so it's let through here on
			// purpose, unlike any other target this check would still drop.
		}
		_ = os.Symlink(ln.target, ln.path)
	}
	return nil
}

// untarXz shells out, because the standard library has no xz decoder and
// vendoring one would cost hop its zero-dependency guarantee.
func untarXz(archive, dest string, strip int) error {
	if xz, err := exec.LookPath("xz"); err == nil {
		cmd := exec.Command(xz, "-dc", "--", archive)
		out, err := cmd.StdoutPipe()
		if err != nil {
			return err
		}
		if err := cmd.Start(); err != nil {
			return err
		}
		uerr := untar(out, dest, strip)
		_ = out.Close()
		werr := cmd.Wait()
		if uerr != nil {
			return uerr
		}
		return werr
	}
	// BSD/GNU tar both decompress xz transparently.
	if tarBin, err := exec.LookPath("tar"); err == nil {
		args := []string{"-xf", archive, "-C", dest}
		if strip > 0 {
			args = append(args, fmt.Sprintf("--strip-components=%d", strip))
		}
		cmd := exec.Command(tarBin, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("tar failed to extract the xz archive: %s", strings.TrimSpace(string(out)))
		}
		return nil
	}
	return errors.New("this artifact is xz-compressed and neither `xz` nor `tar` is available")
}

// unzip extracts a zip archive with the same safety rules as untar.
func unzip(archive, dest string, strip int) error {
	zr, err := zip.OpenReader(archive)
	if err != nil {
		return fmt.Errorf("not a zip archive: %w", err)
	}
	defer zr.Close()

	if len(zr.File) > maxArchiveEntries {
		return fmt.Errorf("archive has more than %d entries; refusing to extract", maxArchiveEntries)
	}

	var total int64
	for _, f := range zr.File {
		name := stripComponents(f.Name, strip)
		if name == "" || name == "." || isAppleDouble(name) || strings.Contains(name, "/._") ||
			strings.HasPrefix(name, "__MACOSX/") {
			continue
		}
		path, err := safeJoin(dest, name)
		if err != nil {
			return err
		}
		if f.FileInfo().IsDir() {
			if err := os.MkdirAll(path, 0o755); err != nil {
				return err
			}
			continue
		}
		if f.Mode()&os.ModeSymlink != 0 {
			rc, err := f.Open()
			if err != nil {
				return err
			}
			target, err := io.ReadAll(io.LimitReader(rc, 4096))
			rc.Close()
			if err != nil || filepath.IsAbs(string(target)) {
				continue
			}
			// Resolved against dest, not against the symlink's own
			// directory — see the matching comment in untar for why:
			// climbing "../" into a sibling that's still inside dest is
			// normal and must not be rejected.
			if _, err := safeJoin(dest, filepath.Join(filepath.Dir(name), string(target))); err != nil {
				continue
			}
			_ = os.MkdirAll(filepath.Dir(path), 0o755)
			_ = os.Symlink(string(target), path)
			continue
		}

		total += int64(f.UncompressedSize64)
		if total > maxArchiveBytes {
			return fmt.Errorf("archive expands beyond %s; refusing to extract", humanBytes(maxArchiveBytes))
		}
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return err
		}
		mode := f.Mode().Perm()
		if mode == 0 {
			mode = 0o644
		}
		rc, err := f.Open()
		if err != nil {
			return err
		}
		out, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
		if err != nil {
			rc.Close()
			return err
		}
		_, cerr := io.Copy(out, io.LimitReader(rc, maxArchiveBytes))
		rc.Close()
		if err := out.Close(); err != nil {
			return err
		}
		if cerr != nil {
			return cerr
		}
	}
	return nil
}

// humanBytes is a local copy so core does not depend on the ui package.
func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	f := float64(n)
	for _, u := range []string{"KB", "MB", "GB", "TB"} {
		f /= unit
		if f < unit {
			return fmt.Sprintf("%.1f %s", f, u)
		}
	}
	return fmt.Sprintf("%.1f PB", f/unit)
}
