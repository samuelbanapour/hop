//go:build !darwin && !linux

package core

// depLocation mirrors the platform-specific definitions.
type depLocation struct {
	storePath string
	version   string
}

// relocateHomebrewBottle has no implementation on platforms that are
// neither darwin nor linux — Homebrew itself does not ship bottles for any
// other OS, so a Homebrew-sourced recipe simply has no artifact to select
// here and this path is never reached in practice.
func relocateHomebrewBottle(root, selfFormula, selfVersion, selfStorePath string, deps map[string]depLocation) error {
	return nil
}
