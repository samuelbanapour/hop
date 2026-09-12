package core

import (
	"strconv"
	"strings"
)

// CompareVersions orders two version strings, returning -1, 0 or 1.
// It handles the shapes real releases actually use: a leading "v", numeric
// segments of differing length ("1.10" > "1.9"), and prereleases sorting
// below their release ("1.2.0-rc.1" < "1.2.0").
func CompareVersions(a, b string) int {
	ar, ap := splitPrerelease(normalizeVersion(a))
	br, bp := splitPrerelease(normalizeVersion(b))

	if c := compareSegments(strings.FieldsFunc(ar, isVersionSep), strings.FieldsFunc(br, isVersionSep)); c != 0 {
		return c
	}
	// Equal release parts: a prerelease ranks below a plain release.
	switch {
	case ap == "" && bp == "":
		return 0
	case ap == "":
		return 1
	case bp == "":
		return -1
	}
	return compareSegments(strings.FieldsFunc(ap, isVersionSep), strings.FieldsFunc(bp, isVersionSep))
}

func isVersionSep(r rune) bool { return r == '.' || r == '_' || r == '+' }

func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "v")
	v = strings.TrimPrefix(v, "V")
	return v
}

// splitPrerelease divides "1.2.3-rc.1" into "1.2.3" and "rc.1".
func splitPrerelease(v string) (release, pre string) {
	if i := strings.IndexByte(v, '-'); i >= 0 {
		return v[:i], v[i+1:]
	}
	return v, ""
}

func compareSegments(a, b []string) int {
	n := len(a)
	if len(b) > n {
		n = len(b)
	}
	for i := 0; i < n; i++ {
		as, bs := "", ""
		if i < len(a) {
			as = a[i]
		}
		if i < len(b) {
			bs = b[i]
		}
		// A missing segment counts as zero, so 1.2 == 1.2.0.
		ai, aerr := strconv.Atoi(as)
		bi, berr := strconv.Atoi(bs)
		switch {
		case aerr == nil && berr == nil:
			if ai != bi {
				return sign(ai - bi)
			}
		case aerr == nil && berr != nil:
			return 1 // numeric outranks alphanumeric (1.2.0 > 1.2.beta)
		case aerr != nil && berr == nil:
			return -1
		default:
			if as != bs {
				if as < bs {
					return -1
				}
				return 1
			}
		}
	}
	return 0
}

func sign(n int) int {
	switch {
	case n < 0:
		return -1
	case n > 0:
		return 1
	default:
		return 0
	}
}

// VersionNewer reports whether have is older than want.
func VersionNewer(have, want string) bool { return CompareVersions(want, have) > 0 }

// VersionSatisfies reports whether version meets a hopfile constraint.
// Supported forms, chosen to match what people already type:
//
//   - latest  (empty)   any version
//     1.2.3                exactly 1.2.3
//     ^1.2.3               >=1.2.3 within major 1
//     ~1.2.3               >=1.2.3 within minor 1.2
//     >=1.2.3  >1.2.3  <=1.2.3  <1.2.3
func VersionSatisfies(constraint, version string) bool {
	c := strings.TrimSpace(constraint)
	if c == "" || c == "*" || c == "latest" || c == "any" {
		return true
	}
	// Comma-separated constraints must all hold.
	if strings.Contains(c, ",") {
		for _, part := range strings.Split(c, ",") {
			if !VersionSatisfies(part, version) {
				return false
			}
		}
		return true
	}

	switch {
	case strings.HasPrefix(c, ">="):
		return CompareVersions(version, c[2:]) >= 0
	case strings.HasPrefix(c, "<="):
		return CompareVersions(version, c[2:]) <= 0
	case strings.HasPrefix(c, ">"):
		return CompareVersions(version, c[1:]) > 0
	case strings.HasPrefix(c, "<"):
		return CompareVersions(version, c[1:]) < 0
	case strings.HasPrefix(c, "^"):
		base := c[1:]
		if CompareVersions(version, base) < 0 {
			return false
		}
		return majorOf(version) == majorOf(base)
	case strings.HasPrefix(c, "~"):
		base := c[1:]
		if CompareVersions(version, base) < 0 {
			return false
		}
		return majorOf(version) == majorOf(base) && minorOf(version) == minorOf(base)
	case strings.HasPrefix(c, "="):
		return CompareVersions(version, c[1:]) == 0
	default:
		return CompareVersions(version, c) == 0
	}
}

func versionPart(v string, i int) string {
	parts := strings.FieldsFunc(normalizeVersion(v), isVersionSep)
	if i < len(parts) {
		return parts[i]
	}
	return "0"
}

func majorOf(v string) string { return versionPart(v, 0) }
func minorOf(v string) string { return versionPart(v, 1) }

// ValidConstraint reports whether a hopfile constraint is parseable, so
// `hop sync` can reject a typo instead of silently matching nothing.
func ValidConstraint(c string) bool {
	c = strings.TrimSpace(c)
	if c == "" || c == "*" || c == "latest" || c == "any" {
		return true
	}
	if strings.Contains(c, ",") {
		for _, p := range strings.Split(c, ",") {
			if !ValidConstraint(p) {
				return false
			}
		}
		return true
	}
	body := strings.TrimLeft(c, "><=^~")
	if body == "" {
		return false
	}
	for _, r := range body {
		if !(r >= '0' && r <= '9') && r != '.' && r != '-' && r != '+' &&
			!(r >= 'a' && r <= 'z') && !(r >= 'A' && r <= 'Z') && r != '_' {
			return false
		}
	}
	return true
}
