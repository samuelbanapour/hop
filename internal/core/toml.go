package core

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// This is a deliberately small TOML reader covering exactly what a hopfile
// uses: comments, `[table]` headers, and `key = value` where value is a
// string, integer, boolean or array of strings. Vendoring a full TOML library
// would cost hop its zero-dependency property for no benefit here, and an
// unsupported construct reports its line number rather than being ignored.

// tomlDoc is a parsed document: table name → key → value.
type tomlDoc struct {
	tables map[string]map[string]tomlValue
	order  []string
}

// tomlValue is one parsed scalar or array, with its source line for errors.
type tomlValue struct {
	str  string
	num  int64
	b    bool
	list []string
	kind byte // 's', 'n', 'b', 'a'
	line int
}

func newTomlDoc() *tomlDoc {
	return &tomlDoc{tables: map[string]map[string]tomlValue{}}
}

func (d *tomlDoc) table(name string) map[string]tomlValue {
	if d.tables[name] == nil {
		d.tables[name] = map[string]tomlValue{}
		d.order = append(d.order, name)
	}
	return d.tables[name]
}

// parseTOML reads the supported subset, returning an error naming the
// offending line so a typo in a hopfile is a one-second fix.
func parseTOML(path string) (*tomlDoc, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	doc := newTomlDoc()
	current := ""
	doc.table(current) // root table

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	lineNo := 0

	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(stripComment(sc.Text()))
		if line == "" {
			continue
		}

		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("%s:%d: unterminated table header", path, lineNo)
			}
			name := strings.TrimSpace(line[1 : len(line)-1])
			if strings.HasPrefix(name, "[") {
				return nil, fmt.Errorf("%s:%d: arrays of tables are not supported in a hopfile", path, lineNo)
			}
			name = strings.Trim(name, `"'`)
			if name == "" {
				return nil, fmt.Errorf("%s:%d: empty table name", path, lineNo)
			}
			current = name
			doc.table(current)
			continue
		}

		eq := strings.IndexByte(line, '=')
		if eq < 0 {
			return nil, fmt.Errorf("%s:%d: expected `key = value`, got %q", path, lineNo, truncateForError(line))
		}
		key := strings.Trim(strings.TrimSpace(line[:eq]), `"'`)
		raw := strings.TrimSpace(line[eq+1:])
		if key == "" {
			return nil, fmt.Errorf("%s:%d: missing key name", path, lineNo)
		}

		v, err := parseTOMLValue(raw)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %s: %w", path, lineNo, key, err)
		}
		v.line = lineNo
		doc.table(current)[key] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return doc, nil
}

// stripComment removes a trailing comment, respecting quoted strings so a
// version like "1.0#2" survives.
func stripComment(s string) string {
	inStr := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr && c == quote:
			inStr = false
		case !inStr && (c == '"' || c == '\''):
			inStr, quote = true, c
		case !inStr && c == '#':
			return s[:i]
		}
	}
	return s
}

func parseTOMLValue(raw string) (tomlValue, error) {
	switch {
	case raw == "":
		return tomlValue{}, fmt.Errorf("missing value")

	case strings.HasPrefix(raw, `"`) || strings.HasPrefix(raw, `'`):
		s, err := unquoteTOML(raw)
		if err != nil {
			return tomlValue{}, err
		}
		return tomlValue{str: s, kind: 's'}, nil

	case strings.HasPrefix(raw, "["):
		if !strings.HasSuffix(raw, "]") {
			return tomlValue{}, fmt.Errorf("multi-line arrays are not supported in a hopfile")
		}
		inner := strings.TrimSpace(raw[1 : len(raw)-1])
		if inner == "" {
			return tomlValue{kind: 'a'}, nil
		}
		var list []string
		for _, part := range splitTopLevel(inner) {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			s, err := unquoteTOML(part)
			if err != nil {
				// Allow bare words in arrays; they are unambiguous here.
				s = strings.Trim(part, `"'`)
			}
			list = append(list, s)
		}
		return tomlValue{list: list, kind: 'a'}, nil

	case raw == "true" || raw == "false":
		return tomlValue{b: raw == "true", kind: 'b'}, nil

	default:
		n, err := strconv.ParseInt(strings.ReplaceAll(raw, "_", ""), 10, 64)
		if err != nil {
			return tomlValue{}, fmt.Errorf("unsupported value %q (hopfiles allow strings, integers, booleans and string arrays)", truncateForError(raw))
		}
		return tomlValue{num: n, kind: 'n'}, nil
	}
}

// splitTopLevel splits on commas that are not inside quotes.
func splitTopLevel(s string) []string {
	var out []string
	depth, start := 0, 0
	inStr := false
	var quote byte
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case inStr && c == quote:
			inStr = false
		case !inStr && (c == '"' || c == '\''):
			inStr, quote = true, c
		case !inStr && c == '[':
			depth++
		case !inStr && c == ']':
			depth--
		case !inStr && c == ',' && depth == 0:
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

func unquoteTOML(s string) (string, error) {
	s = strings.TrimSpace(s)
	if len(s) < 2 {
		return "", fmt.Errorf("unterminated string %q", s)
	}
	switch s[0] {
	case '\'':
		if s[len(s)-1] != '\'' {
			return "", fmt.Errorf("unterminated literal string")
		}
		return s[1 : len(s)-1], nil // literal strings take no escapes
	case '"':
		if s[len(s)-1] != '"' {
			return "", fmt.Errorf("unterminated string")
		}
		body := s[1 : len(s)-1]
		if !strings.Contains(body, `\`) {
			return body, nil
		}
		var b strings.Builder
		for i := 0; i < len(body); i++ {
			if body[i] != '\\' || i == len(body)-1 {
				b.WriteByte(body[i])
				continue
			}
			i++
			switch body[i] {
			case 'n':
				b.WriteByte('\n')
			case 't':
				b.WriteByte('\t')
			case 'r':
				b.WriteByte('\r')
			case '"':
				b.WriteByte('"')
			case '\\':
				b.WriteByte('\\')
			default:
				b.WriteByte('\\')
				b.WriteByte(body[i])
			}
		}
		return b.String(), nil
	}
	return "", fmt.Errorf("not a quoted string")
}

func truncateForError(s string) string {
	if len(s) > 40 {
		return s[:40] + "…"
	}
	return s
}
