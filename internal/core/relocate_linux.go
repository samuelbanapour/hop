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
	// Independent of the placeholder-driven relocation below: a python@X.Y
	// formula's own interpreter needs PYTHONHOME set correctly to even
	// bootstrap at all, which no ELF/RPATH patch can provide. See
	// wrapPythonInterpreter's own doc comment for why.
	if err := wrapPythonInterpreter(root, selfFormula, selfVersion, selfStorePath); err != nil {
		return err
	}

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

// wrapPythonInterpreter replaces a python@X.Y formula's own interpreter
// binary (bin/python3.14, for the formula named python@3.14 — the pattern
// generalizes to any future python@X.Y hop adds) with a small shell script
// that sets PYTHONHOME before exec'ing the real binary, renamed alongside
// it as bin/python3.14-real. Every other formula is left untouched.
//
// Homebrew's Linux Python build bakes a literal, non-placeholder absolute
// path (confirmed for real: python@3.14's own `-c "import sys;
// print(sys.base_prefix)"` prints the fixed
// "/home/linuxbrew/.linuxbrew/Cellar/python@3.14/3.14.7", not wherever it
// actually runs from) into its own C-level bootstrap — not a
// @@HOMEBREW_...@@ token any ELF/RPATH patch in this file can reach, since
// it's baked into the interpreter's own path-resolution logic rather than
// a load command or rpath entry. Without a correct PYTHONHOME the
// interpreter can't even import its own "encodings" module and crashes on
// startup — confirmed for every Python-based Homebrew formula that invokes
// it (csvkit, yt-dlp), not just a standalone `python3.14` run directly.
//
// A pyvenv.cfg file dropped next to the binary — CPython's own supported
// mechanism for exactly this class of problem — was tried first and
// verified NOT sufficient: it only takes effect when the interpreter
// binary is invoked directly, because CPython's own pyvenv.cfg lookup is
// relative to however it was actually invoked (argv[0]), not the resolved
// real binary, so a symlink chain from a dependent formula's own launcher
// (csvkit's, yt-dlp's own libexec/bin/python, both already correctly
// relocated by relocateTextFile/rewriteShebang to this binary's exact
// absolute path) never finds it. PYTHONHOME, a process environment
// variable rather than a file lookup, is consulted first regardless of
// invocation path — which is exactly why it has to be set here, at the
// interpreter itself, rather than left to each caller: wrapping this one
// binary fixes every caller transparently without touching any of them,
// since they already all reference this exact path.
//
// PYTHONHOME alone isn't quite enough, though — confirmed by actually
// running yt-dlp with only that set: it fixes sys.base_prefix (so the
// stdlib bootstraps), but per CPython's own documented behavior PYTHONHOME
// also overrides sys.prefix/sys.exec_prefix to match it exactly, which
// silently breaks the mechanism a Python-based formula's own bundled
// dependencies were relying on — before this wrapper existed at all,
// sys.prefix was correctly derived from however the interpreter had been
// invoked (yt-dlp's own libexec/bin/python, not python@3.14's), and
// site.py's automatic site-packages discovery used that to find yt-dlp's
// own vendored packages under <that prefix>/lib/pythonX.Y/site-packages.
// Confirmed empirically (an instrumented copy of this exact wrapper) that
// $0 reliably still carries that caller-specific path — the shebang chain
// preserves it even through the kernel's own script-interpreter handling —
// so the wrapper restores it explicitly via PYTHONPATH rather than trusting
// PYTHONHOME's side effect to get it right.
func wrapPythonInterpreter(root, selfFormula, selfVersion, selfStorePath string) error {
	pyVersion, ok := strings.CutPrefix(selfFormula, "python@")
	if !ok {
		return nil
	}
	binName := "python" + pyVersion

	currentBin := filepath.Join(root, selfFormula, selfVersion, "bin", binName)
	info, err := os.Lstat(currentBin)
	if err != nil || !info.Mode().IsRegular() {
		return nil // no such interpreter binary in this tree
	}
	currentReal := currentBin + "-real"
	if _, err := os.Stat(currentReal); err == nil {
		return nil // already wrapped; relocation only ever runs once per fresh extraction, but no harm in checking
	}
	if err := os.Rename(currentBin, currentReal); err != nil {
		return fmt.Errorf("wrapping %s: %w", binName, err)
	}

	// The wrapper's own content has to name the future final store path,
	// not root (the current staging location) — the same split every
	// other placeholder resolution in this file makes, and for the same
	// reason: this tree gets renamed into place after relocation finishes.
	finalVersionDir := filepath.Join(selfStorePath, selfFormula, selfVersion)
	finalReal := filepath.Join(finalVersionDir, "bin", binName+"-real")
	sitePackagesRel := filepath.Join("lib", binName, "site-packages")
	wrapper := fmt.Sprintf(`#!/bin/sh
export PYTHONHOME=%q
callerlib=$(dirname "$(dirname "$0")")/%s
if [ -d "$callerlib" ]; then
	export PYTHONPATH="$callerlib${PYTHONPATH:+:$PYTHONPATH}"
fi
exec %q "$@"
`, finalVersionDir, sitePackagesRel, finalReal)
	if err := os.WriteFile(currentBin, []byte(wrapper), 0o755); err != nil {
		return fmt.Errorf("writing %s wrapper: %w", binName, err)
	}
	return nil
}

// maxTextRelocateSize mirrors the darwin implementation's bound on how
// large a file can be and still plausibly be a wrapper script worth
// text-scanning for a placeholder, rather than a compiled binary already
// handled above.
const maxTextRelocateSize = 2 << 20

// placeholderPattern mirrors the darwin implementation: the token plus
// whatever path-like characters follow it, so a placeholder embedded
// anywhere in a script (not just where patchelf would report it) is found.
// The character class includes "@" specifically for versioned formula
// names (python@3.14, openssl@3, gcc@13, ...) — without it, a placeholder
// naming one of these truncates right before the "@" and never matches
// parsePlaceholder's expected shape, silently leaving the placeholder
// unresolved. Confirmed for real: python@3.14's own pip3.14 script ships
// a literal, unpatched "#!@@HOMEBREW_CELLAR@@/python@3.14/3.14.7/bin/
// python3.14" shebang without this, and fails to execute at all.
var placeholderPattern = regexp.MustCompile(`@@HOMEBREW_(?:PREFIX|CELLAR)@@[A-Za-z0-9_./@+-]*`)

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
	resolve := func(placeholder string) (string, bool) {
		// A bare "@@HOMEBREW_PREFIX@@/lib" (optionally with a trailing
		// path) names no formula at all — it's Homebrew-on-Linux's other
		// fixed reference to glibc's own shared lib directory, sitting
		// directly under the prefix root rather than under any per-formula
		// opt/Cellar subdirectory, parallel to homebrewLinuxInterpreter's
		// "@@HOMEBREW_PREFIX@@/lib/ld.so". Confirmed on python@3.14's own
		// raw bottle: its rpath's very first entry is exactly this, with
		// nothing for parsePlaceholder's usual formula-extraction to find.
		if placeholder == homebrewLinuxSharedLib || strings.HasPrefix(placeholder, homebrewLinuxSharedLib+"/") {
			if dep, ok := deps["glibc"]; ok {
				rest := strings.TrimPrefix(strings.TrimPrefix(placeholder, homebrewLinuxSharedLib), "/")
				return filepath.Join(dep.storePath, "glibc", dep.version, "lib", rest), true
			}
			return "", false
		}
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

	if err := relocateELFRpath(path, resolve); err != nil {
		return err
	}
	return relocateELFInterpreter(path, deps)
}

// relocateELFRpath rewrites a single ELF file's RPATH/RUNPATH entries. Not
// every ELF file has one at all — most don't, since only dynamically linked
// binaries and shared libraries carry it, and a static binary or a file
// with no Homebrew placeholder in it needs no change.
func relocateELFRpath(path string, resolve func(string) (string, bool)) error {
	out, err := exec.Command("patchelf", "--print-rpath", path).Output()
	if err != nil {
		return nil
	}
	rpath := strings.TrimSpace(string(out))
	if rpath == "" || (!strings.Contains(rpath, prefixToken) && !strings.Contains(rpath, cellarToken)) {
		return nil
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

// homebrewLinuxInterpreter is the fixed, literal ELF interpreter (PT_INTERP)
// path every Homebrew-on-Linux bottle's real executable carries — not a
// per-formula placeholder like @@HOMEBREW_PREFIX@@/opt/<formula>/... (which
// parsePlaceholder already handles), but a reference to Homebrew's own
// bundled glibc rather than the host's, the same string regardless of
// architecture or which formula the binary belongs to. Homebrew's own
// installer resolves it by linking this path into its shared prefix's
// symlink farm; hop has no such shared farm, so it resolves straight to
// glibc's own extracted bin/ld.so — a relative symlink glibc's bottle
// already ships pointing at the real, architecture-specific loader
// (lib/ld-linux-aarch64.so.1, lib/ld-linux-x86-64.so.2, ...), verified by
// inspecting glibc's own bottle contents rather than guessing the
// architecture-specific filename here.
//
// Confirmed for real, not assumed: every executable in a Homebrew Linux
// bottle (htop, nmap, even ncurses' own bundled tset/tput/...) carries
// exactly this string and, without this fix, fails with "cannot execute:
// required file not found" — the kernel can't even find the loader to
// start the program.
const homebrewLinuxInterpreter = "@@HOMEBREW_PREFIX@@/lib/ld.so"

// homebrewLinuxSharedLib is the other fixed, non-formula-specific
// placeholder Homebrew-on-Linux uses: a bare "@@HOMEBREW_PREFIX@@/lib"
// RPATH entry (no /opt/<formula> or /Cellar/<formula>/<version> segment at
// all), referring to glibc's own shared lib directory sitting directly
// under the prefix root. Confirmed directly on python@3.14's raw,
// unmodified bottle: the very first entry of its own declared rpath is
// exactly this string, with nothing for parsePlaceholder's usual
// formula-extraction to find — resolved in relocateOneELF's own resolve
// closure rather than there, since it needs deps["glibc"] specifically
// rather than a formula name parsed out of the placeholder itself.
const homebrewLinuxSharedLib = "@@HOMEBREW_PREFIX@@/lib"

// relocateELFInterpreter rewrites path's PT_INTERP entry when it's the fixed
// Homebrew-on-Linux glibc reference above. Most ELF files aren't real
// executables at all (shared libraries carry no interpreter), so a missing
// or unrelated interpreter is silently left alone.
func relocateELFInterpreter(path string, deps map[string]depLocation) error {
	out, err := exec.Command("patchelf", "--print-interpreter", path).Output()
	if err != nil {
		return nil // not every ELF file is an executable with an interpreter
	}
	if strings.TrimSpace(string(out)) != homebrewLinuxInterpreter {
		return nil
	}
	dep, ok := deps["glibc"]
	if !ok {
		return nil // no glibc in this closure to resolve against; leave as-is rather than guessed at
	}
	resolved := filepath.Join(dep.storePath, "glibc", dep.version, "bin", "ld.so")
	if out, err := exec.Command("patchelf", "--set-interpreter", resolved, path).CombinedOutput(); err != nil {
		return fmt.Errorf("patchelf --set-interpreter: %w: %s", err, strings.TrimSpace(string(out)))
	}

	// Homebrew's own ld.so carries no compiled-in default library search
	// path of its own — verified directly: --print-rpath on glibc's own
	// loader returns empty — so a binary using it as its interpreter needs
	// glibc's own lib directory in its RPATH to find libc.so.6/libm.so.6/
	// etc. at all. Confirmed for real: the interpreter fix above is enough
	// to get the kernel to start the program, but without this it
	// immediately fails with "error while loading shared libraries:
	// libm.so.6: cannot open shared object file".
	glibcLib := filepath.Join(dep.storePath, "glibc", dep.version, "lib")
	rpathOut, err := exec.Command("patchelf", "--print-rpath", path).Output()
	if err != nil {
		return nil // interpreter is patched; a missing rpath here is unusual but not fatal to report
	}
	current := strings.TrimSpace(string(rpathOut))
	for _, entry := range strings.Split(current, ":") {
		if entry == glibcLib {
			return nil // already present, most likely a reused store entry
		}
	}
	newRpath := glibcLib
	if current != "" {
		newRpath = current + ":" + glibcLib
	}
	if out, err := exec.Command("patchelf", "--set-rpath", newRpath, path).CombinedOutput(); err != nil {
		return fmt.Errorf("patchelf --set-rpath (glibc): %w: %s", err, strings.TrimSpace(string(out)))
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
	case strings.HasPrefix(s, prefixToken+"/Cellar/"):
		// A Linux bottle's own self-reference uses this mixed
		// prefix-token-plus-literal-"Cellar" form rather than the bare
		// @@HOMEBREW_CELLAR@@ token below — confirmed directly on
		// python@3.14's raw bottle, whose own rpath names its own lib
		// directory as "@@HOMEBREW_PREFIX@@/Cellar/python@3.14/3.14.7/lib".
		// Structurally identical to the cellarToken case otherwise.
		parts := strings.SplitN(strings.TrimPrefix(s, prefixToken+"/Cellar/"), "/", 3)
		if len(parts) == 3 {
			return parts[0], parts[2], true
		}
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
