package ghapi

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// schemaServer answers issue pages, recording every query it was sent. It
// rejects any query naming a feature it does not have, the way a GitHub
// Enterprise install running an older schema does.
func schemaServer(t *testing.T, knows schema, pages int) (*httptest.Server, *[]string) {
	t.Helper()
	var queries []string
	served := 0

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Query string `json:"query"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Error(err)
			return
		}
		queries = append(queries, req.Query)

		if !knows.issueTypes && strings.Contains(req.Query, "issueType") {
			io.WriteString(w, `{"errors":[{"message":"Field 'issueType' doesn't exist on type 'Issue'"}]}`)
			return
		}
		if !knows.relations && strings.Contains(req.Query, ParentAdded) {
			io.WriteString(w, `{"errors":[{"message":"Unknown type \"`+ParentAdded+`\"."}]}`)
			return
		}
		served++
		if served < pages {
			io.WriteString(w, `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":true,"endCursor":"c"},"nodes":[]}}}}`)
			return
		}
		io.WriteString(w, `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}`)
	}))
	t.Cleanup(srv.Close)
	return srv, &queries
}

func fetchAll(t *testing.T, srv *httptest.Server) {
	t.Helper()
	if _, err := New(srv.URL, "t").Fetch(Target{Host: PublicHost, Owner: "o", Name: "n"}, Filter{}, nil); err != nil {
		t.Fatal(err)
	}
}

func naming(queries []string, token string) int {
	n := 0
	for _, q := range queries {
		if strings.Contains(q, token) {
			n++
		}
	}
	return n
}

// modern is a server with everything; ancient is one with none of it.
var (
	modern  = schema{issueTypes: true, relations: true}
	ancient = schema{}
)

// A server that does not know issue types is probed once and then believed. A
// schema does not change underneath a run, so re-probing would spend a
// guaranteed failed request on every page — doubling an import's request count
// to keep relearning the first page's answer.
func TestDegradedSchemaProbedOnce(t *testing.T) {
	srv, queries := schemaServer(t, schema{relations: true}, 3)
	fetchAll(t, srv)

	// One rejected probe, then three degraded pages.
	if got := len(*queries); got != 4 {
		t.Errorf("%d requests for a three-page import, want 4:\n%s", got, strings.Join(*queries, "\n---\n"))
	}
	if got := naming(*queries, "issueType"); got != 1 {
		t.Errorf("%d queries named issueType, want 1 — the verdict is not being cached", got)
	}
}

// The two features degrade independently: a server that has relationships but
// not issue types keeps the relationships, and the other way round. Giving up
// both because one was rejected would lose data nobody had to lose.
func TestFeaturesDegradeIndependently(t *testing.T) {
	for _, tc := range []struct {
		name  string
		knows schema
	}{
		{"no issue types", schema{relations: true}},
		{"no relationships", schema{issueTypes: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv, queries := schemaServer(t, tc.knows, 1)
			fetchAll(t, srv)

			// The last query is the one that was answered, and it must still
			// name whatever the server does have.
			final := (*queries)[len(*queries)-1]
			if got := strings.Contains(final, "issueType"); got != tc.knows.issueTypes {
				t.Errorf("final query names issueType = %v, want %v:\n%s", got, tc.knows.issueTypes, final)
			}
			if got := strings.Contains(final, ParentAdded); got != tc.knows.relations {
				t.Errorf("final query names %s = %v, want %v:\n%s", ParentAdded, got, tc.knows.relations, final)
			}
		})
	}
}

// A server with neither gives both up, one probe each, and then imports.
func TestOldestSchemaStillImports(t *testing.T) {
	srv, queries := schemaServer(t, ancient, 3)
	fetchAll(t, srv)

	// Two rejected probes, then three degraded pages.
	if got := len(*queries); got != 5 {
		t.Errorf("%d requests for a three-page import, want 5:\n%s", got, strings.Join(*queries, "\n---\n"))
	}
	if got := naming(*queries, ParentAdded); got != 2 {
		t.Errorf("%d queries named %s, want 2 — the verdict is not being cached", got, ParentAdded)
	}
}

// The cache is per client, so it cannot leak a verdict earned against one host
// onto the next one.
func TestDegradedSchemaNotCachedAcrossClients(t *testing.T) {
	srv, queries := schemaServer(t, ancient, 1)
	fetchAll(t, srv)
	fetchAll(t, srv)

	if got := naming(*queries, "issueType"); got != 2 {
		t.Errorf("%d probes across two clients, want 2", got)
	}
}

// And a server that does know the fields is never asked the degraded question.
func TestHealthySchemaNeverDegrades(t *testing.T) {
	srv, queries := schemaServer(t, modern, 3)
	fetchAll(t, srv)

	if got := len(*queries); got != 3 {
		t.Errorf("%d requests for a three-page import, want 3", got)
	}
	for _, token := range []string{"issueType", ParentAdded, "duplicateOf"} {
		if got := naming(*queries, token); got != 3 {
			t.Errorf("%d of 3 queries named %s, want all of them", got, token)
		}
	}
}

// GraphQL requires first/last to be within 1-100, and a query that asks for
// more fails outright. Pinning it here means a future page-size bump fails in
// the test run rather than against everyone's live import.
func TestPageSizesWithinGraphQLLimit(t *testing.T) {
	for _, p := range []struct {
		name string
		size int
	}{{"issuePage", issuePage}, {"nestedPage", nestedPage}} {
		if p.size < 1 || p.size > 100 {
			t.Errorf("%s = %d, outside GraphQL's 1-100 for first/last", p.name, p.size)
		}
	}
}
