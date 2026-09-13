//go:build linux

package core

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// depLocation mirrors the darwin definition: where an already-materialised
// dependency landed, and its version.
type depLocation struct {
	storePath string
	version   string
}

// relocateHomebrewBottle is the Linux counterpart to the darwin
// implementation: a Homebrew-on-Linux bottle carries the same
// @@HOMEBREW_PREFIX@@/@@HOMEBREW_CELLAR@@ placeholder tokens, but baked
// into ELF RPATH/RUNPATH dynamic-section entries rather than Mach-O load
// commands, so it needs patchelf rather than install_name_tool.
//
// Note on verification: this path was written against Homebrew's
// documented Linux bottle layout and patchelf's well-established
// RPATH-rewrite semantics, but — unlike the darwin implementation, proven
// end-to-end by actually running a patched binary — it has not been
// exercised against a real Linux bottle in this environment, since running
// a produced ELF binary needs a Linux host to execute it on. Treat it as
// unverified until it has been.
func relocateHomebrewBottle(root, selfFormula, selfVersion, selfStorePath string, deps map[string]depLocation) error {
	needsELF, needsText := probeForPlaceholders(root)
	// Mirrors the darwin implementation: a "#!/usr/bin/env ruby"-style
	// shebang carries no @@HOMEBREW_...@@ token at all, so the probe above
	// never sees it, but has the same failure mode — hop keeps a
	// dependency-only package off the user's PATH, so `env` would fall
	// through to whatever interpreter (if any) happens to be ambient on
	// the machine instead of the exact one this package needs.
	// Mirrors the darwin implementation: a relative symlink climbing out to
	// Homebrew's shared "opt/<formula>" symlink farm (a Python-based CLI's
	// bundled venv interpreter link, notably) carries no ELF rpath or
	// @@HOMEBREW_...@@ token either, but is exactly as dangling once
	// extracted into hop's own non-sibling store layout.
	if len(deps) > 0 && !needsText {
		needsText = hasShebangCandidate(root) || hasRelocatableSymlink(root)
	}
	if !needsELF && !needsText {
		return nil
	}
	if needsELF {
		if _, err := exec.LookPath("patchelf"); err != nil {
			return fmt.Errorf("this bottle needs relocation but patchelf is not on PATH " +
				"(install it with your distro's package manager, e.g. apt install patchelf)")
		}
	}

	var walkErr error
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			if err := relocateSymlink(path, selfFormula, selfVersion, selfStorePath, deps); err != nil {
				walkErr = fmt.Errorf("%s: %w", path, err)
			}
			return nil
		}
		if looksLikeELF(path) {
			if err := relocateOneELF(path, selfFormula, selfVersion, selfStorePath, deps); err != nil {
				walkErr = fmt.Errorf("%s: %w", path, err)
			}
			return nil
		}
		// Mirrors the darwin implementation: a pure-interpreter formula's
		// bin/<name> launcher script carries the same placeholder tokens as
		// text, not inside an ELF dynamic section, and needs a plain string
		// substitution instead of patchelf.
		info, err := d.Info()
		if err != nil || info.Size() > maxTextRelocateSize {
			return nil
		}
		if err := relocateTextFile(path, info.Mode(), selfFormula, selfVersion, selfStorePath, deps); err != nil {
			walkErr = fmt.Errorf("%s: %w", path, err)
		}
		return nil
	})
	return walkErr
}

// maxTextRelocateSize mirrors the darwin implementation's bound on how
// large a file can be and still plausibly be a wrapper script worth
// text-scanning for a placeholder, rather than a compiled binary already
// handled above.
const maxTextRelocateSize = 2 << 20

// placeholderPattern mirrors the darwin implementation: the token plus
// whatever path-like characters follow it, so a placeholder embedded
// anywhere in a script (not just where patchelf would report it) is found.
var placeholderPattern = regexp.MustCompile(`@@HOMEBREW_(?:PREFIX|CELLAR)@@[A-Za-z0-9_./+-]*`)

// relocateTextFile mirrors the darwin implementation exactly: it rewrites
// both literal @@HOMEBREW_...@@ placeholder references and "#!/usr/bin/env
// <interp>" shebang lines in a plain-text file.
func relocateTextFile(path string, mode os.FileMode, selfFormula, selfVersion, selfStorePath string, deps map[string]depLocation) error {
	data, err := os.ReadFile(path)
	if err != nil {
		// An unreadable file can't be a placeholder-carrying script either
		// (it couldn't be interpreted as one at runtime); skip rather than
		// fail the whole install over it.
		return nil
	}
	changed := false

	if bytes.Contains(data, []byte(prefixToken)) || bytes.Contains(data, []byte(cellarToken)) {
		data = placeholderPattern.ReplaceAllFunc(data, func(m []byte) []byte {
			formula, rest, ok := parsePlaceholder(string(m))
			if !ok {
				return m
			}
			var resolved string
			if formula == selfFormula {
				resolved = filepath.Join(selfStorePath, selfFormula, selfVersion, rest)
			} else if dep, ok := deps[formula]; ok {
				resolved = filepath.Join(dep.storePath, formula, dep.version, rest)
			} else {
				return m
			}
			changed = true
			return []byte(resolved)
		})
	}

	if newData, ok := rewriteShebang(data, selfFormula, selfVersion, selfStorePath, deps); ok {
		data = newData
		changed = true
	}

	if !changed {
		return nil
	}
	// Some wrapper scripts ship read-only (no owner-write bit); Unix
	// permission bits are enforced against the owner too, so the write
	// below would otherwise fail. Grant owner-write just long enough to
	// rewrite it, then restore the mode exactly as extracted.
	if mode&0o200 == 0 {
		if err := os.Chmod(path, mode|0o200); err != nil {
			return err
		}
		defer os.Chmod(path, mode)
	}
	return os.WriteFile(path, data, mode)
}

// shebangEnvPattern mirrors the darwin implementation exactly.
var shebangEnvPattern = regexp.MustCompile(`^#!\s*/usr/bin/env\s+(?:-S\s+)?(\S+)`)

// hasShebangCandidate mirrors the darwin implementation exactly.
func hasShebangCandidate(root string) bool {
	found := false
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if found || err != nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() < 15 || info.Size() > maxTextRelocateSize {
			return nil
		}
		f, err := os.Open(path)
		if err != nil {
			return nil
		}
		var buf [64]byte
		n, _ := f.Read(buf[:])
		f.Close()
		if bytes.HasPrefix(buf[:n], []byte("#!/usr/bin/env ")) || bytes.HasPrefix(buf[:n], []byte("#! /usr/bin/env ")) {
			found = true
		}
		return nil
	})
	return found
}

// optSymlinkPattern and cellarSymlinkPattern mirror the darwin
// implementation exactly.
var optSymlinkPattern = regexp.MustCompile(`^(?:\.\./)+opt/([^/]+)/(.*)$`)
var cellarSymlinkPattern = regexp.MustCompile(`^(?:\.\./)+Cellar/([^/]+)/[^/]+/(.*)$`)

// hasRelocatableSymlink mirrors the darwin implementation exactly.
func hasRelocatableSymlink(root string) bool {
	found := false
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if found || err != nil || d.Type()&os.ModeSymlink == 0 {
			return nil
		}
		target, err := os.Readlink(path)
		if err != nil {
			return nil
		}
		if optSymlinkPattern.MatchString(target) || cellarSymlinkPattern.MatchString(target) {
			found = true
		}
		return nil
	})
	return found
}

// relocateSymlink mirrors the darwin implementation exactly.
func relocateSymlink(path, selfFormula, selfVersion, selfStorePath string, deps map[string]depLocation) error {
	target, err := os.Readlink(path)
	if err != nil {
		return nil
	}
	var formula, rest string
	if m := optSymlinkPattern.FindStringSubmatch(target); m != nil {
		formula, rest = m[1], m[2]
	} else if m := cellarSymlinkPattern.FindStringSubmatch(target); m != nil {
		formula, rest = m[1], m[2]
	} else {
		return nil
	}

	var resolved string
	if formula == selfFormula {
		resolved = filepath.Join(selfStorePath, selfFormula, selfVersion, rest)
	} else if dep, ok := deps[formula]; ok {
		resolved = filepath.Join(dep.storePath, formula, dep.version, rest)
	} else {
		return nil
	}

	if err := os.Remove(path); err != nil {
		return err
	}
	return os.Symlink(resolved, path)
}

// rewriteShebang mirrors the darwin implementation exactly.
func rewriteShebang(data []byte, selfFormula, selfVersion, selfStorePath string, deps map[string]depLocation) ([]byte, bool) {
	nl := bytes.IndexByte(data, '\n')
	line := data
	if nl >= 0 {
		line = data[:nl]
	}
	loc := shebangEnvPattern.FindSubmatchIndex(line)
	if loc == nil {
		return data, false
	}
	interp := string(line[loc[2]:loc[3]])

	resolve := func(storePath, formula, version string) (string, bool) {
		p := filepath.Join(storePath, formula, version, "bin", interp)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
		return "", false
	}

	resolved, ok := resolve(selfStorePath, selfFormula, selfVersion)
	if !ok {
		for formula, dep := range deps {
			if p, found := resolve(dep.storePath, formula, dep.version); found {
				resolved = p
				ok = true
				break
			}
		}
	}
	if !ok {
		return data, false
	}

	out := make([]byte, 0, len(data)+len(resolved))
	out = append(out, "#!"...)
	out = append(out, resolved...)
	out = append(out, line[loc[1]:]...)
	if nl >= 0 {
		out = append(out, data[nl:]...)
	}
	return out, true
}

func looksLikeELF(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := f.Read(magic[:]); err != nil {
		return false
	}
	return magic == [4]byte{0x7f, 'E', 'L', 'F'}
}

const (
	prefixToken = "@@HOMEBREW_PREFIX@@"
	cellarToken = "@@HOMEBREW_CELLAR@@"
)

// relocateOneELF rewrites a single ELF file's RPATH/RUNPATH entries.
// patchelf reports the current one via --print-rpath as a colon-separated
// list; each Homebrew placeholder entry in it is resolved and replaced,
// entries that resolve to nothing are dropped rather than left dangling.
func relocateOneELF(path, selfFormula, selfVersion, selfStorePath string, deps map[string]depLocation) error {
	out, err := exec.Command("patchelf", "--print-rpath", path).Output()
	if err != nil {
		return nil // not every ELF file has an rpath at all; that's fine
	}
	rpath := strings.TrimSpace(string(out))
	if rpath == "" || (!strings.Contains(rpath, prefixToken) && !strings.Contains(rpath, cellarToken)) {
		return nil
	}

	resolve := func(placeholder string) (string, bool) {
		formula, rest, ok := parsePlaceholder(placeholder)
		if !ok {
			return "", false
		}
		if formula == selfFormula {
			return filepath.Join(selfStorePath, selfFormula, selfVersion, rest), true
		}
		if dep, ok := deps[formula]; ok {
			return filepath.Join(dep.storePath, formula, dep.version, rest), true
		}
		return "", false
	}

	var newEntries []string
	changed := false
	for _, entry := range strings.Split(rpath, ":") {
		if strings.HasPrefix(entry, prefixToken) || strings.HasPrefix(entry, cellarToken) {
			if resolved, ok := resolve(entry); ok {
				newEntries = append(newEntries, resolved)
				changed = true
				continue
			}
			changed = true // dropping an unresolvable placeholder still counts as a change
			continue
		}
		newEntries = append(newEntries, entry)
	}
	if !changed {
		return nil
	}

	if out, err := exec.Command("patchelf", "--set-rpath", strings.Join(newEntries, ":"), path).CombinedOutput(); err != nil {
		return fmt.Errorf("patchelf --set-rpath: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// parsePlaceholder mirrors the darwin implementation exactly; duplicated
// rather than shared across the build-tagged files to keep each platform's
// relocation logic self-contained and independently readable.
func parsePlaceholder(s string) (formula, rest string, ok bool) {
	switch {
	case strings.HasPrefix(s, prefixToken+"/opt/"):
		parts := strings.SplitN(strings.TrimPrefix(s, prefixToken+"/opt/"), "/", 2)
		if len(parts) == 2 {
			return parts[0], parts[1], true
		}
		return parts[0], "", true
	case strings.HasPrefix(s, cellarToken+"/"):
		parts := strings.SplitN(strings.TrimPrefix(s, cellarToken+"/"), "/", 3)
		if len(parts) == 3 {
			return parts[0], parts[2], true
		}
	}
	return "", "", false
}

func probeForPlaceholders(root string) (needsELF, needsText bool) {
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if needsELF && needsText {
			return nil
		}
		if err != nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		isELF := looksLikeELF(path)
		if !isELF && info.Size() > maxTextRelocateSize {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if bytes.Contains(data, []byte(prefixToken)) || bytes.Contains(data, []byte(cellarToken)) {
			if isELF {
				needsELF = true
			} else {
				needsText = true
			}
		}
		return nil
	})
	return needsELF, needsText
}
