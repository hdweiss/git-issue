package adoapi

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func target(t *testing.T, base string) Target {
	t.Helper()
	tt, err := ParseTarget(base + "/DefaultCollection/MyProj/_git/repo")
	if err != nil {
		t.Fatal(err)
	}
	return tt
}

// The area is always a subtree. There is no exact-node form, so UNDER is the
// only operator an area ever produces — see docs/bridge-ado.md.
func TestWIQLScopesByAreaSubtree(t *testing.T) {
	tt := Target{Base: "https://dev.azure.com", Collection: "contoso", Project: "MyProj", Area: "Web/Auth"}
	q := wiql(tt, Filter{})

	if want := `[System.AreaPath] UNDER 'MyProj\Web\Auth'`; !strings.Contains(q, want) {
		t.Errorf("query missing %s:\n%s", want, q)
	}
	if strings.Contains(q, "[System.AreaPath] =") {
		t.Errorf("query scopes by exact area, want UNDER:\n%s", q)
	}
	// The whole project is still an UNDER, of the root.
	tt.Area = ""
	if want := `[System.AreaPath] UNDER 'MyProj'`; !strings.Contains(wiql(tt, Filter{}), want) {
		t.Errorf("root query missing %s", want)
	}
}

// Types and terminal states are asked for as Azure DevOps' own categories
// rather than as lists of names, so a customised process stays correct.
func TestWIQLUsesCategories(t *testing.T) {
	tt := Target{Base: "https://dev.azure.com", Collection: "contoso", Project: "MyProj"}

	q := wiql(tt, Filter{Open: true})
	for _, want := range []string{
		"[System.WorkItemType] NOT IN GROUP 'Microsoft.HiddenCategory'",
		"[System.State] NOT IN GROUP 'Completed'",
		"[System.State] NOT IN GROUP 'Removed'",
		"ORDER BY [System.ChangedDate] ASC",
	} {
		if !strings.Contains(q, want) {
			t.Errorf("query missing %s:\n%s", want, q)
		}
	}

	// An explicit --type replaces the category, since naming a hidden type is
	// how somebody asks for one.
	q = wiql(tt, Filter{Types: []string{"Bug", "Test Case"}})
	if !strings.Contains(q, `[System.WorkItemType] IN ('Bug', 'Test Case')`) {
		t.Errorf("query does not honour explicit types:\n%s", q)
	}
	if strings.Contains(q, "HiddenCategory") {
		t.Errorf("explicit types should not also exclude the hidden category:\n%s", q)
	}

	// Not asking for open work items must not filter states at all.
	if q := wiql(tt, Filter{}); strings.Contains(q, "System.State") {
		t.Errorf("unfiltered query constrains state:\n%s", q)
	}
}

func TestWIQLQuotesLiterals(t *testing.T) {
	tt := Target{Base: "https://dev.azure.com", Collection: "c", Project: "Bob's Proj"}
	if want := `'Bob''s Proj'`; !strings.Contains(wiql(tt, Filter{}), want) {
		t.Errorf("apostrophe not doubled, want %s in:\n%s", want, wiql(tt, Filter{}))
	}
}

func TestWIQLSince(t *testing.T) {
	tt := Target{Base: "https://dev.azure.com", Collection: "c", Project: "P"}
	since := time.Date(2026, 3, 4, 5, 6, 7, 0, time.UTC)
	if want := `[System.ChangedDate] >= '2026-03-04T05:06:07Z'`; !strings.Contains(wiql(tt, Filter{Since: since}), want) {
		t.Errorf("query missing %s", want)
	}
}

// A recorded round trip through a real Client: the HTTP layer, the paging and
// the decoding are all covered, so a server quirk cannot hide behind a
// hand-built struct.
func TestFetchDecodes(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case strings.HasSuffix(r.URL.Path, "/wit/wiql"):
			w.Write([]byte(`{"workItems":[{"id":1234}]}`))
		case strings.HasSuffix(r.URL.Path, "/wit/workitemsbatch"):
			w.Write([]byte(`{"value":[{
				"id":1234,"rev":3,
				"fields":{
					"System.WorkItemType":"Bug",
					"System.Title":"Login fails",
					"Microsoft.VSTS.TCM.ReproSteps":"Press the button",
					"System.Description":"ignored for a bug",
					"System.State":"Resolved",
					"System.AreaPath":"MyProj\\Web\\Auth",
					"System.IterationPath":"MyProj\\Release 1\\Sprint 3",
					"System.Tags":"ux; regression ",
					"System.AssignedTo":{"displayName":"Jane Roe","uniqueName":"JANE@corp.com","id":"guid-1"},
					"System.CreatedBy":"Ann Poe <ann@corp.com>",
					"System.CreatedDate":"2026-01-02T03:04:05Z",
					"System.ChangedDate":"2026-02-02T03:04:05Z"
				},
				"relations":[{"rel":"System.LinkTypes.Hierarchy-Reverse","url":"https://x/_apis/wit/workItems/99"}]
			}]}`))
		case strings.Contains(r.URL.Path, "/updates"):
			if r.URL.Query().Get("$skip") != "0" {
				w.Write([]byte(`{"value":[]}`))
				return
			}
			w.Write([]byte(`{"value":[
				{"rev":1,"revisedBy":{"uniqueName":"ann@corp.com"},"revisedDate":"2026-01-02T03:04:05Z",
				 "fields":{"System.Title":{"newValue":"Login broke"}}},
				{"rev":3,"revisedBy":{"uniqueName":"jane@corp.com"},"revisedDate":"9999-01-01T00:00:00Z",
				 "fields":{"System.Title":{"oldValue":"Login broke","newValue":"Login fails"},
				           "System.ChangedDate":{"newValue":"2026-02-02T03:04:05Z"}}}
			]}`))
		case strings.Contains(r.URL.Path, "/comments"):
			w.Write([]byte(`{"comments":[{"id":7,"version":1,"text":"seen it",
				"createdBy":{"uniqueName":"ann@corp.com"},"createdDate":"2026-01-03T00:00:00Z"}]}`))
		default:
			t.Errorf("unexpected request %s", r.URL.Path)
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	items, err := New("pat").Fetch(target(t, srv.URL), Filter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d work items, want 1", len(items))
	}
	w := items[0]

	// A Bug's body is ReproSteps, not Description.
	if w.Description != "Press the button" {
		t.Errorf("Description = %q, want the ReproSteps", w.Description)
	}
	if w.State != "Resolved" {
		t.Errorf("State = %q, want the raw Azure DevOps state", w.State)
	}
	if got, want := strings.Join(w.Tags, "|"), "ux|regression"; got != want {
		t.Errorf("Tags = %q, want %q", got, want)
	}
	// Iteration drops the project root; area is reported but never imported.
	if w.Iteration != "Release 1/Sprint 3" {
		t.Errorf("Iteration = %q, want the path below the project", w.Iteration)
	}
	if w.AreaPath != `MyProj\Web\Auth` {
		t.Errorf("AreaPath = %q", w.AreaPath)
	}
	if len(w.Relations) != 1 || w.Relations[0].Rel != RelParent || !strings.HasSuffix(w.Relations[0].URL, "/99") {
		t.Errorf("Relations = %+v, want the parent link to 99", w.Relations)
	}
	// The string spelling of an identity decodes as well as the object one.
	if w.CreatedBy.UniqueName != "ann@corp.com" || w.CreatedBy.DisplayName != "Ann Poe" {
		t.Errorf("CreatedBy = %+v", w.CreatedBy)
	}
	if w.AssignedTo.UniqueName != "JANE@corp.com" {
		t.Errorf("AssignedTo = %+v", w.AssignedTo)
	}
	if len(w.Comments) != 1 || w.Comments[0].ID != 7 {
		t.Errorf("Comments = %+v", w.Comments)
	}

	// The newest revision's revisedDate is Azure DevOps' 9999 sentinel, and
	// taking it literally would put that event eight thousand years ahead of
	// every other one in the entity.
	if len(w.Updates) != 2 {
		t.Fatalf("got %d updates, want 2", len(w.Updates))
	}
	if got, want := w.Updates[1].At, time.Date(2026, 2, 2, 3, 4, 5, 0, time.UTC); !got.Equal(want) {
		t.Errorf("newest revision At = %s, want the ChangedDate %s", got, want)
	}
}

// Deleted comments are asked for: a deletion is a fact the thread carries, not
// an absence to be inferred.
func TestCommentsIncludeDeleted(t *testing.T) {
	var asked string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		asked = r.URL.RawQuery
		w.Write([]byte(`{"comments":[]}`))
	}))
	defer srv.Close()

	if _, err := New("pat").comments(target(t, srv.URL), 1); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(asked, "includeDeleted=true") {
		t.Errorf("query = %q, want includeDeleted", asked)
	}
}

// WIQL's result cap is the one API failure a caller can act on, so it has to
// be recognisable without matching on prose.
func TestTooManyResultsIsRecognised(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"message":"VS402337: The number of work items returned exceeds the size limit.","typeKey":"VS402337"}`))
	}))
	defer srv.Close()

	_, err := New("pat").Fetch(target(t, srv.URL), Filter{}, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want an *Error", err)
	}
	if !apiErr.TooManyResults() {
		t.Errorf("TooManyResults() = false for %v", err)
	}
}

// An unauthenticated request is answered with a sign-in page and a 203, not a
// 401. Treating that as success parses HTML as JSON and reports nonsense.
func TestSignInPageIsAnAuthFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNonAuthoritativeInfo)
		w.Write([]byte("<html>sign in</html>"))
	}))
	defer srv.Close()

	_, err := New("").Fetch(target(t, srv.URL), Filter{}, nil)
	var apiErr *Error
	if !errors.As(err, &apiErr) || !apiErr.Unauthorized() {
		t.Fatalf("err = %v, want an unauthorized *Error", err)
	}
}

func TestAreasBuildRelativePaths(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(`{"name":"MyProj","children":[
			{"name":"Web","children":[{"name":"Auth"},{"name":"Api"}]},
			{"name":"Mobile"}
		]}`))
	}))
	defer srv.Close()

	root, err := New("pat").Areas(target(t, srv.URL))
	if err != nil {
		t.Fatal(err)
	}

	var paths []string
	root.Walk(func(node Area, depth int) { paths = append(paths, node.Path) })
	if got, want := strings.Join(paths, ","), ",Web,Web/Auth,Web/Api,Mobile"; got != want {
		t.Errorf("paths = %q, want %q", got, want)
	}

	if node, ok := root.Find("web/auth"); !ok || node.Name != "Auth" {
		t.Errorf("Find(web/auth) = %+v, %v; want the Auth node", node, ok)
	}
	if _, ok := root.Find("Nope"); ok {
		t.Error("Find found an area that is not in the tree")
	}
}
