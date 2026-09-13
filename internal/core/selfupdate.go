package core

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// UpdateRepo is where hop's own releases are published.
const UpdateRepo = "samuelbanapour/hopcli"

// LatestReleaseInfo is the subset of GitHub's release API a self-update
// needs.
type LatestReleaseInfo struct {
	TagName string         `json:"tag_name"`
	HTMLURL string         `json:"html_url"`
	Assets  []ReleaseAsset `json:"assets"`
}

// ReleaseAsset is one file attached to a GitHub release.
type ReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// FindAsset locates one of a release's assets by exact filename.
func (r *LatestReleaseInfo) FindAsset(name string) (ReleaseAsset, bool) {
	for _, a := range r.Assets {
		if a.Name == name {
			return a, true
		}
	}
	return ReleaseAsset{}, false
}

// FetchLatestRelease asks GitHub for hop's latest published release. Pass a
// context with a short deadline for a background version check, or a bare
// one for an explicit `hop upgrade`.
func FetchLatestRelease(ctx context.Context, client *http.Client) (*LatestReleaseInfo, error) {
	url := "https://api.github.com/repos/" + UpdateRepo + "/releases/latest"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("User-Agent", userAgent())
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{URL: url, Status: resp.StatusCode, StatusText: resp.Status}
	}
	var rel LatestReleaseInfo
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&rel); err != nil {
		return nil, fmt.Errorf("decoding release info: %w", err)
	}
	return &rel, nil
}

// AssetName is the exact filename the release workflow publishes for a
// platform — see .github/workflows/release.yml's Package step.
func AssetName(tag, goos, goarch string) string {
	return fmt.Sprintf("hop-%s-%s-%s.tar.gz", tag, goos, goarch)
}

// maxUpdateAssetBytes bounds a self-update download. hop's own binary is a
// few MB; this is generous headroom, not an estimate of the real size.
const maxUpdateAssetBytes = 256 << 20

// downloadBytes fetches a URL's full body, bounded. Self-updating only ever
// needs two small files (the archive and its checksum manifest), so this is
// a plain buffered GET rather than fetch.go's chunked, resumable machinery
// built for arbitrary-size package artifacts.
func downloadBytes(ctx context.Context, client *http.Client, url string, max int64) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent())
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, &HTTPError{URL: url, Status: resp.StatusCode, StatusText: resp.Status}
	}
	return io.ReadAll(io.LimitReader(resp.Body, max))
}

// DownloadUpdate fetches a release's tarball for the running platform and
// its SHA256SUMS manifest, verifies the tarball's digest against the entry
// naming it by exact filename — a name not listed in the manifest is
// treated as unverifiable, not silently skipped — and returns the tarball
// bytes once confirmed, along with the asset name it verified.
func DownloadUpdate(ctx context.Context, client *http.Client, rel *LatestReleaseInfo) ([]byte, string, error) {
	assetName := AssetName(rel.TagName, runtime.GOOS, runtime.GOARCH)
	asset, ok := rel.FindAsset(assetName)
	if !ok {
		return nil, "", fmt.Errorf("release %s has no build for %s-%s", rel.TagName, runtime.GOOS, runtime.GOARCH)
	}
	sums, ok := rel.FindAsset("SHA256SUMS")
	if !ok {
		return nil, "", fmt.Errorf("release %s is missing its SHA256SUMS manifest", rel.TagName)
	}

	sumsBytes, err := downloadBytes(ctx, client, sums.BrowserDownloadURL, 1<<20)
	if err != nil {
		return nil, "", fmt.Errorf("fetching checksums: %w", err)
	}
	want, ok := parseSHA256Sums(sumsBytes, assetName)
	if !ok {
		return nil, "", fmt.Errorf("SHA256SUMS does not list %s", assetName)
	}

	body, err := downloadBytes(ctx, client, asset.BrowserDownloadURL, maxUpdateAssetBytes)
	if err != nil {
		return nil, "", fmt.Errorf("downloading %s: %w", assetName, err)
	}
	sum := sha256.Sum256(body)
	got := hex.EncodeToString(sum[:])
	if got != want {
		return nil, "", &DigestMismatch{
			Name: "hop " + rel.TagName, URL: asset.BrowserDownloadURL,
			Algo: "sha256", Want: want, Got: got,
		}
	}
	return body, assetName, nil
}

// parseSHA256Sums finds name's digest in a `sha256sum`-format manifest —
// "<hex digest>  <filename>" per line, an optional leading "*" on the
// filename marking binary mode ignored like every other reader of this
// format ignores it.
func parseSHA256Sums(b []byte, name string) (string, bool) {
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			continue
		}
		if strings.TrimPrefix(fields[1], "*") == name {
			return fields[0], true
		}
	}
	return "", false
}

// ExtractBinary pulls the "hop" executable out of a release tarball. The
// release workflow always packages it as <archive-root>/hop alongside
// LICENSE and README.md; this matches on the base name alone so it doesn't
// depend on knowing the archive's top-level directory name.
func ExtractBinary(tarGz []byte) ([]byte, os.FileMode, error) {
	gz, err := gzip.NewReader(bytes.NewReader(tarGz))
	if err != nil {
		return nil, 0, fmt.Errorf("not a gzip archive: %w", err)
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, 0, fmt.Errorf("reading archive: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg || filepath.Base(hdr.Name) != "hop" {
			continue
		}
		data, err := io.ReadAll(io.LimitReader(tr, maxUpdateAssetBytes))
		if err != nil {
			return nil, 0, err
		}
		mode := hdr.FileInfo().Mode()
		if mode&0o111 == 0 {
			mode |= 0o755 // the archive always marks it executable; this is a floor, not a guess
		}
		return data, mode, nil
	}
	return nil, 0, fmt.Errorf("archive has no hop binary in it")
}

// ReplaceSelf atomically swaps the running executable for newBinary. The
// running process keeps executing off its old inode until it exits — this
// is what makes it safe to call from inside the very binary being replaced.
// The temp file is created in the same directory as the real target so the
// final rename is same-filesystem, and therefore atomic.
func ReplaceSelf(newBinary []byte, mode os.FileMode) (string, error) {
	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating the running binary: %w", err)
	}
	self, err = filepath.EvalSymlinks(self)
	if err != nil {
		return "", fmt.Errorf("resolving the running binary: %w", err)
	}

	dir := filepath.Dir(self)
	tmp, err := os.CreateTemp(dir, ".hop-update-*")
	if err != nil {
		return "", fmt.Errorf("writing the new binary: %w", err)
	}
	tmpPath := tmp.Name()
	defer os.Remove(tmpPath) // no-op once the rename below succeeds

	if _, err := tmp.Write(newBinary); err != nil {
		tmp.Close()
		return "", fmt.Errorf("writing the new binary: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("writing the new binary: %w", err)
	}
	if err := os.Chmod(tmpPath, mode); err != nil {
		return "", fmt.Errorf("making the new binary executable: %w", err)
	}
	if err := os.Rename(tmpPath, self); err != nil {
		return "", fmt.Errorf("installing the new binary over %s: %w", self, err)
	}
	return self, nil
}

// -------------------------------------------------------------- background check ----

// UpdateCheckState is the small on-disk record of when hop last asked
// GitHub about a newer release, so ordinary commands only do that at most
// once a day rather than on every invocation.
type UpdateCheckState struct {
	LastChecked   string `json:"last_checked"` // RFC3339
	LatestVersion string `json:"latest_version,omitempty"`
}

// UpdateStatePath is where that record lives.
func (l *Layout) UpdateStatePath() string { return filepath.Join(l.Cache(), "update-check.json") }

// ReadUpdateCheckState loads the last background check's result. A missing
// or corrupt file just means "never checked", not an error — this is
// advisory bookkeeping, not state hop's correctness depends on.
func ReadUpdateCheckState(l *Layout) UpdateCheckState {
	var st UpdateCheckState
	b, err := os.ReadFile(l.UpdateStatePath())
	if err != nil {
		return st
	}
	_ = json.Unmarshal(b, &st)
	return st
}

// WriteUpdateCheckState persists the result of a background check. Failure
// to write is silently ignored for the same reason a missing file is
// silently tolerated on read: this is a courtesy, not load-bearing state.
func WriteUpdateCheckState(l *Layout, st UpdateCheckState) {
	b, err := json.Marshal(st)
	if err != nil {
		return
	}
	_ = os.WriteFile(l.UpdateStatePath(), b, 0o644)
}
