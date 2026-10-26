// Package envfile reads and writes the KEY=value file persisted by -install.
//
// The same file is consumed by two independent parsers: this package (for
// -once and daemon runs outside systemd) and systemd's EnvironmentFile= (for
// the installed unit). Both must yield identical values. systemd implements
// shell-like quoting and backslash escapes that are not reimplemented here;
// instead the grammar is restricted to the subset on which the two parsers
// trivially agree: one KEY=value per line, value made only of printable ASCII
// without whitespace, quotes, backslash, '#' or ';'. Encode refuses any other
// value and Parse refuses any other line, so a value can never be read one way
// by cfddns and another way by systemd.
package envfile

import (
	"bufio"
	"fmt"
	"io"
	"strings"
)

// Pair is one KEY=value entry in file order.
type Pair struct {
	Key   string
	Value string
}

// CheckValue reports whether v is representable in the shared grammar.
func CheckValue(v string) error {
	for i := 0; i < len(v); i++ {
		ch := v[i]
		if ch < 0x21 || ch > 0x7e || strings.IndexByte(`"'\#;`, ch) >= 0 {
			return fmt.Errorf("character %q at offset %d not allowed: values may only contain printable ASCII without whitespace, quotes, backslash, '#' or ';'", ch, i)
		}
	}
	return nil
}

// CheckKey reports whether k is a plain environment variable name.
func CheckKey(k string) error {
	if k == "" {
		return fmt.Errorf("empty key")
	}
	for i := 0; i < len(k); i++ {
		ch := k[i]
		if (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9' && i > 0) || ch == '_' {
			continue
		}
		return fmt.Errorf("key %q: only [A-Z_][A-Z0-9_]* allowed", k)
	}
	return nil
}

// Encode renders pairs in order, refusing any key or value outside the grammar.
func Encode(pairs []Pair) (string, error) {
	var b strings.Builder
	for _, p := range pairs {
		if err := CheckKey(p.Key); err != nil {
			return "", err
		}
		if err := CheckValue(p.Value); err != nil {
			return "", fmt.Errorf("%s: %w", p.Key, err)
		}
		b.WriteString(p.Key)
		b.WriteByte('=')
		b.WriteString(p.Value)
		b.WriteByte('\n')
	}
	return b.String(), nil
}

// Parse reads KEY=value lines. Blank lines and lines starting with '#' or ';'
// are ignored (systemd ignores the same). Any other line that is not a
// grammar-conforming assignment is an error rather than a guess.
func Parse(r io.Reader) (map[string]string, error) {
	out := make(map[string]string)
	sc := bufio.NewScanner(r)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected KEY=value", n)
		}
		if err := CheckKey(k); err != nil {
			return nil, fmt.Errorf("line %d: %w", n, err)
		}
		if err := CheckValue(v); err != nil {
			return nil, fmt.Errorf("line %d: %s: %w", n, k, err)
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
