package relay

// Canonical JSON for the remote control protocol (protocol.md 3.3): RFC 8785
// (JCS) under the protocol's I-JSON profile. Go's encoding/json is not used
// for this: it silently replaces lone surrogates and invalid UTF-8 with
// U+FFFD, can't see duplicate keys, and escapes U+2028/U+2029 on output, so
// its bytes are not JCS. This is a small strict parser and a canonicalizer.

import (
	"errors"
	"slices"
	"strconv"
	"strings"
	"unicode/utf16"
	"unicode/utf8"
)

// maxSafeInt is 2^53-1, the largest integer the profile allows.
const maxSafeInt = 1<<53 - 1

// maxDepth bounds nesting so a hostile document can't exhaust the stack.
const maxDepth = 64

// errMalformed is any violation of the JSON grammar or the profile.
var errMalformed = errors.New("malformed")

// parseStrict parses one JSON value under the protocol profile: integers only
// (|n| <= 2^53-1, no fraction or exponent), no duplicate object keys, no lone
// surrogates, valid UTF-8, nothing after the value but whitespace. Objects
// become map[string]any, arrays []any, numbers int64, strings string.
func parseStrict(data []byte) (any, error) {
	if !utf8.Valid(data) {
		return nil, errMalformed
	}
	p := &parser{s: data}
	p.ws()
	v, err := p.value(0)
	if err != nil {
		return nil, err
	}
	p.ws()
	if p.i != len(p.s) {
		return nil, errMalformed
	}
	return v, nil
}

type parser struct {
	s []byte
	i int
}

func (p *parser) ws() {
	for p.i < len(p.s) {
		switch p.s[p.i] {
		case ' ', '\t', '\n', '\r':
			p.i++
		default:
			return
		}
	}
}

func (p *parser) value(depth int) (any, error) {
	if depth > maxDepth || p.i >= len(p.s) {
		return nil, errMalformed
	}
	switch c := p.s[p.i]; {
	case c == '{':
		return p.object(depth)
	case c == '[':
		return p.array(depth)
	case c == '"':
		return p.str()
	case c == '-' || (c >= '0' && c <= '9'):
		return p.number()
	case p.lit("true"):
		return true, nil
	case p.lit("false"):
		return false, nil
	case p.lit("null"):
		return nil, nil
	}
	return nil, errMalformed
}

func (p *parser) lit(word string) bool {
	if len(p.s)-p.i >= len(word) && string(p.s[p.i:p.i+len(word)]) == word {
		p.i += len(word)
		return true
	}
	return false
}

func (p *parser) object(depth int) (any, error) {
	p.i++ // {
	out := map[string]any{}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return out, nil
	}
	for {
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != '"' {
			return nil, errMalformed
		}
		k, err := p.str()
		if err != nil {
			return nil, err
		}
		if _, dup := out[k]; dup {
			return nil, errMalformed
		}
		p.ws()
		if p.i >= len(p.s) || p.s[p.i] != ':' {
			return nil, errMalformed
		}
		p.i++
		p.ws()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out[k] = v
		p.ws()
		if p.i >= len(p.s) {
			return nil, errMalformed
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return out, nil
		default:
			return nil, errMalformed
		}
	}
}

func (p *parser) array(depth int) (any, error) {
	p.i++ // [
	out := []any{}
	p.ws()
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return out, nil
	}
	for {
		p.ws()
		v, err := p.value(depth + 1)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
		p.ws()
		if p.i >= len(p.s) {
			return nil, errMalformed
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case ']':
			p.i++
			return out, nil
		default:
			return nil, errMalformed
		}
	}
}

// number accepts only the integer form of the JSON number grammar.
func (p *parser) number() (any, error) {
	start := p.i
	if p.s[p.i] == '-' {
		p.i++
	}
	digits := p.i
	for p.i < len(p.s) && p.s[p.i] >= '0' && p.s[p.i] <= '9' {
		p.i++
	}
	if p.i == digits || (p.s[digits] == '0' && p.i-digits > 1) {
		return nil, errMalformed // no digits, or a leading zero
	}
	if p.i < len(p.s) && (p.s[p.i] == '.' || p.s[p.i] == 'e' || p.s[p.i] == 'E') {
		return nil, errMalformed // a fraction or an exponent: not an integer token
	}
	n, err := strconv.ParseInt(string(p.s[start:p.i]), 10, 64)
	if err != nil || n > maxSafeInt || n < -maxSafeInt {
		return nil, errMalformed
	}
	return n, nil
}

func hexVal(c byte) (rune, bool) {
	switch {
	case c >= '0' && c <= '9':
		return rune(c - '0'), true
	case c >= 'a' && c <= 'f':
		return rune(c-'a') + 10, true
	case c >= 'A' && c <= 'F':
		return rune(c-'A') + 10, true
	}
	return 0, false
}

func (p *parser) hex4() (rune, bool) {
	if p.i+4 > len(p.s) {
		return 0, false
	}
	var r rune
	for k := 0; k < 4; k++ {
		h, ok := hexVal(p.s[p.i+k])
		if !ok {
			return 0, false
		}
		r = r<<4 | h
	}
	p.i += 4
	return r, true
}

func (p *parser) str() (string, error) {
	p.i++ // opening quote
	var b strings.Builder
	for {
		if p.i >= len(p.s) {
			return "", errMalformed
		}
		c := p.s[p.i]
		switch {
		case c == '"':
			p.i++
			return b.String(), nil
		case c < 0x20:
			return "", errMalformed // raw control characters are not allowed
		case c == '\\':
			p.i++
			if p.i >= len(p.s) {
				return "", errMalformed
			}
			e := p.s[p.i]
			p.i++
			switch e {
			case '"', '\\', '/':
				b.WriteByte(e)
			case 'b':
				b.WriteByte('\b')
			case 'f':
				b.WriteByte('\f')
			case 'n':
				b.WriteByte('\n')
			case 'r':
				b.WriteByte('\r')
			case 't':
				b.WriteByte('\t')
			case 'u':
				r, ok := p.hex4()
				if !ok {
					return "", errMalformed
				}
				switch {
				case utf16.IsSurrogate(r) && r < 0xDC00: // a high surrogate needs a low one next
					if p.i+2 > len(p.s) || p.s[p.i] != '\\' || p.s[p.i+1] != 'u' {
						return "", errMalformed
					}
					p.i += 2
					lo, ok := p.hex4()
					if !ok || lo < 0xDC00 || lo > 0xDFFF {
						return "", errMalformed
					}
					b.WriteRune(utf16.DecodeRune(r, lo))
				case utf16.IsSurrogate(r): // a lone low surrogate
					return "", errMalformed
				default:
					b.WriteRune(r)
				}
			default:
				return "", errMalformed
			}
		default:
			// Raw UTF-8 (validated for the whole input up front).
			_, size := utf8.DecodeRune(p.s[p.i:])
			b.Write(p.s[p.i : p.i+size])
			p.i += size
		}
	}
}

// canonical appends the JCS form of v (a value from parseStrict) to dst.
func canonical(dst []byte, v any) []byte {
	switch x := v.(type) {
	case nil:
		return append(dst, "null"...)
	case bool:
		if x {
			return append(dst, "true"...)
		}
		return append(dst, "false"...)
	case int64:
		return strconv.AppendInt(dst, x, 10)
	case int:
		return strconv.AppendInt(dst, int64(x), 10)
	case string:
		return appendJCSString(dst, x)
	case []any:
		dst = append(dst, '[')
		for i, e := range x {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = canonical(dst, e)
		}
		return append(dst, ']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		slices.SortFunc(keys, compareUTF16)
		dst = append(dst, '{')
		for i, k := range keys {
			if i > 0 {
				dst = append(dst, ',')
			}
			dst = appendJCSString(dst, k)
			dst = append(dst, ':')
			dst = canonical(dst, x[k])
		}
		return append(dst, '}')
	}
	panic("relay: canonical: unsupported type")
}

// compareUTF16 orders strings by their UTF-16 code units, as JCS requires
// (it differs from byte/code point order for characters above U+FFFF).
func compareUTF16(a, b string) int {
	return slices.Compare(utf16.Encode([]rune(a)), utf16.Encode([]rune(b)))
}

// appendJCSString writes s as ECMAScript's JSON.stringify does: only the
// mandatory escapes, lower-case \u00xx for other controls, everything else
// (U+007F, U+2028, U+2029, non-ASCII) as raw UTF-8.
func appendJCSString(dst []byte, s string) []byte {
	const hexdig = "0123456789abcdef"
	dst = append(dst, '"')
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch c {
		case '"':
			dst = append(dst, '\\', '"')
		case '\\':
			dst = append(dst, '\\', '\\')
		case '\b':
			dst = append(dst, '\\', 'b')
		case '\f':
			dst = append(dst, '\\', 'f')
		case '\n':
			dst = append(dst, '\\', 'n')
		case '\r':
			dst = append(dst, '\\', 'r')
		case '\t':
			dst = append(dst, '\\', 't')
		default:
			if c < 0x20 {
				dst = append(dst, '\\', 'u', '0', '0', hexdig[c>>4], hexdig[c&0xF])
			} else {
				dst = append(dst, c)
			}
		}
	}
	return append(dst, '"')
}
