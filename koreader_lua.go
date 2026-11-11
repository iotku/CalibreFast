package main

// Reading and writing the Lua tables KOReader keeps its settings in.
//
// KOReader's files are Lua source: "return { ["key"] = value, … }". They are
// read here as data, by a parser for exactly that: tables, strings, numbers,
// booleans and nil. Nothing is ever executed, and anything else (a function
// call, an operator, a variable) is a syntax error.

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
)

const (
	maxLuaDepth = 32
	maxLuaItems = 200000 // values in one file
)

// luaTable is a Lua table: its array part (keys 1…n, in order) and the rest.
type luaTable struct {
	arr  []any
	hash map[string]any
}

func (t *luaTable) str(key string) string {
	s, _ := t.hash[key].(string)
	return s
}

func (t *luaTable) num(key string) (float64, bool) {
	f, ok := t.hash[key].(float64)
	return f, ok
}

func (t *luaTable) table(key string) *luaTable {
	v, _ := t.hash[key].(*luaTable)
	return v
}

type luaParser struct {
	s     string
	pos   int
	items int
}

// parseLuaTable reads a file of the form "return { … }" (comments allowed).
func parseLuaTable(src string) (*luaTable, error) {
	p := &luaParser{s: src}
	p.skip()
	if !strings.HasPrefix(p.s[p.pos:], "return") {
		return nil, errors.New("not a KOReader settings file: it should start with \"return {\"")
	}
	p.pos += len("return")
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	t, ok := v.(*luaTable)
	if !ok {
		return nil, errors.New("not a KOReader settings file: no table")
	}
	p.skip()
	if p.pos != len(p.s) {
		return nil, p.errorf("unexpected text after the table")
	}
	return t, nil
}

func (p *luaParser) errorf(format string, args ...any) error {
	line := 1 + strings.Count(p.s[:min(p.pos, len(p.s))], "\n")
	return fmt.Errorf("line %d: %s", line, fmt.Sprintf(format, args...))
}

// skip passes over white space and comments ("-- …" and "--[[ … ]]").
func (p *luaParser) skip() {
	for p.pos < len(p.s) {
		switch c := p.s[p.pos]; {
		case c == ' ' || c == '\t' || c == '\r' || c == '\n':
			p.pos++
		case strings.HasPrefix(p.s[p.pos:], "--[["):
			end := strings.Index(p.s[p.pos:], "]]")
			if end < 0 {
				p.pos = len(p.s)
				return
			}
			p.pos += end + 2
		case strings.HasPrefix(p.s[p.pos:], "--"):
			end := strings.IndexByte(p.s[p.pos:], '\n')
			if end < 0 {
				p.pos = len(p.s)
				return
			}
			p.pos += end
		default:
			return
		}
	}
}

func isLuaNameByte(c byte, first bool) bool {
	return c == '_' || (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (!first && c >= '0' && c <= '9')
}

func (p *luaParser) name() string {
	start := p.pos
	for p.pos < len(p.s) && isLuaNameByte(p.s[p.pos], p.pos == start) {
		p.pos++
	}
	return p.s[start:p.pos]
}

func (p *luaParser) value(depth int) (any, error) {
	p.skip()
	if p.pos >= len(p.s) {
		return nil, p.errorf("unexpected end of file")
	}
	if p.items++; p.items > maxLuaItems {
		return nil, errors.New("too large")
	}
	switch c := p.s[p.pos]; {
	case c == '{':
		return p.table(depth)
	case c == '"' || c == '\'':
		return p.quoted()
	case c == '-' || c == '.' || (c >= '0' && c <= '9'):
		return p.number()
	case isLuaNameByte(c, true):
		switch word := p.name(); word {
		case "true":
			return true, nil
		case "false":
			return false, nil
		case "nil":
			return nil, nil
		default:
			return nil, p.errorf("unexpected %q: only plain values can be read", word)
		}
	}
	return nil, p.errorf("unexpected %q", p.s[p.pos])
}

func (p *luaParser) number() (any, error) {
	start := p.pos
	if p.s[p.pos] == '-' {
		p.pos++
	}
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		if (c >= '0' && c <= '9') || c == '.' || c == 'x' || c == 'X' || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F') ||
			((c == '+' || c == '-') && (p.s[p.pos-1] == 'e' || p.s[p.pos-1] == 'E')) {
			p.pos++
			continue
		}
		break
	}
	text := p.s[start:p.pos]
	if f, err := strconv.ParseFloat(text, 64); err == nil {
		return f, nil
	}
	if n, err := strconv.ParseInt(text, 0, 64); err == nil {
		return float64(n), nil
	}
	return nil, p.errorf("not a number: %q", text)
}

func (p *luaParser) quoted() (any, error) {
	quote := p.s[p.pos]
	p.pos++
	var b strings.Builder
	for p.pos < len(p.s) {
		c := p.s[p.pos]
		p.pos++
		switch {
		case c == quote:
			return b.String(), nil
		case c == '\n':
			return nil, p.errorf("unfinished string")
		case c != '\\':
			b.WriteByte(c)
		default:
			if p.pos >= len(p.s) {
				return nil, p.errorf("unfinished string")
			}
			e := p.s[p.pos]
			p.pos++
			switch e {
			case 'n', '\n':
				b.WriteByte('\n') // "\" then a real line break is how %q writes one
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'a':
				b.WriteByte(7)
			case 'b':
				b.WriteByte(8)
			case 'f':
				b.WriteByte(12)
			case 'v':
				b.WriteByte(11)
			case '\\', '"', '\'':
				b.WriteByte(e)
			case 'z': // skips the white space that follows
				for p.pos < len(p.s) && strings.IndexByte(" \t\r\n", p.s[p.pos]) >= 0 {
					p.pos++
				}
			case 'x':
				if p.pos+2 > len(p.s) {
					return nil, p.errorf("bad \\x escape")
				}
				n, err := strconv.ParseUint(p.s[p.pos:p.pos+2], 16, 8)
				if err != nil {
					return nil, p.errorf("bad \\x escape")
				}
				b.WriteByte(byte(n))
				p.pos += 2
			default:
				if e < '0' || e > '9' {
					return nil, p.errorf("unknown escape \\%c", e)
				}
				// Up to three decimal digits: a byte.
				n := int(e - '0')
				for i := 0; i < 2 && p.pos < len(p.s) && p.s[p.pos] >= '0' && p.s[p.pos] <= '9'; i++ {
					n = n*10 + int(p.s[p.pos]-'0')
					p.pos++
				}
				if n > 255 {
					return nil, p.errorf("bad decimal escape")
				}
				b.WriteByte(byte(n))
			}
		}
	}
	return nil, p.errorf("unfinished string")
}

func (p *luaParser) table(depth int) (any, error) {
	if depth >= maxLuaDepth {
		return nil, p.errorf("tables nested too deeply")
	}
	p.pos++ // {
	t := &luaTable{hash: map[string]any{}}
	numbered := map[int]any{}
	for {
		p.skip()
		if p.pos >= len(p.s) {
			return nil, p.errorf("unfinished table")
		}
		if p.s[p.pos] == '}' {
			p.pos++
			break
		}
		var key any // nil: positional
		switch {
		case p.s[p.pos] == '[':
			p.pos++
			k, err := p.value(depth + 1)
			if err != nil {
				return nil, err
			}
			p.skip()
			if p.pos >= len(p.s) || p.s[p.pos] != ']' {
				return nil, p.errorf("expected ]")
			}
			p.pos++
			if k == nil {
				return nil, p.errorf("a key can't be nil")
			}
			key = k
		case isLuaNameByte(p.s[p.pos], true):
			// name = value, unless it's a bare true/false/nil in the array part
			save := p.pos
			word := p.name()
			p.skip()
			if p.pos < len(p.s) && p.s[p.pos] == '=' && !strings.HasPrefix(p.s[p.pos:], "==") {
				key = word
			} else {
				p.pos = save
			}
		}
		if key != nil {
			p.skip()
			if p.pos >= len(p.s) || p.s[p.pos] != '=' {
				return nil, p.errorf("expected =")
			}
			p.pos++
		}
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		switch k := key.(type) {
		case nil:
			t.arr = append(t.arr, v)
		case string:
			t.hash[k] = v
		case float64:
			if k == math.Trunc(k) && k >= 1 && k <= maxLuaItems {
				// Re-check as an integer: k is already in range, but CodeQL
				// only recognises integer bounds checks before narrowing.
				if i := int64(k); i >= 1 && i <= maxLuaItems {
					numbered[int(i)] = v
				}
			} else {
				t.hash[strconv.FormatFloat(k, 'g', -1, 64)] = v
			}
		case bool:
			t.hash[strconv.FormatBool(k)] = v
		default:
			return nil, p.errorf("a table can't be a key")
		}
		p.skip()
		if p.pos < len(p.s) && (p.s[p.pos] == ',' || p.s[p.pos] == ';') {
			p.pos++
		}
	}
	// [1] = …, [2] = … continue the array part; a gap ends it.
	for i := len(t.arr) + 1; ; i++ {
		v, ok := numbered[i]
		if !ok {
			break
		}
		t.arr = append(t.arr, v)
		delete(numbered, i)
	}
	for i, v := range numbered {
		t.hash[strconv.Itoa(i)] = v
	}
	return t, nil
}

// writeLua writes a value the way KOReader's own dump does: one entry per
// line, four spaces a level, array entries as [n] = …, keys sorted.
func writeLua(b *strings.Builder, v any, indent int) {
	switch v := v.(type) {
	case nil:
		b.WriteString("nil")
	case bool:
		b.WriteString(strconv.FormatBool(v))
	case int:
		b.WriteString(strconv.Itoa(v))
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			b.WriteString("0")
		} else {
			b.WriteString(strconv.FormatFloat(v, 'f', -1, 64))
		}
	case string:
		writeLuaString(b, v)
	case *luaTable:
		pad := strings.Repeat("    ", indent+1)
		b.WriteString("{\n")
		for i, item := range v.arr {
			fmt.Fprintf(b, "%s[%d] = ", pad, i+1)
			writeLua(b, item, indent+1)
			b.WriteString(",\n")
		}
		keys := make([]string, 0, len(v.hash))
		for k := range v.hash {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			b.WriteString(pad + "[")
			writeLuaString(b, k)
			b.WriteString("] = ")
			writeLua(b, v.hash[k], indent+1)
			b.WriteString(",\n")
		}
		b.WriteString(strings.Repeat("    ", indent) + "}")
	default:
		b.WriteString("nil")
	}
}

// writeLuaString quotes s as Lua's %q does.
func writeLuaString(b *strings.Builder, s string) {
	b.WriteByte('"')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '"', '\\':
			b.WriteByte('\\')
			b.WriteByte(c)
		case '\n':
			b.WriteString("\\\n")
		case '\r':
			b.WriteString("\\13")
		case 0:
			b.WriteString("\\0")
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('"')
}
