package entity

import (
	"bufio"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The ids docs/blob-format.md and docs/issues.md publish for their worked
// examples. Re-deriving them here makes the documents executable: it is the
// same check AGENTS.md describes doing by hand with awk, run on every build.
var docIDs = map[string]string{
	// blob-format.md
	`{"a":"hdweiss@gmail.com","c":1,"n":"92c4de017f","op":"create","ts":1787000000,"v":1,"val":"issue"}`:                   "e878760e137a63e084cddcb8e1ac14b16b3b5313",
	`{"a":"rev@example.com","c":2,"n":"b90e12f5aa","op":"comment","ts":1787000300,"v":1,"val":"Absent in a fresh clone."}`: "be9e96b43964e9f0cad6dac74b9d01cf40a74786",
	// issues.md
	`{"a":"hdweiss@gmail.com","c":1,"n":"3f9a1c7e2b","op":"create","ts":1787000000,"v":1,"val":"issue"}`:                                                        "4b0755a3e7697bfdf17e42e9f4b307c161ea2a40",
	`{"a":"hdweiss@gmail.com","c":4,"n":"0c4411ab8f","op":"label.add","ts":1787000002,"v":1,"val":"design"}`:                                                    "13687dbd41243bb438112efc972cfb21afaeb63d",
	`{"a":"hdweiss@gmail.com","c":7,"n":"4e2b0f1a9c","op":"rel.add","ref":"e878760e137a63e084cddcb8e1ac14b16b3b5313","ts":1787000500,"v":1,"val":"blocked-by"}`: "240abd6342b5391915861eda15650ee7ea977684",
	`{"a":"rev@example.com","c":5,"n":"b90e12f5aa","op":"comment","ts":1787000300,"v":1,"val":"Confirmed: absent in a fresh clone."}`:                           "e42ebc4ae3c0aad54ac4a1e7a1f93a195c843db7",
}

func TestDocumentedIDs(t *testing.T) {
	for line, want := range docIDs {
		got, err := HashLine(SHA1, []byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if got != want {
			t.Errorf("HashLine(%s)\n got %s\nwant %s", line, got, want)
		}
	}
}

// Every event line printed in the specs must already be canonical. A worked
// example that is not canonical is a documentation bug that would teach a
// second implementation to fork identity.
func TestDocExamplesAreCanonical(t *testing.T) {
	fence := regexp.MustCompile("^```")
	docs, err := filepath.Glob("../../docs/*.md")
	if err != nil || len(docs) == 0 {
		t.Fatalf("no docs found: %v", err)
	}

	checked := 0
	for _, doc := range docs {
		fh, err := os.Open(doc)
		if err != nil {
			t.Fatal(err)
		}
		scan, inFence := bufio.NewScanner(fh), false
		scan.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		for scan.Scan() {
			line := scan.Text()
			if fence.MatchString(line) {
				inFence = !inFence
				continue
			}
			if !inFence || !strings.HasPrefix(line, "{") {
				continue
			}
			checked++
			if !IsCanonical([]byte(line)) {
				got, err := Canonicalize([]byte(line))
				t.Errorf("%s: example is not canonical\n got %s\nwant %s (err %v)",
					doc, line, got, err)
				continue
			}
			// It must also parse as an event and re-serialize to itself.
			ev, ok := ParseEvent(SHA1, []byte(line))
			if !ok {
				t.Errorf("%s: example does not parse as an event: %s", doc, line)
				continue
			}
			round, err := ev.canonical()
			if err != nil {
				t.Errorf("%s: %v", doc, err)
				continue
			}
			if string(round) != line {
				t.Errorf("%s: round trip differs\n got %s\nwant %s", doc, round, line)
			}
		}
		fh.Close()
	}
	if checked == 0 {
		t.Fatal("found no fenced event lines in docs/")
	}
	t.Logf("verified %d event lines in docs/", checked)
}

func TestCanonicalKeyOrderAndOmission(t *testing.T) {
	e := Event{V: 1, C: 3, TS: 1787000000, A: "a@b.c", Op: "label.remove", N: "00ff",
		Ref: "deadbeef"}
	got, err := e.canonical()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":"a@b.c","c":3,"n":"00ff","op":"label.remove","ref":"deadbeef","ts":1787000000,"v":1}`
	if string(got) != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

// encoding/json would emit <>& here and fork every id.
func TestNoHTMLEscaping(t *testing.T) {
	e := Event{V: 1, C: 1, TS: 0, A: "a", Op: "title", N: "00", Val: Str(`<a> & "b" \ c`)}
	got, err := e.canonical()
	if err != nil {
		t.Fatal(err)
	}
	want := `{"a":"a","c":1,"n":"00","op":"title","ts":0,"v":1,"val":"<a> & \"b\" \\ c"}`
	if string(got) != want {
		t.Errorf("\n got %s\nwant %s", got, want)
	}
}

func TestValueKinds(t *testing.T) {
	for _, tc := range []struct {
		val  Value
		want string
	}{
		{Value{}, `{"a":"a","c":1,"n":"00","op":"x","ts":0,"v":1}`},
		{Null(), `{"a":"a","c":1,"n":"00","op":"x","ts":0,"v":1,"val":null}`},
		{Bool(true), `{"a":"a","c":1,"n":"00","op":"x","ts":0,"v":1,"val":true}`},
		{Str("s"), `{"a":"a","c":1,"n":"00","op":"x","ts":0,"v":1,"val":"s"}`},
	} {
		e := Event{V: 1, C: 1, A: "a", Op: "x", N: "00", Val: tc.val}
		got, err := e.canonical()
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != tc.want {
			t.Errorf("\n got %s\nwant %s", got, tc.want)
		}
		back, ok := ParseEvent(SHA1, got)
		if !ok {
			t.Fatalf("parse failed for %s", got)
		}
		if back.Val != tc.val {
			t.Errorf("round trip of val: got %+v want %+v", back.Val, tc.val)
		}
	}
}

func TestParseRejections(t *testing.T) {
	for _, tc := range []struct {
		name, line string
	}{
		{"not json", `this is not json at all`},
		{"not an object", `[1,2,3]`},
		{"newer format", `{"a":"a","c":1,"n":"0","op":"title","ts":0,"v":2,"val":"x"}`},
		{"v as value (old format)", `{"a":"a","c":1,"n":"0","op":"title","ts":0,"v":"a title"}`},
		{"missing v", `{"a":"a","c":1,"n":"0","op":"title","ts":0}`},
		{"missing c", `{"a":"a","n":"0","op":"title","ts":0,"v":1}`},
		{"non-integer c", `{"a":"a","c":1.5,"n":"0","op":"title","ts":0,"v":1}`},
		{"op not a string", `{"a":"a","c":1,"n":"0","op":7,"ts":0,"v":1}`},
	} {
		if _, ok := ParseEvent(SHA1, []byte(tc.line)); ok {
			t.Errorf("%s: expected rejection, got acceptance", tc.name)
		}
	}
}

// Unknown ops carry a clock and must survive: dropping them at parse time
// would be a fold-time decision masquerading as a parse-time one.
func TestParseKeepsUnknownOps(t *testing.T) {
	line := []byte(`{"a":"a","c":9,"n":"0","op":"quantum.entangle","ts":0,"v":1,"val":"x"}`)
	e, ok := ParseEvent(SHA1, line)
	if !ok {
		t.Fatal("unknown op was rejected")
	}
	if e.Op != "quantum.entangle" || e.C != 9 {
		t.Errorf("unexpected parse: %+v", e)
	}
	if string(e.Raw) != string(line) {
		t.Error("Raw does not preserve the original bytes")
	}
}

func TestNewComputesSelfHash(t *testing.T) {
	e, err := NewEvent(SHA1, 1, 1, 1787000000, "hdweiss@gmail.com", "create", Str("issue"), "")
	if err != nil {
		t.Fatal(err)
	}
	again, err := HashLine(SHA1, e.Raw)
	if err != nil {
		t.Fatal(err)
	}
	if e.ID != again {
		t.Errorf("id %s does not match a re-hash of its own line %s", e.ID, again)
	}
	if len(e.N) != 16 {
		t.Errorf("nonce %q is not 64 bits of hex", e.N)
	}
}

func TestSHA256IDWidth(t *testing.T) {
	id, err := HashLine(SHA256, []byte(`{"a":"a","c":1,"n":"0","op":"create","ts":0,"v":1,"val":"issue"}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 64 {
		t.Errorf("sha256 id is %d hex chars, want 64", len(id))
	}
}

// Ordering is (c, id) and never ts. A clock three years fast must lose to a
// genuinely later event with a higher Lamport clock.
func TestLessIgnoresTimestamp(t *testing.T) {
	fastClock := Event{C: 2, TS: 1 << 31, ID: "aaaa"}
	correct := Event{C: 3, TS: 100, ID: "bbbb"}
	if Less(fastClock, correct) >= 0 {
		t.Error("ts won over the Lamport clock")
	}
	if Less(Event{C: 1, ID: "b"}, Event{C: 1, ID: "a"}) <= 0 {
		t.Error("tie-break did not fall through to the id")
	}
}
