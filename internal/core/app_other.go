//go:build !darwin

package core

import "fmt"

// mountAndCopyApp has no implementation outside darwin: a KindApp recipe
// simply has no artifact for any other platform, so the normal
// "unsupported platform" error fires long before this would ever run.
func mountAndCopyApp(dmgPath, stage, appPath string) error {
	return fmt.Errorf("GUI apps (Homebrew casks) only run on macOS")
}
