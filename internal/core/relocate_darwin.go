//go:build darwin

package core

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// depLocation is where an already-materialised dependency's package tree
// landed, and its version — enough to reconstruct the exact absolute path a
// Homebrew placeholder reference is asking for.
type depLocation struct {
	storePath string
	version   string
}

// relocateHomebrewBottle patches every Mach-O binary and dylib under root
// that still carries one of Homebrew's build-time placeholder tokens
// (@@HOMEBREW_PREFIX@@, @@HOMEBREW_CELLAR@@) into the real absolute path
// hop actually extracted that dependency to, then re-signs whatever it
// changed.
//
// Homebrew's own installer does this same rewrite via install_name_tool
// when it pours a bottle into /opt/homebrew or /usr/local — a bottle is
// built once and relocated at install time precisely so it can land in
// different prefixes on different machines. hop's prefix is its own
// content-addressed store rather than a shared Homebrew tree, so the
// mechanism is the same but the target path comes from hop's own package
// map instead of a single fixed prefix.
//
// self is the package currently being materialised: a placeholder
// referencing its own formula name is a self-reference (a dylib's own
// LC_ID_DYLIB, or one output library pointing at a sibling output library
// in the same formula) and resolves against selfStorePath instead of deps.
func relocateHomebrewBottle(root, selfFormula, selfVersion, selfStorePath string, deps map[string]depLocation) error {
	// The overwhelming majority of what hop installs is not a Homebrew
	// bottle at all; this cheap scan means paying nothing extra for those,
	// and turns a missing toolchain into one clear error only when
	// relocation is actually needed rather than a confusing per-file one.
	if !probeForPlaceholders(root) {
		return nil
	}
	if !hasInstallNameTool() {
		return fmt.Errorf("this bottle needs relocation but install_name_tool is not on PATH " +
			"(install Xcode's command-line tools: xcode-select --install)")
	}

	var walkErr error
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() < 4 {
			return nil
		}
		if !looksLikeMachO(path) {
			return nil
		}
		if err := relocateOne(path, selfFormula, selfVersion, selfStorePath, deps); err != nil {
			// One file failing to patch shouldn't sink an otherwise-working
			// install; record it and let the caller decide how loud to be.
			walkErr = fmt.Errorf("%s: %w", path, err)
		}
		return nil
	})
	return walkErr
}

// looksLikeMachO checks the file's magic number rather than trusting a file
// extension or executable bit, both of which a bottle's non-binary files
// (scripts, docs) can also carry.
func looksLikeMachO(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var magic [4]byte
	if _, err := f.Read(magic[:]); err != nil {
		return false
	}
	switch [4]byte(magic) {
	case [4]byte{0xfe, 0xed, 0xfa, 0xce}, // MH_MAGIC (32-bit, big-endian header on disk)
		[4]byte{0xce, 0xfa, 0xed, 0xfe}, // MH_CIGAM
		[4]byte{0xfe, 0xed, 0xfa, 0xcf}, // MH_MAGIC_64
		[4]byte{0xcf, 0xfa, 0xed, 0xfe}, // MH_CIGAM_64
		[4]byte{0xca, 0xfe, 0xba, 0xbe}, // FAT_MAGIC (universal binary)
		[4]byte{0xbe, 0xba, 0xfe, 0xca}: // FAT_CIGAM
		return true
	}
	return false
}

// placeholderPrefixes are Homebrew's own two build-time tokens. A
// @@HOMEBREW_PREFIX@@ reference is prefix-relative ("opt/<formula>/...",
// the /opt symlink farm into the Cellar); a @@HOMEBREW_CELLAR@@ reference
// already names the version directly ("<formula>/<version>/...").
const (
	prefixToken = "@@HOMEBREW_PREFIX@@"
	cellarToken = "@@HOMEBREW_CELLAR@@"
)

// relocateOne patches a single Mach-O file's load commands in place.
func relocateOne(path, selfFormula, selfVersion, selfStorePath string, deps map[string]depLocation) error {
	refs, selfID, err := machOPlaceholders(path)
	if err != nil {
		return err
	}
	if len(refs) == 0 && selfID == "" {
		return nil // nothing to do; the common case for most files
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

	var args []string
	changed := false

	if selfID != "" {
		if newPath, ok := resolve(selfID); ok {
			args = append(args, "-id", newPath)
			changed = true
		}
	}
	for _, old := range refs {
		newPath, ok := resolve(old)
		if !ok {
			continue // an unresolvable reference is left as-is rather than guessed at
		}
		args = append(args, "-change", old, newPath)
		changed = true
	}
	if !changed {
		return nil
	}

	args = append(args, path)
	if out, err := exec.Command("install_name_tool", args...).CombinedOutput(); err != nil {
		return fmt.Errorf("install_name_tool: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// install_name_tool invalidates any existing code signature; an
	// ad-hoc re-sign is what Homebrew's own pour step does too; without it
	// Gatekeeper on Apple Silicon refuses to run the binary at all.
	if out, err := exec.Command("codesign", "--sign", "-", "--force", path).CombinedOutput(); err != nil {
		return fmt.Errorf("codesign: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// parsePlaceholder splits a resolved placeholder path into the formula name
// it names and everything after it — e.g.
// "@@HOMEBREW_PREFIX@@/opt/ncurses/lib/libncursesw.6.dylib" splits into
// ("ncurses", "lib/libncursesw.6.dylib"); "@@HOMEBREW_CELLAR@@/ncurses/6.6/lib/x"
// splits into ("ncurses", "lib/x") — the CELLAR form's own version segment
// is dropped since the caller re-derives it from the resolved package's
// actual installed version, which is authoritative.
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

// machOPlaceholders shells out to otool to list a Mach-O file's dependent
// libraries (-L) and its own install name if it is a dylib (-D), returning
// only the entries that still carry an unresolved Homebrew placeholder.
// otool ships with Xcode's command-line tools, the same toolchain
// install_name_tool itself requires, so there is no new dependency here
// beyond what patching already needs.
func machOPlaceholders(path string) (refs []string, selfID string, err error) {
	out, err := exec.Command("otool", "-L", path).Output()
	if err != nil {
		return nil, "", fmt.Errorf("otool -L: %w", err)
	}
	for _, line := range strings.Split(string(out), "\n")[1:] { // first line is the filename itself
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		field := strings.Fields(line)
		if len(field) == 0 {
			continue
		}
		if strings.HasPrefix(field[0], prefixToken) || strings.HasPrefix(field[0], cellarToken) {
			refs = append(refs, field[0])
		}
	}

	idOut, err := exec.Command("otool", "-D", path).Output()
	if err == nil {
		lines := strings.Split(string(idOut), "\n")
		if len(lines) > 1 {
			id := strings.TrimSpace(lines[1])
			if strings.HasPrefix(id, prefixToken) || strings.HasPrefix(id, cellarToken) {
				selfID = id
			}
		}
	}
	return refs, selfID, nil
}

// hasInstallNameTool reports whether this host can perform relocation at
// all, so callers can turn a missing toolchain into one clear upfront error
// instead of a confusing per-file failure the first time a placeholder is
// actually found.
func hasInstallNameTool() bool {
	_, err := exec.LookPath("install_name_tool")
	return err == nil
}

// probeForPlaceholders does a fast pre-check across a tree, used to decide
// whether relocation (and its toolchain requirement) is needed at all — the
// overwhelming majority of artifacts hop installs are not Homebrew bottles
// and contain no placeholder, so this must stay cheap.
func probeForPlaceholders(root string) bool {
	found := false
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if found || err != nil || d.IsDir() {
			return nil
		}
		if !looksLikeMachO(path) {
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
