//go:build linux

package core

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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
	if !probeForPlaceholders(root) {
		return nil
	}
	if _, err := exec.LookPath("patchelf"); err != nil {
		return fmt.Errorf("this bottle needs relocation but patchelf is not on PATH " +
			"(install it with your distro's package manager, e.g. apt install patchelf)")
	}

	var walkErr error
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if !looksLikeELF(path) {
			return nil
		}
		if err := relocateOneELF(path, selfFormula, selfVersion, selfStorePath, deps); err != nil {
			walkErr = fmt.Errorf("%s: %w", path, err)
		}
		return nil
	})
	return walkErr
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

func probeForPlaceholders(root string) bool {
	found := false
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if found || err != nil || d.IsDir() {
			return nil
		}
		if !looksLikeELF(path) {
			return nil
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if bytes.Contains(data, []byte(prefixToken)) || bytes.Contains(data, []byte(cellarToken)) {
			found = true
		}
		return nil
	})
	return found
}
