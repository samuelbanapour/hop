package core

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// HopfileName is the per-project manifest, and LockfileName its resolved
// counterpart. Together they make an environment reproducible: the hopfile
// records intent, the lockfile records exactly what that resolved to.
const (
	HopfileName  = "hopfile.toml"
	LockfileName = "hop.lock"
)

// Hopfile is a declarative description of a project's tools.
type Hopfile struct {
	Path     string
	Packages map[string]string // name → version constraint
	Prune    bool              // remove packages absent from the file
	Jobs     int               // default parallelism
}

// FindHopfile walks up from dir looking for a hopfile, the way build tools
// find their config, so `hop sync` works from anywhere inside a project.
func FindHopfile(dir string) (string, bool) {
	cur, err := filepath.Abs(dir)
	if err != nil {
		return "", false
	}
	for {
		p := filepath.Join(cur, HopfileName)
		if exists(p) {
			return p, true
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return "", false
		}
		cur = parent
	}
}

// LoadHopfile parses a hopfile.
func LoadHopfile(path string) (*Hopfile, error) {
	doc, err := parseTOML(path)
	if err != nil {
		return nil, err
	}
	h := &Hopfile{Path: path, Packages: map[string]string{}, Prune: true}

	for name, v := range doc.tables["packages"] {
		switch v.kind {
		case 's':
			h.Packages[name] = v.str
		case 'n':
			h.Packages[name] = fmt.Sprint(v.num)
		default:
			return nil, fmt.Errorf("%s:%d: package %q must be a version string like \"*\" or \"^1.2.0\"", path, v.line, name)
		}
	}

	if opts := doc.tables["options"]; opts != nil {
		if v, ok := opts["prune"]; ok && v.kind == 'b' {
			h.Prune = v.b
		}
		if v, ok := opts["jobs"]; ok && v.kind == 'n' {
			h.Jobs = int(v.num)
		}
	}

	if len(h.Packages) == 0 {
		return h, nil // an empty hopfile is valid; `hop sync` then prunes all
	}
	for name, c := range h.Packages {
		if !ValidConstraint(c) {
			return nil, fmt.Errorf("%s: package %q has an invalid version constraint %q", path, name, c)
		}
	}
	return h, nil
}

// Names lists the packages a hopfile declares, sorted.
func (h *Hopfile) Names() []string {
	out := make([]string, 0, len(h.Packages))
	for n := range h.Packages {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// WriteHopfile creates a new hopfile with the given packages.
func WriteHopfile(path string, pkgs map[string]string) error {
	var b strings.Builder
	b.WriteString("# hopfile.toml — the tools this project needs.\n")
	b.WriteString("#\n")
	b.WriteString("#   hop sync      make this machine match this file exactly\n")
	b.WriteString("#   hop add NAME  add a package and record it here\n")
	b.WriteString("#\n")
	b.WriteString("# Version constraints: \"*\" (latest), \"1.2.3\" (exact),\n")
	b.WriteString("# \"^1.2.3\" (same major), \"~1.2.3\" (same minor), \">=1.2.3\".\n\n")
	b.WriteString("[packages]\n")

	names := make([]string, 0, len(pkgs))
	for n := range pkgs {
		names = append(names, n)
	}
	sort.Strings(names)

	width := 0
	for _, n := range names {
		if len(n) > width {
			width = len(n)
		}
	}
	for _, n := range names {
		c := pkgs[n]
		if c == "" {
			c = "*"
		}
		fmt.Fprintf(&b, "%-*s = %q\n", width, n, c)
	}
	if len(names) == 0 {
		b.WriteString("# ripgrep = \"*\"\n")
	}

	b.WriteString("\n[options]\n")
	b.WriteString("# Remove packages that are not listed above when syncing.\n")
	b.WriteString("prune = true\n")

	return writeFileAtomic(path, []byte(b.String()), 0o644)
}

// UpsertPackage adds or updates one entry in an existing hopfile's [packages]
// table, preserving the user's comments, ordering and formatting everywhere
// else in the file. A config file the tool rewrites wholesale is a config file
// people stop hand-editing.
func UpsertPackage(path, name, constraint string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	var lines []string
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	f.Close()
	if err := sc.Err(); err != nil {
		return err
	}

	if constraint == "" {
		constraint = "*"
	}
	entry := fmt.Sprintf("%s = %q", name, constraint)

	inPackages := false
	lastPackagesLine := -1
	packagesHeader := -1

	for i, raw := range lines {
		line := strings.TrimSpace(stripComment(raw))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			table := strings.Trim(strings.TrimSpace(line[1:len(line)-1]), `"'`)
			inPackages = table == "packages"
			if inPackages {
				packagesHeader = i
			}
			continue
		}
		if !inPackages || line == "" {
			continue
		}
		if eq := strings.IndexByte(line, '='); eq > 0 {
			key := strings.Trim(strings.TrimSpace(line[:eq]), `"'`)
			if strings.EqualFold(key, name) {
				lines[i] = entry // update in place
				return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
			}
			lastPackagesLine = i
		}
	}

	switch {
	case lastPackagesLine >= 0:
		// Append after the final existing entry.
		lines = append(lines[:lastPackagesLine+1],
			append([]string{entry}, lines[lastPackagesLine+1:]...)...)
	case packagesHeader >= 0:
		// Empty table: insert right under its header.
		lines = append(lines[:packagesHeader+1],
			append([]string{entry}, lines[packagesHeader+1:]...)...)
	default:
		// No [packages] table at all.
		lines = append(lines, "", "[packages]", entry)
	}
	return writeFileAtomic(path, []byte(strings.Join(lines, "\n")+"\n"), 0o644)
}

// RemovePackage deletes an entry from a hopfile's [packages] table.
func RemovePackage(path, name string) (bool, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return false, err
	}
	lines := strings.Split(string(b), "\n")
	inPackages := false
	removed := false
	out := make([]string, 0, len(lines))

	for _, raw := range lines {
		line := strings.TrimSpace(stripComment(raw))
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			table := strings.Trim(strings.TrimSpace(line[1:len(line)-1]), `"'`)
			inPackages = table == "packages"
			out = append(out, raw)
			continue
		}
		if inPackages && line != "" {
			if eq := strings.IndexByte(line, '='); eq > 0 {
				key := strings.Trim(strings.TrimSpace(line[:eq]), `"'`)
				if strings.EqualFold(key, name) {
					removed = true
					continue // drop this line
				}
			}
		}
		out = append(out, raw)
	}
	if !removed {
		return false, nil
	}
	return true, writeFileAtomic(path, []byte(strings.Join(out, "\n")), 0o644)
}

// ---------------------------------------------------------------- lockfile ----

// LockEntry pins one resolved package.
type LockEntry struct {
	Name     string   `json:"name"`
	Version  string   `json:"version"`
	SHA256   string   `json:"sha256,omitempty"`
	URL      string   `json:"url,omitempty"`
	Platform Platform `json:"platform,omitempty"`
	Deps     []string `json:"deps,omitempty"`
	Explicit bool     `json:"explicit"`
}

// Lockfile is the exact resolution of a hopfile: versions and digests, so a
// second machine reproduces the first byte for byte.
type Lockfile struct {
	Version   int         `json:"version"`
	Generated time.Time   `json:"generated"`
	Platform  Platform    `json:"platform"`
	Hopfile   string      `json:"hopfile,omitempty"`
	Packages  []LockEntry `json:"packages"`
}

// LockfilePathFor returns the lockfile beside a hopfile.
func LockfilePathFor(hopfilePath string) string {
	return filepath.Join(filepath.Dir(hopfilePath), LockfileName)
}

// LoadLockfile reads a lockfile.
func LoadLockfile(path string) (*Lockfile, error) {
	var lf Lockfile
	if err := readJSON(path, &lf); err != nil {
		return nil, err
	}
	if lf.Version > 1 {
		return nil, fmt.Errorf("%s was written by a newer hop (lock version %d); upgrade hop", path, lf.Version)
	}
	return &lf, nil
}

// BuildLockfile snapshots a package set as a lockfile.
func BuildLockfile(pkgs []Installed, plat Platform, hopfilePath string) *Lockfile {
	lf := &Lockfile{
		Version:   1,
		Generated: time.Now().UTC().Truncate(time.Second),
		Platform:  plat,
		Hopfile:   filepath.Base(hopfilePath),
	}
	for _, p := range pkgs {
		lf.Packages = append(lf.Packages, LockEntry{
			Name: p.Name, Version: p.Version, SHA256: p.SHA256,
			URL: p.Source, Platform: p.Platform, Deps: p.Deps, Explicit: p.Explicit,
		})
	}
	sort.Slice(lf.Packages, func(i, j int) bool { return lf.Packages[i].Name < lf.Packages[j].Name })
	return lf
}

// Write saves a lockfile.
func (lf *Lockfile) Write(path string) error { return writeJSONAtomic(path, lf, 0o644) }

// AsConstraints turns a lockfile into exact constraints, so `hop sync` on a
// second machine installs precisely these versions rather than "latest".
func (lf *Lockfile) AsConstraints() map[string]string {
	out := map[string]string{}
	for _, p := range lf.Packages {
		if p.Explicit {
			out[p.Name] = p.Version
		}
	}
	return out
}

// Stale reports whether a lockfile no longer covers the hopfile's packages,
// which is the cue to re-resolve rather than trust the pins.
func (lf *Lockfile) Stale(h *Hopfile) bool {
	if lf == nil {
		return true
	}
	if lf.Platform != CurrentPlatform() {
		return true
	}
	have := map[string]LockEntry{}
	for _, p := range lf.Packages {
		have[strings.ToLower(p.Name)] = p
	}
	for name, c := range h.Packages {
		e, ok := have[strings.ToLower(name)]
		if !ok || !VersionSatisfies(c, e.Version) {
			return true
		}
	}
	return false
}
