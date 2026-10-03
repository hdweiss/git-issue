package entity

import (
	"slices"
	"strings"
	"testing"
)

var vocab = Vocabulary{Scalars: []string{"title", "status"}}

// mk builds an event the way a writer would, so its id is a real hash of its
// own bytes and ordering behaves as it does in production.
func mk(t *testing.T, c int64, ts int64, author, op string, val Value, ref string) Event {
	t.Helper()
	e, err := NewEvent(SHA1, FormatVersion, c, ts, author, op, val, ref)
	if err != nil {
		t.Fatal(err)
	}
	return e
}

// The fold must be commutative: any order of arrival folds to the same state.
// This is the property cat_sort_uniq destroys physical order to enforce.
func TestFoldIsOrderIndependent(t *testing.T) {
	events := []Event{
		mk(t, 1, 100, "a@x", "create", Str("issue"), ""),
		mk(t, 2, 100, "a@x", "title", Str("first"), ""),
		mk(t, 5, 100, "b@x", "title", Str("last"), ""),
		mk(t, 3, 100, "a@x", "status", Str("open"), ""),
	}

	want := Fold(vocab, events)
	reversed := slices.Clone(events)
	slices.Reverse(reversed)
	got := Fold(vocab, reversed)

	if got.Scalar("title").Display() != want.Scalar("title").Display() {
		t.Fatal("fold depends on arrival order")
	}
	if got.Scalar("title").Display() != "last" {
		t.Errorf("title resolved to %q, want the highest (c, id)", got.Scalar("title").Display())
	}
}

// A clock three years fast must not win. Ordering is (c, id), never ts.
func TestScalarIgnoresWallClock(t *testing.T) {
	st := Fold(vocab, []Event{
		mk(t, 1, 100, "a@x", "create", Str("issue"), ""),
		mk(t, 9, 100, "correct@x", "title", Str("correct"), ""),
		mk(t, 2, 1<<31, "brokenrtc@x", "title", Str("from a broken clock"), ""),
	})
	if got := st.Scalar("title").Display(); got != "correct" {
		t.Errorf("title = %q; a future timestamp won the merge", got)
	}
}

// OR-Set: a remove retracts one specific add by id. A concurrent add creates a
// new id the remove does not cover, so the member survives — add-wins.
func TestListIsObservedRemoveSet(t *testing.T) {
	addA := mk(t, 2, 100, "a@x", "label.add", Str("design"), "")
	addB := mk(t, 2, 100, "b@x", "label.add", Str("design"), "")
	rm := mk(t, 3, 200, "a@x", "label.remove", Value{}, addA.ID)

	st := Fold(vocab, []Event{
		mk(t, 1, 100, "a@x", "create", Str("issue"), ""), addA, addB, rm,
	})
	if got := st.List("label"); !slices.Equal(got, []string{"design"}) {
		t.Errorf("labels = %v, want the concurrent add to survive", got)
	}

	// Retracting the survivor too leaves the field empty.
	rm2 := mk(t, 4, 300, "a@x", "label.remove", Value{}, addB.ID)
	st = Fold(vocab, []Event{
		mk(t, 1, 100, "a@x", "create", Str("issue"), ""), addA, addB, rm, rm2,
	})
	if got := st.List("label"); len(got) != 0 {
		t.Errorf("labels = %v, want empty", got)
	}
}

// A remove naming the member's *value* rather than an add's id must do nothing.
func TestListRemoveByValueIsIgnored(t *testing.T) {
	add := mk(t, 2, 100, "a@x", "label.add", Str("design"), "")
	byValue := mk(t, 3, 200, "a@x", "label.remove", Str("design"), "")

	st := Fold(vocab, []Event{
		mk(t, 1, 100, "a@x", "create", Str("issue"), ""), add, byValue,
	})
	if got := st.List("label"); !slices.Equal(got, []string{"design"}) {
		t.Errorf("labels = %v; a value-addressed remove must not retract anything", got)
	}
}

// A reaction is the triple (author, value, target). The same person reacting
// twice — a double click, or two offline replicas — is one reaction.
func TestReactionsDedupeByTriple(t *testing.T) {
	create := mk(t, 1, 100, "a@x", "create", Str("issue"), "")
	events := []Event{
		create,
		mk(t, 2, 100, "a@x", "react", Str("+1"), create.ID),
		mk(t, 3, 101, "a@x", "react", Str("+1"), create.ID),
		mk(t, 4, 102, "b@x", "react", Str("+1"), create.ID),
	}

	st := Fold(vocab, events)
	if len(st.Reactions) != 2 {
		t.Fatalf("got %d reactions, want 2 (one per author)", len(st.Reactions))
	}
	if n := len(ReactionsByTarget(st.Reactions)[create.ID]["+1"]); n != 2 {
		t.Errorf("+1 counted %d authors, want 2", n)
	}
}

// react.remove retracts a specific react event; a surviving duplicate keeps
// the reaction alive, exactly like any other list member.
func TestReactionRemoveIsAddWins(t *testing.T) {
	create := mk(t, 1, 100, "a@x", "create", Str("issue"), "")
	first := mk(t, 2, 100, "a@x", "react", Str("+1"), create.ID)
	second := mk(t, 3, 101, "a@x", "react", Str("+1"), create.ID)

	st := Fold(vocab, []Event{
		create, first, second,
		mk(t, 4, 200, "a@x", "react.remove", Value{}, first.ID),
	})
	if len(st.Reactions) != 1 {
		t.Errorf("got %d reactions, want the un-retracted duplicate to survive", len(st.Reactions))
	}

	st = Fold(vocab, []Event{
		create, first,
		mk(t, 4, 200, "a@x", "react.remove", Value{}, first.ID),
	})
	if len(st.Reactions) != 0 {
		t.Errorf("got %d reactions, want none", len(st.Reactions))
	}
}

// A tombstone hides the entry's own body and nothing else. A cascade would let
// one person destroy other people's content as a side effect of withdrawing
// their own.
func TestTombstoneKeepsChildren(t *testing.T) {
	create := mk(t, 1, 100, "a@x", "create", Str("issue"), "")
	parent := mk(t, 2, 100, "a@x", "comment", Str("original"), "")
	child := mk(t, 3, 101, "b@x", "comment", Str("a reply"), parent.ID)

	st := Fold(vocab, []Event{
		create, parent, child,
		mk(t, 4, 200, "a@x", "comment.remove", Value{}, parent.ID),
	})
	if len(st.Thread) != 2 {
		t.Fatalf("thread has %d entries, want 2", len(st.Thread))
	}

	forest := Forest(st.Thread)
	if n := len(forest[parent.ID]); n != 1 {
		t.Errorf("retracted parent has %d children, want 1", n)
	}
	for _, c := range st.Thread {
		if c.ID() == parent.ID && !c.Retracted {
			t.Error("parent is not marked retracted")
		}
		if c.ID() == child.ID && c.Retracted {
			t.Error("reply was retracted by the cascade")
		}
	}
}

// A comment is addressed by a prefix of its own id, resolved within the one
// entity, and an ambiguous prefix is refused rather than picked between — the
// same contract Store.Resolve gives an entity id.
func TestFindComment(t *testing.T) {
	create := mk(t, 1, 100, "a@x", "create", Str("issue"), "")
	first := mk(t, 2, 100, "a@x", "comment", Str("first"), "")
	second := mk(t, 3, 101, "b@x", "comment", Str("second"), "")
	st := Fold(vocab, []Event{create, first, second})

	// A full id, and the shortest prefix that still tells the two apart.
	for _, prefix := range []string{first.ID, first.ID[:distinguishing(first.ID, second.ID)]} {
		got, err := st.FindComment(prefix)
		if err != nil {
			t.Fatalf("FindComment(%q): %s", prefix, err)
		}
		if got.ID() != first.ID {
			t.Errorf("FindComment(%q) resolved to %s, want %s", prefix, got.ID(), first.ID)
		}
	}

	// The empty prefix matches every entry, which is exactly the case that
	// must not silently resolve to the first one.
	for _, tc := range []struct{ prefix, want string }{
		{"", "no comment given"},
		{"ffffffffffffffffffffffffffffffffffffffff", "unknown comment"},
	} {
		if _, err := st.FindComment(tc.prefix); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("FindComment(%q) = %v, want %q", tc.prefix, err, tc.want)
		}
	}

	// Two entries under one prefix is a refusal, never a choice: which one was
	// meant is not recoverable from what was typed. Seventeen comments put two
	// under some one-character prefix by the pigeonhole principle, whatever
	// the nonces come out as.
	events := []Event{create}
	for i := range 17 {
		events = append(events, mk(t, int64(i+2), 100, "a@x", "comment", Str("body"), ""))
	}
	crowded := Fold(vocab, events)

	seen := map[byte]bool{}
	for _, c := range crowded.Thread {
		hex := c.ID()[0]
		if !seen[hex] {
			seen[hex] = true
			continue
		}
		if _, err := crowded.FindComment(string(hex)); err == nil || !strings.Contains(err.Error(), "ambiguous comment") {
			t.Fatalf("shared prefix %q = %v, want an ambiguity", string(hex), err)
		}
		return
	}
	t.Fatal("17 comments produced 17 distinct leading hex digits, which is impossible")
}

// distinguishing is the length of the shortest prefix of a that b does not
// share — the shortest thing anyone could type to mean a.
func distinguishing(a, b string) int {
	for i := 0; i < len(a) && i < len(b); i++ {
		if a[i] != b[i] {
			return i + 1
		}
	}
	return len(a)
}

// An entry whose parent is unknown displays at root. It must never be hidden
// pending its parent's arrival: parents go missing routinely.
func TestDanglingParentSurfacesAtRoot(t *testing.T) {
	create := mk(t, 1, 100, "a@x", "create", Str("issue"), "")
	orphan := mk(t, 2, 100, "a@x", "comment", Str("orphan"), "0000000000000000000000000000000000000000")

	forest := Forest(Fold(vocab, []Event{create, orphan}).Thread)
	if len(forest[""]) != 1 || forest[""][0].ID() != orphan.ID {
		t.Errorf("orphan is not at root: %+v", forest)
	}
}

// The latest edit by (c, id) wins, and edits address the entry by id so they
// survive a body change or an identically worded neighbour.
func TestCommentEditLastWriteWins(t *testing.T) {
	create := mk(t, 1, 100, "a@x", "create", Str("issue"), "")
	c := mk(t, 2, 100, "a@x", "comment", Str("v1"), "")

	st := Fold(vocab, []Event{
		create, c,
		mk(t, 4, 300, "a@x", "comment.edit", Str("v3"), c.ID),
		mk(t, 3, 200, "a@x", "comment.edit", Str("v2"), c.ID),
	})
	if got := st.Thread[0].Body.Display(); got != "v3" {
		t.Errorf("body = %q, want v3", got)
	}
}

// Unknown ops fold into nothing but must not disturb what is known — they
// still carry a clock, and the entity must resolve completely without them.
func TestUnknownOpsAreInert(t *testing.T) {
	create := mk(t, 1, 100, "a@x", "create", Str("issue"), "")
	st := Fold(vocab, []Event{
		create,
		mk(t, 2, 100, "a@x", "title", Str("known"), ""),
		mk(t, 3, 100, "a@x", "github.origin", Str("MDU6SXNzdWUx"), ""),
		mk(t, 4, 100, "a@x", "quantum.entangle", Str("?"), ""),
	})
	if got := st.Scalar("title").Display(); got != "known" {
		t.Errorf("title = %q", got)
	}
	if len(st.Lists) != 0 || len(st.Thread) != 0 {
		t.Error("an unknown op leaked into folded state")
	}
	if len(st.Events) != 4 {
		t.Errorf("kept %d events, want all 4 preserved", len(st.Events))
	}
}

// A member is the pair (val, ref). One kind pointing at two entities is two
// members; the same pair added twice is one, however many events carry it.
func TestMemberIsThePair(t *testing.T) {
	first := "4b0755a3e7697bfdf17e42e9f4b307c161ea2a40"
	second := "e878760e137a63e084cddcb8e1ac14b16b3b5313"
	st := Fold(vocab, []Event{
		mk(t, 1, 100, "a@x", "create", Str("issue"), ""),
		mk(t, 2, 100, "a@x", "rel.add", Str("blocked-by"), first),
		mk(t, 3, 100, "a@x", "rel.add", Str("blocked-by"), second),
		// The same pair from another clone: two adds, one member.
		mk(t, 3, 100, "b@x", "rel.add", Str("blocked-by"), first),
	})

	if got := len(st.Members("rel")); got != 3 {
		t.Errorf("Members kept %d adds, want all 3 — a remove has to name each one", got)
	}
	distinct := st.Distinct("rel")
	if len(distinct) != 2 {
		t.Fatalf("Distinct gave %d members, want 2: the repeated pair is one member", len(distinct))
	}
	for i, want := range []string{first, second} {
		if distinct[i].Val.Display() != "blocked-by" || distinct[i].Ref != want {
			t.Errorf("member %d = (%q, %s), want (blocked-by, %s)", i, distinct[i].Val.Display(), distinct[i].Ref, want)
		}
	}
}

// An annotation addresses the add event, and the highest (c, id) wins. One
// naming an add nothing here holds is ignored rather than resolved onto
// something else.
func TestMemberAnnotation(t *testing.T) {
	target := "4b0755a3e7697bfdf17e42e9f4b307c161ea2a40"
	add := mk(t, 2, 100, "a@x", "rel.add", Str("blocked-by"), target)
	st := Fold(vocab, []Event{
		mk(t, 1, 100, "a@x", "create", Str("issue"), ""), add,
		mk(t, 3, 100, "a@x", "rel.note", Str("first"), add.ID),
		mk(t, 4, 100, "a@x", "rel.note", Str("second"), add.ID),
		mk(t, 5, 100, "a@x", "rel.note", Str("orphan"), target),
	})

	members := st.Distinct("rel")
	if len(members) != 1 {
		t.Fatalf("got %d members, want 1: a note is not a member", len(members))
	}
	if got := members[0].Note.Display(); got != "second" {
		t.Errorf("note = %q, want the highest (c, id)", got)
	}
}

// A member carries the id of the add that put it there, which is the only
// thing a writer can legitimately address a remove to. Two adds of the same
// value are two members, so removing that value means removing both.
func TestMembersCarryTheirAddID(t *testing.T) {
	addA := mk(t, 2, 100, "a@x", "label.add", Str("design"), "")
	addB := mk(t, 3, 100, "b@x", "label.add", Str("design"), "")
	st := Fold(vocab, []Event{
		mk(t, 1, 100, "a@x", "create", Str("issue"), ""), addA, addB,
	})

	members := st.Members("label")
	if len(members) != 2 {
		t.Fatalf("got %d members, want both adds", len(members))
	}
	for i, want := range []Event{addA, addB} {
		if members[i].ID != want.ID {
			t.Errorf("member %d has id %s, want %s", i, members[i].ID, want.ID)
		}
		if members[i].Val.Display() != "design" {
			t.Errorf("member %d has value %q", i, members[i].Val.Display())
		}
	}
	// Retracting one leaves the other, and the survivor still names its own add.
	st = Fold(vocab, []Event{
		mk(t, 1, 100, "a@x", "create", Str("issue"), ""), addA, addB,
		mk(t, 4, 200, "a@x", "label.remove", Value{}, addA.ID),
	})
	if members := st.Members("label"); len(members) != 1 || members[0].ID != addB.ID {
		t.Errorf("members = %v, want only the add that was not retracted", members)
	}
}

// A new event's clock is one past the highest already in the blob, so it
// resolves above every event this writer has seen — and ties with one it has
// not are broken by id, as they are anywhere else.
func TestNextClock(t *testing.T) {
	var empty State
	if got := empty.NextClock(); got != 1 {
		t.Errorf("empty state's next clock is %d, want 1", got)
	}

	st := Fold(vocab, []Event{
		mk(t, 1, 100, "a@x", "create", Str("issue"), ""),
		mk(t, 7, 100, "b@x", "title", Str("later"), ""),
		mk(t, 3, 100, "a@x", "status", Str("open"), ""),
	})
	if got := st.NextClock(); got != 8 {
		t.Errorf("next clock is %d, want one past the highest (7)", got)
	}
}

// A value added twice is one member, not two. docs/blob-format.md defines
// presence per member, and a push-then-pull cycle produces a second add for
// every value it sent — so without this a mirrored issue's labels double on
// every round trip.
//
// The adds themselves survive: a remove names one add event, so retracting a
// value that was added twice takes two removes, and Members still offers both.
func TestListDedupesByValue(t *testing.T) {
	first := mk(t, 1, 100, "a@x", "label.add", Str("bug"), "")
	second := mk(t, 2, 100, "github:someone", "label.add", Str("bug"), "")
	st := Fold(Vocabulary{}, []Event{first, second})

	if got := st.List("label"); len(got) != 1 || got[0] != "bug" {
		t.Errorf("List = %v, want one bug", got)
	}
	if got := st.Members("label"); len(got) != 2 {
		t.Errorf("Members = %d, want both adds so a remove can name each", len(got))
	}

	// Retracting only one of them leaves the member present, which is the
	// OR-Set behaviour the dedup must not disturb.
	st = Fold(Vocabulary{}, []Event{first, second,
		mk(t, 3, 100, "a@x", "label.remove", Value{}, first.ID)})
	if got := st.List("label"); len(got) != 1 || got[0] != "bug" {
		t.Errorf("after one remove List = %v, want bug still present", got)
	}

	// Retracting both takes it away.
	st = Fold(Vocabulary{}, []Event{first, second,
		mk(t, 3, 100, "a@x", "label.remove", Value{}, first.ID),
		mk(t, 4, 100, "a@x", "label.remove", Value{}, second.ID)})
	if got := st.List("label"); len(got) != 0 {
		t.Errorf("after both removes List = %v, want empty", got)
	}
}

// SameAs compares what a reader sees, not what the blob holds. The distinction
// is the whole point: pushing to a bridge makes the tracker record an event of
// its own for the change, and the next import writes that event faithfully — so
// the blob grows while nothing anybody can see moves.
func TestSameAs(t *testing.T) {
	base := []Event{
		mk(t, 1, 100, "a@x", "title", Str("Fix the parser"), ""),
		mk(t, 2, 100, "a@x", "label.add", Str("bug"), ""),
	}
	vocab := Vocabulary{Scalars: []string{"title", "milestone"}}

	// The echo: the tracker's own event for a change this clone already made.
	// Different bytes, different author, same value.
	echo := append(append([]Event(nil), base...),
		mk(t, 3, 100, "github:someone", "title", Str("Fix the parser"), ""),
		mk(t, 4, 100, "github:someone", "label.add", Str("bug"), ""))

	if !Fold(vocab, base).SameAs(Fold(vocab, echo)) {
		t.Error("an echo of what the blob already said was reported as a change")
	}
	// And it is symmetric, since a caller may hold either side.
	if !Fold(vocab, echo).SameAs(Fold(vocab, base)) {
		t.Error("SameAs is not symmetric")
	}

	for _, tc := range []struct {
		name  string
		event Event
	}{
		{"a scalar moved", mk(t, 3, 100, "a@x", "title", Str("Something else"), "")},
		{"a scalar appeared", mk(t, 3, 100, "a@x", "milestone", Str("v2"), "")},
		{"a member appeared", mk(t, 3, 100, "a@x", "label.add", Str("urgent"), "")},
		{"a comment appeared", mk(t, 3, 100, "a@x", "comment", Str("hello"), "")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := append(append([]Event(nil), base...), tc.event)
			if Fold(vocab, base).SameAs(Fold(vocab, changed)) {
				t.Error("a real change was reported as unchanged")
			}
		})
	}

	// Retracting a member is a change, even though the add events remain.
	removed := append(append([]Event(nil), base...),
		mk(t, 3, 100, "a@x", "label.remove", Value{}, base[1].ID))
	if Fold(vocab, base).SameAs(Fold(vocab, removed)) {
		t.Error("a removed member was reported as unchanged")
	}
}
