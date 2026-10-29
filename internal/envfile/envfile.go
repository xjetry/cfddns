// Package envfile reads and writes the KEY=value file persisted by -install.
//
// cfddns is the file's only parser (every init system starts the bare
// binary, which reads the file itself), and the grammar is a documented
// contract with the operator: one KEY=value per line, the value made only of
// printable ASCII without whitespace, quotes, backslash, '#' or ';'. The
// restriction keeps the file unambiguous for a human editing it and for any
// shell-style reader an operator may point at it ("set -a; . /etc/cfddns.env",
// "env $(cat ...)"), since every such reader agrees with cfddns on a value
// that contains none of the characters they treat specially. Encode refuses
// any other value and Parse refuses any other line, so what -install writes
// from the command line reads back identically, and a hand edit that would
// read differently somewhere is an error here instead of a surprise.
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
