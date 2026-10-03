package ghreview

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/review"
)

const fixtureDir = "../../../../testdata/github"

// fetchFixture serves the recorded response to a real client, so the test
// covers the query layer's unmarshalling as well as the mapping. Anything the
// two disagree about — a field name, a null, a union member — shows up here
// rather than against the live API.
func fetchFixture(t *testing.T) []ghapi.PullRequest {
	t.Helper()
	body, err := os.ReadFile(filepath.Join(fixtureDir, "pulls.json"))
	if err != nil {
		t.Fatal(err)
	}

	// An import reads a repository in two passes — which pull requests there
	// are, then their detail by id — so the fixture is served two ways: whole
	// for the identity walk, which reads the ids and timestamps out of it, and
	// per id for the detail queries.
	var fixture struct {
		Data struct {
			Repository struct {
				PullRequests struct {
					Nodes []json.RawMessage `json:"nodes"`
				} `json:"pullRequests"`
			} `json:"repository"`
		} `json:"data"`
	}
	if err := json.Unmarshal(body, &fixture); err != nil {
		t.Fatal(err)
	}
	byID := map[string]json.RawMessage{}
	for _, n := range fixture.Data.Repository.PullRequests.Nodes {
		var probe struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(n, &probe); err != nil {
			t.Fatal(err)
		}
		byID[probe.ID] = n
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "bearer test-token" {
			t.Errorf("Authorization = %q", got)
		}
		var req struct {
			Query     string `json:"query"`
			Variables struct {
				IDs []string `json:"ids"`
			} `json:"variables"`
		}
		json.NewDecoder(r.Body).Decode(&req)
		w.Header().Set("Content-Type", "application/json")

		if !strings.Contains(req.Query, "nodes(ids:") {
			w.Write(body)
			return
		}
		nodes := make([]json.RawMessage, 0, len(req.Variables.IDs))
		for _, id := range req.Variables.IDs {
			nodes = append(nodes, byID[id])
		}
		json.NewEncoder(w).Encode(map[string]any{"data": map[string]any{"nodes": nodes}})
	}))
	t.Cleanup(srv.Close)

	pulls, err := ghapi.New(srv.URL, "test-token").FetchPulls(ghapi.Target{
		Host: ghapi.PublicHost, Owner: "hdweiss", Name: "git-issue",
	}, ghapi.Filter{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(pulls) != 2 {
		t.Fatalf("fetched %d pull requests, want 2", len(pulls))
	}
	return pulls
}

// folded imports one fixture pull request and folds it, which is what a reader
// would see.
func folded(t *testing.T, number int, known Lookup) (Entity, entity.State) {
	t.Helper()
	for _, pr := range fetchFixture(t) {
		if pr.Number != number {
			continue
		}
		e, err := Import(entity.SHA1, pr, known)
		if err != nil {
			t.Fatalf("importing #%d: %v", number, err)
		}
		return e, entity.Fold(review.Vocabulary, e.Events())
	}
	t.Fatalf("no pull request #%d in the fixture", number)
	return Entity{}, entity.State{}
}

func TestImportFields(t *testing.T) {
	e, st := folded(t, 41, nil)

	if got := review.Status(st); got != review.StatusOpen {
		t.Errorf("status = %q, want open", got)
	}
	if got := st.Scalar("title").Display(); got != "Fix the area subtree walk" {
		t.Errorf("title = %q", got)
	}
	if got := review.Base(st); got != "main" {
		t.Errorf("base = %q, want main", got)
	}
	// The head is qualified by the fork that holds it, not by a remote name: a
	// remote name is local to one clone and this string is shared.
	if got := review.HeadRef(st); got != "contributor/fix-area-walk" {
		t.Errorf("head = %q, want contributor/fix-area-walk", got)
	}
	if got := review.Head(st); got != "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d" {
		t.Errorf("head.sha = %q", got)
	}
	if got := st.Scalar("milestone").Display(); got != "v2" {
		t.Errorf("milestone = %q", got)
	}
	if review.Draft(st) {
		t.Error("this pull request is not a draft")
	}

	labels := st.List("label")
	if len(labels) != 2 {
		t.Errorf("labels = %v", labels)
	}
	// A requested reviewer is what the assignee list means on a review, and a
	// team names itself by slug where a user names itself by login.
	who := st.List("assignee")
	if len(who) != 2 || !strings.Contains(strings.Join(who, ","), "github:maintainers") {
		t.Errorf("reviewers = %v", who)
	}

	if e.Origin != "PR_kwDOAbCdEf4AAAAB" || e.URL == "" {
		t.Errorf("origin/url = %q %q", e.Origin, e.URL)
	}
	// The node id and the URL live on the ledger, never in the blob.
	if strings.Contains(string(e.Blob()), "PR_kwDO") {
		t.Error("the node id must not reach the blob")
	}
}

// MERGED is the one status a local writer may never assert, and an import is
// what is entitled to: GitHub performed the merge and is reporting it.
func TestImportMergedAndDraft(t *testing.T) {
	_, st := folded(t, 40, nil)

	if got := review.Status(st); got != review.StatusMerged {
		t.Errorf("status = %q, want merged", got)
	}
	if !review.Terminal(review.Status(st)) {
		t.Error("merged is terminal")
	}
	if !review.Draft(st) {
		t.Error("this pull request is a draft")
	}
	// A head repository that has been deleted leaves the bare branch name,
	// which is the honest remainder rather than a guess.
	if got := review.HeadRef(st); got != "retire-walker" {
		t.Errorf("head = %q, want the bare branch", got)
	}
}

// A review thread becomes a root comment carrying the anchor, with the rest as
// replies under it.
func TestImportReviewThread(t *testing.T) {
	e, st := folded(t, 41, nil)

	threads := review.Threads(st, nil)
	var anchored []review.Thread
	for _, tr := range threads {
		if tr.Anchored {
			anchored = append(anchored, tr)
		}
	}
	if len(anchored) != 1 {
		t.Fatalf("got %d anchored threads, want 1 (of %d)", len(anchored), len(threads))
	}

	tr := anchored[0]
	if tr.Anchor.Path != "internal/issue/area.go" || tr.Anchor.First != 42 {
		t.Errorf("anchor = %+v, want area.go:42", tr.Anchor)
	}
	// originalCommit, not commit: GitHub re-anchors a thread onto later
	// revisions as the branch moves, and importing that would rewrite where
	// somebody was looking.
	if tr.Anchor.Revision != "9f2c1ab74e0d3b5f8c6e2a1d40b7e93c5a8f1d26" {
		t.Errorf("revision = %q, want the original commit", tr.Anchor.Revision)
	}
	if !tr.Resolved {
		t.Error("the thread is resolved upstream")
	}
	if tr.Replies != 1 {
		t.Errorf("replies = %d, want 1", tr.Replies)
	}

	// The thread's root is mapped for the ledger, which is what a push needs in
	// order to resolve one.
	if len(e.Threads) != 1 || e.Threads[0].Upstream != "PRRT_kwDO1" {
		t.Errorf("threads = %+v", e.Threads)
	}
}

// GitHub says outdated; this bridge does not import that. Whether an anchor
// still describes the head is derived from the object store, because the
// comparison GitHub made is not the one docs/reviews.md defines.
func TestOutdatedIsDerivedNotImported(t *testing.T) {
	_, st := folded(t, 41, nil)
	for _, tr := range review.Threads(st, nil) {
		if tr.Anchored && tr.Currency != review.Unknown {
			t.Errorf("currency = %v with no repository to ask; want Unknown", tr.Currency)
		}
	}
}

// Each review is one person's position, carrying the revision it was cast
// against — which is what makes staleness derivable without consulting a clock.
func TestImportVerdicts(t *testing.T) {
	_, st := folded(t, 41, nil)

	v, ok := review.VerdictBy(st, "github:reviewer")
	if !ok {
		t.Fatal("the reviewer's verdict is missing")
	}
	// The later approval supersedes the earlier request for changes; both stay
	// in the blob.
	if v.Value != review.VerdictApprove {
		t.Errorf("value = %q, want the later approve", v.Value)
	}
	if v.Stale {
		t.Error("the approval is against the current head")
	}
	if len(st.Members(review.VerdictField)) != 2 {
		t.Errorf("got %d members, want both kept", len(st.Members(review.VerdictField)))
	}
	if n := len(review.Approvals(st)); n != 1 {
		t.Errorf("Approvals = %d, want 1", n)
	}

	// A PENDING review is visible only to its author, and importing one would
	// publish a draft nobody sent.
	if _, ok := review.VerdictBy(st, "github:someone"); ok {
		t.Error("a pending review must not be imported")
	}
}

// A dismissed review is imported as the verdict it was and then retracted, so
// the record that somebody read this survives while their position does not.
func TestImportDismissedVerdict(t *testing.T) {
	_, st := folded(t, 40, nil)

	if _, ok := review.VerdictBy(st, "github:reviewer"); ok {
		t.Error("a dismissed verdict does not stand")
	}
	if len(st.Members(review.VerdictField)) != 0 {
		t.Error("the dismissal retracts the add")
	}
	// The body it carried is still a comment: the words were said.
	found := false
	for _, c := range st.Thread {
		if strings.Contains(c.Body.Display(), "Superseded by the rewrite") {
			found = true
		}
	}
	if !found {
		t.Error("the dismissed review's body should survive as a comment")
	}
}

// Checks are a fact about a commit, so they are handed back for the caller to
// write on their own ref and never reach the review's blob.
func TestChecksAreSeparateFromTheBlob(t *testing.T) {
	e, st := folded(t, 41, nil)

	if len(e.Checks) != 2 {
		t.Fatalf("got %d checks, want 2", len(e.Checks))
	}
	byName := map[string]string{}
	for _, c := range e.Checks {
		byName[c.Run.Name] = c.Run.Conclusion
		if c.Commit != "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d" {
			t.Errorf("check %q is keyed by %q", c.Run.Name, c.Commit)
		}
	}
	// A CheckRun is qualified by its app, because two apps can register checks
	// under one name; a StatusContext names itself.
	if got := byName["GitHub-Actions/build"]; got != review.ConclusionPass {
		t.Errorf("build = %q, want pass (have %v)", got, byName)
	}
	if got := byName["sonarqube/quality-gate"]; got != review.ConclusionFail {
		t.Errorf("quality gate = %q, want fail", got)
	}

	if len(review.ChecksOf(st)) != 0 {
		t.Error("no check may reach the review's own blob")
	}
	if strings.Contains(string(e.Blob()), "check.add") {
		t.Error("no check event may reach the review's own blob")
	}
}

// fakeLookup answers what the ledger would.
type fakeLookup struct {
	entities map[string]string
	claimed  map[string]bool
	verdicts map[string]string
}

func (f fakeLookup) Entity(upstream string) (string, bool) {
	id, ok := f.entities[upstream]
	return id, ok
}
func (f fakeLookup) ClaimedComment(upstream string) bool { return f.claimed[upstream] }
func (f fakeLookup) ClaimedVerdict(upstream string) (string, bool) {
	id, ok := f.verdicts[upstream]
	return id, ok
}

// `closes` is written only when this clone holds the issue it names, and
// counted when it does not — which is what makes importing a batch twice worth
// the second pass and no more.
func TestClosesNeedsTheTargetLocally(t *testing.T) {
	e, st := folded(t, 41, nil)
	if len(review.Closes(st)) != 0 {
		t.Error("an unresolvable target must be left unwritten")
	}
	if e.Unresolved != 1 {
		t.Errorf("Unresolved = %d, want 1", e.Unresolved)
	}

	known := fakeLookup{entities: map[string]string{
		"I_kwDOAbCdEf1": "4b0755a3e7697bfdf17e42e9f4b307c161ea2a40",
	}}
	e, st = folded(t, 41, known)
	closes := review.Closes(st)
	if len(closes) != 1 || closes[0].Target != "4b0755a3e7697bfdf17e42e9f4b307c161ea2a40" {
		t.Fatalf("closes = %+v", closes)
	}
	if e.Unresolved != 0 {
		t.Errorf("Unresolved = %d, want 0", e.Unresolved)
	}
	// The link is recorded and the review's own status is untouched. Nothing
	// here closes the issue, now or on merge.
	if review.Status(st) != review.StatusOpen {
		t.Error("a closes link must not touch anything else")
	}
}

// A comment this repository pushed is already here, and the ledger says so.
// Importing it again would show one comment twice.
func TestClaimedCommentsAreSkipped(t *testing.T) {
	known := fakeLookup{claimed: map[string]bool{"IC_kwDOxYzAbC1": true}}
	_, st := folded(t, 41, known)

	for _, c := range st.Thread {
		if strings.Contains(c.Body.Display(), "Thanks for picking this up") {
			t.Error("a claimed comment must not be imported again")
		}
	}
}

// Re-import is a no-op. Every nonce derives from stable upstream identity, so
// running the import twice produces byte-identical events — which is the whole
// reason a nonce is derived rather than generated.
func TestImportIsIdempotent(t *testing.T) {
	for _, pr := range fetchFixture(t) {
		first, err := Import(entity.SHA1, pr, nil)
		if err != nil {
			t.Fatal(err)
		}
		second, err := Import(entity.SHA1, pr, nil)
		if err != nil {
			t.Fatal(err)
		}
		if first.ID != second.ID {
			t.Errorf("#%d: id %s then %s", pr.Number, first.ID, second.ID)
		}
		if string(first.Blob()) != string(second.Blob()) {
			t.Errorf("#%d: a second import produced different bytes", pr.Number)
		}
	}
}

// The create event's id is the entity's, and it is derived from the node id
// alone — so every implementation that imports this pull request agrees on
// what it is called.
func TestEntityIDIsTheCreateEventHash(t *testing.T) {
	e, st := folded(t, 41, nil)
	if st.Create == nil {
		t.Fatal("no create event")
	}
	if st.Create.ID != e.ID {
		t.Errorf("create id %s, entity id %s", st.Create.ID, e.ID)
	}
	if got := st.Create.Val.Display(); got != review.Type {
		t.Errorf("create val = %q, want %q", got, review.Type)
	}
	if st.Create.N != Nonce("PR_kwDOAbCdEf4AAAAB") {
		t.Errorf("the create nonce must derive from the node id")
	}
}

func TestStatusAndVerdictMapping(t *testing.T) {
	for in, want := range map[string]string{
		"OPEN": review.StatusOpen, "CLOSED": review.StatusClosed, "MERGED": review.StatusMerged,
		"something-else": review.StatusOpen,
	} {
		if got := Status(in); got != want {
			t.Errorf("Status(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"APPROVED": review.VerdictApprove, "CHANGES_REQUESTED": review.VerdictRequestChanges,
		"COMMENTED": review.VerdictComment, "PENDING": "", "nonsense": "",
	} {
		if got := Verdict(in); got != want {
			t.Errorf("Verdict(%q) = %q, want %q", in, got, want)
		}
	}
	for in, want := range map[string]string{
		"SUCCESS": review.ConclusionPass, "FAILURE": review.ConclusionFail,
		"IN_PROGRESS": review.ConclusionPending, "": review.ConclusionPending,
		"CANCELLED": review.ConclusionCancelled, "WEIRD": "weird",
	} {
		if got := Conclusion(in); got != want {
			t.Errorf("Conclusion(%q) = %q, want %q", in, got, want)
		}
	}
}

// Actions are the units a commit is written from, oldest first, so the log
// reads as a history rather than as a replay from birth.
func TestActionsAreOrderedAndAttributed(t *testing.T) {
	e, _ := folded(t, 41, nil)
	if len(e.Actions) < 4 {
		t.Fatalf("got %d actions, want the create plus each upstream act", len(e.Actions))
	}
	for i := 1; i < len(e.Actions); i++ {
		if e.Actions[i].Author.When.Before(e.Actions[i-1].Author.When) {
			t.Errorf("action %d is older than the one before it", i)
		}
	}
	if !strings.HasPrefix(e.Actions[0].Message, "Create review ") {
		t.Errorf("first action says %q", strings.SplitN(e.Actions[0].Message, "\n", 2)[0])
	}
	// The trailer joins a commit back to the entity, and names the review type
	// rather than the issue one.
	if !strings.Contains(e.Actions[0].Message, "Review: "+e.ID) {
		t.Error("the commit should carry a Review: trailer")
	}
	if !strings.Contains(e.Actions[0].Message, "Origin: https://github.com/") {
		t.Error("the commit should carry the upstream locator")
	}
}
