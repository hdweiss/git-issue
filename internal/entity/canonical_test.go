package entity

import "testing"

func TestCanonicalize(t *testing.T) {
	for _, tc := range []struct{ name, in, want string }{
		{"key order", `{"b":1,"a":2}`, `{"a":2,"b":1}`},
		{"whitespace", "{ \"a\" : 1 , \"b\" : [ 1 , 2 ] }", `{"a":1,"b":[1,2]}`},
		{"nested", `{"z":{"b":1,"a":2},"a":[{"d":1,"c":2}]}`, `{"a":[{"c":2,"d":1}],"z":{"a":2,"b":1}}`},

		// RFC 8785 escapes minimally. encoding/json would emit <&
		// here by default, which would give the same event two different ids
		// depending on which library wrote it.
		{"no html escaping", `{"a":"<b> & 'c' / d"}`, `{"a":"<b> & 'c' / d"}`},
		{"quotes and backslash", `{"a":"say \"hi\" \\ bye"}`, `{"a":"say \"hi\" \\ bye"}`},
		{"short control escapes", `{"a":"\b\t\n\f\r"}`, `{"a":"\b\t\n\f\r"}`},
		{"long control escapes", "{\"a\":\"\\u0001\"}", `{"a":"\u0001"}`},
		{"escapes are lowercase hex", "{\"a\":\"\\u001F\"}", `{"a":"\u001f"}`},

		// Non-ASCII stays literal UTF-8 rather than becoming \u escapes.
		{"unicode literal", `{"a":"ünïcode ✓"}`, `{"a":"ünïcode ✓"}`},
		{"unicode escapes become literal", "{\"a\":\"\\u00fcn\\u00efcode \\u2014 \\u2713\"}", `{"a":"ünïcode — ✓"}`},
		{"astral plane", `{"a":"😀"}`, `{"a":"😀"}`},

		{"literals", `{"a":true,"b":false,"c":null}`, `{"a":true,"b":false,"c":null}`},
		{"integer forms", `{"a":1.0,"b":1e2,"c":-0}`, `{"a":1,"b":100,"c":0}`},
		{"array order is preserved", `[3,1,2]`, `[3,1,2]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := Canonicalize([]byte(tc.in))
			if err != nil {
				t.Fatal(err)
			}
			if string(got) != tc.want {
				t.Errorf("\n got %s\nwant %s", got, tc.want)
			}
			// Canonical output must be a fixed point.
			if !IsCanonical(got) {
				t.Errorf("output %s is not itself canonical", got)
			}
		})
	}
}

// Keys sort by UTF-16 code unit, which differs from UTF-8 byte order above the
// BMP: U+10000 encodes as the surrogate D800 DC00, below U+E000.
func TestKeySortIsUTF16(t *testing.T) {
	got, err := Canonicalize([]byte(`{"𐀀":1,"":2}`))
	if err != nil {
		t.Fatal(err)
	}
	want := "{\"\U00010000\":1,\"\":2}"
	if string(got) != want {
		t.Errorf("\n got %q\nwant %q", got, want)
	}
}

func TestRejections(t *testing.T) {
	for _, tc := range []struct{ name, in string }{
		{"not json", `nope`},
		{"trailing data", `{"a":1} {"b":2}`},
		{"truncated", `{"a":`},
		// Deliberate: the event schema holds only integers, and approximating
		// ECMAScript number formatting would be an unverified guess.
		{"non-integer number", `{"a":1.5}`},
		{"out of safe range", `{"a":1e300}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := Canonicalize([]byte(tc.in)); err == nil {
				t.Errorf("expected an error for %s", tc.in)
			}
		})
	}
}

func TestIsCanonical(t *testing.T) {
	if IsCanonical([]byte(`{"b":1,"a":2}`)) {
		t.Error("unsorted keys reported as canonical")
	}
	if !IsCanonical([]byte(`{"a":2,"b":1}`)) {
		t.Error("canonical input reported as non-canonical")
	}
}
