package review

import (
	"testing"

	"github.com/hdweiss/git-issue/internal/entity"
)

// events folds a hand-written blob the way a store would, so a test can state
// the bytes it means rather than the calls that produce them.
func fold(t *testing.T, lines ...string) entity.State {
	t.Helper()
	var events []entity.Event
	for _, line := range lines {
		ev, ok := entity.ParseEvent(entity.SHA1, []byte(line))
		if !ok {
			t.Fatalf("unparseable event: %s", line)
		}
		events = append(events, ev)
	}
	return entity.Fold(Vocabulary, events)
}

// id is the event id of a line, which is how one event addresses another.
func id(t *testing.T, line string) string {
	t.Helper()
	h, err := entity.HashLine(entity.SHA1, []byte(line))
	if err != nil {
		t.Fatal(err)
	}
	return h
}

const (
	sha1Rev = "9f2c1ab74e0d3b5f8c6e2a1d40b7e93c5a8f1d26"
	sha2Rev = "3ac8e05f19b7d24c6e0a8f3b51d97c4e2b60af8d"
)

func TestStatusDefaultsToOpen(t *testing.T) {
	st := fold(t, `{"a":"a@b","c":1,"n":"01","op":"create","ts":1,"v":1,"val":"review"}`)
	if got := Status(st); got != StatusOpen {
		t.Errorf("Status = %q, want %q", got, StatusOpen)
	}
	if Terminal(StatusOpen) {
		t.Error("open must not be terminal")
	}
	for _, s := range []string{StatusMerged, StatusClosed, "Abandoned", "Completed"} {
		if !Terminal(s) {
			t.Errorf("%q should be terminal", s)
		}
	}
	// An unrecognised status is not terminal: leaving something listed is the
	// failure that can be noticed.
	if Terminal("Needs Baking") {
		t.Error("an unknown status must not be terminal")
	}
}

// A verdict is an ordinary OR-Set with a latest-per-author reader rule, so an
// author who reconsiders has one current position and both events survive.
func TestVerdictsResolvePerAuthor(t *testing.T) {
	first := `{"a":"rev@x","c":2,"n":"02","op":"verdict.add","ref":"` + sha1Rev + `","ts":2,"v":1,"val":"request-changes"}`
	second := `{"a":"rev@x","c":3,"n":"03","op":"verdict.add","ref":"` + sha1Rev + `","ts":3,"v":1,"val":"approve"}`
	other := `{"a":"b@x","c":4,"n":"04","op":"verdict.add","ref":"` + sha1Rev + `","ts":4,"v":1,"val":"approve"}`

	st := fold(t,
		`{"a":"a@b","c":1,"n":"01","op":"create","ts":1,"v":1,"val":"review"}`,
		`{"a":"a@b","c":1,"n":"05","op":"head.sha","ts":1,"v":1,"val":"`+sha1Rev+`"}`,
		first, second, other,
	)

	got := VerdictsOf(st)
	if len(got) != 2 {
		t.Fatalf("got %d verdicts, want 2 (one per author)", len(got))
	}
	if v, ok := VerdictBy(st, "rev@x"); !ok || v.Value != VerdictApprove {
		t.Errorf("rev@x resolves to %+v, want the later approve", v)
	}
	if n := len(Approvals(st)); n != 2 {
		t.Errorf("Approvals = %d, want 2", n)
	}
	// Both events stay in the blob: the record that they once asked for
	// changes is what a review most needs to keep.
	if len(st.Members(VerdictField)) != 3 {
		t.Errorf("got %d members, want all 3 kept", len(st.Members(VerdictField)))
	}
}

// A verdict cast against an older revision is stale, and is reported rather
// than dropped or re-pointed.
func TestVerdictGoesStaleWhenHeadMoves(t *testing.T) {
	st := fold(t,
		`{"a":"a@b","c":1,"n":"01","op":"create","ts":1,"v":1,"val":"review"}`,
		`{"a":"a@b","c":2,"n":"02","op":"head.sha","ts":2,"v":1,"val":"`+sha1Rev+`"}`,
		`{"a":"rev@x","c":3,"n":"03","op":"verdict.add","ref":"`+sha1Rev+`","ts":3,"v":1,"val":"approve"}`,
		`{"a":"a@b","c":4,"n":"04","op":"head.sha","ts":4,"v":1,"val":"`+sha2Rev+`"}`,
	)

	v, ok := VerdictBy(st, "rev@x")
	if !ok {
		t.Fatal("the verdict must still be there")
	}
	if !v.Stale {
		t.Error("a verdict against an older revision is stale")
	}
	if v.Revision != sha1Rev {
		t.Errorf("Revision = %q; an anchor is never re-pointed", v.Revision)
	}
	if n := len(Approvals(st)); n != 0 {
		t.Errorf("Approvals = %d, want 0: a stale approval does not count", n)
	}
}

// Dismissal names the add it retracts, and the author falls back to their
// previous position.
func TestDismissFallsBackToThePreviousVerdict(t *testing.T) {
	first := `{"a":"rev@x","c":2,"n":"02","op":"verdict.add","ref":"` + sha1Rev + `","ts":2,"v":1,"val":"request-changes"}`
	second := `{"a":"rev@x","c":3,"n":"03","op":"verdict.add","ref":"` + sha1Rev + `","ts":3,"v":1,"val":"approve"}`

	st := fold(t,
		`{"a":"a@b","c":1,"n":"01","op":"create","ts":1,"v":1,"val":"review"}`,
		`{"a":"a@b","c":1,"n":"05","op":"head.sha","ts":1,"v":1,"val":"`+sha1Rev+`"}`,
		first, second,
		`{"a":"boss@x","c":4,"n":"04","op":"verdict.remove","ref":"`+id(t, second)+`","ts":4,"v":1}`,
	)

	v, ok := VerdictBy(st, "rev@x")
	if !ok {
		t.Fatal("dismissing the approval leaves the earlier verdict standing")
	}
	if v.Value != VerdictRequestChanges {
		t.Errorf("Value = %q, want the surviving request-changes", v.Value)
	}
}

func TestAnchorRoundTrip(t *testing.T) {
	for _, want := range []string{
		sha1Rev + " internal/review/anchor.go 42",
		sha1Rev + " internal/review/anchor.go 42-44",
		sha1Rev + " a/b.go 7 side=left",
		sha1Rev + " a/b.go 7 ctx=8b31e07f",
	} {
		a, err := ParseAnchor(want)
		if err != nil {
			t.Fatalf("ParseAnchor(%q): %v", want, err)
		}
		if got := a.String(); got != want {
			t.Errorf("round trip: got %q, want %q", got, want)
		}
	}
}

// An unrecognised key=value pair survives a round trip, which is what lets one
// be added without a format change.
func TestAnchorPreservesUnknownPairs(t *testing.T) {
	in := sha1Rev + " a/b.go 7 side=left gitlab.position=abc"
	a, err := ParseAnchor(in)
	if err != nil {
		t.Fatal(err)
	}
	if got := a.String(); got != in {
		t.Errorf("got %q, want %q", got, in)
	}
}

func TestParseWhere(t *testing.T) {
	path, first, last, err := ParseWhere("internal/review/anchor.go:42-44")
	if err != nil {
		t.Fatal(err)
	}
	if path != "internal/review/anchor.go" || first != 42 || last != 44 {
		t.Errorf("got %q %d %d", path, first, last)
	}
	// A Windows-ish path with a drive letter would break a first-colon split,
	// which is why the last colon is the separator.
	if p, f, _, err := ParseWhere("C:/src/a.go:9"); err != nil || p != "C:/src/a.go" || f != 9 {
		t.Errorf("got %q %d %v", p, f, err)
	}
	if _, _, _, err := ParseWhere("no-line-here"); err == nil {
		t.Error("a --on without a line should be refused")
	}
}

// fakeRepo answers the two questions an anchor asks, without a repository.
type fakeRepo struct {
	reachable map[string]bool
	changed   map[string]bool
}

func (f fakeRepo) Reachable(commit, head string) (bool, error) {
	return f.reachable[commit+"->"+head], nil
}
func (f fakeRepo) Changed(from, to, path string) (bool, error) {
	return f.changed[from+"->"+to+":"+path], nil
}

// Outdated and detached are different things, and ancestry alone cannot tell
// them apart: an anchor's commit stays an ancestor across every ordinary push.
func TestAnchorCurrency(t *testing.T) {
	a := Anchor{Revision: sha1Rev, Path: "a.go", First: 1, Last: 1}

	if got := a.Currency(nil, sha2Rev); got != Unknown {
		t.Errorf("no repo: got %v, want Unknown", got)
	}
	if got := a.Currency(fakeRepo{}, sha1Rev); got != Current {
		t.Errorf("same revision: got %v, want Current", got)
	}

	reachableUnchanged := fakeRepo{
		reachable: map[string]bool{sha1Rev + "->" + sha2Rev: true},
		changed:   map[string]bool{},
	}
	if got := a.Currency(reachableUnchanged, sha2Rev); got != Current {
		t.Errorf("reachable and untouched: got %v, want Current", got)
	}

	reachableChanged := fakeRepo{
		reachable: map[string]bool{sha1Rev + "->" + sha2Rev: true},
		changed:   map[string]bool{sha1Rev + "->" + sha2Rev + ":a.go": true},
	}
	if got := a.Currency(reachableChanged, sha2Rev); got != Outdated {
		t.Errorf("reachable but the file moved on: got %v, want Outdated", got)
	}

	if got := a.Currency(fakeRepo{}, sha2Rev); got != Detached {
		t.Errorf("unreachable: got %v, want Detached", got)
	}
}

// Only a root's anchor is read, and a resolve addresses the root.
func TestThreadsReadOnlyTheRootAnchor(t *testing.T) {
	root := `{"a":"rev@x","c":2,"n":"02","op":"comment","ts":2,"v":1,"val":"here"}`
	rootID := id(t, root)
	reply := `{"a":"a@b","c":4,"n":"04","op":"comment","ref":"` + rootID + `","ts":4,"v":1,"val":"ok"}`

	st := fold(t,
		`{"a":"a@b","c":1,"n":"01","op":"create","ts":1,"v":1,"val":"review"}`,
		`{"a":"a@b","c":1,"n":"09","op":"head.sha","ts":1,"v":1,"val":"`+sha1Rev+`"}`,
		root,
		`{"a":"rev@x","c":3,"n":"03","op":"comment.anchor","ref":"`+rootID+`","ts":3,"v":1,"val":"`+sha1Rev+` a.go 42"}`,
		reply,
		// An anchor on a reply is preserved on disk and ignored: a thread is
		// attached to one place in the code.
		`{"a":"a@b","c":5,"n":"05","op":"comment.anchor","ref":"`+id(t, reply)+`","ts":5,"v":1,"val":"`+sha1Rev+` elsewhere.go 9"}`,
		`{"a":"a@b","c":6,"n":"06","op":"comment.resolve","ref":"`+rootID+`","ts":6,"v":1,"val":true}`,
	)

	threads := Threads(st, nil)
	if len(threads) != 1 {
		t.Fatalf("got %d threads, want 1 root", len(threads))
	}
	tr := threads[0]
	if !tr.Anchored || tr.Anchor.Path != "a.go" || tr.Anchor.First != 42 {
		t.Errorf("anchor = %+v, want a.go:42", tr.Anchor)
	}
	if tr.Replies != 1 {
		t.Errorf("Replies = %d, want 1", tr.Replies)
	}
	if !tr.Resolved {
		t.Error("the thread should be resolved")
	}
	if len(OpenQuestions(threads)) != 0 {
		t.Error("a resolved thread is not an open question")
	}
}

// Only anchored threads block. A top-level remark is discussion, and counting
// it would make every review anyone spoke on permanently unready.
func TestOnlyAnchoredThreadsAreOpenQuestions(t *testing.T) {
	anchored := `{"a":"rev@x","c":2,"n":"02","op":"comment","ts":2,"v":1,"val":"fix this"}`
	loose := `{"a":"rev@x","c":4,"n":"04","op":"comment","ts":4,"v":1,"val":"nice work"}`

	st := fold(t,
		`{"a":"a@b","c":1,"n":"01","op":"create","ts":1,"v":1,"val":"review"}`,
		`{"a":"a@b","c":1,"n":"09","op":"head.sha","ts":1,"v":1,"val":"`+sha1Rev+`"}`,
		anchored,
		`{"a":"rev@x","c":3,"n":"03","op":"comment.anchor","ref":"`+id(t, anchored)+`","ts":3,"v":1,"val":"`+sha1Rev+` a.go 42"}`,
		loose,
	)

	threads := Threads(st, nil)
	if len(Unresolved(threads)) != 2 {
		t.Errorf("Unresolved = %d, want both", len(Unresolved(threads)))
	}
	open := OpenQuestions(threads)
	if len(open) != 1 || open[0].Anchor.Path != "a.go" {
		t.Errorf("OpenQuestions = %+v, want only the anchored one", open)
	}
}

// A re-run supersedes the previous result of the same name.
func TestChecksResolveLatestPerName(t *testing.T) {
	st := fold(t,
		`{"a":"ci@x","c":1,"n":"01","op":"check.add","ts":1,"v":1,"val":"build fail url=u1"}`,
		`{"a":"ci@x","c":2,"n":"02","op":"check.add","ts":2,"v":1,"val":"build pass url=u2"}`,
		`{"a":"ci@x","c":3,"n":"03","op":"check.add","ts":3,"v":1,"val":"lint pass"}`,
	)

	checks := ChecksOf(st)
	if len(checks) != 2 {
		t.Fatalf("got %d checks, want 2 names", len(checks))
	}
	// Sorted by name, so build comes first.
	if checks[0].Name != "build" || checks[0].Conclusion != ConclusionPass || checks[0].URL != "u2" {
		t.Errorf("build = %+v, want the later pass", checks[0])
	}

	s := Summarize(checks)
	if s.Passed != 2 || s.Failed != 0 {
		t.Errorf("Summarize = %+v, want 2 passed", s)
	}
}

func TestCheckFailedAndPendingClassify(t *testing.T) {
	for _, c := range []Check{{Conclusion: "fail"}, {Conclusion: "FAILURE"}, {Conclusion: "error"}} {
		if !c.Failed() {
			t.Errorf("%q should count as failed", c.Conclusion)
		}
	}
	for _, c := range []Check{{Conclusion: "pending"}, {Conclusion: "in_progress"}} {
		if !c.Pending() {
			t.Errorf("%q should count as pending", c.Conclusion)
		}
	}
	// An unrecognised conclusion does not block: this client classifies what it
	// knows, and treating an unknown word as a failure would block on a
	// platform's vocabulary rather than on its verdict.
	unknown := Check{Conclusion: "flaky-ish"}
	if unknown.Failed() || unknown.Pending() {
		t.Error("an unknown conclusion must be neither failed nor pending")
	}
}

// A terminal review is over, not blocked: reporting its failing build as
// something in the way of a merge would send an agent to fix a branch nobody
// wants.
func TestStatusOfTerminalReviewReportsOnlyItsStatus(t *testing.T) {
	st := fold(t,
		`{"a":"a@b","c":1,"n":"01","op":"create","ts":1,"v":1,"val":"review"}`,
		`{"a":"a@b","c":2,"n":"02","op":"status","ts":2,"v":1,"val":"closed"}`,
	)
	r := StatusOf("x", st, []Check{{Name: "build", Conclusion: "fail"}}, nil, Requirements{})
	if len(r.Blockers) != 1 || r.Blockers[0].Kind != BlockerStatus {
		t.Errorf("Blockers = %+v, want only the status", r.Blockers)
	}
}

func TestStatusOfReportsEachBlocker(t *testing.T) {
	anchored := `{"a":"rev@x","c":3,"n":"03","op":"comment","ts":3,"v":1,"val":"fix this"}`
	st := fold(t,
		`{"a":"a@b","c":1,"n":"01","op":"create","ts":1,"v":1,"val":"review"}`,
		`{"a":"a@b","c":2,"n":"02","op":"head.sha","ts":2,"v":1,"val":"`+sha1Rev+`"}`,
		anchored,
		`{"a":"rev@x","c":4,"n":"04","op":"comment.anchor","ref":"`+id(t, anchored)+`","ts":4,"v":1,"val":"`+sha1Rev+` a.go 42"}`,
		`{"a":"rev@x","c":5,"n":"05","op":"verdict.add","ref":"`+sha1Rev+`","ts":5,"v":1,"val":"request-changes"}`,
	)

	r := StatusOf("x", st, []Check{
		{Name: "build", Conclusion: "fail"},
		{Name: "lint", Conclusion: "pending"},
	}, nil, Requirements{MinApprovals: 1})

	want := map[BlockerKind]bool{
		BlockerChecks:     true,
		BlockerPending:    true,
		BlockerChanges:    true,
		BlockerThreads:    true,
		BlockerNoApproval: true,
	}
	for _, b := range r.Blockers {
		delete(want, b.Kind)
	}
	if len(want) > 0 {
		t.Errorf("missing blockers: %v (got %+v)", want, r.Blockers)
	}
	if r.Ready() {
		t.Error("Ready() must be false with blockers")
	}
}

// The checks ref is not core state: a clone that has never fetched it reports
// no checks failing rather than failing to report.
func TestStatusOfWithoutChecksReportsNone(t *testing.T) {
	st := fold(t,
		`{"a":"a@b","c":1,"n":"01","op":"create","ts":1,"v":1,"val":"review"}`,
		`{"a":"a@b","c":2,"n":"02","op":"head.sha","ts":2,"v":1,"val":"`+sha1Rev+`"}`,
	)
	r := StatusOf("x", st, nil, nil, Requirements{})
	if !r.Ready() {
		t.Errorf("no checks should not block; got %+v", r.Blockers)
	}
}

func TestClosesIsALinkAndNothingMore(t *testing.T) {
	target := "4b0755a3e7697bfdf17e42e9f4b307c161ea2a40"
	st := fold(t,
		`{"a":"a@b","c":1,"n":"01","op":"create","ts":1,"v":1,"val":"review"}`,
		`{"a":"a@b","c":2,"n":"02","op":"rel.add","ref":"`+target+`","ts":2,"v":1,"val":"closes"}`,
	)
	closes := Closes(st)
	if len(closes) != 1 || closes[0].Target != target {
		t.Fatalf("Closes = %+v", closes)
	}
	// Nothing about the review's own state follows from the link.
	if Status(st) != StatusOpen {
		t.Error("a closes link must not touch the review's status")
	}
}

func TestReasonTakesTarget(t *testing.T) {
	if kind, ok := ReasonTakesTarget(ReasonSuperseded); !ok || kind != KindSupersededBy {
		t.Errorf("superseded should name %q, got %q %v", KindSupersededBy, kind, ok)
	}
	if _, ok := ReasonTakesTarget(ReasonNotPlanned); ok {
		t.Error("not_planned names nothing else")
	}
	if got := NormalizeReason("Not-Planned"); got != ReasonNotPlanned {
		t.Errorf("NormalizeReason = %q, want %q", got, ReasonNotPlanned)
	}
	if KnownReason("completed") {
		t.Error("completed is redundant here: merged already says it")
	}
}

// The vocabulary is the review type's, and `type` is deliberately not in it.
func TestVocabularyExcludesIssueOnlyFields(t *testing.T) {
	for _, op := range Vocabulary.Scalars {
		if op == "type" || op == "pinned" {
			t.Errorf("%q has no meaning on a review", op)
		}
	}
	for _, want := range []string{"base", "head", "head.sha", "draft"} {
		found := false
		for _, op := range Vocabulary.Scalars {
			if op == want {
				found = true
			}
		}
		if !found {
			t.Errorf("%q must be a review scalar", want)
		}
	}
}
