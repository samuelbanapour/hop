//go:build !unix

package core

import "time"

// Guard is a no-op lock on platforms without flock.
type Guard struct{}

// Acquire succeeds without locking; concurrent runs are the user's problem
// on platforms hop cannot lock safely.
func Acquire(*Layout, time.Duration) (*Guard, error) { return &Guard{}, nil }

// Release is a no-op.
func (g *Guard) Release() {}
