// Command genimages adds OS/VM image recipes (KindImage) to hop's built-in
// index: cloud disk images and container rootfs tarballs that hop can pull,
// verify and keep current, without ever extracting them or touching PATH.
//
// Unlike genindex, these don't come from GitHub Releases — each distro
// publishes its own checksum manifest, in its own format, at a stable
// "latest" URL. genimages resolves the current version and digest straight
// from that manifest (the same official infrastructure `apt`/`cloud-init`
// trust), then HEAD-requests the artifact to record its size. Nothing here
// is guessed, same discipline as genindex: every digest traces back to bytes
// hop's own request actually observed from the manifest.
//
//	go run ./tools/genimages
package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"sort"
	"strings"
	"time"
)

// artifact/recipe/index mirror internal/core's JSON shape. Duplicated rather
// than imported so this generator can never be broken by an engine refactor.
type artifact struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256,omitempty"`
	SHA512 string `json:"sha512,omitempty"`
	Size   int64  `json:"size,omitempty"`
	Format string `json:"format,omitempty"`
}

type recipe struct {
	Name        string               `json:"name"`
	Version     string               `json:"version"`
	Description string               `json:"description,omitempty"`
	Homepage    string               `json:"homepage,omitempty"`
	License     string               `json:"license,omitempty"`
	Keywords    []string             `json:"keywords,omitempty"`
	Kind        string               `json:"kind,omitempty"`
	Artifacts   map[string]*artifact `json:"artifacts"`
	Caveats     string               `json:"caveats,omitempty"`
}

type index struct {
	Schema    int       `json:"schema"`
	Source    string    `json:"source"`
	Generated time.Time `json:"generated"`
	Recipes   []*recipe `json:"recipes"`
}

// imageArch describes one architecture's build of an image. An OS image is
// not built per *host* platform the way a CLI tool is — an arm64 Ubuntu
// image runs in a VM regardless of whether hop itself is running on darwin
// or linux — so each arch is fanned out to every host-platform key that
// shares it (darwin-arm64 and linux-arm64 both get the arm64 build).
type imageArch struct {
	arch   string // "amd64" or "arm64"
	url    string
	sha256 string
	sha512 string
}

var hostOSes = []string{"darwin", "linux"}

func main() {
	client := &http.Client{Timeout: 60 * time.Second}

	specs := []struct {
		name string
		fn   func(*http.Client) (*recipe, error)
	}{
		{"ubuntu-cloud", ubuntuCloud},
		{"debian-cloud", debianCloud},
		{"alpine-minirootfs", alpineMinirootfs},
		{"freebsd-vm", freebsdVM},
		{"raspios-lite", raspiosLite},
	}

	var built []*recipe
	for _, s := range specs {
		r, err := s.fn(client)
		if err != nil {
			warn("%-20s skipped: %v", s.name, err)
			continue
		}
		logf("%-20s %-10s %d %s", r.Name, r.Version, len(r.Artifacts), plural(len(r.Artifacts), "artifact", "artifacts"))
		built = append(built, r)
	}
	if len(built) == 0 {
		die("nothing resolved; nothing written")
	}

	const outPath = "internal/core/data/index.json"
	b, err := os.ReadFile(outPath)
	if err != nil {
		die("reading %s: %v", outPath, err)
	}
	var ix index
	if err := json.Unmarshal(b, &ix); err != nil {
		die("parsing %s: %v", outPath, err)
	}

	have := map[string]bool{}
	for _, r := range built {
		have[r.Name] = true
	}
	kept := ix.Recipes[:0]
	for _, r := range ix.Recipes {
		if !have[r.Name] {
			kept = append(kept, r)
		}
	}
	ix.Recipes = append(kept, built...)
	sort.Slice(ix.Recipes, func(i, j int) bool { return ix.Recipes[i].Name < ix.Recipes[j].Name })
	ix.Generated = time.Now().UTC().Truncate(time.Second)

	out, err := json.MarshalIndent(&ix, "", "  ")
	if err != nil {
		die("encoding index: %v", err)
	}
	if err := os.WriteFile(outPath, append(out, '\n'), 0o644); err != nil {
		die("writing %s: %v", outPath, err)
	}

	arts := 0
	for _, r := range ix.Recipes {
		arts += len(r.Artifacts)
	}
	logf("")
	logf("wrote %s: %d recipes, %d artifacts (%d images added)", outPath, len(ix.Recipes), arts, len(built))
}

// fanOut expands per-arch builds into the full set of host-platform keys.
func fanOut(archs []imageArch, format string) map[string]*artifact {
	out := map[string]*artifact{}
	for _, a := range archs {
		art := &artifact{URL: a.url, SHA256: a.sha256, SHA512: a.sha512, Format: format}
		for _, osName := range hostOSes {
			out[osName+"-"+a.arch] = art
		}
	}
	return out
}

// setSizes HEAD-requests every distinct URL to record Content-Length.
func setSizes(client *http.Client, arts map[string]*artifact) error {
	seen := map[string]int64{}
	for _, a := range arts {
		if sz, ok := seen[a.URL]; ok {
			a.Size = sz
			continue
		}
		req, err := http.NewRequest(http.MethodHead, a.URL, nil)
		if err != nil {
			return err
		}
		req.Header.Set("User-Agent", "hop-genimages")
		resp, err := client.Do(req)
		if err != nil {
			return fmt.Errorf("HEAD %s: %w", a.URL, err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("HEAD %s: %s", a.URL, resp.Status)
		}
		a.Size = resp.ContentLength
		seen[a.URL] = resp.ContentLength
	}
	return nil
}

// ------------------------------------------------------------------ ubuntu ----

// ubuntuCloud resolves the current Ubuntu 24.04 LTS server cloud image from
// Canonical's own SHA256SUMS manifest.
func ubuntuCloud(client *http.Client) (*recipe, error) {
	const base = "https://cloud-images.ubuntu.com/releases/noble/release/"
	sums, err := fetchText(client, base+"SHA256SUMS")
	if err != nil {
		return nil, err
	}

	var archs []imageArch
	for _, arch := range []string{"amd64", "arm64"} {
		file := fmt.Sprintf("ubuntu-24.04-server-cloudimg-%s.img", arch)
		hash, ok := findSum(sums, file)
		if !ok {
			return nil, fmt.Errorf("manifest has no entry for %s", file)
		}
		archs = append(archs, imageArch{arch: arch, url: base + file, sha256: hash})
	}

	arts := fanOut(archs, "raw")
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "ubuntu-cloud", Version: "24.04", Kind: "image",
		Description: "Ubuntu 24.04 LTS server cloud image (qcow2), for qemu, cloud-init and VM provisioning",
		Homepage:    "https://cloud-images.ubuntu.com",
		License:     "GPL-2.0 and others (Ubuntu system license)",
		Keywords:    []string{"vm", "cloud", "qemu", "image", "ubuntu"},
		Artifacts:   arts,
		Caveats: "This is a disk image, not a command. Find it with:\n\n" +
			"    hop info ubuntu-cloud\n\n" +
			"Boot it with qemu, or import it into your hypervisor of choice.",
	}, nil
}

// ------------------------------------------------------------------ debian ----

// debianCloud resolves the current Debian 12 (bookworm) generic cloud image.
// Debian's own manifest is SHA-512 only, which is exactly what hop's SHA512
// artifact field exists for.
func debianCloud(client *http.Client) (*recipe, error) {
	const base = "https://cloud.debian.org/images/cloud/bookworm/latest/"
	sums, err := fetchText(client, base+"SHA512SUMS")
	if err != nil {
		return nil, err
	}

	var archs []imageArch
	for _, arch := range []struct{ hop, debian string }{{"amd64", "amd64"}, {"arm64", "arm64"}} {
		file := fmt.Sprintf("debian-12-generic-%s.qcow2", arch.debian)
		hash, ok := findSum(sums, file)
		if !ok {
			return nil, fmt.Errorf("manifest has no entry for %s", file)
		}
		archs = append(archs, imageArch{arch: arch.hop, url: base + file, sha512: hash})
	}

	arts := fanOut(archs, "raw")
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "debian-cloud", Version: "12", Kind: "image",
		Description: "Debian 12 (bookworm) generic cloud image (qcow2), for qemu, cloud-init and VM provisioning",
		Homepage:    "https://cloud.debian.org/images/cloud/",
		License:     "DFSG (Debian system license)",
		Keywords:    []string{"vm", "cloud", "qemu", "image", "debian"},
		Artifacts:   arts,
		Caveats: "This is a disk image, not a command. Find it with:\n\n" +
			"    hop info debian-cloud\n\n" +
			"Boot it with qemu, or import it into your hypervisor of choice.",
	}, nil
}

// -------------------------------------------------------------- alpine ----

// alpineMinirootfs resolves the current Alpine 3.20 minirootfs — the
// container base image most `FROM alpine` Dockerfiles ultimately trace to —
// straight from Alpine's own per-file .sha256 sidecars.
func alpineMinirootfs(client *http.Client) (*recipe, error) {
	var archs []imageArch
	for _, a := range []struct{ hop, alpine string }{{"amd64", "x86_64"}, {"arm64", "aarch64"}} {
		dir := fmt.Sprintf("https://dl-cdn.alpinelinux.org/alpine/v3.20/releases/%s/", a.alpine)
		yaml, err := fetchText(client, dir+"latest-releases.yaml")
		if err != nil {
			return nil, err
		}
		file, ok := findYAMLField(yaml, "alpine-minirootfs", "file")
		if !ok {
			return nil, fmt.Errorf("no minirootfs entry in %s's latest-releases.yaml", a.alpine)
		}
		// Alpine's manifest carries the digest inline, wrapped across lines
		// with a trailing backslash; strip that before using it.
		hash, ok := findYAMLField(yaml, "alpine-minirootfs", "sha256")
		if !ok || hash == "" {
			return nil, fmt.Errorf("no minirootfs sha256 in %s's latest-releases.yaml", a.alpine)
		}
		archs = append(archs, imageArch{arch: a.hop, url: dir + file, sha256: hash})
	}

	arts := fanOut(archs, "tar.gz")
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	ver := "3.20"
	return &recipe{
		Name: "alpine-minirootfs", Version: ver, Kind: "image",
		Description: "Alpine Linux minimal root filesystem (musl, ~3MB), for containers and chroots",
		Homepage:    "https://alpinelinux.org",
		License:     "MIT (Alpine base packages, mixed)",
		Keywords:    []string{"container", "rootfs", "alpine", "musl", "image"},
		Artifacts:   arts,
		Caveats: "This is a root filesystem tarball, not a command. Find it with:\n\n" +
			"    hop info alpine-minirootfs\n\n" +
			"Extract it into a chroot, or `docker import` it as a base image.",
	}, nil
}

// ------------------------------------------------------------------ freebsd ----

// freebsdVM resolves the current FreeBSD RELEASE's plain VM image — a
// non-Linux, non-cloud-init general-purpose bootable disk image, from
// FreeBSD's own CHECKSUM.SHA256 for each architecture.
func freebsdVM(client *http.Client) (*recipe, error) {
	version, err := latestFreeBSDRelease(client)
	if err != nil {
		return nil, err
	}

	var archs []imageArch
	for _, a := range []struct{ hop, freebsd string }{{"amd64", "amd64"}, {"arm64", "aarch64"}} {
		dir := fmt.Sprintf("https://download.freebsd.org/releases/VM-IMAGES/%s/%s/Latest/", version, a.freebsd)
		sums, err := fetchText(client, dir+"CHECKSUM.SHA256")
		if err != nil {
			return nil, err
		}
		file := fmt.Sprintf("FreeBSD-%s-%s-ufs.qcow2.xz", version, a.freebsd)
		if a.hop == "arm64" {
			file = fmt.Sprintf("FreeBSD-%s-arm64-aarch64-ufs.qcow2.xz", version)
		}
		hash, ok := findBSDSum(sums, file)
		if !ok {
			return nil, fmt.Errorf("manifest has no entry for %s", file)
		}
		archs = append(archs, imageArch{arch: a.hop, url: dir + file, sha256: hash})
	}

	arts := fanOut(archs, "xz")
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "freebsd-vm", Version: version, Kind: "image",
		Description: "FreeBSD " + version + " general-purpose VM image (qcow2, xz-compressed)",
		Homepage:    "https://www.freebsd.org",
		License:     "BSD-2-Clause (FreeBSD base system)",
		Keywords:    []string{"vm", "bsd", "freebsd", "qemu", "image"},
		Artifacts:   arts,
		Caveats: "This is a disk image, not a command. Find it with:\n\n" +
			"    hop info freebsd-vm\n\n" +
			"It's xz-compressed: decompress before booting —\n" +
			"    xz -d <image path>\n\n" +
			"Then boot it with qemu, or import it into your hypervisor of choice.",
	}, nil
}

// latestFreeBSDRelease finds the newest non-beta, non-RC entry under
// VM-IMAGES/. FreeBSD's directory listing has no "latest" alias, unlike
// Ubuntu and Debian, so this reads the index and picks the newest RELEASE.
func latestFreeBSDRelease(client *http.Client) (string, error) {
	body, err := fetchText(client, "https://download.freebsd.org/releases/VM-IMAGES/")
	if err != nil {
		return "", err
	}
	var best string
	for _, line := range strings.Split(body, "\n") {
		i := strings.Index(line, `href="`)
		if i < 0 {
			continue
		}
		rest := line[i+len(`href="`):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			continue
		}
		name := strings.TrimSuffix(rest[:j], "/")
		if !strings.HasSuffix(name, "-RELEASE") {
			continue // skip BETA/RC/ALPHA snapshots
		}
		if best == "" || compareFreeBSDVersion(name, best) > 0 {
			best = name
		}
	}
	if best == "" {
		return "", fmt.Errorf("no *-RELEASE directory found")
	}
	return best, nil
}

// compareFreeBSDVersion compares "15.1-RELEASE"-style names numerically by
// their leading major.minor, falling back to a string compare on ties.
func compareFreeBSDVersion(a, b string) int {
	an, bn := strings.TrimSuffix(a, "-RELEASE"), strings.TrimSuffix(b, "-RELEASE")
	af := strings.SplitN(an, ".", 2)
	bf := strings.SplitN(bn, ".", 2)
	if len(af) > 0 && len(bf) > 0 && af[0] != bf[0] {
		if len(af[0]) != len(bf[0]) {
			return len(af[0]) - len(bf[0]) // "15" > "9" as numbers, not strings
		}
		return strings.Compare(af[0], bf[0])
	}
	return strings.Compare(an, bn)
}

// -------------------------------------------------------------- raspberry pi ----

// raspiosLite resolves the current Raspberry Pi OS Lite (arm64) image — a
// real, non-cloud, flash-to-SD-card OS with no x86_64 build at all, since it
// targets Raspberry Pi hardware exclusively. hop reports that honestly:
// installing it on an amd64 host correctly fails with "no artifact for this
// platform" rather than pretending an x86 build exists.
func raspiosLite(client *http.Client) (*recipe, error) {
	const listURL = "https://downloads.raspberrypi.com/raspios_lite_arm64/images/"
	listing, err := fetchText(client, listURL)
	if err != nil {
		return nil, err
	}
	dirName, ok := latestHrefDir(listing, "raspios_lite_arm64-")
	if !ok {
		return nil, fmt.Errorf("no dated release directory found")
	}
	dir := listURL + dirName + "/"

	inner, err := fetchText(client, dir)
	if err != nil {
		return nil, err
	}
	file, ok := latestHrefFile(inner, ".img.xz")
	if !ok {
		return nil, fmt.Errorf("no .img.xz in %s", dir)
	}
	sumText, err := fetchText(client, dir+file+".sha256")
	if err != nil {
		return nil, err
	}
	fields := strings.Fields(sumText)
	if len(fields) == 0 {
		return nil, fmt.Errorf("empty checksum for %s", file)
	}

	// Version is the date embedded in the filename, e.g. 2026-06-18.
	version := file
	if len(file) >= 10 {
		version = file[:10]
	}

	arts := map[string]*artifact{
		"darwin-arm64": {URL: dir + file, SHA256: fields[0], Format: "xz"},
		"linux-arm64":  {URL: dir + file, SHA256: fields[0], Format: "xz"},
	}
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "raspios-lite", Version: version, Kind: "image",
		Description: "Raspberry Pi OS Lite (arm64), for flashing to an SD card — no desktop environment",
		Homepage:    "https://www.raspberrypi.com/software/",
		License:     "GPL and others (Debian-derived base system)",
		Keywords:    []string{"raspberry-pi", "sbc", "arm", "image"},
		Artifacts:   arts,
		Caveats: "This is a disk image, not a command. Find it with:\n\n" +
			"    hop info raspios-lite\n\n" +
			"It's xz-compressed: decompress before flashing —\n" +
			"    xz -d <image path>\n\n" +
			"Then write it to an SD card with `rpi-imager` or `dd`.\n\n" +
			"Raspberry Pi hardware is arm64-only, so hop has no build for amd64 hosts to boot directly.",
	}, nil
}

// latestHrefDir returns the lexicographically last href in an Apache-style
// listing whose name starts with prefix and ends in "/" — Raspberry Pi's
// directories are date-stamped, so the last one sorted is the newest.
func latestHrefDir(listing, prefix string) (string, bool) {
	var best string
	for _, name := range hrefNames(listing) {
		name = strings.TrimSuffix(name, "/")
		if strings.HasPrefix(name, prefix) && name > best {
			best = name
		}
	}
	return best, best != ""
}

// latestHrefFile returns the href ending in suffix, for a directory that
// holds exactly one image (Raspberry Pi's dated folders do).
func latestHrefFile(listing, suffix string) (string, bool) {
	for _, name := range hrefNames(listing) {
		if strings.HasSuffix(name, suffix) {
			return name, true
		}
	}
	return "", false
}

func hrefNames(listing string) []string {
	var out []string
	for _, line := range strings.Split(listing, "\n") {
		i := strings.Index(line, `href="`)
		if i < 0 {
			continue
		}
		rest := line[i+len(`href="`):]
		j := strings.Index(rest, `"`)
		if j < 0 {
			continue
		}
		name := rest[:j]
		if name == "" || strings.HasPrefix(name, "?") || strings.HasPrefix(name, "/") {
			continue
		}
		out = append(out, name)
	}
	return out
}

// ---------------------------------------------------------------- helpers ----

func fetchText(client *http.Client, url string) (string, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", "hop-genimages")
	resp, err := client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	buf := make([]byte, 0, 64<<10)
	tmp := make([]byte, 32<<10)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
		if len(buf) > 4<<20 { // manifests are tiny; refuse anything absurd
			return "", fmt.Errorf("manifest at %s is implausibly large", url)
		}
	}
	return string(buf), nil
}

// findSum parses a "<hex>  <filename>" or "<hex> *<filename>" checksum
// manifest line for the named file.
func findSum(manifest, file string) (string, bool) {
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		name := strings.TrimPrefix(fields[1], "*")
		if name == file {
			return fields[0], true
		}
	}
	return "", false
}

// findBSDSum parses the BSD-style checksum line format FreeBSD publishes —
// "SHA256 (filename) = hexdigest" — which is nothing like the GNU
// coreutils "hexdigest  filename" format findSum handles.
func findBSDSum(manifest, file string) (string, bool) {
	want := "(" + file + ") ="
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		i := strings.Index(line, want)
		if i < 0 {
			continue
		}
		return strings.TrimSpace(line[i+len(want):]), true
	}
	return "", false
}

// findYAMLField does just enough YAML reading for Alpine's flat
// latest-releases.yaml: a top-level sequence of mappings, each block
// starting with a bare "-" line. It finds the block whose "flavor:" is
// wantFlavor and returns one of its scalar fields. A value wrapped across
// lines with a trailing backslash (as Alpine's long digests are) is
// rejoined. A full YAML parser would be a dependency hop otherwise has no
// use for.
func findYAMLField(doc, wantFlavor, field string) (string, bool) {
	for _, block := range strings.Split("\n"+doc, "\n-\n") {
		if !strings.Contains(block, "flavor: "+wantFlavor+"\n") {
			continue
		}
		lines := strings.Split(block, "\n")
		for i := 0; i < len(lines); i++ {
			line := strings.TrimSpace(lines[i])
			if !strings.HasPrefix(line, field+":") {
				continue
			}
			val := strings.TrimSpace(strings.TrimPrefix(line, field+":"))
			for strings.HasSuffix(val, `\`) && i+1 < len(lines) {
				i++
				val = strings.TrimSuffix(val, `\`) + strings.TrimSpace(lines[i])
			}
			return strings.Trim(val, `"`), true
		}
	}
	return "", false
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
