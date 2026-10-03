package ghapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// capture records the GraphQL variables of the first request and answers with
// an empty page, so a Filter can be asserted on what it actually sends rather
// than on what it was set to.
func capture(t *testing.T, f Filter) map[string]any {
	t.Helper()
	var vars map[string]any

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Fatal(err)
		}
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		if err := json.Unmarshal(body, &req); err != nil {
			t.Fatal(err)
		}
		if vars == nil {
			vars = req.Variables
		}
		io.WriteString(w, `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}`)
	}))
	defer srv.Close()

	if _, err := New(srv.URL, "t").Fetch(Target{Host: PublicHost, Owner: "o", Name: "n"}, f, nil); err != nil {
		t.Fatal(err)
	}
	return vars
}

// The default is open issues only; asking for everything means sending no
// states argument at all.
//
// Null and empty are not interchangeable here: a null states argument is "no
// restriction", while an empty list would match nothing and import zero issues.
func TestFilterStates(t *testing.T) {
	open := capture(t, Filter{States: []string{StateOpen}})
	states, ok := open["states"].([]any)
	if !ok || len(states) != 1 || states[0] != StateOpen {
		t.Errorf("states = %#v, want [OPEN]", open["states"])
	}

	all := capture(t, Filter{})
	if all["states"] != nil {
		t.Errorf("states = %#v with no filter, want null", all["states"])
	}
}

// since is sent as RFC 3339 in UTC, and is absent rather than zero when unset.
func TestFilterSince(t *testing.T) {
	at := time.Date(2026, 3, 1, 9, 0, 0, 0, time.UTC)
	got := capture(t, Filter{Since: at})
	if got["since"] != "2026-03-01T09:00:00Z" {
		t.Errorf("since = %#v", got["since"])
	}
	if none := capture(t, Filter{}); none["since"] != nil {
		t.Errorf("since = %#v with no filter, want null", none["since"])
	}
}

// The timeline selection is requested by enum name, not by type name. Sending
// type names fails the whole query against the live API, so this is pinned.
func TestFilterRequestsTimelineEnumNames(t *testing.T) {
	got := capture(t, Filter{})
	kinds, ok := got["kinds"].([]any)
	if !ok || len(kinds) == 0 {
		t.Fatalf("kinds = %#v", got["kinds"])
	}
	if kinds[0] != "LABELED_EVENT" {
		t.Errorf("kinds[0] = %v, want LABELED_EVENT", kinds[0])
	}
	if enumName("RenamedTitleEvent") != "RENAMED_TITLE_EVENT" {
		t.Errorf("enumName = %q", enumName("RenamedTitleEvent"))
	}
}
