package core

import (
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// writeFileAtomic writes data to path via a temporary file and a rename, so a
// crash or a full disk can never leave a half-written manifest behind. Disk
// exhaustion is a live concern on the volumes hop runs on, so the temp file is
// always cleaned up on failure.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".tmp*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer func() {
		if err != nil {
			f.Close()
			os.Remove(tmp)
		}
	}()

	if _, err = f.Write(data); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Chmod(tmp, perm); err != nil {
		return err
	}
	err = os.Rename(tmp, path)
	return err
}

// writeJSONAtomic marshals v as indented JSON and writes it atomically.
func writeJSONAtomic(path string, v any, perm os.FileMode) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeFileAtomic(path, append(b, '\n'), perm)
}

// readJSON loads and decodes a JSON file.
func readJSON(path string, v any) error {
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, v)
}

// replaceSymlink points link at target atomically: symlinks cannot be
// rewritten in place, so it creates a sibling and renames over. This one
// operation is what makes activation and rollback atomic.
func replaceSymlink(target, link string) error {
	dir := filepath.Dir(link)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp := filepath.Join(dir, "."+filepath.Base(link)+".new")
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("creating symlink %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, link); err != nil {
		os.Remove(tmp)
		return fmt.Errorf("activating symlink %s: %w", link, err)
	}
	return nil
}

// isSymlink reports whether path is a symlink, without following it.
func isSymlink(path string) bool {
	fi, err := os.Lstat(path)
	return err == nil && fi.Mode()&os.ModeSymlink != 0
}

// exists reports whether path exists, following symlinks.
func exists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// lexists reports whether path exists as a directory entry, even if it is a
// dangling symlink.
func lexists(path string) bool {
	_, err := os.Lstat(path)
	return err == nil
}

// hashFileAll returns the lowercase hex SHA-256, SHA-512 and SHA-1 of a
// file's contents in a single pass, since upstreams disagree on which one
// they publish for verification.
func hashFileAll(path string) (digests, error) {
	f, err := os.Open(path)
	if err != nil {
		return digests{}, err
	}
	defer f.Close()
	h256, h512, h1 := sha256.New(), sha512.New(), sha1.New()
	if _, err := io.Copy(io.MultiWriter(h256, h512, h1), f); err != nil {
		return digests{}, err
	}
	return digests{
		sha256: hex.EncodeToString(h256.Sum(nil)),
		sha512: hex.EncodeToString(h512.Sum(nil)),
		sha1:   hex.EncodeToString(h1.Sum(nil)),
	}, nil
}

// dirSize sums the apparent size of a tree, skipping symlinks so a store path
// is never double-counted through its own links.
func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // a partially-removed tree should still report a size
		}
		if d.IsDir() {
			return nil
		}
		fi, err := d.Info()
		if err != nil || fi.Mode()&os.ModeSymlink != 0 {
			return nil
		}
		total += fi.Size()
		return nil
	})
	return total, err
}

// freeSpace reports the bytes available on the filesystem holding path.
func freeSpace(path string) (int64, error) { return statfsAvail(path) }

// isAppleDouble reports whether name is a macOS AppleDouble sidecar. These
// appear for every file written to exFAT/FAT volumes and must never be
// treated as package content: they break extraction and binary discovery.
func isAppleDouble(name string) bool {
	b := filepath.Base(name)
	return strings.HasPrefix(b, "._") || b == ".DS_Store"
}

// removeTree deletes a tree, first clearing read-only bits that extracted
// archives sometimes carry, which otherwise make os.RemoveAll fail.
func removeTree(root string) error {
	if !lexists(root) {
		return nil
	}
	if err := os.RemoveAll(root); err == nil {
		return nil
	}
	_ = filepath.WalkDir(root, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.IsDir() {
			_ = os.Chmod(p, 0o755)
		} else {
			_ = os.Chmod(p, 0o644)
		}
		return nil
	})
	return os.RemoveAll(root)
}

// safeJoin joins root and an archive-supplied relative path, refusing any
// result that escapes root. This is hop's defence against the classic
// "../../.ssh/authorized_keys" tarball, and against absolute member paths.
func safeJoin(root, rel string) (string, error) {
	if rel == "" {
		return "", fmt.Errorf("empty path in archive")
	}
	if filepath.IsAbs(rel) || strings.HasPrefix(rel, "/") || strings.HasPrefix(rel, `\`) {
		return "", fmt.Errorf("archive contains absolute path %q", rel)
	}
	if strings.Contains(rel, "\x00") {
		return "", fmt.Errorf("archive path contains a NUL byte")
	}
	clean := filepath.Clean(rel)
	if clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("archive path %q escapes the destination", rel)
	}
	joined := filepath.Join(root, clean)
	// Re-verify after joining, which also catches symlink-free trickery.
	rootAbs := filepath.Clean(root) + string(filepath.Separator)
	if joined != filepath.Clean(root) && !strings.HasPrefix(joined, rootAbs) {
		return "", fmt.Errorf("archive path %q escapes the destination", rel)
	}
	return joined, nil
}
