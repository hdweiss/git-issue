package review

import (
	"testing"

	"github.com/hdweiss/git-issue/internal/entity"
)

const entID = "aaaa1111"

// maps is a Mapped built from literals, standing in for the origin ledger.
type maps struct {
	comments map[string]string
	threads  map[string]string
	verdicts map[string]string
}

func (m maps) Comment(id, event string) (string, bool) { return look(m.comments, id, event) }
func (m maps) Thread(id, root string) (string, bool)   { return look(m.threads, id, root) }
func (m maps) Verdict(id, event string) (string, bool) { return look(m.verdicts, id, event) }

func look(index map[string]string, id, key string) (string, bool) {
	v, ok := index[id+" "+key]
	return v, ok
}

func mkEvent(t *testing.T, c int64, op string, val entity.Value, ref, nonce string) entity.Event {
	t.Helper()
	e, err := entity.NewEventWithNonce(entity.SHA1, entity.Event{
		V: entity.FormatVersion, C: c, TS: 1772700000, A: "a@example.com",
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

func folded(events ...entity.Event) entity.State { return entity.Fold(Vocabulary, events) }

// The three-way table for scalars. These are the whole contract: what pushes,
// what stays put, and what conflicts.
func TestDiffScalars(t *testing.T) {
	for _, tc := range []struct {
		name            string
		base, local, up string
		wantSet         string
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
			mk := func(title string) entity.State {
				return folded(mkStr(t, 1, "title", title, "1111111111111111"))
			}
			d, conflicts := Diff(entID, mk(tc.base), mk(tc.local),
				Upstream{State: mk(tc.up)}, maps{})

			if got := len(conflicts) > 0; got != tc.wantConflict {
				t.Fatalf("conflict = %v, want %v", got, tc.wantConflict)
			}
			got := ""
			if v, ok := d.Set["title"]; ok {
				got = v.Display()
			}
			if got != tc.wantSet {
				t.Errorf("Set[title] = %q, want %q", got, tc.wantSet)
			}
		})
	}
}

// A review carrying no status event is open, and one carrying no draft event is
// not a draft. Without those defaults every review that was never closed, and
// every one somebody explicitly marked ready, would look like a change waiting
// to be pushed forever.
func TestDiffImpliedDefaults(t *testing.T) {
	// The local side says out loud what the tracker leaves unsaid.
	local := folded(
		mkStr(t, 1, "title", "t", "1111111111111111"),
		mkStr(t, 2, "status", StatusOpen, "2222222222222222"),
		mkEvent(t, 3, "draft", entity.Bool(false), "", "3333333333333333"),
	)
	up := folded(mkStr(t, 1, "title", "t", "1111111111111111"))

	d, conflicts := Diff(entID, up, local, Upstream{State: up}, maps{})
	if len(conflicts) > 0 {
		t.Fatalf("unexpected conflicts: %+v", conflicts)
	}
	if !d.Empty() {
		t.Errorf("spelling out a default is not a change to push: %+v", d.Set)
	}
}

// `head` and `head.sha` describe a branch and a commit the tracker learns from
// the git remote. A bridge that pushed them would be claiming to have moved code
// it never sent.
func TestDiffLeavesTheHeadAlone(t *testing.T) {
	base := folded(mkStr(t, 1, "title", "t", "1111111111111111"))
	local := folded(
		mkStr(t, 1, "title", "t", "1111111111111111"),
		mkStr(t, 2, "head.sha", "3ac8e05f", "2222222222222222"),
		mkStr(t, 3, "head", "contributor/x", "3333333333333333"),
	)
	d, _ := Diff(entID, base, local, Upstream{State: base}, maps{})
	for _, field := range []string{"head", "head.sha"} {
		if _, ok := d.Set[field]; ok {
			t.Errorf("%s must never be pushed", field)
		}
	}
}

// A verdict casts once. Two conditions stop a repeat, and they exclude different
// things: an imported verdict is somebody else's position, and one this clone
// already submitted came back as a different event.
func TestDiffVerdicts(t *testing.T) {
	cast := mkEvent(t, 2, VerdictAdd, entity.Str(VerdictApprove), "3ac8e05f", "2222222222222222")
	local := folded(mkStr(t, 1, "title", "t", "1111111111111111"), cast)
	base := folded(mkStr(t, 1, "title", "t", "1111111111111111"))

	// Nothing upstream knows about it: send it.
	d, _ := Diff(entID, base, local, Upstream{State: base}, maps{})
	if len(d.VerdictsCast) != 1 || d.VerdictsCast[0].Verdict.Value != VerdictApprove {
		t.Fatalf("a new verdict should be cast: %+v", d.VerdictsCast)
	}

	// The same event is in the imported state, so the tracker authored it.
	d, _ = Diff(entID, base, local, Upstream{State: local}, maps{})
	if len(d.VerdictsCast) != 0 {
		t.Errorf("an imported verdict must not be re-cast: %+v", d.VerdictsCast)
	}

	// The ledger says this clone already submitted it.
	sent := maps{verdicts: map[string]string{entID + " " + cast.ID: "PRR_1"}}
	d, _ = Diff(entID, base, local, Upstream{State: base}, sent)
	if len(d.VerdictsCast) != 0 {
		t.Errorf("a verdict already submitted must not be cast twice: %+v", d.VerdictsCast)
	}
}

// A verdict's message goes up as the review's body rather than as a comment
// beside it — which is exactly what an import of the result reads back.
func TestDiffPairsAVerdictWithItsMessage(t *testing.T) {
	message := mkStr(t, 2, "comment", "Looks right.", "2222222222222222")
	cast := mkEvent(t, 3, VerdictAdd, entity.Str(VerdictApprove), "3ac8e05f", "3333333333333333")
	aside := mkStr(t, 4, "comment", "Unrelated remark.", "4444444444444444")

	base := folded(mkStr(t, 1, "title", "t", "1111111111111111"))
	local := folded(mkStr(t, 1, "title", "t", "1111111111111111"), message, cast, aside)

	d, _ := Diff(entID, base, local, Upstream{State: base}, maps{})
	if len(d.VerdictsCast) != 1 || d.VerdictsCast[0].Body != "Looks right." {
		t.Fatalf("the verdict did not claim its message: %+v", d.VerdictsCast)
	}
	if len(d.CommentsNew) != 1 || d.CommentsNew[0].Entry.Body.Display() != "Unrelated remark." {
		t.Fatalf("the message was posted twice, or the aside was swallowed: %+v", d.CommentsNew)
	}
}

// A comment's upstream shape decides which mutation edits it, and a bridge
// cannot tell from the id alone — so the delta says which.
func TestDiffNamesEachCommentShape(t *testing.T) {
	plain := mkStr(t, 2, "comment", "plain", "2222222222222222")
	root := mkStr(t, 3, "comment", "anchored", "3333333333333333")
	anchor := Anchor{Revision: "3ac8e05f", Path: "a.go", First: 1, Last: 1}
	reply := mkEvent(t, 5, "comment", entity.Str("reply"), root.ID, "5555555555555555")
	message := mkStr(t, 6, "comment", "verdict body", "6666666666666666")
	cast := mkEvent(t, 7, VerdictAdd, entity.Str(VerdictComment), "3ac8e05f", "7777777777777777")

	base := folded(mkStr(t, 1, "title", "t", "1111111111111111"))
	local := folded(
		mkStr(t, 1, "title", "t", "1111111111111111"),
		plain, root,
		mkEvent(t, 4, AnchorOp, entity.Str(anchor.String()), root.ID, "4444444444444444"),
		reply, message, cast,
	)

	// Every entry mapped, so each one is a candidate for an edit rather than a
	// post — which is the path where the shape decides the mutation.
	sent := maps{
		comments: map[string]string{
			entID + " " + plain.ID:   "IC_1",
			entID + " " + root.ID:    "PRRC_1",
			entID + " " + reply.ID:   "PRRC_2",
			entID + " " + message.ID: "PRR_1",
		},
		threads:  map[string]string{entID + " " + root.ID: "PRRT_1"},
		verdicts: map[string]string{entID + " " + cast.ID: "PRR_1"},
	}
	up := Upstream{State: base, Comments: map[string]string{
		"IC_1": "old", "PRRC_1": "old", "PRRC_2": "old", "PRR_1": "old",
	}}

	d, _ := Diff(entID, base, local, up, sent)
	want := map[string]string{
		plain.ID:   CommentPlain,
		root.ID:    CommentAnchored,
		reply.ID:   CommentAnchored,
		message.ID: CommentVerdict,
	}
	if len(d.CommentsEdited) != len(want) {
		t.Fatalf("edited %d entries, want %d: %+v", len(d.CommentsEdited), len(want), d.CommentsEdited)
	}
	for _, c := range d.CommentsEdited {
		if got := c.Kind; got != want[c.Entry.ID()] {
			t.Errorf("%s is kind %q, want %q", c.Entry.Body.Display(), got, want[c.Entry.ID()])
		}
	}
}

// Resolution is a scalar comparison over the thread's root: a local resolve
// pushes, and a resolve the tracker made and this clone imported does not push
// back.
func TestDiffThreadResolution(t *testing.T) {
	root := mkStr(t, 2, "comment", "why?", "2222222222222222")
	anchor := Anchor{Revision: "3ac8e05f", Path: "a.go", First: 1, Last: 1}
	title := mkStr(t, 1, "title", "t", "1111111111111111")
	anchored := mkEvent(t, 3, AnchorOp, entity.Str(anchor.String()), root.ID, "3333333333333333")

	base := folded(title, root, anchored)
	local := folded(title, root, anchored,
		mkEvent(t, 4, ResolveOp, entity.Bool(true), root.ID, "4444444444444444"))
	sent := maps{
		comments: map[string]string{entID + " " + root.ID: "PRRC_1"},
		threads:  map[string]string{entID + " " + root.ID: "PRRT_1"},
	}

	// Local resolved it, the tracker has not heard.
	d, _ := Diff(entID, base, local, Upstream{
		State:    base,
		Comments: map[string]string{"PRRC_1": "why?"},
		Threads:  map[string]bool{"PRRT_1": false},
	}, sent)
	if len(d.ThreadsResolved) != 1 || d.ThreadsResolved[0].Upstream != "PRRT_1" {
		t.Fatalf("the resolution should be sent: %+v", d.ThreadsResolved)
	}

	// Both sides say resolved: nothing to do, however it got that way.
	d, _ = Diff(entID, local, local, Upstream{
		State:    local,
		Comments: map[string]string{"PRRC_1": "why?"},
		Threads:  map[string]bool{"PRRT_1": true},
	}, sent)
	if len(d.ThreadsResolved) != 0 {
		t.Errorf("an agreed resolution is not work: %+v", d.ThreadsResolved)
	}
}

// The OR-Set asymmetry, over labels: anything local the tracker does not have is
// sent, while only a member both sides once had and this one has since dropped
// is retracted. A member the tracker gained since is a change this clone never
// saw, and deleting it would be an overwrite.
func TestDiffLists(t *testing.T) {
	title := mkStr(t, 1, "title", "t", "1111111111111111")
	bug := mkStr(t, 2, "label.add", "bug", "2222222222222222")
	urgent := mkStr(t, 3, "label.add", "urgent", "3333333333333333")

	base := folded(title, bug)
	local := folded(title, bug, urgent)
	// Upstream has bug, and has gained "area" this clone has not seen.
	up := folded(title, bug, mkStr(t, 4, "label.add", "area", "4444444444444444"))

	d, _ := Diff(entID, base, local, Upstream{State: up}, maps{})
	if len(d.LabelsAdded) != 1 || d.LabelsAdded[0] != "urgent" {
		t.Errorf("added = %v, want [urgent]", d.LabelsAdded)
	}
	if len(d.LabelsRemoved) != 0 {
		t.Errorf("removed = %v; a label the tracker gained is not ours to drop", d.LabelsRemoved)
	}
}

// Common is the events both sides hold, compared as whole canonical lines —
// which is what event identity means.
func TestCommon(t *testing.T) {
	shared := mkStr(t, 1, "title", "t", "1111111111111111")
	mine := mkStr(t, 2, "description", "local only", "2222222222222222")
	theirs := mkStr(t, 2, "description", "tracker only", "3333333333333333")

	base := Common(Vocabulary, []entity.Event{shared, mine}, []entity.Event{shared, theirs})
	if base.Scalar("title").Display() != "t" {
		t.Error("a shared event belongs in the base")
	}
	if base.Scalar("description").Display() != "" {
		t.Errorf("an event only one side holds is not agreed: %q", base.Scalar("description").Display())
	}
}
