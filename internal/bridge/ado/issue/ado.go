// Package ado maps Azure DevOps work items onto the tracker's event
// vocabulary, per docs/bridge-ado.md.
//
// It is the only package that knows both what an issue is and what Azure
// DevOps is. internal/bridge/ado/api below it knows Azure DevOps and nothing else;
// internal/issue below that knows issues and nothing else. Keeping the seam
// here is what lets the mapping be tested end to end from recorded API
// responses, with no network and no repository.
//
// Everything here is a pure function of what Azure DevOps returned. That is a
// correctness requirement, not a style: an import that depended on local state
// would produce different bytes on a second run, and since an event's id is
// the hash of its own bytes, a re-import would duplicate the entity instead of
// converging on it.
package adoissue

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/bridge"
	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/issue"
)

// Entity is one imported work item: the id its create event hashes to, every
// action the import produced for it, oldest first, and the mappings it
// establishes.
//
// The mappings do not go into the blob. Which work item an issue corresponds
// to is not a fact about the issue — see "The origin ledger" in
// docs/storage-model.md — so it is reported here for the caller to record on
// the ledger ref.
type Entity struct {
	ID      string
	Actions []entity.Action

	// Origin is the work item's identity and URL its web address: an item
	// moved between projects keeps the first and gets a new second.
	Origin string
	URL    string

	Comments []bridge.CommentOrigin

	// Unresolved is how many relation events this import could not write
	// because the ledger does not hold the work item they name. It is what
	// makes importing a batch twice worth the second pass and no more: an
	// entity with none has nothing to gain from being read again.
	Unresolved int
}

// Events is every event the import produced, in the order the actions carry
// them.
func (e Entity) Events() []entity.Event {
	var out []entity.Event
	for _, a := range e.Actions {
		out = append(out, a.Events...)
	}
	return out
}

// Blob is the entity's events as a note body: one canonical line each.
func (e Entity) Blob() []byte {
	var b strings.Builder
	for _, ev := range e.Events() {
		b.Write(ev.Raw)
		b.WriteByte('\n')
	}
	return []byte(b.String())
}

// Origin is the identity string for a work item: the nonce input for its
// create event, and the upstream id recorded on the origin ledger. The two are
// the same string on purpose, so the ledger and the hash cannot drift apart.
//
// It is keyed on the collection rather than the project because a work item id
// is unique per collection and survives being moved between projects. See
// "Identity, and where it is recorded" in docs/bridge-ado.md.
func Origin(t adoapi.Target, id int) string {
	return t.WorkItemOrigin(id)
}

// CommentOrigin is the identity string for one comment on a work item.
func CommentOrigin(t adoapi.Target, workItem, comment int) string {
	return Origin(t, workItem) + "/comments/" + strconv.Itoa(comment)
}

// Import maps one work item onto actions, each a group of events that one
// person did at one moment: the item being filed, a comment being posted, a
// revision being saved.
//
// known names what this repository already holds, so that a comment it posted
// does not come back as a second one and a parent link can name a real entity.
// Passing nil claims nothing and links nothing.
func Import(format entity.ObjectFormat, t adoapi.Target, w adoapi.WorkItem, known bridge.Lookup) (Entity, error) {
	b := &builder{
		format:    format,
		target:    t,
		item:      w,
		known:     known,
		origin:    Origin(t, w.ID),
		removed:   map[string]string{},
		labels:    newMembers(),
		assignees: newMembers(),
		rels:      newMembers(),
	}
	b.build()
	if b.err != nil {
		return Entity{}, b.err
	}
	return Entity{
		ID:         b.id,
		Actions:    b.actions(),
		Origin:     b.origin,
		URL:        t.WebURL(w.ID),
		Comments:   b.comments,
		Unresolved: b.unresolved,
	}, nil
}

// group is one upstream action's events, with who did it and when.
type group struct {
	at     time.Time
	who    adoapi.Identity
	seq    int
	events []entity.Event
}

type builder struct {
	format entity.ObjectFormat
	target adoapi.Target
	item   adoapi.WorkItem
	known  bridge.Lookup
	origin string

	groups []*group
	cur    *group
	create *group
	id     string
	err    error

	removed    map[string]string
	comments   []bridge.CommentOrigin
	unresolved int

	labels    *members
	assignees *members
	rels      *members
}

// begin starts a new action. Everything emitted until the next begin belongs
// to it.
func (b *builder) begin(at time.Time, who adoapi.Identity) *group {
	g := &group{at: at, who: who, seq: len(b.groups)}
	b.groups = append(b.groups, g)
	b.cur = g
	return g
}

func (b *builder) build() {
	created, author := b.item.CreatedAt, Author(b.item.CreatedBy)

	// Filing the work item is one action, however many events it takes to
	// record what it was filed with.
	b.create = b.begin(created, b.item.CreatedBy)

	// The create event first, and its id is the entity's. Its nonce is SHA-256
	// of the identity string exactly as docs/bridge-ado.md publishes it — this
	// one derivation fixes the identity of every work item ever imported by
	// any implementation, so it does not get a variation.
	create := b.emit(created, author, "create", entity.Str(issue.Type), "", b.origin)
	b.id = create.ID

	// No ado.* events at all. The work item id and the URL go on the origin
	// ledger, and the area path goes nowhere: it is a fact about the work item
	// in Azure DevOps rather than about this entity, and it is scope, not
	// state. See "The area is scope, and only scope" in docs/bridge-ado.md.

	b.initialFields(created, author)
	b.revisions()
	b.commentThread()
	b.reconcile(created, author)
}

// initialFields records what the work item was filed with.
//
// The values come from the creation revision where there is one, so that a
// field changed later still shows what it started as. Their nonces derive from
// the work item and the operation rather than from the revision, which keeps
// them stable even when the revision feed is not available at all.
func (b *builder) initialFields(created time.Time, author string) {
	if t := b.initial(adoapi.FieldType, b.item.Type); t != "" {
		b.emitField(created, author, "type", entity.Str(strings.ToLower(t)))
	}
	if title := b.initial(adoapi.FieldTitle, b.item.Title); title != "" {
		b.emitField(created, author, "title", entity.Str(title))
	}
	if body := b.initial(b.bodyField(), b.item.Description); body != "" {
		b.emitField(created, author, "description", entity.Str(body))
	}
	// Unlike GitHub's bridge, an initial status is written. Azure DevOps has
	// no implied opening state — "New", "To Do" and "Proposed" are all real
	// values a process defines — so leaving it out would lose what the work
	// item was actually filed as. Nothing ties here either: every later state
	// change carries its own revision's timestamp.
	if state := b.initial(adoapi.FieldState, b.item.State); state != "" {
		b.emitField(created, author, "status", entity.Str(state))
	}
	// The project root is not an iteration, so a work item that was never
	// scheduled has no milestone rather than an empty one.
	iteration := b.item.Iteration
	if raw := b.initial(adoapi.FieldIteration, ""); raw != "" {
		iteration = relative(raw, b.target.Project)
	}
	if iteration != "" {
		b.emitField(created, author, "milestone", entity.Str(iteration))
	}

	for _, tag := range adoapi.SplitTags(b.initial(adoapi.FieldTags, adoapi.JoinTags(b.item.Tags))) {
		e := b.emit(created, author, "label.add", entity.Str(tag), "", b.origin+":label.add:"+tag)
		b.labels.add(tag, e.ID)
	}
	if who := assignee(b.initial(adoapi.FieldAssignedTo, b.item.AssignedTo.UniqueName)); who != "" {
		e := b.emit(created, author, "assignee.add", entity.Str(who), "", b.origin+":assignee.add:"+who)
		b.assignees.add(who, e.ID)
	}
}

// initial is the value a field was created with.
//
// The creation revision names it outright. Failing that, the earliest revision
// that changed the field carries what it was before — the same trick GitHub's
// bridge uses on renames, and it generalises to every field here because Azure
// DevOps reports an old value for all of them. Only when neither exists does
// this fall back to the current value, which is the best available answer when
// there is no history to read.
func (b *builder) initial(field, current string) string {
	revisions := b.sortedUpdates()
	for _, u := range revisions {
		if change, ok := u.Fields[field]; ok {
			if u.Rev <= 1 {
				return change.New
			}
			return change.Old
		}
	}
	return current
}

// bodyField is the field holding this work item's body, which is not the same
// field for every type.
func (b *builder) bodyField() string { return adoapi.BodyField(b.item.Type) }

// revisions replays the update feed.
//
// This is where the import stops approximating. Every revision has an actor, a
// timestamp and old and new values for each field it touched, so a tag removal
// can name the specific addition it retracts and the OR-Set is populated
// properly rather than reconstructed from a final state.
func (b *builder) revisions() {
	for _, u := range b.sortedUpdates() {
		if u.Rev <= 1 {
			// The creation revision is the create action, already emitted.
			continue
		}
		at, author := u.At, Author(u.By)
		if at.IsZero() {
			continue
		}
		// One revision is one action, whatever it takes to record it: a state
		// change and a reassignment saved together are one thing that
		// happened, not two.
		b.begin(at, u.By)

		for _, field := range sortedFields(u.Fields) {
			change := u.Fields[field]
			switch field {
			case adoapi.FieldTitle:
				b.emitRevScalar(at, author, u.Rev, "title", entity.Str(change.New))
			case b.bodyField():
				b.emitRevScalar(at, author, u.Rev, "description", entity.Str(change.New))
			case adoapi.FieldState:
				b.emitRevScalar(at, author, u.Rev, "status", entity.Str(change.New))
			case adoapi.FieldType:
				b.emitRevScalar(at, author, u.Rev, "type", entity.Str(strings.ToLower(change.New)))
			case adoapi.FieldIteration:
				b.iteration(at, author, u.Rev, change.New)
			case adoapi.FieldTags:
				b.tags(at, author, u.Rev, change)
			case adoapi.FieldAssignedTo:
				b.assignee(at, author, u.Rev, change)
			}
			// System.AreaPath is deliberately absent: the area is scope, not
			// state. So is System.History — it *is* the comment text, and the
			// comments API is the authoritative view of it, so replaying both
			// would file every comment twice.
		}

		b.links(at, author, u)
	}
}

func (b *builder) iteration(at time.Time, author string, rev int, value string) {
	path := relative(value, b.target.Project)
	if path == "" {
		// The project root is no iteration at all, which docs/issues.md spells
		// as a null milestone rather than an empty name.
		b.emitRevScalar(at, author, rev, "milestone", entity.Null())
		return
	}
	b.emitRevScalar(at, author, rev, "milestone", entity.Str(path))
}

// tags turns one revision's whole-string change into the additions and
// removals it stands for.
//
// Azure DevOps stores tags as a single semicolon-separated string and reports
// the old and new string rather than the difference, so the difference is
// computed here. A removal names the id of the addition it retracts, never the
// tag's text: a concurrent addition of the same tag from another clone is a
// different member of the set and has to survive.
func (b *builder) tags(at time.Time, author string, rev int, change adoapi.FieldChange) {
	before := adoapi.SplitTags(change.Old)
	after := adoapi.SplitTags(change.New)

	for _, tag := range difference(before, after) {
		if target, ok := b.labels.remove(tag); ok {
			b.emitRev(at, author, rev, "label.remove", entity.Value{}, target, tag)
			b.removed[target] = tag
		}
	}
	for _, tag := range difference(after, before) {
		e := b.emitRev(at, author, rev, "label.add", entity.Str(tag), "", tag)
		b.labels.add(tag, e.ID)
	}
}

// assignee maps a scalar upstream field onto a list field here.
//
// Azure DevOps holds one assignee. docs/issues.md makes assignees a list
// because two people assigning themselves concurrently must not be a silent
// overwrite, so a reassignment imports as the pair it is: the standing
// assignment retracted, then the new one added.
func (b *builder) assignee(at time.Time, author string, rev int, change adoapi.FieldChange) {
	if old := assignee(change.Old); old != "" {
		if target, ok := b.assignees.remove(old); ok {
			b.emitRev(at, author, rev, "assignee.remove", entity.Value{}, target, old)
			b.removed[target] = old
		}
	}
	if who := assignee(change.New); who != "" {
		e := b.emitRev(at, author, rev, "assignee.add", entity.Str(who), "", who)
		b.assignees.add(who, e.ID)
	}
}

// assignee normalises an assignee's spelling.
//
// Azure DevOps is not consistent about the case of a unique name, and an
// assignee is a *value* in an OR-Set rather than a scalar to overwrite — so
// two spellings of one person would not correct each other, they would sit in
// the set as two assignees. Lowercasing matches what Author does for the same
// reason.
func assignee(s string) string { return strings.ToLower(strings.TrimSpace(s)) }

// relKinds maps Azure DevOps' link types onto the relation kinds
// docs/issues.md defines, and relTypes is the same table read backwards, which
// is what a push writes through.
//
// Only the reverse half of each directional family is here, and that is the
// whole of the direction question. Azure DevOps stores an edge on *both* work
// items — Hierarchy-Reverse on the child and Hierarchy-Forward on the parent,
// Dependency-Reverse on the blocked item and Dependency-Forward on the blocker —
// while a relation here is stored once, on the dependent end. So the forward
// halves are dropped rather than imported: the same edge arrives with the other
// work item, from the end that owns it, and importing both would file the epic
// under its own child.
var relKinds = map[string]string{
	adoapi.RelParent:      issue.KindParent,
	adoapi.RelPredecessor: issue.KindBlockedBy,
	adoapi.RelDuplicateOf: issue.KindDuplicate,
	adoapi.RelRelated:     issue.KindRelated,
}

var relTypes = func() map[string]string {
	out := make(map[string]string, len(relKinds))
	for rel, kind := range relKinds {
		out[kind] = rel
	}
	return out
}()

// LinkType is the Azure DevOps link type that stands for a relation kind, and
// whether there is one at all — a kind another bridge carried in may have none.
func LinkType(kind string) (string, bool) {
	rel, ok := relTypes[kind]
	return rel, ok
}

// links maps a work item's link changes onto relations.
//
// A relation holds an entity id — the hash of the target's own create event —
// which cannot be derived from a work item id without fetching it. So a link is
// written only where the ledger already holds the target, and one outside the
// import's scope is left alone rather than guessed at. This is the same
// constraint docs/storage-model.md records for cross-repository references;
// Azure DevOps sharpens it, since a link may cross to another project of the
// same collection.
func (b *builder) links(at time.Time, author string, u adoapi.Update) {
	// A removal names the add it retracts, so the id of this import's own add
	// is what makes the retraction possible — the same bookkeeping tags need.
	for _, rel := range u.RelationsRemoved {
		kind, target, ok := b.relation(rel)
		if !ok {
			continue
		}
		if add, ok := b.rels.remove(relKey(kind, target)); ok {
			b.emitRev(at, author, u.Rev, issue.RelRemove, entity.Value{}, add, "removed:"+kind+":"+target)
			// What the retracted add said, for the commit message: the pair is
			// not recoverable from the removal itself.
			b.removed[add] = kind + " " + target
		}
	}
	for _, rel := range u.RelationsAdded {
		kind, target, ok := b.relation(rel)
		if !ok {
			continue
		}
		e := b.emitRev(at, author, u.Rev, issue.RelAdd, entity.Str(kind), target, kind+":"+target)
		b.rels.add(relKey(kind, target), e.ID)
	}
}

// relation resolves one link into the kind and the entity it names here, and
// reports whether this import can represent it at all.
func (b *builder) relation(rel adoapi.Relation) (kind, target string, ok bool) {
	kind, ok = relKinds[rel.Rel]
	if !ok {
		return "", "", false
	}
	target, ok = b.entity(rel.URL)
	return kind, target, ok
}

// entity is the local entity a linked work item is filed as, if any.
func (b *builder) entity(url string) (string, bool) { return b.entityOf(workItemID(url)) }

// entityOf answers for a work item id, counting the links it could not answer
// for.
func (b *builder) entityOf(id int) (string, bool) {
	if id == 0 {
		return "", false
	}
	var found string
	var ok bool
	if b.known != nil {
		found, ok = b.known.Entity(Origin(b.target, id))
	}
	if !ok {
		b.unresolved++
	}
	return found, ok
}

// relKey names one relation for the add/remove bookkeeping: the pair, which is
// what identifies a member (docs/blob-format.md).
func relKey(kind, target string) string {
	return issue.Relation{Kind: kind, Target: target}.Key()
}

// commentThread imports the comments, with their edit history.
func (b *builder) commentThread() {
	comments := make([]adoapi.Comment, len(b.item.Comments))
	copy(comments, b.item.Comments)
	sort.SliceStable(comments, func(i, j int) bool { return comments[i].ID < comments[j].ID })

	for _, c := range comments {
		upstream := CommentOrigin(b.target, b.item.ID, c.ID)
		// A comment this repository posted is already here as a local entry,
		// and the ledger says so. Importing it again would show one comment
		// twice — two entries with the same text are legitimately two
		// comments, so nothing downstream could tell them apart.
		if b.known != nil && b.known.ClaimedComment(upstream) {
			continue
		}

		// The comment carries what was *posted*, not what it currently says.
		// Azure DevOps keeps every version, so the first one is the original
		// and each one after it is an edit with its own author and timestamp —
		// history GitHub cannot offer. Using c.Text here instead would file
		// the latest text twice: once as the comment and once as its own edit.
		posted, edits := c.Text, c.Versions
		if len(edits) > 0 {
			posted, edits = edits[0].Text, edits[1:]
		}

		b.begin(c.CreatedAt, c.CreatedBy)
		e := b.emit(c.CreatedAt, Author(c.CreatedBy), "comment", entity.Str(posted), "", upstream)
		b.comments = append(b.comments, bridge.CommentOrigin{EventID: e.ID, Upstream: upstream})

		for _, v := range edits {
			b.begin(v.CreatedAt, v.CreatedBy)
			b.emit(v.CreatedAt, Author(v.CreatedBy), "comment.edit", entity.Str(v.Text), e.ID,
				upstream+"/versions/"+strconv.Itoa(v.Version))
		}
		if c.Deleted {
			b.begin(c.CreatedAt, c.CreatedBy)
			b.emit(c.CreatedAt, Author(c.CreatedBy), "comment.remove", entity.Value{}, e.ID, upstream+":removed")
		}
	}
}

// reconcile fills in list members Azure DevOps reports as currently true that
// the replay did not account for.
//
// The replay is complete by construction when the revision feed is — the
// current value is the creation value plus every difference since. This covers
// the case where it is not: a work item whose history is unavailable, or a feed
// that reports a tag the revisions never mention. Scalars need no equivalent,
// because their creation-time event stands whenever nothing superseded it.
func (b *builder) reconcile(created time.Time, author string) {
	b.cur = b.create

	surviving := b.labels.surviving()
	for _, tag := range b.item.Tags {
		if !surviving[tag] {
			e := b.emit(created, author, "label.add", entity.Str(tag), "", b.origin+":label.add:"+tag)
			b.labels.add(tag, e.ID)
		}
	}
	if who := assignee(b.item.AssignedTo.UniqueName); who != "" && !b.assignees.surviving()[who] {
		e := b.emit(created, author, "assignee.add", entity.Str(who), "", b.origin+":assignee.add:"+who)
		b.assignees.add(who, e.ID)
	}

	// Links are reported both ways — as revisions that added and removed them,
	// and as the array the work item currently holds — so this is where the
	// second answer covers what the first could not: a work item whose revision
	// feed is unavailable, or one whose links predate the history the feed goes
	// back to, still says what it hangs under and what it waits on.
	//
	// Sorted by what the link says rather than taken in the order the array came
	// in, for the reason sortedFields gives: nothing here depends on the order,
	// so two imports of one work item should not differ by it.
	for _, r := range b.currentRels() {
		key := r.Key()
		if b.rels.surviving()[key] {
			continue
		}
		e := b.emit(created, author, issue.RelAdd, entity.Str(r.Kind), r.Target, b.origin+":rel.add:"+key)
		b.rels.add(key, e.ID)
	}
}

// currentRels is every link the work item holds now that this import can
// represent, in a fixed order.
func (b *builder) currentRels() []issue.Relation {
	var out []issue.Relation
	for _, rel := range b.item.Relations {
		kind, target, ok := b.relation(rel)
		if !ok {
			continue
		}
		out = append(out, issue.Relation{Kind: kind, Target: target})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Key() < out[j].Key() })
	return out
}

// actions turns the collected groups into the units Apply commits, oldest
// first, each carrying the title the work item answered to at the time.
func (b *builder) actions() []entity.Action {
	groups := make([]*group, len(b.groups))
	copy(groups, b.groups)
	sort.SliceStable(groups, func(i, j int) bool {
		if !groups[i].at.Equal(groups[j].at) {
			return groups[i].at.Before(groups[j].at)
		}
		return groups[i].seq < groups[j].seq
	})

	var trailers []issue.Trailer
	if url := b.target.WebURL(b.item.ID); url != "" {
		trailers = []issue.Trailer{{Key: "Origin", Value: url}}
	}

	title := ""
	out := make([]entity.Action, 0, len(groups))
	for _, g := range groups {
		if len(g.events) == 0 {
			continue
		}
		out = append(out, entity.Action{
			Author: Identity(g.who, g.at),
			Message: issue.Action{
				ID:       b.id,
				Title:    title,
				Events:   g.events,
				Removed:  b.removed,
				Trailers: trailers,
			}.Message(),
			Events: g.events,
		})
		for _, e := range g.events {
			if e.Op == "title" {
				title = e.Val.Display()
			}
		}
	}
	return out
}

// sortedUpdates is the revision feed in revision order.
//
// The order is established here rather than assumed, because what a tag
// removal points at depends on it.
func (b *builder) sortedUpdates() []adoapi.Update {
	updates := make([]adoapi.Update, len(b.item.Updates))
	copy(updates, b.item.Updates)
	sort.SliceStable(updates, func(i, j int) bool { return updates[i].Rev < updates[j].Rev })
	return updates
}

// sortedFields is a revision's field names in a fixed order.
//
// A map's iteration order is random, and several events from one revision
// would otherwise be emitted in a different order on every run. Their ids do
// not depend on it, so the blob would still converge — but the commits would
// not, and a diff between two imports of the same work item should be empty.
func sortedFields(fields map[string]adoapi.FieldChange) []string {
	names := make([]string, 0, len(fields))
	for name := range fields {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// emit builds one event and appends it.
//
// `c` is the upstream timestamp, not a position in the feed. That is forced:
// re-import must reproduce every event's bytes exactly, so `c` has to be a
// pure function of one upstream event and never of its neighbours. A counter
// over the feed fails that — deleting one comment shifts every later position,
// changing every later event's id, and the blob is a grow-only set, so the old
// lines stay and every comment after the deleted one appears twice.
func (b *builder) emit(at time.Time, author, op string, val entity.Value, ref, nonceInput string) entity.Event {
	if b.err != nil {
		return entity.Event{}
	}
	unix := at.Unix()
	e, err := entity.NewEventWithNonce(b.format, entity.Event{
		V:   entity.FormatVersion,
		C:   unix,
		TS:  unix,
		A:   author,
		Op:  op,
		N:   Nonce(nonceInput),
		Ref: ref,
		Val: val,
	})
	if err != nil {
		b.err = fmt.Errorf("%s: %w", op, err)
		return entity.Event{}
	}
	b.cur.events = append(b.cur.events, e)
	return e
}

// emitRev is emit for an event that stands for a field change within one
// revision.
//
// The operation is always part of the nonce, not only when a revision happens
// to change more than one field. Deciding that per revision would make the
// nonce depend on the revision's neighbouring fields, which is the same class
// of mistake as deriving `c` from a position: it would change on the next
// import if a field were added to that revision's payload.
//
// The discriminator is the member's value for a list operation, which is what
// separates two additions made in one revision.
func (b *builder) emitRev(at time.Time, author string, rev int, op string, val entity.Value, ref, discriminator string) entity.Event {
	input := fmt.Sprintf("%s/revisions/%d/%s", b.origin, rev, op)
	if discriminator != "" {
		input += ":" + discriminator
	}
	return b.emit(at, author, op, val, ref, input)
}

// emitRevScalar is emitRev for a scalar, which needs no discriminator: one
// revision can change a scalar only once.
func (b *builder) emitRevScalar(at time.Time, author string, rev int, op string, val entity.Value) entity.Event {
	return b.emitRev(at, author, rev, op, val, "", "")
}

// emitField is emit for an event standing for the work item as filed, rather
// than for a revision of its own. Its nonce derives from the work item and the
// operation, which is stable across imports and distinct per field.
func (b *builder) emitField(at time.Time, author, op string, val entity.Value) entity.Event {
	return b.emit(at, author, op, val, "", b.origin+":"+op)
}

// members tracks which addition is currently in force for each value of an
// OR-Set field, so that a removal can point at it.
type members struct {
	stacks map[string][]string
}

func newMembers() *members { return &members{stacks: map[string][]string{}} }

func (m *members) add(value, eventID string) {
	m.stacks[value] = append(m.stacks[value], eventID)
}

func (m *members) remove(value string) (string, bool) {
	stack := m.stacks[value]
	if len(stack) == 0 {
		return "", false
	}
	target := stack[len(stack)-1]
	m.stacks[value] = stack[:len(stack)-1]
	return target, true
}

func (m *members) surviving() map[string]bool {
	out := make(map[string]bool, len(m.stacks))
	for value, stack := range m.stacks {
		if len(stack) > 0 {
			out[value] = true
		}
	}
	return out
}

// difference is the values in a that are not in b, in a's order.
func difference(a, b []string) []string {
	have := make(map[string]bool, len(b))
	for _, v := range b {
		have[v] = true
	}
	var out []string
	for _, v := range a {
		if !have[v] {
			out = append(out, v)
		}
	}
	return out
}

// relative drops the leading project from an area or iteration path and
// renders the rest with forward slashes.
func relative(path, project string) string {
	path = strings.ReplaceAll(path, `\`, "/")
	path = strings.TrimPrefix(path, project)
	return strings.Trim(path, "/")
}

// workItemID reads the id off a work item's API URL, which is how a relation
// names its target.
func workItemID(rawURL string) int {
	idx := strings.LastIndex(rawURL, "/")
	if idx < 0 {
		return 0
	}
	id, err := strconv.Atoi(rawURL[idx+1:])
	if err != nil {
		return 0
	}
	return id
}

// Vocabulary is this bridge's contribution to the fold, and it is empty.
//
// Nothing Azure DevOps holds needs a namespaced operation. The work item id
// and its URL are relationship state and live on the origin ledger; the area
// path is scope rather than state and is not stored at all. It stays declared
// rather than being deleted so that the absence is deliberate rather than an
// oversight, and because the composition in cmd/git-issue's open() is the seam
// a bridge with genuine namespaced state would use.
var Vocabulary = entity.Vocabulary{}

// Nonce derives an event's nonce from stable upstream identity: the first 16
// hex characters of its SHA-256, per docs/storage-model.md.
func Nonce(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:16]
}

// Author maps an Azure DevOps identity onto an event author, and Identity maps
// one onto a commit's byline. Both rules live in internal/bridge/ado/api, shared with
// the pull request bridge: `a` is hashed into every event id, so the two
// bridges spelling an author differently would stop the re-import convergence
// the design rests on.
func Author(id adoapi.Identity) string { return id.EventAuthor() }

func Identity(id adoapi.Identity, at time.Time) gitx.Identity { return id.CommitIdentity(at) }
