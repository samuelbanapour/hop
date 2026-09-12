//go:build unix

package core

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Guard is a held advisory lock over the hop root. Every mutating command
// takes it, so two concurrent `hop install` runs cannot interleave
// generation numbers or race on the same store path.
type Guard struct {
	f *os.File
}

// Acquire takes the root lock, waiting up to timeout. The error names the
// process holding it, because "resource busy" alone tells a user nothing.
func Acquire(l *Layout, timeout time.Duration) (*Guard, error) {
	f, err := os.OpenFile(l.LockPath(), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, fmt.Errorf("cannot open lock file %s: %w", l.LockPath(), err)
	}

	deadline := time.Now().Add(timeout)
	for {
		err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			// Record who holds it, for the benefit of the next waiter.
			_ = f.Truncate(0)
			_, _ = f.WriteAt([]byte(strconv.Itoa(os.Getpid())+"\n"), 0)
			return &Guard{f: f}, nil
		}
		if time.Now().After(deadline) {
			holder := "another hop process"
			if b, rerr := os.ReadFile(l.LockPath()); rerr == nil {
				if pid := strings.TrimSpace(string(b)); pid != "" {
					holder = "hop (pid " + pid + ")"
				}
			}
			f.Close()
			return nil, fmt.Errorf("%s is already modifying %s; waited %s", holder, l.Root, timeout)
		}
		time.Sleep(120 * time.Millisecond)
	}
}

// Release drops the lock. It is safe to call more than once.
func (g *Guard) Release() {
	if g == nil || g.f == nil {
		return
	}
	_ = syscall.Flock(int(g.f.Fd()), syscall.LOCK_UN)
	_ = g.f.Close()
	g.f = nil
}
