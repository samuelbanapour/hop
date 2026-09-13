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
	"bytes"
	"compress/gzip"
	"encoding/json"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// artifact/recipe/index mirror internal/core's JSON shape exactly, field for
// field — every field any of the three generators (genindex, genbrew,
// genimages) can set, even ones this particular generator never populates
// itself. Each generator's own run reads the *entire* existing index.json,
// decodes it into its own local structs, and writes the whole thing back
// out; a field missing from one generator's struct is silently dropped from
// every other generator's recipes the moment this one runs. This happened
// for real: an earlier version of this struct without OCITokenURL, Bin,
// NoExecutables, Deps or Aliases stripped all of those from every Homebrew
// formula the first time this generator ran after genbrew added them. Keep
// this struct pair a complete superset in lockstep with
// internal/core/index.go's Artifact and Recipe types, not just the subset
// this generator happens to write.
type artifact struct {
	URL           string   `json:"url"`
	SHA256        string   `json:"sha256,omitempty"`
	SHA512        string   `json:"sha512,omitempty"`
	SHA1          string   `json:"sha1,omitempty"`
	Size          int64    `json:"size,omitempty"`
	OCITokenURL   string   `json:"oci_token_url,omitempty"`
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
		{"fedora-workstation", fedoraWorkstation},
		{"archlinux-iso", archlinuxISO},
		{"macos-recovery", macosRecovery},
		{"openbsd-vm", openbsdVM},
		{"netbsd-iso", netbsdISO},
		{"opensuse-tumbleweed", openSUSETumbleweed},
		{"rocky-linux", rockyLinux},
		{"void-linux", voidLinux},
		{"kali-linux", kaliLinux},
		{"parrot-security", parrotSecurity},
		{"proxmox-ve", proxmoxVE},
		{"tails", tails},
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

	// macOS installers are the one source that legitimately yields several
	// recipes (one per version reachable at all) rather than exactly one,
	// so it runs outside the single-recipe specs loop above.
	installers, err := macosInstallers(client)
	if err != nil {
		warn("macos installers: %v", err)
	}
	for _, r := range installers {
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
		hash, ok := findSumLen(sums, file, 128) // Debian's own manifest is SHA-512, not SHA-256
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

// ------------------------------------------------------------------ openbsd ----

// openbsdVM resolves the current OpenBSD release's install ISO — a second,
// genuinely distinct BSD alongside FreeBSD, from OpenBSD's own
// BSD-style CHECKSUM manifest (the "SHA256 (file) = digest" format,
// findBSDSum already handles). OpenBSD has no "latest" alias either, so
// this reuses the same newest-numbered-directory approach as FreeBSD.
func openbsdVM(client *http.Client) (*recipe, error) {
	listing, err := fetchText(client, "https://cdn.openbsd.org/pub/OpenBSD/")
	if err != nil {
		return nil, err
	}
	version, ok := latestOpenBSDVersion(listing)
	if !ok {
		return nil, fmt.Errorf("no version directory found")
	}
	short := strings.ReplaceAll(version, ".", "") // "7.9" -> "79", matching installNN.iso

	var archs []imageArch
	for _, a := range []struct{ hop, openbsd string }{{"amd64", "amd64"}, {"arm64", "arm64"}} {
		dir := fmt.Sprintf("https://cdn.openbsd.org/pub/OpenBSD/%s/%s/", version, a.openbsd)
		sums, err := fetchText(client, dir+"SHA256")
		if err != nil {
			return nil, err
		}
		file := fmt.Sprintf("install%s.iso", short)
		hash, ok := findBSDSum(sums, file)
		if !ok {
			return nil, fmt.Errorf("manifest has no entry for %s", file)
		}
		archs = append(archs, imageArch{arch: a.hop, url: dir + file, sha256: hash})
	}

	arts := fanOut(archs, "raw")
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "openbsd-vm", Version: version, Kind: "image",
		Description: "OpenBSD " + version + " install ISO — a second, independent BSD",
		Homepage:    "https://www.openbsd.org",
		License:     "ISC and BSD-2-Clause (OpenBSD base system)",
		Keywords:    []string{"vm", "bsd", "openbsd", "iso", "image"},
		Artifacts:   arts,
		Caveats: "This is an install ISO, not a command. Find it with:\n\n" +
			"    hop info openbsd-vm\n\n" +
			"Boot it with qemu, write it to a USB drive, or mount it in a hypervisor.",
	}, nil
}

// latestOpenBSDVersion picks the newest "N.N/" directory from OpenBSD's
// release index — the same pattern FreeBSD's own listing needs, since
// neither publishes a "latest" alias.
func latestOpenBSDVersion(listing string) (string, bool) {
	best := ""
	for _, name := range hrefNames(listing) {
		name = strings.TrimSuffix(name, "/")
		if !isDottedVersion(name) {
			continue
		}
		if best == "" || compareFreeBSDVersion(name+"-RELEASE", best+"-RELEASE") > 0 {
			best = name
		}
	}
	return best, best != ""
}

// isDottedVersion reports whether s looks like "7.9" — digits, one dot,
// digits, nothing else — filtering out the non-version entries (README,
// "snapshots/", etc.) mixed into the same directory listing.
func isDottedVersion(s string) bool {
	parts := strings.SplitN(s, ".", 2)
	if len(parts) != 2 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
		for _, r := range p {
			if r < '0' || r > '9' {
				return false
			}
		}
	}
	return true
}

// ------------------------------------------------------------------ netbsd ----

// netbsdISO resolves the current NetBSD release's install ISO for amd64 and
// its arm64 counterpart (NetBSD calls it "evbarm-aarch64" — a different
// board/port naming convention than every other distro in this index, kept
// exactly as NetBSD spells it since that's what the real URL needs).
// NetBSD's own manifest is SHA-512, verified the same BSD-style way as
// FreeBSD's SHA-256 one.
func netbsdISO(client *http.Client) (*recipe, error) {
	listing, err := fetchText(client, "https://cdn.netbsd.org/pub/NetBSD/")
	if err != nil {
		return nil, err
	}
	version, ok := latestNetBSDVersion(listing)
	if !ok {
		return nil, fmt.Errorf("no NetBSD-N.N release directory found")
	}

	dir := fmt.Sprintf("https://cdn.netbsd.org/pub/NetBSD/NetBSD-%s/images/", version)
	sums, err := fetchText(client, dir+"SHA512")
	if err != nil {
		return nil, err
	}

	var archs []imageArch
	for _, a := range []struct{ hop, file string }{
		{"amd64", fmt.Sprintf("NetBSD-%s-amd64.iso", version)},
		{"arm64", fmt.Sprintf("NetBSD-%s-evbarm-aarch64.iso", version)},
	} {
		hash, ok := findBSDSum(sums, a.file)
		if !ok {
			return nil, fmt.Errorf("manifest has no entry for %s", a.file)
		}
		archs = append(archs, imageArch{arch: a.hop, url: dir + a.file, sha512: hash})
	}

	arts := fanOut(archs, "raw")
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "netbsd-iso", Version: version, Kind: "image",
		Description: "NetBSD " + version + " install ISO — the BSD built to run on almost anything",
		Homepage:    "https://www.netbsd.org",
		License:     "BSD-2-Clause and others (NetBSD base system)",
		Keywords:    []string{"vm", "bsd", "netbsd", "iso", "image"},
		Artifacts:   arts,
		Caveats: "This is an install ISO, not a command. Find it with:\n\n" +
			"    hop info netbsd-iso\n\n" +
			"Boot it with qemu, write it to a USB drive, or mount it in a hypervisor.",
	}, nil
}

// latestNetBSDVersion picks the newest "NetBSD-N.N/" directory.
func latestNetBSDVersion(listing string) (string, bool) {
	best := ""
	for _, name := range hrefNames(listing) {
		name = strings.TrimSuffix(name, "/")
		v := strings.TrimPrefix(name, "NetBSD-")
		if v == name || !isDottedVersion(v) {
			continue
		}
		if best == "" || compareFreeBSDVersion(v+"-RELEASE", best+"-RELEASE") > 0 {
			best = v
		}
	}
	return best, best != ""
}

// -------------------------------------------------------------- opensuse ----

// openSUSETumbleweed resolves openSUSE's rolling-release Tumbleweed DVD
// installer. "Current.iso" is a stable alias Tumbleweed itself maintains
// (it always points at today's snapshot); the per-file ".sha256" sidecar
// redirects through openSUSE's mirror-selection system, so it's fetched
// with fetchText's own client, which already follows redirects.
func openSUSETumbleweed(client *http.Client) (*recipe, error) {
	var archs []imageArch
	for _, a := range []struct{ hop, suse string }{{"amd64", "x86_64"}, {"arm64", "aarch64"}} {
		base := "https://download.opensuse.org/tumbleweed/iso/"
		if a.suse == "aarch64" {
			base = "https://download.opensuse.org/ports/aarch64/tumbleweed/iso/"
		}
		file := fmt.Sprintf("openSUSE-Tumbleweed-DVD-%s-Current.iso", a.suse)
		sumLine, err := fetchText(client, base+file+".sha256")
		if err != nil {
			return nil, err
		}
		fields := strings.Fields(sumLine)
		if len(fields) == 0 {
			return nil, fmt.Errorf("empty checksum for %s", file)
		}
		archs = append(archs, imageArch{arch: a.hop, url: base + file, sha256: fields[0]})
	}

	arts := fanOut(archs, "raw")
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "opensuse-tumbleweed", Version: "tumbleweed", Kind: "image",
		Description: "openSUSE Tumbleweed DVD installer — the rolling-release openSUSE",
		Homepage:    "https://get.opensuse.org/tumbleweed/",
		License:     "GPL and others (openSUSE base system)",
		Keywords:    []string{"iso", "installer", "opensuse", "suse", "linux", "image"},
		Artifacts:   arts,
		Caveats: "This is an installer ISO, not a command. Find it with:\n\n" +
			"    hop info opensuse-tumbleweed\n\n" +
			"Boot it with qemu, write it to a USB drive, or mount it in a hypervisor.\n\n" +
			"Tumbleweed is a rolling release: \"Current.iso\" always points at the\n" +
			"newest snapshot, so this recipe's version is nominal, not point-in-time.",
	}, nil
}

// ------------------------------------------------------------------ rocky ----

// rockyLinux resolves the current Rocky Linux 9 minimal installer ISO — a
// free, community-rebuilt RHEL, real per-file BSD-style CHECKSUM sidecars.
func rockyLinux(client *http.Client) (*recipe, error) {
	var archs []imageArch
	for _, a := range []struct{ hop, rocky string }{{"amd64", "x86_64"}, {"arm64", "aarch64"}} {
		dir := fmt.Sprintf("https://download.rockylinux.org/pub/rocky/9/isos/%s/", a.rocky)
		file := fmt.Sprintf("Rocky-9-latest-%s-minimal.iso", a.rocky)
		sums, err := fetchText(client, dir+file+".CHECKSUM")
		if err != nil {
			return nil, err
		}
		hash, ok := findBSDSum(sums, file)
		if !ok {
			return nil, fmt.Errorf("manifest has no entry for %s", file)
		}
		archs = append(archs, imageArch{arch: a.hop, url: dir + file, sha256: hash})
	}

	arts := fanOut(archs, "raw")
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "rocky-linux", Version: "9", Kind: "image",
		Description: "Rocky Linux 9 minimal installer ISO — a free, community-rebuilt RHEL",
		Homepage:    "https://rockylinux.org",
		License:     "GPL-2.0 (Rocky Linux base system)",
		Keywords:    []string{"iso", "installer", "rhel", "enterprise", "linux", "image"},
		Artifacts:   arts,
		Caveats: "This is an installer ISO, not a command. Find it with:\n\n" +
			"    hop info rocky-linux\n\n" +
			"Boot it with qemu, write it to a USB drive, or mount it in a hypervisor.",
	}, nil
}

// -------------------------------------------------------------- void linux ----

// voidLinux resolves the current Void Linux live ISO (glibc base variant)
// — an independent Linux distribution with its own package manager (xbps)
// and an init system that isn't systemd, genuinely distinct from every
// other Linux in this index. Void's manifest is BSD-style, one combined
// file covering every image it publishes for that architecture.
func voidLinux(client *http.Client) (*recipe, error) {
	const dir = "https://repo-default.voidlinux.org/live/current/"
	sums, err := fetchText(client, dir+"sha256sum.txt")
	if err != nil {
		return nil, err
	}

	date, ok := latestVoidDate(sums)
	if !ok {
		return nil, fmt.Errorf("could not find a dated void-live-x86_64 ISO in the manifest")
	}

	var archs []imageArch
	for _, a := range []struct{ hop, void string }{{"amd64", "x86_64"}, {"arm64", "aarch64"}} {
		file := fmt.Sprintf("void-live-%s-%s-base.iso", a.void, date)
		hash, ok := findBSDSum(sums, file)
		if !ok {
			return nil, fmt.Errorf("manifest has no entry for %s", file)
		}
		archs = append(archs, imageArch{arch: a.hop, url: dir + file, sha256: hash})
	}

	arts := fanOut(archs, "raw")
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "void-linux", Version: date, Kind: "image",
		Description: "Void Linux live ISO (glibc, base) — an independent distro, its own xbps package manager, no systemd",
		Homepage:    "https://voidlinux.org",
		License:     "MIT and others (Void base system)",
		Keywords:    []string{"iso", "installer", "void", "xbps", "linux", "image"},
		Artifacts:   arts,
		Caveats: "This is a live/installer ISO, not a command. Find it with:\n\n" +
			"    hop info void-linux\n\n" +
			"Boot it with qemu, write it to a USB drive, or mount it in a hypervisor.",
	}, nil
}

// latestVoidDate extracts the release date stamp (e.g. "20250202") from a
// "void-live-x86_64-<date>-base.iso" entry in Void's checksum manifest, so
// the other architecture's filename can be built from the same release.
func latestVoidDate(sums string) (string, bool) {
	const marker = "void-live-x86_64-"
	i := strings.Index(sums, marker)
	if i < 0 {
		return "", false
	}
	rest := sums[i+len(marker):]
	j := strings.IndexByte(rest, '-')
	if j <= 0 {
		return "", false
	}
	return rest[:j], true
}

// -------------------------------------------------------------- kali ----

// kaliLinux resolves the current Kali Linux installer ISO — the
// Debian-derived security/penetration-testing distribution maintained by
// Offensive Security. Entirely legitimate, freely distributed open-source
// software with real official checksums; no different in kind from any
// other Linux distro in this index. cdimage.kali.org redirects every
// request to a geographically appropriate mirror — the stable address is
// this one, and Go's own HTTP client already follows the redirect
// transparently both here and at real install time.
func kaliLinux(client *http.Client) (*recipe, error) {
	const dir = "https://cdimage.kali.org/current/"
	sums, err := fetchText(client, dir+"SHA256SUMS")
	if err != nil {
		return nil, err
	}

	version, ok := latestKaliVersion(sums)
	if !ok {
		return nil, fmt.Errorf("could not find a kali-linux-*-installer-amd64.iso entry in the manifest")
	}

	var archs []imageArch
	for _, a := range []struct{ hop, kali string }{{"amd64", "amd64"}, {"arm64", "arm64"}} {
		file := fmt.Sprintf("kali-linux-%s-installer-%s.iso", version, a.kali)
		hash, ok := findSum(sums, file)
		if !ok {
			return nil, fmt.Errorf("manifest has no entry for %s", file)
		}
		archs = append(archs, imageArch{arch: a.hop, url: dir + file, sha256: hash})
	}

	arts := fanOut(archs, "raw")
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "kali-linux", Version: version, Kind: "image",
		Description: "Kali Linux " + version + " installer ISO — Debian-derived, for security testing",
		Homepage:    "https://www.kali.org",
		License:     "GPL and others (Debian/Kali base system)",
		Keywords:    []string{"iso", "installer", "kali", "security", "linux", "image"},
		Artifacts:   arts,
		Caveats: "This is an installer ISO, not a command. Find it with:\n\n" +
			"    hop info kali-linux\n\n" +
			"Boot it with qemu, write it to a USB drive, or mount it in a hypervisor.\n\n" +
			"For authorised security testing and research use, per Kali's own terms.",
	}, nil
}

// latestKaliVersion extracts "2026.2" from a
// "kali-linux-2026.2-installer-amd64.iso" entry in the manifest.
func latestKaliVersion(sums string) (string, bool) {
	const prefix, suffix = "kali-linux-", "-installer-amd64.iso"
	i := strings.Index(sums, prefix)
	for i >= 0 {
		rest := sums[i+len(prefix):]
		if j := strings.Index(rest, suffix); j > 0 {
			candidate := rest[:j]
			if !strings.Contains(candidate, " ") && !strings.Contains(candidate, "\n") {
				return candidate, true
			}
		}
		next := strings.Index(sums[i+1:], prefix)
		if next < 0 {
			break
		}
		i = i + 1 + next
	}
	return "", false
}

// -------------------------------------------------------------- parrot ----

// parrotSecurity resolves the current Parrot Security ISO — a
// Debian-derived security/privacy distro alongside Kali, maintained by a
// different team with a different default toolset and desktop. amd64 only:
// Parrot's arm64 build ships as a UTM-specific tarball, not a plain ISO, so
// there's nothing equivalent to fan out to darwin/linux-arm64 here.
func parrotSecurity(client *http.Client) (*recipe, error) {
	listing, err := fetchText(client, "https://deb.parrot.sh/parrot/iso/")
	if err != nil {
		return nil, err
	}
	version, ok := latestParrotVersion(listing)
	if !ok {
		return nil, fmt.Errorf("no version directory found")
	}

	dir := fmt.Sprintf("https://deb.parrot.sh/parrot/iso/%s/", version)
	file := fmt.Sprintf("Parrot-security-%s_amd64.iso", version)
	sums, err := fetchText(client, dir+"signed-hashes.txt")
	if err != nil {
		return nil, err
	}
	hash, ok := findSum(sums, file) // findSum's 64-hex-char check picks the SHA-256 line, not the MD5 or SHA-512 one also present
	if !ok {
		return nil, fmt.Errorf("manifest has no SHA-256 entry for %s", file)
	}

	art := &artifact{URL: dir + file, SHA256: hash, Format: "raw"}
	if err := setSizes(client, map[string]*artifact{"x": art}); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "parrot-security", Version: version, Kind: "image",
		Description: "Parrot Security " + version + " installer ISO (x86_64 only) — Debian-derived, for security testing",
		Homepage:    "https://parrotsec.org",
		License:     "GPL and others (Debian/Parrot base system)",
		Keywords:    []string{"iso", "installer", "parrot", "security", "linux", "image"},
		Artifacts:   map[string]*artifact{"darwin-amd64": art, "linux-amd64": art},
		Caveats: "This is an installer ISO, not a command. Find it with:\n\n" +
			"    hop info parrot-security\n\n" +
			"Boot it with qemu, write it to a USB drive, or mount it in a hypervisor.\n\n" +
			"x86_64 only: Parrot's arm64 build ships as a UTM-specific tarball, not a plain ISO.\n" +
			"For authorised security testing and research use, per Parrot's own terms.",
	}, nil
}

// latestParrotVersion picks the newest "N.N/" directory from Parrot's
// release index, skipping the non-numeric "caine/" and "latest/" entries
// mixed into the same listing.
func latestParrotVersion(listing string) (string, bool) {
	best := ""
	for _, name := range hrefNames(listing) {
		name = strings.TrimSuffix(name, "/")
		if !isDottedVersion(name) {
			continue
		}
		if best == "" || compareFreeBSDVersion(name+"-RELEASE", best+"-RELEASE") > 0 {
			best = name
		}
	}
	return best, best != ""
}

// -------------------------------------------------------------- proxmox ----

// proxmoxVE resolves the current Proxmox VE installer ISO — a
// Debian-derived type-1 hypervisor, a genuinely different category
// (virtualization platform, not a general-purpose desktop/server distro)
// from everything else in this index.
func proxmoxVE(client *http.Client) (*recipe, error) {
	listing, err := fetchText(client, "https://enterprise.proxmox.com/iso/")
	if err != nil {
		return nil, err
	}
	version, ok := latestProxmoxVersion(listing)
	if !ok {
		return nil, fmt.Errorf("no proxmox-ve_N.N-N.iso entry found")
	}

	const base = "https://enterprise.proxmox.com/iso/"
	var archs []imageArch
	for _, a := range []struct {
		hop, file string
	}{
		{"amd64", fmt.Sprintf("proxmox-ve_%s.iso", version)},
		{"arm64", fmt.Sprintf("proxmox-ve_%s-arm64.iso", version)},
	} {
		sumLine, err := fetchText(client, base+a.file+".sha256")
		if err != nil {
			continue // an arm64 build isn't published for every point release
		}
		fields := strings.Fields(sumLine)
		if len(fields) == 0 {
			continue
		}
		archs = append(archs, imageArch{arch: a.hop, url: base + a.file, sha256: fields[0]})
	}
	if len(archs) == 0 {
		return nil, fmt.Errorf("no artifact resolved for any platform")
	}

	arts := fanOut(archs, "raw")
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "proxmox-ve", Version: version, Kind: "image",
		Description: "Proxmox VE " + version + " installer ISO — a Debian-derived type-1 hypervisor",
		Homepage:    "https://www.proxmox.com/en/proxmox-virtual-environment/overview",
		License:     "AGPL-3.0 (Proxmox VE)",
		Keywords:    []string{"iso", "installer", "proxmox", "hypervisor", "virtualization", "image"},
		Artifacts:   arts,
		Caveats: "This is an installer ISO, not a command. Find it with:\n\n" +
			"    hop info proxmox-ve\n\n" +
			"Boot it with qemu, write it to a USB drive, or mount it directly on server\n" +
			"hardware — Proxmox VE is a hypervisor, meant to be installed as the host OS.",
	}, nil
}

// latestProxmoxVersion extracts the newest "N.N-N" version from a
// "proxmox-ve_N.N-N.iso" entry — deliberately excluding "-arm64" suffixed
// and other Proxmox product ISOs (Backup Server, Mail Gateway, Datacenter
// Manager) also listed in the same directory.
func latestProxmoxVersion(listing string) (string, bool) {
	best := ""
	for _, name := range hrefNames(listing) {
		name = strings.TrimPrefix(name, "./") // Proxmox's own listing hrefs are relative, e.g. "./proxmox-ve_9.2-1.iso"
		if !strings.HasPrefix(name, "proxmox-ve_") || !strings.HasSuffix(name, ".iso") {
			continue
		}
		v := strings.TrimSuffix(strings.TrimPrefix(name, "proxmox-ve_"), ".iso")
		if strings.Contains(v, "-arm64") {
			continue
		}
		if best == "" || compareFreeBSDVersion(v+"-RELEASE", best+"-RELEASE") > 0 {
			best = v
		}
	}
	return best, best != ""
}

// -------------------------------------------------------------- tails ----

// tails resolves the current Tails release — an amnesic, privacy-focused
// live OS that routes all traffic through Tor by design and leaves no
// trace on the host it ran from, genuinely distinct in purpose from every
// other Linux in this index.
//
// Tails publishes only a PGP signature (.sig) for its image, not a plain
// checksum file hop can verify a digest against — hop has no OpenPGP
// verifier, and building one is a materially different, larger undertaking
// than reading a manifest. This recipe is trust-on-first-use, the same
// honest fallback already used for the pre-catalog macOS installers that
// have no published digest either, rather than a checksum invented to fill
// the field.
func tails(client *http.Client) (*recipe, error) {
	const redirector = "https://download.tails.net/tails/stable/"
	listing, err := fetchText(client, redirector)
	if err != nil {
		return nil, err
	}
	version, ok := latestTailsVersion(listing)
	if !ok {
		return nil, fmt.Errorf("no tails-amd64-N.N directory found")
	}

	url := fmt.Sprintf("%stails-amd64-%s/tails-amd64-%s.iso", redirector, version, version)
	art := &artifact{URL: url, Format: "raw"}
	if err := setSizes(client, map[string]*artifact{"x": art}); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "tails", Version: version, Kind: "image",
		Description: "Tails " + version + " (x86_64 only) — amnesic live OS, routes all traffic through Tor",
		Homepage:    "https://tails.net",
		License:     "GPL and others (Debian/Tails base system)",
		Keywords:    []string{"iso", "installer", "tails", "privacy", "tor", "linux", "image"},
		Artifacts:   map[string]*artifact{"darwin-amd64": art, "linux-amd64": art},
		Caveats: "This is a live-USB ISO, not a command. Find it with:\n\n" +
			"    hop info tails\n\n" +
			"Write it to a USB drive (Tails is meant to boot from removable media, not a\n" +
			"virtual disk) or mount it in a hypervisor for testing.\n\n" +
			"Tails publishes only a PGP signature for this image, not a plain checksum,\n" +
			"so hop cannot pre-verify it the way it does everything else in this index —\n" +
			"the digest is recorded on first install (trust-on-first-use) instead.\n" +
			"Verify the signature yourself against Tails' signing key if that matters for\n" +
			"your use case: https://tails.net/tails-signing.key",
	}, nil
}

// latestTailsVersion picks the newest "tails-amd64-N.N/" directory.
func latestTailsVersion(listing string) (string, bool) {
	best := ""
	for _, name := range hrefNames(listing) {
		name = strings.TrimSuffix(name, "/")
		v := strings.TrimPrefix(name, "tails-amd64-")
		if v == name || !isDottedVersion(v) {
			continue
		}
		if best == "" || compareFreeBSDVersion(v+"-RELEASE", best+"-RELEASE") > 0 {
			best = v
		}
	}
	return best, best != ""
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

// -------------------------------------------------------------- fedora ----

// fedoraWorkstation resolves the current Fedora Workstation Live ISO — a
// real installer image, not a cloud image — from Fedora's own official
// releases.json, which is a clean structured manifest rather than a
// directory listing to scrape.
func fedoraWorkstation(client *http.Client) (*recipe, error) {
	body, err := fetchTextN(client, "https://fedoraproject.org/releases.json", 8<<20)
	if err != nil {
		return nil, err
	}
	var entries []struct {
		Version string `json:"version"`
		Arch    string `json:"arch"`
		Variant string `json:"variant"`
		Link    string `json:"link"`
		SHA256  string `json:"sha256"`
		Size    string `json:"size"`
	}
	if err := json.Unmarshal([]byte(body), &entries); err != nil {
		return nil, fmt.Errorf("parsing releases.json: %w", err)
	}

	// Find the newest version with both architectures present as a plain
	// Live ISO (not an ociarchive or other container-native variant).
	best := ""
	byVerArch := map[string]map[string]struct{ url, sha256 string }{}
	for _, e := range entries {
		if e.Variant != "Workstation" || !strings.HasSuffix(e.Link, ".iso") {
			continue
		}
		if e.Arch != "x86_64" && e.Arch != "aarch64" {
			continue
		}
		if byVerArch[e.Version] == nil {
			byVerArch[e.Version] = map[string]struct{ url, sha256 string }{}
		}
		byVerArch[e.Version][e.Arch] = struct{ url, sha256 string }{e.Link, e.SHA256}
		if best == "" || compareFreeBSDVersion(e.Version+"-RELEASE", best+"-RELEASE") > 0 {
			best = e.Version
		}
	}
	pair, ok := byVerArch[best]
	x86, hasX86 := pair["x86_64"]
	arm, hasArm := pair["aarch64"]
	if !ok || !hasX86 || !hasArm {
		return nil, fmt.Errorf("no complete x86_64+aarch64 Workstation Live ISO pair found")
	}

	archs := []imageArch{
		{arch: "amd64", url: x86.url, sha256: x86.sha256},
		{arch: "arm64", url: arm.url, sha256: arm.sha256},
	}
	arts := fanOut(archs, "raw")
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "fedora-workstation", Version: best, Kind: "image",
		Description: "Fedora Workstation " + best + " Live ISO installer",
		Homepage:    "https://fedoraproject.org",
		License:     "GPL and others (Fedora base system)",
		Keywords:    []string{"iso", "installer", "fedora", "linux", "image"},
		Artifacts:   arts,
		Caveats: "This is an installer ISO, not a command. Find it with:\n\n" +
			"    hop info fedora-workstation\n\n" +
			"Boot it with qemu, write it to a USB drive, or mount it in a hypervisor.",
	}, nil
}

// ------------------------------------------------------------ arch linux ----

// archlinuxISO resolves the current Arch Linux install ISO. Arch publishes
// x86_64 only — there is no official arm64 build (Arch Linux ARM is a
// separate, differently-run project) — so this recipe honestly has no
// darwin-arm64/linux-arm64 artifact at all.
func archlinuxISO(client *http.Client) (*recipe, error) {
	const base = "https://geo.mirror.pkgbuild.com/iso/latest/"
	sums, err := fetchText(client, base+"sha256sums.txt")
	if err != nil {
		return nil, err
	}
	hash, ok := findSum(sums, "archlinux-x86_64.iso")
	if !ok {
		return nil, fmt.Errorf("manifest has no entry for archlinux-x86_64.iso")
	}

	arts := map[string]*artifact{
		"darwin-amd64": {URL: base + "archlinux-x86_64.iso", SHA256: hash, Format: "raw"},
		"linux-amd64":  {URL: base + "archlinux-x86_64.iso", SHA256: hash, Format: "raw"},
	}
	if err := setSizes(client, arts); err != nil {
		return nil, err
	}
	return &recipe{
		Name: "archlinux-iso", Version: "latest", Kind: "image",
		Description: "Arch Linux install ISO (x86_64 only — Arch publishes no official arm64 build)",
		Homepage:    "https://archlinux.org",
		License:     "GPL and others (Arch base system)",
		Keywords:    []string{"iso", "installer", "arch", "linux", "image"},
		Artifacts:   arts,
		Caveats: "This is an installer ISO, not a command. Find it with:\n\n" +
			"    hop info archlinux-iso\n\n" +
			"Boot it with qemu, write it to a USB drive, or mount it in a hypervisor.\n\n" +
			"x86_64 only: Arch itself publishes no official arm64 ISO.",
	}, nil
}

// -------------------------------------------------------------- macos ----

// macosRecovery resolves the current macOS full restore image for Apple
// Silicon Macs, via the same public Apple CDN (updates.cdn-apple.com) that
// Apple Configurator, and open-source macOS-VM tools such as Tart and UTM,
// already use to provision macOS VMs under Apple's own Virtualization
// framework — a legitimate, documented distribution channel, distinct from
// (and much smaller a claim than) redistributing a macOS installer image
// yourself. The manifest is read from api.ipsw.me, a long-standing public
// aggregator of Apple's own signed firmware metadata; the URL and SHA-256 it
// reports both point straight back to apple.com's own infrastructure.
//
// Deliberately x86_64-less: Intel Macs restore from Internet Recovery, not
// a downloadable IPSW, so there is no equivalent artifact to offer there.
//
// This restore image runs 15-20+ GB. genindex/genimages verify every other
// recipe here by downloading the full artifact and hashing the bytes
// themselves; doing that for this one specifically was judged impractical
// for a single generator run, so this recipe's checksum is taken from the
// manifest rather than re-derived locally, same as the trust an OS's own
// package manager places in a signed repository index.
func macosRecovery(client *http.Client) (*recipe, error) {
	const device = "Mac14,2" // MacBook Air M2: a representative, currently-supported Apple Silicon board
	body, err := fetchTextN(client, "https://api.ipsw.me/v4/device/"+device, 4<<20)
	if err != nil {
		return nil, err
	}
	var info struct {
		Firmwares []struct {
			Version  string `json:"version"`
			BuildID  string `json:"buildid"`
			SHA256   string `json:"sha256sum"`
			URL      string `json:"url"`
			FileSize int64  `json:"filesize"`
			Signed   bool   `json:"signed"`
		} `json:"firmwares"`
	}
	if err := json.Unmarshal([]byte(body), &info); err != nil {
		return nil, fmt.Errorf("parsing ipsw.me response: %w", err)
	}
	if len(info.Firmwares) == 0 {
		return nil, fmt.Errorf("no firmwares listed for %s", device)
	}
	// The list is newest-first in practice, but don't assume it: pick the
	// highest build explicitly, and require it still be Apple-signed —
	// an unsigned (superseded) restore image will not actually restore.
	best := info.Firmwares[0]
	for _, f := range info.Firmwares {
		if f.Signed && (!best.Signed || f.BuildID > best.BuildID) {
			best = f
		}
	}
	if !best.Signed {
		return nil, fmt.Errorf("no currently-signed restore image found for %s", device)
	}

	art := &artifact{URL: best.URL, SHA256: best.SHA256, Format: "raw", Size: best.FileSize}
	return &recipe{
		Name: "macos-recovery", Version: best.Version, Kind: "image",
		Description: fmt.Sprintf("macOS %s full restore image (Apple Silicon), for provisioning a macOS VM", best.Version),
		Homepage:    "https://support.apple.com/guide/vt/welcome/web",
		License:     "Apple Software License Agreement (macOS itself; running it is subject to Apple's terms)",
		Keywords:    []string{"macos", "apple", "vm", "recovery", "ipsw", "image"},
		Artifacts: map[string]*artifact{
			// Apple Silicon only: this restore path doesn't exist for Intel Macs.
			"darwin-arm64": art,
		},
		Caveats: fmt.Sprintf(
			"This is a %s restore image, not a command. Find it with:\n\n"+
				"    hop info macos-recovery\n\n"+
				"Feed it to Apple's own Virtualization.framework (e.g. via Tart or UTM) to\n"+
				"provision a macOS VM — the same mechanism Apple Configurator uses.\n\n"+
				"Running macOS is subject to Apple's software license agreement, which\n"+
				"permits it only on Apple hardware (including a VM on an Apple Silicon Mac).\n"+
				"Only Apple Silicon builds exist: Intel Macs restore over the network via\n"+
				"Internet Recovery, not a downloadable image like this one.",
			humanBytes(best.FileSize)),
	}, nil
}

// macosInstallerCandidate is one full-installer product found in the live
// catalog, before being turned into a recipe.
type macosInstallerCandidate struct {
	name, version, build, url, digest string
	size                              int64
}

// macosInstallers resolves every full macOS installer hop can legitimately
// reach — the same multi-gigabyte "Install macOS <Name>.app" package the
// Mac App Store hands you, not the low-level recovery firmware
// macos-recovery already covers — spanning as much real version history as
// Apple's own infrastructure actually still serves.
//
// Two sources, both Apple's own, both already verified live rather than
// guessed:
//
//  1. The live software-update catalog (swscan.apple.com), the same
//     infrastructure macOS itself uses to check for updates — no
//     authentication, no bot-detection, nothing gated. It does not publish
//     inline which macOS version a given product is; that requires fetching
//     the product's own English ".dist" installer script and reading
//     version strings out of it, the same technique the long-standing
//     open-source tool mist-cli uses (Sources/Mist/Helpers/HTTP.swift). In
//     practice this catalog only carries recent-generation installers
//     (roughly High Sierra 10.13 onward) — Apple simply doesn't keep older
//     ones in current serving infrastructure.
//  2. For everything the live catalog no longer carries, a short list of
//     specific historical Apple CDN URLs — Lion 10.7.5 through Sierra
//     10.12.6 — that mist-cli's own maintainers have spent years verifying
//     still resolve, re-checked live here rather than trusted blind (each
//     one is HEAD-requested for a real Content-Length before being kept).
//
// That combination is the actual limit of what's legitimately available:
// nothing before Lion exists anywhere in Apple's own infrastructure —
// Mac OS X Server 10.1 through Snow Leopard 10.6 predate the Mac App
// Store/catalog system by years, shipped only on physical media, and were
// never re-hosted by Apple in any digital form. Even mist-cli, a project
// entirely dedicated to hunting down every Apple-hosted macOS URL still
// alive, has never found one from that era — which is itself the evidence
// that none exists to find.
func macosInstallers(client *http.Client) ([]*recipe, error) {
	live, err := macosInstallersFromCatalog(client)
	if err != nil {
		warn("macos-installer (live catalog): %v", err)
	}
	legacy := macosInstallersLegacy(client)

	all := append(live, legacy...)
	if len(all) == 0 {
		return nil, fmt.Errorf("no macOS installer resolved from any source")
	}
	return all, nil
}

// macosInstallersFromCatalog walks Apple's live catalog and returns one
// recipe per distinct major version found — the catalog often retains
// several recent generations at once, not just the newest.
func macosInstallersFromCatalog(client *http.Client) ([]*recipe, error) {
	// mist-cli's Catalog.swift enumerates several of these (customer/
	// developer/beta seeds); "standard" is the one that carries public
	// releases, which is the only one an index meant for everyone should
	// point at.
	const catalogURL = "https://swscan.apple.com/content/catalogs/others/" +
		"index-26-15-14-13-12-10.16-10.15-10.14-10.13-10.12-10.11-10.10-10.9-" +
		"mountainlion-lion-snowleopard-leopard.merged-1.sucatalog.gz"

	body, err := fetchBinary(client, catalogURL, 32<<20)
	if err != nil {
		return nil, err
	}
	raw, err := gunzipOrSelf(body)
	if err != nil {
		return nil, err
	}
	root, err := parsePlist(raw)
	if err != nil {
		return nil, fmt.Errorf("parsing software update catalog: %w", err)
	}
	products, _ := root["Products"].(map[string]any)
	if products == nil {
		return nil, fmt.Errorf("catalog has no Products dictionary")
	}

	// Keep the newest build per major version (the integer before the first
	// dot: "15" from "15.6.1"), so a catalog carrying several point
	// releases of the same generation yields one recipe, not several.
	bestByMajor := map[string]*macosInstallerCandidate{}

	for _, raw := range products {
		product, _ := raw.(map[string]any)
		if product == nil {
			continue
		}
		// A full-installer product is marked by carrying
		// InstallAssistantPackageIdentifiers under ExtendedMetaInfo — every
		// other catalog entry (security updates, CLTools, firmware) lacks it.
		meta, _ := product["ExtendedMetaInfo"].(map[string]any)
		if meta == nil {
			continue
		}
		if _, ok := meta["InstallAssistantPackageIdentifiers"]; !ok {
			continue
		}
		distributions, _ := product["Distributions"].(map[string]any)
		distURL, _ := distributions["English"].(string)
		if distURL == "" {
			continue
		}
		packages, _ := product["Packages"].([]any)
		var pkgURL, pkgDigest string
		var pkgSize int64
		for _, p := range packages {
			pkg, _ := p.(map[string]any)
			u, _ := pkg["URL"].(string)
			if !strings.HasSuffix(u, "/InstallAssistant.pkg") {
				continue
			}
			pkgURL = u
			pkgDigest, _ = pkg["Digest"].(string)
			if sz, ok := pkg["Size"].(int64); ok {
				pkgSize = sz
			}
		}
		if pkgURL == "" || pkgDigest == "" {
			continue // this product bundles no full installer payload
		}

		dist, err := fetchTextN(client, distURL, 8<<20)
		if err != nil {
			continue // a handful of stale catalog entries 404; skip, don't fail the run
		}
		version := plistDistField(dist, "VERSION")
		build := plistDistField(dist, "BUILD")
		name := distSuDisabledGroupID(dist)
		if version == "" || build == "" {
			continue
		}
		// mist-cli's own definition of "beta": a build ID ending in a
		// lowercase letter. Skip those for the index everyone installs from.
		if len(build) > 0 && build[len(build)-1] >= 'a' && build[len(build)-1] <= 'z' {
			continue
		}

		major := strings.SplitN(version, ".", 2)[0]
		c := &macosInstallerCandidate{name: name, version: version, build: build, url: pkgURL, digest: pkgDigest, size: pkgSize}
		if prev, ok := bestByMajor[major]; !ok ||
			compareFreeBSDVersion(c.version+"-RELEASE", prev.version+"-RELEASE") > 0 ||
			(c.version == prev.version && c.build > prev.build) {
			bestByMajor[major] = c
		}
	}

	var out []*recipe
	for _, c := range bestByMajor {
		out = append(out, macosInstallerRecipe(c.name, c.version, c.build, c.url, c.digest, "", c.size, true))
	}
	return out, nil
}

// legacyMacOSInstaller is one historical release no longer in the live
// catalog, with a specific Apple CDN URL mist-cli's maintainers have kept
// verified across years of the project's history.
type legacyMacOSInstaller struct {
	name, version, build, url string
}

// legacyMacOSInstallers is deliberately short: it is exactly what
// mist-cli's own hand-maintained legacy list contains, nothing extrapolated
// beyond it. That list stops at Lion 10.7.5 because that is where Apple's
// own re-hostable archive of installer media stops — nothing earlier has
// ever been found, by this project or any other.
var legacyMacOSInstallers = []legacyMacOSInstaller{
	{"OS X Lion", "10.7.5", "11G63", "https://updates.cdn-apple.com/2021/macos/041-7683-20210614-E610947E-C7CE-46EB-8860-D26D71F0D3EA/InstallMacOSX.dmg"},
	{"OS X Mountain Lion", "10.8.5", "12F45", "https://updates.cdn-apple.com/2021/macos/031-0627-20210614-90D11F33-1A65-42DD-BBEA-E1D9F43A6B3F/InstallMacOSX.dmg"},
	{"OS X Yosemite", "10.10.5", "14F27", "https://updates.cdn-apple.com/2019/cert/061-41343-20191023-02465f92-3ab5-4c92-bfe2-b725447a070d/InstallMacOSX.dmg"},
	{"OS X El Capitan", "10.11.6", "15G31", "https://updates.cdn-apple.com/2019/cert/061-41424-20191024-218af9ec-cf50-4516-9011-228c78eda3d2/InstallMacOSX.dmg"},
	{"macOS Sierra", "10.12.6", "16G29", "https://updates.cdn-apple.com/2019/cert/061-39476-20191023-48f365f4-0015-4c41-9f44-39d3d2aca067/InstallOS.dmg"},
}

// macosInstallersLegacy HEAD-checks every entry in legacyMacOSInstallers and
// returns a recipe only for the ones that still actually resolve right now
// — Apple could take any of these down at any time, and this must never
// silently ship a dead link.
func macosInstallersLegacy(client *http.Client) []*recipe {
	var out []*recipe
	for _, l := range legacyMacOSInstallers {
		req, err := http.NewRequest(http.MethodHead, l.url, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "hop-genimages")
		resp, err := client.Do(req)
		if err != nil {
			warn("macos-%s: HEAD failed: %v", slugify(l.name), err)
			continue
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			warn("macos-%s: no longer available (%s) — Apple has taken this one down", slugify(l.name), resp.Status)
			continue
		}
		// These predate Apple publishing any digest at all for installer
		// media of this vintage; hop records the checksum it observes on
		// first install (trust-on-first-use), same as any other unpinned
		// artifact, rather than pretending to a precision the source
		// doesn't offer.
		out = append(out, macosInstallerRecipe(l.name, l.version, l.build, l.url, "", "dmg", resp.ContentLength, false))
	}
	return out
}

// macosInstallerRecipe builds one recipe from a resolved installer,
// live-catalog or legacy. digest is a SHA-1 "Digest" when the catalog
// supplied one, empty for the legacy entries the catalog no longer lists.
func macosInstallerRecipe(name, version, build, url, digest, format string, size int64, current bool) *recipe {
	if name == "" {
		name = "macOS " + version
	}
	if format == "" {
		format = "raw"
	}
	slug := slugify(name)

	art := &artifact{URL: url, SHA1: digest, Format: format, Size: size}
	kind := "installer package"
	expand := "expand it with `pkgutil --expand-full` to reach the app bundle, or"
	if format == "dmg" {
		kind = "disk image"
		expand = "mount it (`hdiutil attach`) to reach the installer app inside, or"
	}

	r := &recipe{
		Name: "macos-" + slug, Version: version, Kind: "image",
		Description: fmt.Sprintf("%s (build %s) full installer, straight from Apple's own infrastructure", name, build),
		Homepage:    "https://support.apple.com/guide/mac-help/reinstall-macos-mchl46d531d6/mac",
		License:     "Apple Software License Agreement (macOS itself; running it is subject to Apple's terms)",
		Keywords:    []string{"macos", "apple", "installer", "image"},
		// The installer package itself is architecture-agnostic (it bundles
		// payloads for every Mac the release supports); expose it under both
		// darwin host keys since either can legitimately fetch and store it.
		Artifacts: map[string]*artifact{"darwin-amd64": art, "darwin-arm64": art},
		Caveats: fmt.Sprintf(
			"This is the %s for %s, not a command. Find it with:\n\n"+
				"    hop info macos-%s\n\n"+
				"It's what \"Install %s.app\" is built from — %s use it\n"+
				"directly with a macOS VM tool such as UTM's installer-creation flow.\n\n"+
				"Resolved from Apple's own infrastructure — the live software update\n"+
				"catalog for recent releases, or a specific historical Apple CDN URL,\n"+
				"HEAD-checked live, for anything the live catalog no longer carries.",
			kind, name, slug, name, expand),
	}
	if current {
		r.Description += " (current)"
	}
	return r
}

// slugify turns a display name into a recipe-name-safe slug:
// "OS X El Capitan" -> "el-capitan", "macOS Sequoia" -> "sequoia".
func slugify(name string) string {
	name = strings.TrimPrefix(name, "macOS ")
	name = strings.TrimPrefix(name, "OS X ")
	name = strings.TrimPrefix(name, "Mac OS X ")
	name = strings.ToLower(name)
	name = strings.ReplaceAll(name, " ", "-")
	return name
}

// plistDistField extracts a scalar from a distribution script's embedded
// plist fragment: "<key>NAME</key>\n<string>VALUE</string>".
func plistDistField(dist, key string) string {
	marker := "<key>" + key + "</key>"
	i := strings.Index(dist, marker)
	if i < 0 {
		return ""
	}
	rest := dist[i+len(marker):]
	j := strings.Index(rest, "<string>")
	if j < 0 {
		return ""
	}
	rest = rest[j+len("<string>"):]
	k := strings.Index(rest, "</string>")
	if k < 0 {
		return ""
	}
	return strings.TrimSpace(rest[:k])
}

// distSuDisabledGroupID extracts the installer's display name from its
// suDisabledGroupID attribute, e.g. suDisabledGroupID="Install macOS Tahoe"
// -> "macOS Tahoe". mist-cli reads the same attribute the same way.
func distSuDisabledGroupID(dist string) string {
	const marker = `suDisabledGroupID="`
	i := strings.Index(dist, marker)
	if i < 0 {
		return ""
	}
	rest := dist[i+len(marker):]
	j := strings.IndexByte(rest, '"')
	if j < 0 {
		return ""
	}
	return strings.TrimPrefix(rest[:j], "Install ")
}

// ---------------------------------------------------------------- helpers ----

// fetchText fetches a small text manifest, refusing anything over 4MB —
// every manifest genimages reads (checksum files, YAML, small JSON) is
// tiny, so anything bigger signals something has gone wrong upstream.
func fetchText(client *http.Client, url string) (string, error) {
	return fetchTextN(client, url, 4<<20)
}

// fetchTextN is fetchText with an explicit size limit, for a manifest that
// is legitimately larger than the usual small checksum file (Fedora's
// releases.json lists hundreds of variants and runs a few hundred KB).
func fetchTextN(client *http.Client, url string, limit int) (string, error) {
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
		if len(buf) > limit {
			return "", fmt.Errorf("manifest at %s is implausibly large", url)
		}
	}
	return string(buf), nil
}

// humanBytes formats a byte count for a caveat message.
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

// findSum parses a "<hex>  <filename>" or "<hex> *<filename>" checksum
// manifest line for the named file.
func findSum(manifest, file string) (string, bool) {
	return findSumLen(manifest, file, 64) // sha256sum is always exactly 64 hex chars
}

// findSumLen is findSum with an explicit expected hash length, for a
// manifest that lists more than one algorithm per file (Parrot publishes
// MD5, SHA-256 and SHA-512 for the same filename on three separate lines;
// without a length check, the first — weakest — one would win).
func findSumLen(manifest, file string, hexLen int) (string, bool) {
	for _, line := range strings.Split(manifest, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 || len(fields[0]) != hexLen {
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

// fetchBinary fetches a URL's raw bytes, up to limit. Used for the (gzipped)
// software update catalog, which is a small binary blob, not text.
func fetchBinary(client *http.Client, url string, limit int64) ([]byte, error) {
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "hop-genimages")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// gunzipOrSelf decompresses b if it looks gzipped, else returns it as-is —
// Apple serves the catalog gzip-encoded regardless of the ".gz" suffix
// sometimes being handled transparently by the HTTP layer already.
func gunzipOrSelf(b []byte) ([]byte, error) {
	if len(b) < 2 || b[0] != 0x1f || b[1] != 0x8b {
		return b, nil
	}
	zr, err := gzip.NewReader(bytes.NewReader(b))
	if err != nil {
		return nil, fmt.Errorf("not valid gzip: %w", err)
	}
	defer zr.Close()
	return io.ReadAll(io.LimitReader(zr, 256<<20))
}

// ------------------------------------------------------------------ plist ----

// parsePlist decodes just enough of Apple's XML property list format to
// walk a software update catalog: <dict>, <array>, <key>, <string>,
// <integer>, <real>, <date>, <true/>, <false/> and <data>. Values decode to
// map[string]any / []any / string / int64 / float64 / bool / time.Time,
// mirroring encoding/json's untyped decoding so callers can type-assert the
// same way. Go's standard library has no plist decoder; this is a small,
// purpose-built one rather than a dependency, in keeping with the rest of
// hop's zero-dependency tooling.
func parsePlist(data []byte) (map[string]any, error) {
	dec := xml.NewDecoder(bytes.NewReader(data))
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, fmt.Errorf("reading plist: %w", err)
		}
		if se, ok := tok.(xml.StartElement); ok && se.Name.Local == "plist" {
			break
		}
	}
	v, err := plistValue(dec)
	if err != nil {
		return nil, err
	}
	root, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("plist root is not a dictionary")
	}
	return root, nil
}

// plistValue finds the decoder's next StartElement (a dict, array, or
// scalar) and decodes it through its matching EndElement. Used only for the
// document's single root value; every nested value goes through
// plistValueFromStart instead, since plistDict/plistArray already have the
// StartElement in hand from their own token loop.
func plistValue(dec *xml.Decoder) (any, error) {
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		if se, ok := tok.(xml.StartElement); ok {
			return plistValueFromStart(dec, se)
		}
	}
}

// plistDict decodes a <dict> body: alternating <key> and value elements.
func plistDict(dec *xml.Decoder) (map[string]any, error) {
	out := map[string]any{}
	var pendingKey string
	haveKey := false

	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			if t.Name.Local == "key" {
				k, err := plistTextBody(dec, "key")
				if err != nil {
					return nil, err
				}
				pendingKey, haveKey = k, true
				continue
			}
			v, err := plistValueFromStart(dec, t)
			if err != nil {
				return nil, err
			}
			if haveKey {
				out[pendingKey] = v
				haveKey = false
			}
		case xml.EndElement:
			if t.Name.Local == "dict" {
				return out, nil
			}
		}
	}
}

// plistArray decodes an <array> body: a sequence of value elements.
func plistArray(dec *xml.Decoder) ([]any, error) {
	var out []any
	for {
		tok, err := dec.Token()
		if err != nil {
			return nil, err
		}
		switch t := tok.(type) {
		case xml.StartElement:
			v, err := plistValueFromStart(dec, t)
			if err != nil {
				return nil, err
			}
			out = append(out, v)
		case xml.EndElement:
			if t.Name.Local == "array" {
				return out, nil
			}
		}
	}
}

// plistValueFromStart decodes one value given its already-consumed opening
// tag — the counterpart to plistValue for callers (plistDict, plistArray)
// that read the StartElement themselves to distinguish it from <key>/</dict>.
func plistValueFromStart(dec *xml.Decoder, se xml.StartElement) (any, error) {
	switch se.Name.Local {
	case "dict":
		return plistDict(dec)
	case "array":
		return plistArray(dec)
	case "string", "data":
		return plistTextBody(dec, se.Name.Local)
	case "integer":
		s, err := plistTextBody(dec, "integer")
		if err != nil {
			return nil, err
		}
		return strconv.ParseInt(s, 10, 64)
	case "real":
		s, err := plistTextBody(dec, "real")
		if err != nil {
			return nil, err
		}
		return strconv.ParseFloat(s, 64)
	case "true":
		return true, consumeSelfOrEnd(dec, se)
	case "false":
		return false, consumeSelfOrEnd(dec, se)
	case "date":
		s, err := plistTextBody(dec, "date")
		if err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339, s)
		return t, nil
	default:
		if err := dec.Skip(); err != nil {
			return nil, err
		}
		return nil, nil
	}
}

// plistTextBody reads character data up to the given element's EndElement.
// <true/> and <false/> are self-closing with no body, handled separately by
// consumeSelfOrEnd.
func plistTextBody(dec *xml.Decoder, name string) (string, error) {
	var b strings.Builder
	for {
		tok, err := dec.Token()
		if err != nil {
			return "", err
		}
		switch t := tok.(type) {
		case xml.CharData:
			b.Write(t)
		case xml.EndElement:
			if t.Name.Local == name {
				return b.String(), nil
			}
		}
	}
}

// consumeSelfOrEnd absorbs a self-closing element's implicit end. Go's
// encoding/xml always emits a matching EndElement token even for tags
// written as <true/>, so this just reads and discards it.
func consumeSelfOrEnd(dec *xml.Decoder, se xml.StartElement) error {
	for {
		tok, err := dec.Token()
		if err != nil {
			return err
		}
		if end, ok := tok.(xml.EndElement); ok && end.Name.Local == se.Name.Local {
			return nil
		}
	}
}
