//go:build !no_wasm

package wasm

import (
	"fmt"
	"strconv"
	"strings"
)

// parseTOML parses the small TOML subset used by sidecar files: bare/quoted
// keys, strings, integers, booleans, arrays, inline tables and [table]
// headers (one level). It exists to avoid a new dependency.
func parseTOML(src string) (map[string]any, error) {
	p := &tomlParser{s: src}
	root := map[string]any{}
	cur := root
	for {
		p.skipSpace(true)
		if p.eof() {
			return root, nil
		}
		if p.peek() == '[' {
			p.pos++
			end := strings.IndexByte(p.s[p.pos:], ']')
			if end < 0 {
				return nil, p.errf("unterminated table header")
			}
			name := strings.TrimSpace(p.s[p.pos : p.pos+end])
			p.pos += end + 1
			if name == "" || strings.ContainsAny(name, ".[") {
				return nil, p.errf("unsupported table header %q", name)
			}
			t := map[string]any{}
			root[name] = t
			cur = t
			continue
		}
		key, err := p.key()
		if err != nil {
			return nil, err
		}
		p.skipSpace(false)
		if p.eof() || p.peek() != '=' {
			return nil, p.errf("expected '=' after key %q", key)
		}
		p.pos++
		p.skipSpace(false)
		v, err := p.value()
		if err != nil {
			return nil, err
		}
		if _, dup := cur[key]; dup {
			return nil, p.errf("duplicate key %q", key)
		}
		cur[key] = v
		p.skipSpace(false)
		if !p.eof() && p.peek() != '\n' && p.peek() != '\r' && p.peek() != '#' {
			return nil, p.errf("unexpected %q after value", string(p.peek()))
		}
	}
}

type tomlParser struct {
	s   string
	pos int
}

func (p *tomlParser) eof() bool  { return p.pos >= len(p.s) }
func (p *tomlParser) peek() byte { return p.s[p.pos] }
func (p *tomlParser) errf(f string, a ...any) error {
	line := 1 + strings.Count(p.s[:min(p.pos, len(p.s))], "\n")
	return fmt.Errorf("toml line %d: %s", line, fmt.Sprintf(f, a...))
}

// skipSpace skips blanks and comments; with nl it also skips newlines.
func (p *tomlParser) skipSpace(nl bool) {
	for !p.eof() {
		c := p.peek()
		switch {
		case c == ' ' || c == '\t':
			p.pos++
		case nl && (c == '\n' || c == '\r'):
			p.pos++
		case c == '#':
			for !p.eof() && p.peek() != '\n' {
				p.pos++
			}
		default:
			return
		}
	}
}

func (p *tomlParser) key() (string, error) {
	if p.eof() {
		return "", p.errf("expected key")
	}
	if c := p.peek(); c == '"' || c == '\'' {
		s, err := p.str()
		return s, err
	}
	start := p.pos
	for !p.eof() {
		c := p.peek()
		if c == '_' || c == '-' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') {
			p.pos++
			continue
		}
		break
	}
	if start == p.pos {
		return "", p.errf("invalid key")
	}
	return p.s[start:p.pos], nil
}

func (p *tomlParser) str() (string, error) {
	q := p.peek()
	p.pos++
	start := p.pos
	if q == '\'' {
		end := strings.IndexByte(p.s[p.pos:], '\'')
		if end < 0 || strings.ContainsRune(p.s[p.pos:p.pos+end], '\n') {
			return "", p.errf("unterminated string")
		}
		p.pos += end + 1
		return p.s[start : start+end], nil
	}
	for !p.eof() {
		switch p.peek() {
		case '\\':
			p.pos += 2
		case '"':
			raw := p.s[start-1 : p.pos+1]
			p.pos++
			s, err := strconv.Unquote(raw)
			if err != nil {
				return "", p.errf("invalid string escape")
			}
			return s, nil
		case '\n':
			return "", p.errf("unterminated string")
		default:
			p.pos++
		}
	}
	return "", p.errf("unterminated string")
}

func (p *tomlParser) value() (any, error) {
	if p.eof() {
		return nil, p.errf("expected value")
	}
	switch c := p.peek(); {
	case c == '"' || c == '\'':
		return p.str()
	case c == '[':
		p.pos++
		arr := []any{}
		for {
			p.skipSpace(true)
			if p.eof() {
				return nil, p.errf("unterminated array")
			}
			if p.peek() == ']' {
				p.pos++
				return arr, nil
			}
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			arr = append(arr, v)
			p.skipSpace(true)
			if !p.eof() && p.peek() == ',' {
				p.pos++
			}
		}
	case c == '{':
		p.pos++
		m := map[string]any{}
		for {
			p.skipSpace(false)
			if p.eof() {
				return nil, p.errf("unterminated inline table")
			}
			if p.peek() == '}' {
				p.pos++
				return m, nil
			}
			k, err := p.key()
			if err != nil {
				return nil, err
			}
			p.skipSpace(false)
			if p.eof() || p.peek() != '=' {
				return nil, p.errf("expected '=' in inline table")
			}
			p.pos++
			p.skipSpace(false)
			v, err := p.value()
			if err != nil {
				return nil, err
			}
			m[k] = v
			p.skipSpace(false)
			if !p.eof() && p.peek() == ',' {
				p.pos++
			}
		}
	default:
		start := p.pos
		for !p.eof() && !strings.ContainsRune(",]}\n\r# \t", rune(p.peek())) {
			p.pos++
		}
		tok := p.s[start:p.pos]
		switch tok {
		case "true":
			return true, nil
		case "false":
			return false, nil
		}
		n, err := strconv.ParseInt(strings.ReplaceAll(tok, "_", ""), 0, 64)
		if err != nil {
			return nil, p.errf("invalid value %q", tok)
		}
		return n, nil
	}
}
