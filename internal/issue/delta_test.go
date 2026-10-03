package issue

import (
	"slices"
	"testing"

	"github.com/hdweiss/git-issue/internal/entity"
)

// maps is a Mapped built from a literal, standing in for the origin ledger.
type maps map[string]string

func (m maps) Comment(id, event string) (string, bool) {
	v, ok := m[id+" "+event]
	return v, ok
}

// state folds a set of events the way a store would, with the issue vocabulary.
func state(t *testing.T, events ...entity.Event) entity.State {
	t.Helper()
	return entity.Fold(Vocabulary, events)
}

// ev builds a canonical event with a fixed nonce.
func mkEvent(t *testing.T, c int64, op string, val entity.Value, ref, nonce string) entity.Event {
	t.Helper()
	e, err := entity.NewEventWithNonce(entity.SHA1, entity.Event{
		V: entity.FormatVersion, C: c, TS: c, A: "a@example.com",
		Op: op, N: nonce, Ref: ref, Val: val,
	})
	if err != nil {
		t.Fatal(err)
	}
	return e
}

func mkStr(t *testing.T, c int64, op, val, nonce string) entity.Event {
	return mkEvent(t, c, op, entity.Str(val), "", nonce)
}

const entID = "4f2a1c9"

// The three-way table, one case per row. These are the whole contract: what
// pushes, what stays put, and what conflicts.
func TestDiffScalars(t *testing.T) {
	for _, tc := range []struct {
		name            string
		base, local, up string
		wantSet         string // "" for nothing to push
		wantConflict    bool
	}{
		{name: "nobody moved", base: "old", local: "old", up: "old"},
		{name: "only local moved", base: "old", local: "new", up: "old", wantSet: "new"},
		{name: "only upstream moved", base: "old", local: "old", up: "theirs"},
		{name: "both moved the same way", base: "old", local: "same", up: "same"},
		{name: "both moved apart", base: "old", local: "mine", up: "theirs", wantConflict: true},
		// The one that stops a repeat push from re-pushing forever: the change
		// went out on an earlier run and came back as the tracker's own event.
		{name: "already pushed", base: "new", local: "new", up: "new"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			base := state(t, mkStr(t, 1, "title", tc.base, "1111111111111111"))
			local := state(t, mkStr(t, 2, "title", tc.local, "2222222222222222"))
			up := Upstream{State: state(t, mkStr(t, 3, "title", tc.up, "3333333333333333"))}

			d, conflicts := Diff(entID, base, local, up, maps{})

			if tc.wantConflict {
				if len(conflicts) != 1 || conflicts[0].Field != "title" {
					t.Fatalf("conflicts = %+v, want one on title", conflicts)
				}
				if conflicts[0].Local != tc.local || conflicts[0].Upstream != tc.up {
					t.Errorf("conflict = %+v, want %s vs %s", conflicts[0], tc.local, tc.up)
				}
				if _, ok := d.Set["title"]; ok {
					t.Error("a conflicted field was also queued to push")
				}
				return
			}
			if len(conflicts) != 0 {
				t.Fatalf("unexpected conflicts %+v", conflicts)
			}
			got, ok := d.Set["title"]
			if tc.wantSet == "" {
				if ok {
					t.Errorf("queued %q, want nothing", got.Display())
				}
				return
			}
			if !ok || got.Display() != tc.wantSet {
				t.Errorf("queued %q, want %q", got.Display(), tc.wantSet)
			}
		})
	}
}

// An issue that has never been closed must not look like a pending status
// change. Open is the implied status of an issue with no status event.
func TestDiffStatusDefaultsToOpen(t *testing.T) {
	empty := state(t)
	d, conflicts := Diff(entID, empty, empty, Upstream{State: empty}, maps{})
	if len(conflicts) != 0 || !d.Empty() {
		t.Errorf("an untouched issue produced %+v / %+v", d, conflicts)
	}

	// Closing locally is a real change, though.
	local := state(t, mkStr(t, 2, "status", StatusClosed, "2222222222222222"))
	d, _ = Diff(entID, empty, local, Upstream{State: empty}, maps{})
	if got, ok := d.Set["status"]; !ok || got.Display() != StatusClosed {
		t.Errorf("closing locally queued %v, %v", got.Display(), ok)
	}
}

// A list is an OR-Set difference, and cannot conflict. What matters is that a
// member the tracker added on its own is not deleted by a clone that never saw
// it.
func TestDiffLists(t *testing.T) {
	label := func(t *testing.T, c int64, name, nonce string) entity.Event {
		return mkStr(t, c, "label.add", name, nonce)
	}

	base := state(t, label(t, 1, "bug", "1111111111111111"))
	local := state(t,
		label(t, 1, "bug", "1111111111111111"),
		label(t, 2, "urgent", "2222222222222222"),
	)
	up := Upstream{State: state(t,
		label(t, 1, "bug", "1111111111111111"),
		label(t, 3, "theirs", "3333333333333333"),
	)}

	d, conflicts := Diff(entID, base, local, up, maps{})
	if len(conflicts) != 0 {
		t.Fatalf("a list produced conflicts: %+v", conflicts)
	}
	if !slices.Equal(d.LabelsAdded, []string{"urgent"}) {
		t.Errorf("LabelsAdded = %v, want [urgent]", d.LabelsAdded)
	}
	// "theirs" is in upstream but not in base: the tracker added it after the
	// last agreement, so this clone has not dropped it — it has never seen it.
	if len(d.LabelsRemoved) != 0 {
		t.Errorf("LabelsRemoved = %v, want none", d.LabelsRemoved)
	}

	// Dropping a label both sides held is a removal.
	dropped := state(t)
	d, _ = Diff(entID, base, dropped, Upstream{State: base}, maps{})
	if !slices.Equal(d.LabelsRemoved, []string{"bug"}) {
		t.Errorf("LabelsRemoved = %v, want [bug]", d.LabelsRemoved)
	}
}

// Threads go by the ledger, never by text: two entries with the same body are
// legitimately two comments, which is the whole reason events carry a nonce.
func TestDiffThread(t *testing.T) {
	posted := mkStr(t, 1, "comment", "already there", "1111111111111111")
	fresh := mkStr(t, 2, "comment", "not yet", "2222222222222222")
	edited := mkStr(t, 3, "comment", "changed here", "3333333333333333")
	local := state(t, posted, fresh, edited)

	ledger := maps{
		entID + " " + posted.ID: "IC_posted",
		entID + " " + edited.ID: "IC_edited",
	}
	up := Upstream{
		State: state(t),
		Comments: map[string]string{
			"IC_posted": "already there",
			"IC_edited": "what it used to say",
		},
	}

	d, _ := Diff(entID, state(t), local, up, ledger)

	if len(d.CommentsNew) != 1 || d.CommentsNew[0].Entry.ID() != fresh.ID {
		t.Errorf("CommentsNew = %+v, want just the unmapped entry", d.CommentsNew)
	}
	if len(d.CommentsEdited) != 1 || d.CommentsEdited[0].Upstream != "IC_edited" {
		t.Errorf("CommentsEdited = %+v, want the one whose body moved", d.CommentsEdited)
	}
	if len(d.CommentsRemoved) != 0 {
		t.Errorf("CommentsRemoved = %+v, want none", d.CommentsRemoved)
	}
}

// A mapped entry the tracker no longer reports was deleted there. An absent
// body is not an empty one: treating it as one makes every such entry look
// edited, and pushing that edit either fails or resurrects what somebody
// deliberately removed.
func TestDiffIgnoresCommentsDeletedUpstream(t *testing.T) {
	posted := mkStr(t, 1, "comment", "still here locally", "1111111111111111")
	local := state(t, posted)

	ledger := maps{entID + " " + posted.ID: "IC_goneUpstream"}
	up := Upstream{State: state(t), Comments: map[string]string{}}

	d, _ := Diff(entID, state(t), local, up, ledger)
	if !d.Empty() {
		t.Errorf("a comment deleted upstream produced %+v", d)
	}
}

// A mapped entry that was retracted is deleted upstream; an unmapped one was
// never posted, so there is nothing to delete.
func TestDiffRetractedComments(t *testing.T) {
	postedThenPulled := mkStr(t, 1, "comment", "posted", "1111111111111111")
	neverPosted := mkStr(t, 2, "comment", "never sent", "2222222222222222")
	local := state(t,
		postedThenPulled, neverPosted,
		mkEvent(t, 3, "comment.remove", entity.Value{}, postedThenPulled.ID, "3333333333333333"),
		mkEvent(t, 4, "comment.remove", entity.Value{}, neverPosted.ID, "4444444444444444"),
	)

	ledger := maps{entID + " " + postedThenPulled.ID: "IC_posted"}
	d, _ := Diff(entID, state(t), local, Upstream{State: state(t)}, ledger)

	if len(d.CommentsRemoved) != 1 || d.CommentsRemoved[0].Upstream != "IC_posted" {
		t.Errorf("CommentsRemoved = %+v, want only the one that was posted", d.CommentsRemoved)
	}
	if len(d.CommentsNew) != 0 {
		t.Errorf("a retracted entry that was never posted was queued: %+v", d.CommentsNew)
	}
}

// Base is the intersection of raw event lines. An event both sides hold came
// from the tracker; one only this clone holds did not.
func TestBase(t *testing.T) {
	shared := mkStr(t, 1, "title", "from upstream", "1111111111111111")
	mine := mkStr(t, 2, "title", "my edit", "2222222222222222")
	theirs := mkStr(t, 3, "milestone", "v2", "3333333333333333")

	base := Base(Vocabulary, []entity.Event{shared, mine}, []entity.Event{shared, theirs})
	if got := base.Scalar("title").Display(); got != "from upstream" {
		t.Errorf("base title = %q, want the shared event's value", got)
	}
	if got := base.Scalar("milestone").Display(); got != "" {
		t.Errorf("base picked up %q from an event only the import had", got)
	}
}

// The end-to-end shape of a fresh push: nothing shared, so everything local is
// something to send.
func TestDiffCreate(t *testing.T) {
	local := state(t,
		mkStr(t, 1, "title", "Crash on empty input", "1111111111111111"),
		mkStr(t, 2, "description", "steps follow", "2222222222222222"),
		mkStr(t, 3, "label.add", "bug", "3333333333333333"),
		mkStr(t, 4, "comment", "first", "4444444444444444"),
	)
	empty := state(t)

	d, conflicts := Diff(entID, empty, local, Upstream{State: empty}, maps{})
	d.Create = true

	if len(conflicts) != 0 {
		t.Fatalf("a create conflicted: %+v", conflicts)
	}
	if d.Empty() {
		t.Fatal("a create has nothing to send")
	}
	if d.Set["title"].Display() != "Crash on empty input" {
		t.Errorf("title = %q", d.Set["title"].Display())
	}
	if !slices.Equal(d.LabelsAdded, []string{"bug"}) {
		t.Errorf("LabelsAdded = %v", d.LabelsAdded)
	}
	if len(d.CommentsNew) != 1 {
		t.Errorf("CommentsNew = %+v", d.CommentsNew)
	}
	// Status is not among them: an issue filed upstream is open already, and
	// saying so would be a redundant write on every create.
	if _, ok := d.Set["status"]; ok {
		t.Error("a create queued a status change")
	}
}

func TestDeltaFields(t *testing.T) {
	d := Delta{
		Set:         map[string]entity.Value{"title": entity.Str("x"), "milestone": entity.Str("v2")},
		LabelsAdded: []string{"bug"},
		CommentsNew: []CommentPush{{}, {}},
	}
	want := []string{"title", "milestone", "labels", "2 comments"}
	if got := d.Fields(); !slices.Equal(got, want) {
		t.Errorf("Fields() = %v, want %v", got, want)
	}
}

// Links diff as the OR-Set they are, on the pair rather than on the event id.
//
// The pair is what matters because a link pushed to a tracker comes back as
// that tracker's own `rel.add`: a different event saying the same thing. Compare
// event identity and a mirror pushes the same link forever.
func TestDiffRelations(t *testing.T) {
	const epic, blocker = "1111111111111111111111111111111111111111", "2222222222222222222222222222222222222222"
	link := func(t *testing.T, c int64, kind, target, nonce string) entity.Event {
		return mkEvent(t, c, RelAdd, entity.Str(kind), target, nonce)
	}

	// Written here, never sent.
	local := state(t, link(t, 1, KindParent, epic, "1111111111111111"))
	d, conflicts := Diff(entID, state(t), local, Upstream{}, maps{})
	if len(conflicts) != 0 {
		t.Fatalf("a link produced conflicts: %+v", conflicts)
	}
	if len(d.RelationsAdded) != 1 || d.RelationsAdded[0].Target != epic {
		t.Fatalf("RelationsAdded = %+v, want the parent", d.RelationsAdded)
	}
	if got := d.Fields(); len(got) != 1 || got[0] != "links" {
		t.Errorf("Fields() = %v, want links", got)
	}

	// The same link arriving back from the tracker as its own event settles the
	// difference: same pair, so there is nothing left to send.
	theirs := state(t, link(t, 9, KindParent, epic, "9999999999999999"))
	both := state(t,
		link(t, 1, KindParent, epic, "1111111111111111"),
		link(t, 9, KindParent, epic, "9999999999999999"),
	)
	d, _ = Diff(entID, theirs, both, Upstream{State: theirs}, maps{})
	if len(d.RelationsAdded) != 0 || !d.Empty() {
		t.Errorf("a link already upstream would be pushed again: %+v", d.RelationsAdded)
	}

	// A link both sides held and this one dropped is a retraction; one the
	// tracker gained since is not, because this clone never saw it to drop.
	base := state(t, link(t, 1, KindParent, epic, "1111111111111111"))
	up := Upstream{State: state(t,
		link(t, 1, KindParent, epic, "1111111111111111"),
		link(t, 3, KindBlockedBy, blocker, "3333333333333333"),
	)}
	d, _ = Diff(entID, base, state(t), up, maps{})
	if len(d.RelationsRemoved) != 1 || d.RelationsRemoved[0].Kind != KindParent {
		t.Errorf("RelationsRemoved = %+v, want just the parent", d.RelationsRemoved)
	}
}
