// The push side of the issue vocabulary: what one issue's local copy says that
// an upstream tracker does not, and how that is worked out.
//
// No platform appears in this file. A bridge hands in three folded states and
// gets back a delta named entirely in the terms docs/issues.md defines, which is
// what lets a second bridge reuse the whole comparison.

package issue

import (
	"slices"
	"strconv"

	"github.com/hdweiss/git-issue/internal/entity"
)

// Pushable are the scalar fields a push considers, in the order a summary names
// them. A field absent here is never pushed, whatever it says locally.
var Pushable = []string{"title", "description", "type", "milestone", "status", "status.reason"}

// PushableLists are the list fields a push considers.
var PushableLists = []string{"label", "assignee"}

// Upstream is one tracker's current state of an entity, as a bridge reports it.
//
// State is folded from a faithful re-import — see "Pushing to an external
// tracker" in docs/storage-model.md. Comments is the same tracker's comment
// bodies keyed by its own ids, which the fold cannot supply: an imported entry's
// event carries a nonce derived from the upstream id, and a nonce is one-way.
type Upstream struct {
	State    entity.State
	Comments map[string]string
}

// Mapped answers, for one tracker, which upstream object a local thread entry
// was posted as. The origin ledger satisfies it.
type Mapped interface {
	Comment(entity, event string) (string, bool)
}

// CommentPush is one thread entry a push has something to do about, with the
// upstream object it corresponds to — empty when there is not one yet.
type CommentPush struct {
	Entry    entity.Comment
	Upstream string
}

// Delta is what one issue's local copy says that one upstream tracker does not.
type Delta struct {
	ID string
	// Create reports that this tracker has no counterpart at all, so everything
	// below describes an issue to be filed rather than changes to one.
	Create bool

	// Set holds each scalar to write, already resolved. A cleared field carries
	// a null value rather than an empty string, which is how docs/issues.md
	// spells "no milestone".
	Set map[string]entity.Value

	LabelsAdded, LabelsRemoved       []string
	AssigneesAdded, AssigneesRemoved []string

	// RelationsAdded and RelationsRemoved are links whose target is still an
	// entity id. Translating one into the object a tracker knows it as is the
	// origin ledger's job and the bridge's to ask for, because the answer is
	// per tracker: the same issue is a different object in an upstream and in
	// a fork.
	RelationsAdded, RelationsRemoved []Relation

	CommentsNew     []CommentPush
	CommentsEdited  []CommentPush
	CommentsRemoved []CommentPush
}

// Conflict is one scalar both sides moved since they last agreed.
type Conflict struct {
	ID, Field       string
	Local, Upstream string
}

// Diff works out what to push, three ways.
//
// base is the state both sides last agreed came from the tracker, reconstructed
// by folding the events the local blob and a fresh import share. The three kinds
// resolve differently and deliberately:
//
//   - A scalar is a three-way comparison, and the only kind that can conflict.
//   - A list is an OR-Set difference. Concurrent add and remove is add-wins by
//     definition, so there is nothing here for a conflict to mean.
//   - A thread is neither: an entry is upstream exactly when the ledger says
//     which object it was posted as, because two entries with the same text are
//     legitimately two comments.
//
// Everything compared is a folded *value*. That is what makes a mirror
// terminate: a change pushed to one tracker comes back from it as a second event
// carrying the same value, and comparing values makes that a no-op where
// comparing event sets would find work to do forever.
func Diff(id string, base, local entity.State, up Upstream, m Mapped) (Delta, []Conflict) {
	d := Delta{ID: id, Set: map[string]entity.Value{}}
	var conflicts []Conflict

	for _, field := range Pushable {
		b, l, u := scalar(base, field), scalar(local, field), scalar(up.State, field)
		switch {
		case l == u:
			// Already agrees, however it got that way.
		case l == b:
			// Only the tracker moved. That is a pull's business, not a push's.
		case u != b:
			conflicts = append(conflicts, Conflict{ID: id, Field: field, Local: l, Upstream: u})
		default:
			d.Set[field] = local.Scalar(field)
		}
	}

	for _, field := range PushableLists {
		haveLocal := local.List(field)
		haveUp := up.State.List(field)
		haveBase := base.List(field)

		var added, removed []string
		for _, v := range haveLocal {
			if !slices.Contains(haveUp, v) {
				added = append(added, v)
			}
		}
		// Only a member both sides once had and this one has since dropped is a
		// removal. A member the tracker has added since is simply new there, and
		// deleting it would be this clone overwriting a change it never saw.
		for _, v := range haveUp {
			if slices.Contains(haveBase, v) && !slices.Contains(haveLocal, v) {
				removed = append(removed, v)
			}
		}
		if field == "label" {
			d.LabelsAdded, d.LabelsRemoved = added, removed
		} else {
			d.AssigneesAdded, d.AssigneesRemoved = added, removed
		}
	}

	d.RelationsAdded, d.RelationsRemoved = diffRelations(base, local, up.State)

	for _, entry := range local.Thread {
		upstreamID, mapped := m.Comment(id, entry.ID())
		switch {
		case !mapped:
			// Never posted. A retracted entry that was never posted has nothing
			// to retract, so it is simply not sent.
			if !entry.Retracted {
				d.CommentsNew = append(d.CommentsNew, CommentPush{Entry: entry})
			}
		case entry.Retracted:
			d.CommentsRemoved = append(d.CommentsRemoved, CommentPush{Entry: entry, Upstream: upstreamID})
		default:
			// An absent upstream body is not an empty one. The ledger says this
			// entry was posted and the tracker no longer reports it, which means
			// somebody deleted it there — so there is nothing to edit, and
			// re-posting would resurrect what they removed.
			body, present := up.Comments[upstreamID]
			if present && body != entry.Body.Display() {
				d.CommentsEdited = append(d.CommentsEdited, CommentPush{Entry: entry, Upstream: upstreamID})
			}
		}
	}

	return d, conflicts
}

// diffRelations is the OR-Set difference again, over links rather than labels.
//
// A member is the pair (kind, target) — docs/blob-format.md — so that is what
// is compared, and by value rather than by event id: a link pushed to a tracker
// comes back as that tracker's own `rel.add` carrying the same pair, and only a
// comparison on values makes the second push a no-op.
//
// The asymmetry between adds and removes is the same one labels have, and for
// the same reason. Anything local that the tracker does not have is sent; only a
// link both sides once had and this one has since dropped is retracted, because
// a link the tracker gained since is a change this clone never saw.
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

// scalar reads a field for comparison, with the one default docs/issues.md
// defines: an issue carrying no status event is open. Without that, every issue
// that has never been closed would look like a status change waiting to be
// pushed.
func scalar(st entity.State, field string) string {
	v := st.Scalar(field).Display()
	if field == "status" && v == "" {
		return StatusOpen
	}
	return v
}

// Empty reports that there is nothing to send.
func (d Delta) Empty() bool {
	return !d.Create &&
		len(d.Set) == 0 &&
		len(d.LabelsAdded) == 0 && len(d.LabelsRemoved) == 0 &&
		len(d.AssigneesAdded) == 0 && len(d.AssigneesRemoved) == 0 &&
		len(d.RelationsAdded) == 0 && len(d.RelationsRemoved) == 0 &&
		len(d.CommentsNew) == 0 && len(d.CommentsEdited) == 0 && len(d.CommentsRemoved) == 0
}

// Fields names what changed, for a one-line summary. Scalars come first in the
// order Pushable gives, so two issues changing the same things read the same
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
	if len(d.AssigneesAdded) > 0 || len(d.AssigneesRemoved) > 0 {
		out = append(out, "assignees")
	}
	if len(d.RelationsAdded) > 0 || len(d.RelationsRemoved) > 0 {
		out = append(out, "links")
	}
	if n := len(d.CommentsNew); n > 0 {
		out = append(out, countOf(n, "comment"))
	}
	if n := len(d.CommentsEdited); n > 0 {
		out = append(out, countOf(n, "comment edit"))
	}
	if n := len(d.CommentsRemoved); n > 0 {
		out = append(out, countOf(n, "comment removal"))
	}
	return out
}

func countOf(n int, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// Base reconstructs the state both sides last agreed on: the events the local
// blob and a fresh import both hold.
//
// Whole canonical lines, because that is what event identity means — two lines
// differing anywhere are two events. An event present in both is one the tracker
// authored and this clone received, which is exactly the definition of a base.
func Base(vocab entity.Vocabulary, local, imported []entity.Event) entity.State {
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
