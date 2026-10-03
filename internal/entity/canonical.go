// Canonical JSON, RFC 8785 (JCS). See doc.go for why this must be exact.
//
// One deliberate restriction: non-integer numbers are rejected rather than
// formatted. RFC 8785 defers to ECMAScript's Number::toString for those, and
// reimplementing it is a large amount of subtle code for a format whose eight
// fields hold only integers (v, c, ts). Rejecting is honest; silently emitting
// bytes we have not verified against the ES6 algorithm would not be. Note this
// costs nothing in preservation terms: events written by other clients are
// kept byte for byte and never re-serialized.

package entity

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"slices"
	"strconv"
	"unicode/utf16"
)

// Canonicalize returns the canonical JSON form of an arbitrary JSON document.
//
// It is used to write new events and, in tests, to assert that the worked
// examples in docs/ are already canonical.
func Canonicalize(src []byte) ([]byte, error) {
	dec := json.NewDecoder(bytes.NewReader(src))
	dec.UseNumber()

	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	// Reject trailing content: "1 2" must not canonicalize to "1".
	if _, err := dec.Token(); err != io.EOF {
		return nil, fmt.Errorf("trailing data after JSON value")
	}

	var buf bytes.Buffer
	if err := writeValue(&buf, v); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func writeValue(buf *bytes.Buffer, v any) error {
	switch t := v.(type) {
	case nil:
		buf.WriteString("null")
	case bool:
		buf.WriteString(strconv.FormatBool(t))
	case string:
		writeJSONString(buf, t)
	case json.Number:
		return writeNumber(buf, t)
	case []any:
		buf.WriteByte('[')
		for i, e := range t {
			if i > 0 {
				buf.WriteByte(',')
			}
			if err := writeValue(buf, e); err != nil {
				return err
			}
		}
		buf.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(t))
		for k := range t {
			keys = append(keys, k)
		}
		sortKeys(keys)
		buf.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				buf.WriteByte(',')
			}
			writeJSONString(buf, k)
			buf.WriteByte(':')
			if err := writeValue(buf, t[k]); err != nil {
				return err
			}
		}
		buf.WriteByte('}')
	default:
		return fmt.Errorf("jcs: unsupported value %T", v)
	}
	return nil
}

// writeNumber emits an integer literal. See the package comment for why
// non-integer numbers are refused rather than approximated.
func writeNumber(buf *bytes.Buffer, n json.Number) error {
	s := n.String()
	if i, err := strconv.ParseInt(s, 10, 64); err == nil {
		buf.WriteString(strconv.FormatInt(i, 10))
		return nil
	}
	// "1e2" and "1.0" are integers mathematically but not lexically; accept
	// them when they land exactly on an integer inside the IEEE-754 safe
	// range, which is the only range JSON parsers agree on.
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return fmt.Errorf("jcs: unparseable number %q", s)
	}
	if f != math.Trunc(f) || math.Abs(f) > 1<<53 {
		return fmt.Errorf("jcs: non-integer number %q is not supported", s)
	}
	buf.WriteString(strconv.FormatInt(int64(f), 10))
	return nil
}

// writeJSONString appends a canonically escaped JSON string.
//
// Per RFC 8785 section 3.2.2.2 the escape set is minimal: only '"', '\' and
// the C0 controls. '<', '>', '&' and '/' are emitted literally, and non-ASCII
// is emitted as raw UTF-8 rather than \u escapes.
func writeJSONString(buf *bytes.Buffer, s string) {
	buf.WriteByte('"')
	for _, r := range s {
		switch r {
		case '"':
			buf.WriteString(`\"`)
		case '\\':
			buf.WriteString(`\\`)
		case '\b':
			buf.WriteString(`\b`)
		case '\t':
			buf.WriteString(`\t`)
		case '\n':
			buf.WriteString(`\n`)
		case '\f':
			buf.WriteString(`\f`)
		case '\r':
			buf.WriteString(`\r`)
		default:
			if r < 0x20 {
				buf.WriteString(`\u00`)
				const hex = "0123456789abcdef"
				buf.WriteByte(hex[r>>4])
				buf.WriteByte(hex[r&0xf])
			} else {
				buf.WriteRune(r)
			}
		}
	}
	buf.WriteByte('"')
}

// sortKeys orders object keys as RFC 8785 requires: by UTF-16 code unit, not
// by UTF-8 byte. The two disagree only above the BMP, which the event schema's
// fixed key set never reaches — but a general canonicalizer must still be right.
func sortKeys(keys []string) {
	slices.SortFunc(keys, func(a, b string) int {
		switch {
		case lessUTF16(a, b):
			return -1
		case a == b:
			return 0
		default:
			return 1
		}
	})
}

func lessUTF16(a, b string) bool {
	ua, ub := utf16.Encode([]rune(a)), utf16.Encode([]rune(b))
	for i := 0; i < len(ua) && i < len(ub); i++ {
		if ua[i] != ub[i] {
			return ua[i] < ub[i]
		}
	}
	return len(ua) < len(ub)
}

// IsCanonical reports whether src is already in canonical form.
func IsCanonical(src []byte) bool {
	out, err := Canonicalize(src)
	return err == nil && bytes.Equal(out, src)
}
