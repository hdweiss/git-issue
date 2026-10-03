// The event schema of docs/blob-format.md: the eight fields, canonical
// serialization, derived ids, and the ordering rule. See doc.go.

package entity

import (
	"bytes"
	"crypto/rand"
	"crypto/sha1"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"hash"
	"strconv"
)

// FormatVersion is the highest v this client understands. Events above it are
// ignored by Parse but still preserved on disk, per docs/blob-format.md.
const FormatVersion = 1

// ObjectFormat is a repository's hash algorithm. Entity ids are git note keys,
// so their width is the repo's object-name width (docs/storage-model.md).
type ObjectFormat string

const (
	SHA1   ObjectFormat = "sha1"
	SHA256 ObjectFormat = "sha256"
)

func (f ObjectFormat) new() (hash.Hash, error) {
	switch f {
	case SHA1, "":
		return sha1.New(), nil
	case SHA256:
		return sha256.New(), nil
	}
	return nil, fmt.Errorf("unsupported object format %q", string(f))
}

// Value is an event's val payload. The format allows a string, a boolean or
// null; Present distinguishes null from absent, which matters because
// `milestone: null` clears the field while an absent val means the op carries
// none at all.
type Value struct {
	Present bool
	Kind    Kind
	Str     string
	Bool    bool
}

type Kind uint8

const (
	KindNull Kind = iota
	KindString
	KindBool
)

func Str(s string) Value { return Value{Present: true, Kind: KindString, Str: s} }
func Bool(b bool) Value  { return Value{Present: true, Kind: KindBool, Bool: b} }
func Null() Value        { return Value{Present: true, Kind: KindNull} }

// IsString reports whether the value is a string, the common case.
func (v Value) IsString() bool { return v.Present && v.Kind == KindString }

// Display renders a value the way the folded state exposes it: strings as
// themselves, booleans and null in their JSON spelling, absent as "".
func (v Value) Display() string {
	switch {
	case !v.Present:
		return ""
	case v.Kind == KindString:
		return v.Str
	case v.Kind == KindBool:
		return strconv.FormatBool(v.Bool)
	default:
		return ""
	}
}

// Truthy mirrors the prototype's Python-flavoured emptiness test, which the
// renderer's header block depends on: absent, null, "" and false are all falsy.
func (v Value) Truthy() bool {
	switch {
	case !v.Present:
		return false
	case v.Kind == KindString:
		return v.Str != ""
	case v.Kind == KindBool:
		return v.Bool
	default:
		return false
	}
}

// Event is one line of an entity blob.
type Event struct {
	V   int64
	C   int64 // Lamport clock: the ordering key
	TS  int64 // wall clock, display only
	A   string
	Op  string
	N   string
	Ref string // id of another event this one targets; omitted when empty
	Val Value

	// Raw is the line exactly as it was read, and ID is its hash. Keeping the
	// original bytes is the preservation invariant: an event this client does
	// not understand must survive a round trip byte for byte, so nothing here
	// ever re-serializes a line it did not itself create.
	Raw []byte
	ID  string
}

// New builds an event and computes its id from its own canonical bytes.
func NewEvent(f ObjectFormat, v, c, ts int64, a, op string, val Value, ref string) (Event, error) {
	n, err := Nonce()
	if err != nil {
		return Event{}, err
	}
	return build(f, Event{V: v, C: c, TS: ts, A: a, Op: op, N: n, Ref: ref, Val: val})
}

// NewWithNonce is New for callers that must derive the nonce rather than
// generate one — bridges, per docs/storage-model.md, where a random nonce would
// make every re-import duplicate every
func NewEventWithNonce(f ObjectFormat, e Event) (Event, error) { return build(f, e) }

func build(f ObjectFormat, e Event) (Event, error) {
	raw, err := e.canonical()
	if err != nil {
		return Event{}, err
	}
	e.Raw = raw
	e.ID, err = HashLine(f, raw)
	if err != nil {
		return Event{}, err
	}
	return e, nil
}

// canonical serializes the eight fields in canonical form. The schema's key
// set — a, c, n, op, ref, ts, v, val — is already sorted by code point, so the
// order below is the canonical order and needs no sort at runtime.
func (e Event) canonical() ([]byte, error) {
	var b bytes.Buffer
	b.WriteByte('{')

	b.WriteString(`"a":`)
	writeJSONString(&b, e.A)
	b.WriteString(`,"c":`)
	b.WriteString(strconv.FormatInt(e.C, 10))
	b.WriteString(`,"n":`)
	writeJSONString(&b, e.N)
	b.WriteString(`,"op":`)
	writeJSONString(&b, e.Op)
	if e.Ref != "" {
		b.WriteString(`,"ref":`)
		writeJSONString(&b, e.Ref)
	}
	b.WriteString(`,"ts":`)
	b.WriteString(strconv.FormatInt(e.TS, 10))
	b.WriteString(`,"v":`)
	b.WriteString(strconv.FormatInt(e.V, 10))
	if e.Val.Present {
		b.WriteString(`,"val":`)
		switch e.Val.Kind {
		case KindString:
			writeJSONString(&b, e.Val.Str)
		case KindBool:
			b.WriteString(strconv.FormatBool(e.Val.Bool))
		default:
			b.WriteString("null")
		}
	}
	b.WriteByte('}')

	// Cheap insurance that the hand-rolled writer above and the general
	// canonicalizer agree; they hash the same bytes, so a divergence here
	// would fork identity silently.
	out := b.Bytes()
	if !IsCanonical(out) {
		return nil, fmt.Errorf("event: serialization is not canonical: %s", out)
	}
	return out, nil
}

// HashLine returns `git hash-object` of one event line, newline included.
// This is a plain git blob hash, re-derivable with stock tooling.
func HashLine(f ObjectFormat, line []byte) (string, error) {
	h, err := f.new()
	if err != nil {
		return "", err
	}
	payload := make([]byte, 0, len(line)+1)
	payload = append(payload, line...)
	payload = append(payload, '\n')

	fmt.Fprintf(h, "blob %d\x00", len(payload))
	h.Write(payload)
	return hex.EncodeToString(h.Sum(nil)), nil
}

// Nonce returns 64 bits of randomness, hex. Without it, two people posting
// identical text in the same second write byte-identical events and the merge's
// uniq step silently drops one of them.
func Nonce() (string, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

// Parse decodes one line. It returns ok=false for anything this client cannot
// interpret; callers keep such lines on disk regardless.
//
// The checks mirror the prototype exactly, including that a non-integer v is
// dropped rather than fatal — an earlier draft of the format used v for the
// *value*, and such notes may still exist (AGENTS.md, "Traps").
func ParseEvent(f ObjectFormat, line []byte) (Event, bool) {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(line, &raw); err != nil {
		return Event{}, false
	}

	v, ok := jsonInt(raw["v"])
	if !ok || v > FormatVersion {
		return Event{}, false
	}
	c, ok := jsonInt(raw["c"])
	if !ok {
		return Event{}, false
	}
	op, ok := jsonString(raw["op"])
	if !ok {
		return Event{}, false
	}

	id, err := HashLine(f, line)
	if err != nil {
		return Event{}, false
	}

	e := Event{V: v, C: c, Op: op, ID: id, Raw: append([]byte(nil), line...)}
	e.TS, _ = jsonInt(raw["ts"])
	e.A, _ = jsonString(raw["a"])
	e.N, _ = jsonString(raw["n"])
	e.Ref, _ = jsonString(raw["ref"])
	if val, present := raw["val"]; present {
		e.Val = parseValue(val)
	}
	return e, true
}

func parseValue(b json.RawMessage) Value {
	var v any
	if err := json.Unmarshal(b, &v); err != nil {
		return Value{}
	}
	switch t := v.(type) {
	case string:
		return Str(t)
	case bool:
		return Bool(t)
	case nil:
		return Null()
	default:
		// Numbers, arrays and objects are not vocabulary this client knows.
		// The line is still preserved; only its val is uninterpretable.
		return Value{Present: true, Kind: KindString, Str: string(b)}
	}
}

func jsonInt(b json.RawMessage) (int64, bool) {
	if len(b) == 0 {
		return 0, false
	}
	// json.Unmarshal into any yields float64 and would accept 1.5 as an int;
	// parse the literal instead so only genuine integers pass.
	i, err := strconv.ParseInt(string(bytes.TrimSpace(b)), 10, 64)
	if err != nil {
		return 0, false
	}
	return i, true
}

func jsonString(b json.RawMessage) (string, bool) {
	if len(b) == 0 {
		return "", false
	}
	var s string
	if err := json.Unmarshal(b, &s); err != nil {
		return "", false
	}
	return s, true
}

// Less is the ordering rule: by Lamport clock, then by event id. The id
// tie-break is arbitrary, meaningless, and identical on every replica — which
// is all a tie-break needs to be.
func Less(a, b Event) int {
	switch {
	case a.C != b.C:
		if a.C < b.C {
			return -1
		}
		return 1
	case a.ID < b.ID:
		return -1
	case a.ID > b.ID:
		return 1
	}
	return 0
}
