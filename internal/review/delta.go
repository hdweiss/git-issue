// The push side of the review vocabulary: what one review's local copy says
// that an upstream tracker does not, and how that is worked out.
//
// No platform appears in this file, exactly as none appears in
// internal/issue/delta.go. A bridge hands in three folded states and gets back a
// delta named entirely in the terms docs/reviews.md defines.
//
// The shape is the issue delta's plus the three things a review has that an
// issue does not: comments that are anchored to a place in the diff and grouped
// into threads, a resolved bit per thread, and verdicts. Each of those is
// compared by the same rule as everything else — folded values, never event
// sets — because that is what makes a mirror terminate.

package review

import (
	"slices"
	"strconv"

	"github.com/hdweiss/git-issue/internal/entity"
)

// Pushable are the scalar fields a push considers, in the order a summary names
// them.
//
// `head` and `head.sha` are deliberately absent. They describe a branch and a
// commit, which a tracker learns from the git remote rather than from an API
// call — a bridge that wrote them would be claiming to have moved code it never
// pushed. `locked` is absent for the same reason internal/issue leaves it out:
// nothing local writes it.
var Pushable = []string{"title", "description", "base", "milestone", "status", "status.reason", "draft"}

// PushableLists are the list fields a push considers. On a review, `assignee`
// is who was asked to read it — see the note in the GitHub bridge, which maps it
// onto review requests rather than onto GitHub's own assignees.
var PushableLists = []string{"label", "assignee"}

// Upstream is one tracker's current state of a review, as a bridge reports it.
//
// State is folded from a faithful re-import, per "Pushing to an external
// tracker" in docs/storage-model.md. The three maps are the parts the fold
// cannot supply, because each is keyed by an upstream id and a derived nonce is
// one-way.
type Upstream struct {
	State entity.State
	// Comments is the tracker's comment bodies keyed by its own ids. It spans
	// every shape of comment a review carries — the conversation, the bodies of
	// submitted reviews, and review-thread entries — because a local thread
	// entry maps onto exactly one of them and the delta does not care which.
	Comments map[string]string
	// Threads is whether each upstream review thread is resolved there.
	Threads map[string]bool
	// Dismissed is whether each upstream review has been dismissed there, so a
	// local dismissal that has already happened is not sent again.
	Dismissed map[string]bool
}

// Mapped answers, for one tracker, which upstream object each part of a local
// review was written as. The origin ledger satisfies it.
//
// Three questions rather than the issue vocabulary's one. A review thread and
// its root entry are different upstream objects — the thread is what gets
// resolved, the entry is what gets edited — and a verdict is a third, because
// the upstream review that carries it is what a dismissal has to name.
type Mapped interface {
	Comment(entity, event string) (string, bool)
	Thread(entity, root string) (string, bool)
	Verdict(entity, event string) (string, bool)
}

// The upstream shapes a local thread entry can correspond to.
//
// One local vocabulary, three upstream objects — and they are not
// interchangeable: the mutation that edits one rejects the ids of the other two.
// A bridge cannot tell them apart from the id alone, so the delta says which,
// derived from what the local blob already records.
const (
	// CommentPlain is the review's own conversation: an entry with no anchor
	// that is nobody's reply.
	CommentPlain = "comment"
	// CommentAnchored is an entry of a thread pinned to the diff — the root that
	// opened it, or a reply inside it.
	CommentAnchored = "anchored"
	// CommentVerdict is the body of a submitted verdict, which upstream is part
	// of the verdict rather than a comment beside it. See VerdictMessage.
	CommentVerdict = "verdict"
)

// CommentPush is one thread entry a push has something to do about.
type CommentPush struct {
	Entry entity.Comment
	// Kind is which of the three upstream shapes this entry is.
	Kind string
	// Upstream is the object this entry was posted as, empty when it has not
	// been posted.
	Upstream string

	// Anchor is where the entry sits in the code, set only on a root that opens
	// an anchored thread. It is what tells a bridge to open a review thread
	// rather than to post a plain comment.
	Anchor   Anchor
	Anchored bool

	// Root is the local thread root this entry replies to, empty for an entry
	// that is itself a root.
	Root string
	// Thread is the upstream review thread Root corresponds to, empty when the
	// root has not been posted yet — in which case a bridge learns the id by
	// creating the thread in the same run, and the delta orders roots first.
	Thread string
}

// ThreadPush is one thread whose resolved bit differs from the tracker's.
type ThreadPush struct {
	// Root is the local thread-root event id, which is what a resolution
	// annotation addresses.
	Root string
	// Upstream is the review thread it corresponds to, empty when this run is
	// creating the thread and will know the id only after it has.
	Upstream string
}

// VerdictPush is one verdict to cast or to dismiss.
type VerdictPush struct {
	Verdict Verdict
	// Body is the comment written in the same action, which upstream is the
	// review's own body rather than a comment beside it. Comment is that
	// comment's local event id, so the caller can record where it was posted.
	Body    string
	Comment string
	// Upstream is the review a dismissal names. Empty on a cast.
	Upstream string
}

// Delta is what one review's local copy says that one upstream tracker does not.
type Delta struct {
	ID string
	// Create reports that this tracker has no counterpart at all.
	Create bool

	// Set holds each scalar to write, already resolved.
	Set map[string]entity.Value

	LabelsAdded, LabelsRemoved       []string
	ReviewersAdded, ReviewersRemoved []string

	// RelationsAdded and RelationsRemoved are links whose target is still an
	// entity id, exactly as in the issue delta: translating one into an upstream
	// object is the ledger's job and the bridge's to ask for.
	RelationsAdded, RelationsRemoved []Relation

	CommentsNew     []CommentPush
	CommentsEdited  []CommentPush
	CommentsRemoved []CommentPush

	ThreadsResolved   []ThreadPush
	ThreadsUnresolved []ThreadPush

	VerdictsCast      []VerdictPush
	VerdictsDismissed []VerdictPush
}

// Conflict is one scalar both sides moved since they last agreed.
type Conflict struct {
	ID, Field       string
	Local, Upstream string
}

// Diff works out what to push, three ways.
//
// base is the state both sides last agreed came from the tracker, reconstructed
// by folding the events the local blob and a fresh import share — Base, below.
// Everything compared is a folded *value*, so a change pushed to a tracker and
// then imported back from it is a no-op on the next run rather than work to do
// forever.
func Diff(id string, base, local entity.State, up Upstream, m Mapped) (Delta, []Conflict) {
	d := Delta{ID: id, Set: map[string]entity.Value{}}
	var conflicts []Conflict

	for _, field := range Pushable {
		b, l, u := scalar(base, field), scalar(local, field), scalar(up.State, field)
		switch {
		case l == u:
			// Already agrees, however it got that way.
		case l == b:
			// Only the tracker moved. That is a pull's business.
		case u != b:
			conflicts = append(conflicts, Conflict{ID: id, Field: field, Local: l, Upstream: u})
		default:
			d.Set[field] = local.Scalar(field)
		}
	}

	for _, field := range PushableLists {
		added, removed := diffList(base, local, up.State, field)
		if field == "label" {
			d.LabelsAdded, d.LabelsRemoved = added, removed
		} else {
			d.ReviewersAdded, d.ReviewersRemoved = added, removed
		}
	}

	d.RelationsAdded, d.RelationsRemoved = diffRelations(base, local, up.State)

	// Verdicts before comments: a verdict claims the comment written alongside
	// it, and a comment claimed twice is a body posted twice.
	messages := VerdictMessages(local)
	claimed := diffVerdicts(&d, id, local, up, m, messages)
	diffComments(&d, id, local, up, m, claimed, messages)
	diffThreads(&d, id, base, local, up, m)

	return d, conflicts
}

// diffList is the OR-Set difference, with the asymmetry docs/issues.md
// describes: anything local the tracker does not have is sent, while only a
// member both sides once had and this one has since dropped is retracted.
func diffList(base, local, up entity.State, field string) (added, removed []string) {
	haveLocal, haveUp, haveBase := local.List(field), up.List(field), base.List(field)
	for _, v := range haveLocal {
		if !slices.Contains(haveUp, v) {
			added = append(added, v)
		}
	}
	for _, v := range haveUp {
		if slices.Contains(haveBase, v) && !slices.Contains(haveLocal, v) {
			removed = append(removed, v)
		}
	}
	return added, removed
}

// diffRelations is diffList again, over links rather than names, keyed on the
// pair (kind, target) the way the fold keys them.
func diffRelations(base, local, up entity.State) (added, removed []Relation) {
	haveUp, haveBase, haveLocal := relationKeys(up), relationKeys(base), relationKeys(local)
	for _, r := range Relations(local) {
		if !haveUp[r.Key()] {
			added = append(added, r)
		}
	}
	for _, r := range Relations(up) {
		if haveBase[r.Key()] && !haveLocal[r.Key()] {
			removed = append(removed, r)
		}
	}
	return added, removed
}

func relationKeys(st entity.State) map[string]bool {
	out := map[string]bool{}
	for _, r := range Relations(st) {
		out[r.Key()] = true
	}
	return out
}

// diffVerdicts works out which positions to cast and which to withdraw, and
// returns the comment events the verdicts consumed.
//
// Two conditions have to hold before a verdict is sent, and they exclude
// different things. An event the import produced is already upstream by
// definition — that is what "the tracker said this" means — so it is recognised
// by being present in the imported state, which needs no ledger. A verdict this
// clone has already submitted is recognised by the ledger, because the import
// of it comes back as a *different* event: a different author, a different
// nonce, and therefore a different id.
func diffVerdicts(d *Delta, id string, local entity.State, up Upstream, m Mapped, messages map[string]string) map[string]bool {
	claimed := map[string]bool{}
	message := func(verdict string) (body, comment string) {
		for c, v := range messages {
			if v == verdict {
				entry, ok := commentByID(local, c)
				if !ok {
					continue
				}
				claimed[c] = true
				return entry.Body.Display(), c
			}
		}
		return "", ""
	}

	for _, v := range VerdictsOf(local) {
		if _, upstream := eventByID(up.State, v.ID); upstream {
			continue
		}
		if _, sent := m.Verdict(id, v.ID); sent {
			continue
		}
		push := VerdictPush{Verdict: v}
		push.Body, push.Comment = message(v.ID)
		d.VerdictsCast = append(d.VerdictsCast, push)
	}

	// A dismissal names the add it undoes, so it is read from the events rather
	// than from folded state: the member it retracted is gone from the fold,
	// which is the whole point of it.
	for _, ev := range local.Events {
		if ev.Op != VerdictRemove || ev.Ref == "" {
			continue
		}
		if _, upstream := eventByID(up.State, ev.ID); upstream {
			continue
		}
		review, sent := m.Verdict(id, ev.Ref)
		if !sent || up.Dismissed[review] {
			continue
		}
		push := VerdictPush{Upstream: review}
		push.Body, push.Comment = message(ev.ID)
		d.VerdictsDismissed = append(d.VerdictsDismissed, push)
	}

	return claimed
}

// diffComments is the issue vocabulary's thread comparison, with each entry
// carrying enough for a bridge to tell the three shapes apart: a plain comment,
// a root that opens an anchored thread, and a reply inside one.
//
// An entry is upstream exactly when the ledger says which object it was posted
// as. Two entries with the same text are legitimately two comments, so nothing
// here compares bodies to decide that.
func diffComments(d *Delta, id string, local entity.State, up Upstream, m Mapped, claimed map[string]bool, messages map[string]string) {
	for _, entry := range local.Thread {
		if claimed[entry.ID()] {
			continue
		}
		push := CommentPush{Entry: entry, Kind: CommentPlain, Root: entry.Parent()}
		switch {
		case messages[entry.ID()] != "":
			push.Kind = CommentVerdict
		case push.Root != "":
			push.Kind = CommentAnchored
			push.Thread, _ = m.Thread(id, push.Root)
		default:
			if push.Anchor, push.Anchored = AnchorOf(local, entry.ID()); push.Anchored {
				push.Kind = CommentAnchored
			}
		}

		upstreamID, mapped := m.Comment(id, entry.ID())
		push.Upstream = upstreamID
		switch {
		case !mapped:
			if entry.Retracted {
				continue
			}
			d.CommentsNew = append(d.CommentsNew, push)
		case entry.Retracted:
			d.CommentsRemoved = append(d.CommentsRemoved, push)
		default:
			// An absent upstream body is not an empty one: the ledger says this
			// entry was posted and the tracker no longer reports it, so somebody
			// deleted it there and re-posting would resurrect what they removed.
			body, present := up.Comments[upstreamID]
			if present && body != entry.Body.Display() {
				d.CommentsEdited = append(d.CommentsEdited, push)
			}
		}
	}
}

// diffThreads compares the resolved bit, three ways like a scalar.
//
// A thread this run is creating has no upstream id yet and is still listed: the
// bridge learns the id by creating it, and a comment posted resolved is a
// perfectly ordinary thing an agent does. A thread the local side never
// resolved and the tracker did is left alone, because that is a change this
// clone has not seen rather than one it made.
func diffThreads(d *Delta, id string, base, local entity.State, up Upstream, m Mapped) {
	for _, t := range Threads(local, nil) {
		root := t.Root.ID()
		upstreamID, mapped := m.Thread(id, root)
		if mapped {
			if up.Threads[upstreamID] == t.Resolved {
				continue
			}
			if Resolved(base, root) == t.Resolved {
				continue
			}
		} else if !t.Resolved {
			// Nothing upstream and nothing to say: an unresolved new thread is
			// created unresolved.
			continue
		}
		push := ThreadPush{Root: root, Upstream: upstreamID}
		if t.Resolved {
			d.ThreadsResolved = append(d.ThreadsResolved, push)
		} else {
			d.ThreadsUnresolved = append(d.ThreadsUnresolved, push)
		}
	}
}

// VerdictMessages maps each comment that was written as part of casting or
// withdrawing a verdict onto that verdict's event id.
//
// SetVerdict and Dismiss both write the message first and the verdict second, in
// one action: same author, same timestamp, and the clock immediately before. That
// exact triple is the pairing, and nothing looser — an import writes its verdict
// and its body at the same clock, because `c` is the upstream timestamp there,
// and pairing on equality would let any comment made in the same second by the
// same person be swallowed into a verdict.
//
// Recognising the pair is what keeps a push from posting `approve -m "ship it"`
// as a review body *and* a comment beside it, and what makes a later edit of
// that comment reach updatePullRequestReview rather than a mutation that rejects
// the id. A verdict written with no message, and one whose message was said
// separately, appear here not at all; those comments are ordinary comments,
// which is what they are.
func VerdictMessages(st entity.State) map[string]string {
	out := map[string]string{}
	for _, ev := range st.Events {
		if ev.Op != VerdictAdd && ev.Op != VerdictRemove {
			continue
		}
		for _, c := range st.Thread {
			if c.Parent() != "" || out[c.ID()] != "" {
				continue
			}
			if c.Event.A == ev.A && c.Event.TS == ev.TS && c.Event.C == ev.C-1 {
				out[c.ID()] = ev.ID
				break
			}
		}
	}
	return out
}

// commentByID finds one thread entry by its event id.
func commentByID(st entity.State, id string) (entity.Comment, bool) {
	for _, c := range st.Thread {
		if c.ID() == id {
			return c, true
		}
	}
	return entity.Comment{}, false
}

// scalar reads a field for comparison, with the two defaults docs/reviews.md
// defines: a review carrying no status event is open, and one carrying no draft
// event is not a draft. Without them every review that has never been closed,
// and every one somebody explicitly marked ready, would look like a change
// waiting to be pushed.
func scalar(st entity.State, field string) string {
	v := st.Scalar(field).Display()
	switch {
	case field == "status" && v == "":
		return StatusOpen
	case field == "draft" && v == "":
		return "false"
	}
	return v
}

// Empty reports that there is nothing to send.
func (d Delta) Empty() bool {
	return !d.Create &&
		len(d.Set) == 0 &&
		len(d.LabelsAdded) == 0 && len(d.LabelsRemoved) == 0 &&
		len(d.ReviewersAdded) == 0 && len(d.ReviewersRemoved) == 0 &&
		len(d.RelationsAdded) == 0 && len(d.RelationsRemoved) == 0 &&
		len(d.CommentsNew) == 0 && len(d.CommentsEdited) == 0 && len(d.CommentsRemoved) == 0 &&
		len(d.ThreadsResolved) == 0 && len(d.ThreadsUnresolved) == 0 &&
		len(d.VerdictsCast) == 0 && len(d.VerdictsDismissed) == 0
}

// Fields names what changed, for a one-line summary. Scalars first, in the
// order Pushable gives, so two reviews changing the same things read the same
// way.
func (d Delta) Fields() []string {
	var out []string
	for _, field := range Pushable {
		if _, ok := d.Set[field]; ok {
			out = append(out, field)
		}
	}
	if len(d.LabelsAdded) > 0 || len(d.LabelsRemoved) > 0 {
		out = append(out, "labels")
	}
	if len(d.ReviewersAdded) > 0 || len(d.ReviewersRemoved) > 0 {
		out = append(out, "reviewers")
	}
	if len(d.RelationsAdded) > 0 || len(d.RelationsRemoved) > 0 {
		out = append(out, "links")
	}
	for _, c := range []struct {
		n    int
		noun string
	}{
		{len(d.CommentsNew), "comment"},
		{len(d.CommentsEdited), "comment edit"},
		{len(d.CommentsRemoved), "comment removal"},
		{len(d.ThreadsResolved), "resolution"},
		{len(d.ThreadsUnresolved), "reopened thread"},
		{len(d.VerdictsCast), "verdict"},
		{len(d.VerdictsDismissed), "dismissal"},
	} {
		if c.n > 0 {
			out = append(out, countOf(c.n, c.noun))
		}
	}
	return out
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// Common reconstructs the state both sides last agreed on: the events the local
// blob and a fresh import both hold. It is internal/issue's Base under another
// name, because on a review `Base` is already the branch being merged into.
//
// Whole canonical lines, because that is what event identity means. An event
// present in both is one the tracker authored and this clone received, which is
// exactly the definition of a base.
func Common(vocab entity.Vocabulary, local, imported []entity.Event) entity.State {
	have := make(map[string]bool, len(imported))
	for _, e := range imported {
		have[string(e.Raw)] = true
	}
	var shared []entity.Event
	for _, e := range local {
		if have[string(e.Raw)] {
			shared = append(shared, e)
		}
	}
	return entity.Fold(vocab, shared)
}
