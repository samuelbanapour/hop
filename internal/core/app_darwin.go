//go:build darwin

package core

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// mountAndCopyApp mounts a cask's .dmg read-only, copies its .app bundle
// into stage with ditto (which preserves resource forks, extended
// attributes and code signatures — a plain recursive copy can silently
// corrupt a signed bundle in ways Gatekeeper only complains about later),
// and unmounts it again. Nothing about the mounted volume is ever written to.
func mountAndCopyApp(dmgPath, stage, appPath string) error {
	mountPoint, err := os.MkdirTemp("", "hop-dmg-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(mountPoint)

	attach := exec.Command("hdiutil", "attach", "-nobrowse", "-noautoopen", "-readonly", "-mountpoint", mountPoint, dmgPath)
	var stderr bytes.Buffer
	attach.Stderr = &stderr
	if err := attach.Run(); err != nil {
		return fmt.Errorf("hdiutil attach: %w: %s", err, strings.TrimSpace(stderr.String()))
	}
	defer detachDMG(mountPoint)

	var src string
	if appPath != "" {
		src = filepath.Join(mountPoint, filepath.Clean(appPath))
		if fi, err := os.Stat(src); err != nil || !fi.IsDir() || !strings.HasSuffix(src, ".app") {
			return fmt.Errorf("app_path %q does not point at a .app bundle inside the disk image", appPath)
		}
	} else {
		found, err := findAppBundle(mountPoint, 2)
		if err != nil {
			return err
		}
		src = found
	}

	dst := filepath.Join(stage, filepath.Base(src))
	if out, err := exec.Command("ditto", src, dst).CombinedOutput(); err != nil {
		return fmt.Errorf("ditto: %w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// detachDMG unmounts a disk image, retrying briefly since something
// transient (Spotlight indexing the volume we just mounted, most often)
// can hold it busy for a moment right after ditto finishes.
func detachDMG(mountPoint string) {
	for i := 0; i < 5; i++ {
		if exec.Command("hdiutil", "detach", mountPoint, "-quiet").Run() == nil {
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	_ = exec.Command("hdiutil", "detach", mountPoint, "-force", "-quiet").Run()
}
