package indexio

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestMergePreservesUnknownFields is the regression test for the exact bug
// that motivated this package: a recipe untouched by this Merge call must
// survive byte-for-byte, including fields a narrower caller-side struct
// wouldn't know how to decode. htop here stands in for a Homebrew formula
// carrying oci_token_url/bin/deps that a struct like genimages' once didn't
// declare, and this test simulates exactly that generator's own recipe
// struct calling Merge for an unrelated image recipe.
func TestMergePreservesUnknownFields(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")

	initial := `{
  "schema": 1,
  "source": "builtin",
  "generated": "2020-01-01T00:00:00Z",
  "recipes": [
    {
      "name": "htop",
      "version": "3.5.3",
      "kind": "",
      "deps": ["ncurses"],
      "artifacts": {
        "darwin-arm64": {
          "url": "https://ghcr.io/v2/homebrew/core/htop/blobs/sha256:deadbeef",
          "sha256": "deadbeef",
          "oci_token_url": "https://ghcr.io/token?scope=repository:homebrew/core/htop:pull",
          "format": "tar.gz",
          "bin": ["htop"]
        }
      }
    }
  ]
}`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	// A narrow struct, like genimages' own recipe/artifact types, which
	// knows nothing about oci_token_url, bin, or deps.
	type narrowArtifact struct {
		URL    string `json:"url"`
		SHA256 string `json:"sha256,omitempty"`
		Format string `json:"format,omitempty"`
	}
	type narrowRecipe struct {
		Name      string                     `json:"name"`
		Version   string                     `json:"version"`
		Kind      string                     `json:"kind,omitempty"`
		Artifacts map[string]*narrowArtifact `json:"artifacts"`
	}

	built := []narrowRecipe{{
		Name:    "ubuntu-cloud",
		Version: "24.04",
		Kind:    "image",
		Artifacts: map[string]*narrowArtifact{
			"darwin-arm64": {URL: "https://example.com/ubuntu.img", SHA256: "cafef00d", Format: "raw"},
		},
	}}

	total, err := Merge(path, built)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if total != 2 {
		t.Fatalf("total = %d, want 2", total)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Recipes []map[string]any `json:"recipes"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatalf("output is not valid JSON: %v", err)
	}

	var htop map[string]any
	for _, r := range got.Recipes {
		if r["name"] == "htop" {
			htop = r
		}
	}
	if htop == nil {
		t.Fatal("htop recipe missing from merged output")
	}

	deps, _ := htop["deps"].([]any)
	if len(deps) != 1 || deps[0] != "ncurses" {
		t.Errorf("htop.deps = %v, want [\"ncurses\"] — a field the narrow struct in this call doesn't declare must survive untouched", htop["deps"])
	}
	arts, _ := htop["artifacts"].(map[string]any)
	art, _ := arts["darwin-arm64"].(map[string]any)
	if art["oci_token_url"] == nil || art["oci_token_url"] == "" {
		t.Errorf("htop's darwin-arm64 oci_token_url was dropped: %v", art)
	}
	binField, _ := art["bin"].([]any)
	if len(binField) != 1 || binField[0] != "htop" {
		t.Errorf("htop's darwin-arm64 bin was dropped or changed: %v", art["bin"])
	}
}

// TestMergeAddsAndReplaces checks the ordinary, non-regression path: a new
// recipe is added, an existing one with the same name is replaced with the
// freshly built version, and the result stays sorted by name.
func TestMergeAddsAndReplaces(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "index.json")
	initial := `{
  "schema": 1,
  "source": "builtin",
  "generated": "2020-01-01T00:00:00Z",
  "recipes": [
    {"name": "bat", "version": "0.1.0", "artifacts": {}},
    {"name": "zoxide", "version": "1.0.0", "artifacts": {}}
  ]
}`
	if err := os.WriteFile(path, []byte(initial), 0o644); err != nil {
		t.Fatal(err)
	}

	type rec struct {
		Name    string `json:"name"`
		Version string `json:"version"`
	}
	built := []rec{
		{Name: "bat", Version: "0.2.0"}, // replaces
		{Name: "fd", Version: "1.0.0"},  // new
	}

	total, err := Merge(path, built)
	if err != nil {
		t.Fatalf("Merge: %v", err)
	}
	if total != 3 {
		t.Fatalf("total = %d, want 3 (bat replaced, zoxide kept, fd added)", total)
	}

	out, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Recipes []rec `json:"recipes"`
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Recipes) != 3 {
		t.Fatalf("got %d recipes, want 3", len(got.Recipes))
	}
	names := []string{got.Recipes[0].Name, got.Recipes[1].Name, got.Recipes[2].Name}
	want := []string{"bat", "fd", "zoxide"}
	for i := range want {
		if names[i] != want[i] {
			t.Errorf("recipes[%d].Name = %q, want %q (sorted order: %v)", i, names[i], want[i], names)
		}
	}
	for _, r := range got.Recipes {
		if r.Name == "bat" && r.Version != "0.2.0" {
			t.Errorf("bat.Version = %q, want replaced value %q", r.Version, "0.2.0")
		}
	}
}
