// Package indexio merges freshly generated recipes into hop's built-in
// index.json without ever decoding a recipe this call didn't generate into
// a typed struct.
//
// genindex, genbrew and genimages each understand only part of the real
// recipe schema (internal/core/index.go's Artifact and Recipe) — on
// purpose, so no one generator's struct has to track every field every
// other generator might set, and none of them import internal/core itself.
// The failure mode that creates is exactly the one this package exists to
// close: a generator that reads the whole index, decodes every recipe into
// its own necessarily-partial struct, and writes the whole thing back out
// silently drops any field its struct doesn't declare — even from recipes
// it never meant to touch. This happened for real: a genbrew run stripped
// "kind":"image" from every OS-image recipe because genbrew's struct had
// no Kind field, and the genimages run that followed, fixing that, then
// stripped oci_token_url/bin/deps from every Homebrew formula in the other
// direction.
//
// Merge closes this structurally rather than by keeping every generator's
// struct manually in sync (which is exactly what drifted out of sync
// before): a recipe this call isn't replacing is carried through as
// unparsed json.RawMessage, byte-for-byte, regardless of what fields it
// contains or which future generator added them.
package indexio

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"time"
)

// shell mirrors the outer index.json shape. Recipes stays as raw JSON
// specifically so a recipe this generator doesn't recognize a field of is
// never round-tripped through a struct that would drop it.
type shell struct {
	Schema    int               `json:"schema"`
	Source    string            `json:"source"`
	Generated time.Time         `json:"generated"`
	Recipes   []json.RawMessage `json:"recipes"`
}

type named struct {
	Name string `json:"name"`
}

// nameOf extracts just the "name" field from a recipe's raw JSON, the one
// piece Merge needs without decoding anything else about it.
func nameOf(raw json.RawMessage) (string, error) {
	var n named
	if err := json.Unmarshal(raw, &n); err != nil {
		return "", fmt.Errorf("recipe has invalid JSON: %w", err)
	}
	if n.Name == "" {
		return "", fmt.Errorf("recipe has no name: %s", raw)
	}
	return n.Name, nil
}

// Merge reads the index at path, replaces or adds one entry per recipe in
// built (each marshaled with encoding/json, so it only needs to implement
// whatever subset of the schema the calling generator's own recipe type
// declares), leaves every other existing recipe's JSON completely
// untouched, and writes the result back sorted by name.
//
// Returns the total recipe count after merging, for the caller's own
// summary line.
func Merge[T any](path string, built []T) (total int, err error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("reading %s: %w", path, err)
	}
	var ix shell
	if err := json.Unmarshal(b, &ix); err != nil {
		return 0, fmt.Errorf("parsing %s: %w", path, err)
	}

	all := make(map[string]json.RawMessage, len(ix.Recipes)+len(built))
	order := make([]string, 0, len(ix.Recipes)+len(built))

	for _, raw := range ix.Recipes {
		name, err := nameOf(raw)
		if err != nil {
			return 0, fmt.Errorf("%s: %w", path, err)
		}
		if _, dup := all[name]; !dup {
			order = append(order, name)
		}
		all[name] = raw
	}

	for _, r := range built {
		raw, err := json.Marshal(r)
		if err != nil {
			return 0, fmt.Errorf("encoding recipe: %w", err)
		}
		name, err := nameOf(raw)
		if err != nil {
			return 0, fmt.Errorf("generated recipe: %w", err)
		}
		if _, existed := all[name]; !existed {
			order = append(order, name)
		}
		all[name] = raw
	}

	sort.Strings(order)
	ix.Recipes = make([]json.RawMessage, len(order))
	for i, name := range order {
		ix.Recipes[i] = all[name]
	}
	ix.Generated = time.Now().UTC().Truncate(time.Second)

	out, err := json.MarshalIndent(&ix, "", "  ")
	if err != nil {
		return 0, fmt.Errorf("encoding index: %w", err)
	}
	if err := os.WriteFile(path, append(out, '\n'), 0o644); err != nil {
		return 0, fmt.Errorf("writing %s: %w", path, err)
	}
	return len(ix.Recipes), nil
}
