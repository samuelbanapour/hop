package core

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"
)

// ApplyOptions configures a transaction.
type ApplyOptions struct {
	Jobs    int
	Sink    Sink
	Client  *http.Client
	Command string // recorded in the generation, e.g. "hop install fd bat"
}

// ApplyResult describes a completed transaction.
type ApplyResult struct {
	Generation *Generation
	Added      []Installed
	Changed    []Installed
	Removed    []string
	Bytes      int64 // bytes transferred over the network
	Reused     int   // artifacts served from cache
	Duration   time.Duration
	Conflicts  []Conflict
	Caveats    map[string]string // package name → caveat text
	Recorded   map[string]string // package name → digest recorded on first use
}

// TransactionError aborts a transaction with every failure it found, rather
// than the first. Nothing has been changed when this is returned: the whole
// point of building a new generation is that a failure costs the user nothing.
type TransactionError struct {
	Failures []StepFailure
}

// StepFailure is one package's reason for aborting the transaction.
type StepFailure struct {
	Name string
	Err  error
}

func (e *TransactionError) Error() string {
	if len(e.Failures) == 1 {
		return fmt.Sprintf("%s: %v", e.Failures[0].Name, e.Failures[0].Err)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d packages failed:", len(e.Failures))
	for _, f := range e.Failures {
		fmt.Fprintf(&b, "\n  %s: %v", f.Name, f.Err)
	}
	return b.String()
}

// Unwrap exposes the first failure for errors.As.
func (e *TransactionError) Unwrap() error {
	if len(e.Failures) == 0 {
		return nil
	}
	return e.Failures[0].Err
}

// Apply executes a plan as a single transaction: download and verify
// everything, materialise it into the store, then build and activate a new
// generation. Any failure before activation leaves the live environment
// exactly as it was.
func Apply(ctx context.Context, l *Layout, cur *Generation, plan *Plan, opts ApplyOptions) (*ApplyResult, error) {
	start := time.Now()
	if opts.Sink == nil {
		opts.Sink = DiscardSink
	}
	if opts.Client == nil {
		opts.Client = NewHTTPClient()
	}
	if opts.Jobs < 1 {
		opts.Jobs = 1
	}

	res := &ApplyResult{
		Caveats:  map[string]string{},
		Recorded: map[string]string{},
	}

	// ---- 1. Download everything the plan needs, in parallel. ----------------
	dl := plan.Downloads()
	reqs := make([]*FetchRequest, 0, len(dl))
	for _, s := range dl {
		reqs = append(reqs, &FetchRequest{
			Name:        s.Name,
			URL:         s.Artifact.URL,
			SHA256:      s.Artifact.SHA256,
			SHA512:      s.Artifact.SHA512,
			SHA1:        s.Artifact.SHA1,
			Size:        s.Artifact.Size,
			OCITokenURL: s.Artifact.OCITokenURL,
		})
	}

	var fetched []*FetchResult
	if len(reqs) > 0 {
		fetched = FetchAll(ctx, opts.Client, l, reqs, opts.Jobs, opts.Sink)
	}

	var failures []StepFailure
	byName := map[string]*FetchResult{}
	for _, f := range fetched {
		if f == nil {
			continue
		}
		if f.Err != nil {
			failures = append(failures, StepFailure{Name: f.Req.Name, Err: f.Err})
			continue
		}
		byName[strings.ToLower(f.Req.Name)] = f
		if f.Cached {
			res.Reused++
		} else {
			res.Bytes += f.Size
		}
	}
	if len(failures) > 0 {
		return nil, &TransactionError{Failures: failures}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// ---- 2. Materialise into the content-addressed store. -------------------
	// Store paths are keyed by the digest we actually observed, so a package
	// whose upstream artifact changed gets a distinct path and cannot corrupt
	// the tree an older generation still points at.
	//
	// depLoc tracks where every already-resolved package actually lives, for
	// one reason: a Homebrew bottle references its own runtime dependencies
	// by an unresolved build-time placeholder that Materialise has to
	// rewrite into a real hop store path (see relocateHomebrewBottle). It is
	// seeded from whatever is already installed, then grows as this
	// transaction materialises each package — dl is walked in
	// dependency-first order (resolve.go's expand() guarantees it), so a
	// dependency is always in depLoc before anything that might need to
	// relocate against it.
	depLoc := map[string]depLocation{}
	if cur != nil {
		for _, p := range cur.Packages {
			if p.StorePath != "" {
				depLoc[p.Name] = depLocation{storePath: p.StorePath, version: p.Version}
			}
		}
	}

	materialised := map[string]*StoreEntry{}
	for _, s := range dl {
		f := byName[strings.ToLower(s.Name)]
		if f == nil {
			failures = append(failures, StepFailure{Name: s.Name, Err: errors.New("internal: download result missing")})
			continue
		}
		entry, err := Materialise(l, s.Recipe, s.Artifact, s.Platform, f.Path, f.SHA256(), depLoc)
		if err != nil {
			failures = append(failures, StepFailure{Name: s.Name, Err: err})
			continue
		}
		materialised[strings.ToLower(s.Name)] = entry
		depLoc[s.Recipe.Name] = depLocation{storePath: entry.Path, version: s.Recipe.Version}

		if s.Artifact.SHA256 == "" && s.Artifact.SHA512 == "" && s.Artifact.SHA1 == "" {
			res.Recorded[s.Name] = f.SHA256()
		}
		if s.Recipe.Caveats != "" {
			res.Caveats[s.Name] = s.Recipe.Caveats
		}
	}
	if len(failures) > 0 {
		return nil, &TransactionError{Failures: failures}
	}

	// ---- 3. Assemble the new generation's package set. ----------------------
	final := make([]Installed, 0, len(plan.Final))
	for _, want := range plan.Final {
		key := strings.ToLower(want.Name)

		if entry, ok := materialised[key]; ok {
			want.StorePath = entry.Path
			want.Bins = relativiseBins(entry.Path, entry.Bins)
			want.Mans = relativise(entry.Path, entry.Mans)
			want.Size = entry.Size
			want.Platform = entry.Platform
			if f := byName[key]; f != nil {
				want.SHA256 = f.SHA256()
				want.SHA512 = f.SHA512()
				want.SHA1 = f.Digests.sha1
			}
			want.At = time.Now()
			final = append(final, want)
			continue
		}

		// Unchanged package: carry its existing store record forward.
		if prev, ok := cur.Find(want.Name); ok && prev.StorePath != "" {
			carried := *prev
			carried.Explicit = want.Explicit || prev.Explicit
			if HaveStorePath(carried.StorePath) {
				final = append(final, carried)
				continue
			}
			// The store path vanished (a too-eager gc, or manual deletion).
			failures = append(failures, StepFailure{
				Name: want.Name,
				Err: fmt.Errorf("store path %s is missing; reinstall with `hop install --force %s`",
					carried.StorePath, want.Name),
			})
			continue
		}

		failures = append(failures, StepFailure{
			Name: want.Name,
			Err:  errors.New("internal: package in plan was neither downloaded nor already installed"),
		})
	}
	if len(failures) > 0 {
		return nil, &TransactionError{Failures: failures}
	}

	// ---- 4. Commit and activate. -------------------------------------------
	gen, err := Commit(l, final, opts.Command)
	if err != nil {
		return nil, fmt.Errorf("recording the new generation: %w", err)
	}
	if err := Activate(l, gen.ID); err != nil {
		// The generation is written but not live; the old one still is, so the
		// user's environment is untouched.
		return nil, fmt.Errorf("activating generation %d: %w", gen.ID, err)
	}

	res.Generation = gen
	res.Conflicts = Conflicts()
	res.Duration = time.Since(start)

	// ---- 5. Summarise for the caller. --------------------------------------
	for _, s := range plan.Steps {
		switch s.Action {
		case ActionInstall:
			if inst, ok := gen.Find(s.Name); ok {
				res.Added = append(res.Added, *inst)
			}
		case ActionUpgrade, ActionDowngrade, ActionReinstall:
			if inst, ok := gen.Find(s.Name); ok {
				res.Changed = append(res.Changed, *inst)
			}
		case ActionRemove:
			res.Removed = append(res.Removed, s.Name)
		}
	}
	sort.Strings(res.Removed)
	return res, nil
}

// relativiseBins rewrites discovered absolute bin paths as store-relative, so
// a manifest stays valid if the hop root is ever moved.
func relativiseBins(base string, bins []BinLink) []BinLink {
	out := make([]BinLink, 0, len(bins))
	for _, b := range bins {
		if rel := strings.TrimPrefix(b.Path, base); rel != b.Path {
			b.Path = strings.TrimPrefix(rel, "/")
		}
		out = append(out, b)
	}
	return out
}

// relativise stores man paths relative to their store path, for the same
// reason.
func relativise(base string, paths []string) []string {
	out := make([]string, 0, len(paths))
	for _, p := range paths {
		if rel := strings.TrimPrefix(p, base); rel != p {
			out = append(out, strings.TrimPrefix(rel, "/"))
			continue
		}
		out = append(out, p)
	}
	return out
}

// ApplyRemoveOnly commits a plan that needs no downloads. Removals are pure
// bookkeeping: the new generation simply omits the package, and the store is
// left for `hop gc`, which is why `hop remove` is instant and reversible.
func ApplyRemoveOnly(l *Layout, plan *Plan, command string) (*ApplyResult, error) {
	start := time.Now()
	gen, err := Commit(l, plan.Final, command)
	if err != nil {
		return nil, err
	}
	if err := Activate(l, gen.ID); err != nil {
		return nil, err
	}
	res := &ApplyResult{
		Generation: gen,
		Conflicts:  Conflicts(),
		Duration:   time.Since(start),
		Caveats:    map[string]string{},
		Recorded:   map[string]string{},
	}
	for _, s := range plan.Steps {
		if s.Action == ActionRemove {
			res.Removed = append(res.Removed, s.Name)
		}
	}
	sort.Strings(res.Removed)
	return res, nil
}

// Verify re-hashes every store path in a generation against its manifest,
// detecting on-disk corruption or tampering after installation.
type VerifyIssue struct {
	Name    string
	Problem string
}

// Verify checks a generation's integrity: store paths present, completion
// sentinels intact, and every declared binary still resolvable.
func Verify(l *Layout, g *Generation) []VerifyIssue {
	var issues []VerifyIssue
	if g == nil {
		return nil
	}
	for _, p := range g.Packages {
		if !exists(p.StorePath) {
			issues = append(issues, VerifyIssue{p.Name, "store path is missing: " + p.StorePath})
			continue
		}
		if !HaveStorePath(p.StorePath) {
			issues = append(issues, VerifyIssue{p.Name, "store path is incomplete (no completion marker)"})
			continue
		}
		for _, b := range p.Bins {
			full := b.Path
			if !strings.HasPrefix(full, "/") {
				full = p.StorePath + "/" + full
			}
			if !exists(full) {
				issues = append(issues, VerifyIssue{p.Name, "missing binary " + b.Path})
			}
		}
		link := l.ProfileBin(g.ID)
		for _, b := range p.Bins {
			if !lexists(link + "/" + b.Name) {
				issues = append(issues, VerifyIssue{p.Name, "not linked into generation " + fmt.Sprint(g.ID) + ": " + b.Name})
			}
		}
	}
	return issues
}
