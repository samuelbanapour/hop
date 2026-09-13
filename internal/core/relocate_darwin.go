//go:build darwin

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
	needsMachO, needsText := probeForPlaceholders(root)
	// A "#!/usr/bin/env ruby"-style shebang carries no @@HOMEBREW_...@@
	// token at all, so probeForPlaceholders never sees it — but it has the
	// exact same failure mode: hop deliberately keeps a dependency-only
	// package like ruby off the user's PATH (only the formula that was
	// actually requested gets PATH-linked), so `env` falls through to
	// whatever ruby happens to be ambient on the machine instead of the
	// exact version this package was built against. Homebrew's own
	// installer rewrites these shebangs to an absolute path for the same
	// reason. Only worth the extra scan when there is a dependency closure
	// at all to resolve such a shebang against.
	// A relative symlink like
	// "../../../../../opt/python@3.14/bin/python3.14" (yt-dlp's bundled
	// venv, notably) assumes Homebrew's own fixed Cellar tree, where every
	// formula lives as a sibling under one shared prefix and "opt/<formula>"
	// is a version-independent symlink-farm entry pointing at whichever
	// version is current. hop extracts each formula into its own separate,
	// non-sibling store directory, so a symlink like this is dangling the
	// moment it's extracted — the same failure class as the
	// @@HOMEBREW_...@@ placeholders, just expressed as a symlink target
	// rather than embedded text.
	if len(deps) > 0 && !needsText {
		needsText = hasShebangCandidate(root) || hasRelocatableSymlink(root)
	}
	if !needsMachO && !needsText {
		return nil
	}
	if needsMachO && !hasInstallNameTool() {
		return fmt.Errorf("this bottle needs relocation but install_name_tool is not on PATH " +
			"(install Xcode's command-line tools: xcode-select --install)")
	}

	var walkErr error
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if d.Type()&os.ModeSymlink != 0 {
			// os.Readlink never follows the link, so this is safe even when
			// the target is (or resolves through) a directory — unlike the
			// os.ReadFile calls below, which is exactly why a symlink can't
			// simply fall through to the same text-file handling.
			if err := relocateSymlink(path, selfFormula, selfVersion, selfStorePath, deps); err != nil {
				walkErr = fmt.Errorf("%s: %w", path, err)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil || info.Size() < 4 {
			return nil
		}
		if looksLikeMachO(path) {
			if err := relocateOne(path, selfFormula, selfVersion, selfStorePath, deps); err != nil {
				// One file failing to patch shouldn't sink an otherwise-working
				// install; record it and let the caller decide how loud to be.
				walkErr = fmt.Errorf("%s: %w", path, err)
			}
			return nil
		}
		// Not a compiled binary: Homebrew also bakes the same placeholder
		// tokens into plain-text wrapper scripts (a pure-Ruby/Python gem's
		// bin/<name> launcher setting GEM_HOME or exec'ing an interpreter by
		// absolute path is the common case — lolcat, among others). Those
		// need a straight string substitution instead of install_name_tool,
		// and unlike a Mach-O file the substitution is free to change the
		// file's length.
		if info.Size() <= maxTextRelocateSize {
			if err := relocateTextFile(path, info.Mode(), selfFormula, selfVersion, selfStorePath, deps); err != nil {
				walkErr = fmt.Errorf("%s: %w", path, err)
			}
		}
		return nil
	})
	return walkErr
}

// maxTextRelocateSize bounds the text-placeholder pass to files a wrapper
// script could plausibly be. A file larger than this is either a real
// Mach-O binary (already handled above) or something Homebrew's own
// installer wouldn't text-relocate either, so skipping it keeps the pass
// cheap for the common case of a tree with a handful of large binaries and
// no scripts at all.
const maxTextRelocateSize = 2 << 20

// placeholderPattern matches a full placeholder reference — the token plus
// whatever path-like characters follow it — so relocateTextFile can find and
// replace occurrences embedded anywhere inside a script, not just ones
// otool would have reported as a load command.
// The character class includes "@" specifically for versioned formula
// names (python@3.14, openssl@3, gcc@13, ...) — without it, a placeholder
// naming one of these truncates right before the "@" and never matches
// parsePlaceholder's expected shape, silently leaving the placeholder
// unresolved. Confirmed for real on the Linux side (python@3.14's own
// pip3.14 script ships a literal, unpatched "#!@@HOMEBREW_CELLAR@@/
// python@3.14/3.14.7/bin/python3.14" shebang without this, and fails to
// execute at all); fixed here too since the regex is otherwise identical
// and the same versioned-formula-name shebangs can appear in any bottle.
var placeholderPattern = regexp.MustCompile(`@@HOMEBREW_(?:PREFIX|CELLAR)@@[A-Za-z0-9_./@+-]*`)

// relocateTextFile rewrites both kinds of text-level indirection Homebrew
// bottles carry: literal @@HOMEBREW_...@@ placeholder references (a
// shell/Ruby/Python wrapper script, a .pc file, etc.) and "#!/usr/bin/env
// <interp>" shebang lines that need pinning to this package's own
// dependency closure. Either, both, or neither may apply to a given file.
func relocateTextFile(path string, mode os.FileMode, selfFormula, selfVersion, selfStorePath string, deps map[string]depLocation) error {
	data, err := os.ReadFile(path)
	if err != nil {
		// An unreadable file (Ruby gem test fixtures occasionally ship
		// mode-0000 files on purpose, e.g. to exercise permission-denied
		// behavior) can't be a placeholder-carrying script either — if it
		// can't be read, it can't be interpreted as a script at runtime, so
		// skipping it costs nothing.
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
				return m // unresolvable, leave as-is rather than guessed at
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
	// Homebrew ships some wrapper scripts read-only (r-xr-xr-x — lolcat's
	// bin/lolcat, notably), which blocks the write below even for its
	// owner: Unix permission bits are enforced against the owner too. Grant
	// owner-write just long enough to rewrite it, then put the original
	// mode back so the file's permissions end up exactly as extracted.
	if mode&0o200 == 0 {
		if err := os.Chmod(path, mode|0o200); err != nil {
			return err
		}
		defer os.Chmod(path, mode)
	}
	return os.WriteFile(path, data, mode)
}

// shebangEnvPattern matches a standard "#!/usr/bin/env [-S] <interpreter>"
// shebang line, capturing just the interpreter name — the one piece needed
// to look it up in this package's own dependency closure.
var shebangEnvPattern = regexp.MustCompile(`^#!\s*/usr/bin/env\s+(?:-S\s+)?(\S+)`)

// optSymlinkPattern and cellarSymlinkPattern match a relative symlink target
// that climbs out of a formula's own version directory to reach Homebrew's
// shared "opt/<formula>" symlink-farm entry or another formula's Cellar
// entry directly — the pattern Homebrew's own bottles use for cross-formula
// references that aren't in a Mach-O load command or embedded text (a
// Python-based CLI's bundled venv interpreter symlink, notably).
var optSymlinkPattern = regexp.MustCompile(`^(?:\.\./)+opt/([^/]+)/(.*)$`)
var cellarSymlinkPattern = regexp.MustCompile(`^(?:\.\./)+Cellar/([^/]+)/[^/]+/(.*)$`)

// hasRelocatableSymlink does a cheap Readlink-only scan across a tree for
// any symlink matching the Homebrew sibling-Cellar pattern, used to decide
// whether relocateHomebrewBottle's full walk is worth paying for when no
// literal placeholder or env shebang was found either.
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

// relocateSymlink rewrites a Homebrew sibling-Cellar-relative symlink
// target to an absolute path inside hop's own store, the same resolution
// rules used everywhere else in this file. A target that doesn't match
// either pattern is left exactly as extracted — it's either a legitimate
// same-directory link (lolcat's bin/python -> python3.14 pattern, still
// correct under hop's layout since both ends move together) or something
// this pass doesn't understand, and guessing at it would be worse than
// leaving it dangling in an already-broken way.
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
		return nil // unresolvable, leave as-is rather than guessed at
	}

	if err := os.Remove(path); err != nil {
		return err
	}
	return os.Symlink(resolved, path)
}

// hasShebangCandidate does a cheap first-bytes-only scan across a tree for
// any file that might carry an env shebang, used purely to decide whether
// relocateHomebrewBottle's full walk is worth paying for at all when no
// literal @@HOMEBREW_...@@ placeholder was found either.
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

// rewriteShebang resolves a "#!/usr/bin/env <interpreter>" line to an
// absolute path when <interpreter> is a binary this package's own
// dependency closure (or the package itself) actually provides — mirroring
// what Homebrew's own installer does for every Ruby/Python/Perl-gem-based
// formula it pours. Relying on PATH here would pick up whatever
// interpreter, if any, happens to be ambient on the user's machine rather
// than the exact version this package was built and tested against: hop
// deliberately keeps a dependency-only package off the user's PATH (only
// the formula actually requested gets PATH-linked), so an unrewritten `env`
// shebang would silently run against a different — or entirely absent —
// interpreter than the one this package needs.
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
		return data, false // no dependency provides this interpreter; leave as-is rather than guessed at
	}

	out := make([]byte, 0, len(data)+len(resolved))
	out = append(out, "#!"...)
	out = append(out, resolved...)
	out = append(out, line[loc[1]:]...) // any trailing args on the shebang line, preserved verbatim
	if nl >= 0 {
		out = append(out, data[nl:]...)
	}
	return out, true
}

// looksLikeMachO checks the file's magic number rather than trusting a file
// extension or executable bit, both of which a bottle's non-binary files
// (scripts, docs) can also carry.
//
// 0xCAFEBABE is famously ambiguous: it's both Mach-O's FAT_MAGIC (a
// universal binary's header) and the unrelated magic number every Java
// .class file starts with — and Homebrew bottles that touch Java (nmap's
// bundled NSE scripts, notably) really do ship .class files. A real fat
// Mach-O header's next four bytes are nfat_arch — the number of
// architecture slices, always small (2-6 in practice; no real universal
// binary has more than a handful) — while a class file's are its
// major/minor version, and Java's major version alone has started at 45
// since JDK 1.1, comfortably clear of any real nfat_arch. Ten is a
// deliberately generous cutoff between the two.
func looksLikeMachO(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	var header [8]byte
	n, err := f.Read(header[:])
	if err != nil || n < 4 {
		return false
	}
	magic := [4]byte{header[0], header[1], header[2], header[3]}
	switch magic {
	case [4]byte{0xfe, 0xed, 0xfa, 0xce}, // MH_MAGIC (32-bit, big-endian header on disk)
		[4]byte{0xce, 0xfa, 0xed, 0xfe}, // MH_CIGAM
		[4]byte{0xfe, 0xed, 0xfa, 0xcf}, // MH_MAGIC_64
		[4]byte{0xcf, 0xfa, 0xed, 0xfe}: // MH_CIGAM_64
		return true
	case [4]byte{0xca, 0xfe, 0xba, 0xbe}: // FAT_MAGIC — on-disk Mach-O fat headers are always big-endian, so FAT_CIGAM never appears here
		if n < 8 {
			return false
		}
		nfatArch := uint32(header[4])<<24 | uint32(header[5])<<16 | uint32(header[6])<<8 | uint32(header[7])
		return nfatArch <= 10
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
// and contain no placeholder, so this must stay cheap. It reports the two
// kinds of file relocateHomebrewBottle knows how to patch separately, since
// only the Mach-O case needs install_name_tool on PATH.
func probeForPlaceholders(root string) (needsMachO, needsText bool) {
	filepath.WalkDir(root, func(path string, d os.DirEntry, err error) error {
		if needsMachO && needsText {
			return nil
		}
		if err != nil || d.IsDir() || d.Type()&os.ModeSymlink != 0 {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return nil
		}
		isMachO := looksLikeMachO(path)
		if !isMachO && info.Size() > maxTextRelocateSize {
			return nil // too large to be a wrapper script, and not a binary this probes
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		if bytes.Contains(data, []byte(prefixToken)) || bytes.Contains(data, []byte(cellarToken)) {
			if isMachO {
				needsMachO = true
			} else {
				needsText = true
			}
		}
		return nil
	})
	return needsMachO, needsText
}
