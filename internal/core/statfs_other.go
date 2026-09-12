//go:build !unix

package core

import "errors"

// statfsAvail is unsupported here; callers treat the error as "unknown" and
// skip the preflight disk check rather than refusing to run.
func statfsAvail(string) (int64, error) { return 0, errors.New("unsupported") }
