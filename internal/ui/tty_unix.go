//go:build unix

package ui

import (
	"os"
	"syscall"
	"unsafe"
)

// isTerminal reports whether f refers to a character device we can drive with
// ANSI escapes. It uses TIOCGWINSZ rather than a Stat mode check because
// /dev/null is also a character device.
func isTerminal(f *os.File) bool {
	_, _, err := terminalSize(f)
	return err == nil
}

type winsize struct {
	rows, cols, xpixel, ypixel uint16
}

// terminalSize returns the current width and height in character cells.
func terminalSize(f *os.File) (w, h int, err error) {
	var ws winsize
	_, _, errno := syscall.Syscall(
		syscall.SYS_IOCTL,
		f.Fd(),
		uintptr(syscall.TIOCGWINSZ),
		uintptr(unsafe.Pointer(&ws)),
	)
	if errno != 0 {
		return 0, 0, errno
	}
	return int(ws.cols), int(ws.rows), nil
}
