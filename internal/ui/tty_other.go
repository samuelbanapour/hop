//go:build !unix

package ui

import "os"

// isTerminal conservatively reports false on platforms without an ioctl we
// can use, which disables live progress rendering rather than corrupting it.
func isTerminal(*os.File) bool { return false }

// terminalSize reports a failure so callers fall back to the 80-column default.
func terminalSize(*os.File) (int, int, error) { return 0, 0, os.ErrInvalid }
