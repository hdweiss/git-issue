// Package github maps GitHub issues onto the tracker's event vocabulary, per
// docs/bridge-github.md.
//
// It is the only package that knows both what an issue is and what GitHub is.
// internal/bridge/github/api below it knows GitHub and nothing else; internal/issue below
// that knows issues and nothing else. Keeping the seam here is what lets the
// mapping be tested end to end from recorded API responses, with no network
// and no repository.
//
// Everything here is a pure function of what GitHub returned. That is a
// correctness requirement, not a style: an import that depended on local state
// would produce different bytes on a second run, and since an event's id is
// the hash of its own bytes, a re-import would duplicate the entity instead of
// converging on it.
package ghissue

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/bridge"
	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/issue"
)

// Entity is one imported issue: the id its create event hashes to, every action
// the import produced for it, oldest first, and the mappings it establishes.
//
// The mappings do not go into the blob. Which GitHub object an issue
// corresponds to is not a fact about the issue — see "The origin ledger" in
// docs/storage-model.md — so it is reported here for the caller to record on the
// ledger ref, where one issue can be linked to several trackers at once and a
// tracker can be dropped without rewriting anything.
type Entity struct {
	ID      string
	Actions []entity.Action

	// Origin is the issue's node id and URL its web address: the identity and
	// the locator, separately, because a transferred issue keeps the first and
	// gets a new second.
	Origin string
	URL    string

	// Comments ties each thread-entry event this import produced to the upstream
	// comment it came from.
	Comments []bridge.CommentOrigin

	// Unresolved is how many relation events this import could not write
	// because the ledger does not hold the issue they name. It is what makes
	// importing a batch twice worth the second pass and no more: an entity with
	// none has nothing to gain from being read again.
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

// Import maps one GitHub issue onto actions, each a group of events that one
// person did at one moment: the issue being filed, a comment being posted, a
// label being added.
//
// The grouping decides nothing about the events themselves — their bytes, and
// therefore their ids and the entity's, are exactly what they were when an
// import wrote a single commit per run. It decides only what a commit says and
// who it is attributed to.
//
// known is the origin ledger, and answers the two questions the response cannot.
// Which upstream comments are already present locally as entries this repository
// authored and pushed — skipping those is what stops a pushed comment from
// coming back as a second one — and which local entity an upstream issue is
// filed as, which is what a relation's target has to be. Passing nil claims
// nothing and resolves nothing.
//
// The relations are the one place where the purity above is qualified, and the
// qualification is worth stating: a link is written only when this clone already
// holds the issue it names, so an import can produce fewer events than the same
// response would produce later, once the target has been imported too. It never
// produces different ones. Re-import still converges — the blob is a grow-only
// set and the missing link is simply added when it becomes resolvable.
func Import(format entity.ObjectFormat, gh ghapi.Issue, known bridge.Lookup) (Entity, error) {
	b := &builder{format: format, gh: gh, removed: map[string]string{}, known: known}
	b.build()
	if b.err != nil {
		return Entity{}, b.err
	}
	return Entity{
		ID:         b.id,
		Actions:    b.actions(),
		Origin:     gh.ID,
		URL:        gh.URL,
		Comments:   b.comments,
		Unresolved: b.unresolved,
	}, nil
}

// group is one upstream action's events, with who did it and when.
type group struct {
	at     time.Time
	login  string
	seq    int
	events []entity.Event
}

type builder struct {
	format     entity.ObjectFormat
	gh         ghapi.Issue
	groups     []*group
	cur        *group
	create     *group
	id         string
	err        error
	removed    map[string]string
	known      bridge.Lookup
	comments   []bridge.CommentOrigin
	unresolved int

	// What the timeline replay accounted for, so reconcile fills in only what
	// it did not.
	survivingLabels    map[string]bool
	survivingAssignees map[string]bool
	survivingRels      map[string]bool
	sawClose           bool
	sawMilestone       bool
	sawLock            bool
}

// begin starts a new action. Everything emitted until the next begin belongs
// to it.
func (b *builder) begin(at time.Time, login string) *group {
	g := &group{at: at, login: login, seq: len(b.groups)}
	b.groups = append(b.groups, g)
	b.cur = g
	return g
}

func (b *builder) build() {
	created := b.gh.CreatedAt
	author := Author(b.gh.Author)

	// Filing the issue is one action, however many events it takes to record
	// what it was filed with.
	b.create = b.begin(created, login(b.gh.Author))

	// The create event first, and its id is the entity's. Its nonce is
	// SHA-256 of the node id exactly as docs/bridge-github.md publishes it —
	// this one derivation fixes the identity of every GitHub issue ever
	// imported by any implementation, so it does not get a variation.
	create := b.emit(created, author, "create", entity.Str(issue.Type), "", b.gh.ID)
	b.id = create.ID

	// No github.origin or github.url event. The node id and the URL are
	// recorded on the origin ledger instead: they say where the issue lives
	// upstream rather than anything about the issue, and keeping them out of the
	// blob is what lets one issue be linked to an upstream and a fork at once.

	// Fields that GitHub keeps no history for import once, at creation time,
	// and later changes upstream are invisible. That is a property of GitHub
	// rather than of the format, and it is already what the spec says about
	// bodies; issue types are in the same position.
	b.emitField(created, author, "title", entity.Str(b.originalTitle()))
	if b.gh.Body != "" {
		b.emitField(created, author, "description", entity.Str(b.gh.Body))
	}
	if b.gh.Type != "" {
		b.emitField(created, author, "type", entity.Str(b.gh.Type))
	}
	// No `status: open` event. Open is the implied status of an issue that has
	// none (docs/issues.md), and writing it explicitly would sit at the same
	// `c` as a close reconciled from current state — leaving the tie-break on
	// id to decide whether a closed issue imports as closed.

	for _, c := range b.gh.Comments {
		// A comment this repository posted is already here as a local entry, and
		// the ledger says so. Importing it again would show one comment twice —
		// two entries with the same text are legitimately two comments, so
		// nothing downstream could tell them apart.
		if b.known != nil && b.known.ClaimedComment(c.ID) {
			continue
		}
		b.begin(c.CreatedAt, login(c.Author))
		e := b.emit(c.CreatedAt, Author(c.Author), "comment", entity.Str(c.Body), "", c.ID)
		b.comments = append(b.comments, bridge.CommentOrigin{EventID: e.ID, Upstream: c.ID})
	}

	b.timeline()
	b.reconcile()
}

// actions turns the collected groups into the units Apply commits, oldest
// first, each carrying the title the issue answered to at the time.
//
// The groups are ordered by their upstream timestamp rather than by the order
// they were collected in, because they were collected by kind — every comment,
// then every timeline entry — and a comment posted after a rename has to see
// the name the issue had when it was posted.
func (b *builder) actions() []entity.Action {
	groups := make([]*group, len(b.groups))
	copy(groups, b.groups)
	sort.SliceStable(groups, func(i, j int) bool {
		if !groups[i].at.Equal(groups[j].at) {
			return groups[i].at.Before(groups[j].at)
		}
		return groups[i].seq < groups[j].seq
	})

	// The locator, not the identity: a transferred issue keeps its node id and
	// gets a new URL, so this is where a reader is sent rather than what the
	// commit is keyed on. GitHub gives no per-comment or per-event URL through
	// the node ids this bridge reads, so every action points at the issue.
	var trailers []issue.Trailer
	if b.gh.URL != "" {
		trailers = []issue.Trailer{{Key: "Origin", Value: b.gh.URL}}
	}

	title := ""
	out := make([]entity.Action, 0, len(groups))
	for _, g := range groups {
		if len(g.events) == 0 {
			continue
		}
		out = append(out, entity.Action{
			Author: Identity(g.login, g.at),
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

// originalTitle is the title the issue was created with.
//
// Titles are the one field GitHub keeps complete history for: every rename
// carries the previous title, so the earliest rename names what the issue was
// called before it. With no renames, the current title is the original.
func (b *builder) originalTitle() string {
	earliest := ""
	var at time.Time
	for _, item := range b.gh.Timeline {
		if item.Kind != ghapi.Renamed {
			continue
		}
		if at.IsZero() || item.CreatedAt.Before(at) {
			at, earliest = item.CreatedAt, item.PreviousTitle
		}
	}
	if !at.IsZero() {
		return earliest
	}
	return b.gh.Title
}

// timeline replays the upstream event feed.
//
// This is where the import stops approximating: every entry has an actor, a
// timestamp and a stable id, so a label removal can name the specific addition
// it retracts and the OR-Set is populated properly rather than reconstructed
// from a final state.
func (b *builder) timeline() {
	items := make([]ghapi.TimelineItem, len(b.gh.Timeline))
	copy(items, b.gh.Timeline)
	// GitHub returns the timeline in order, but the add an removal points at
	// depends on that order, so it is established here rather than assumed.
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})

	labels := newMembers()
	assignees := newMembers()
	rels := newMembers()

	for _, item := range items {
		at, actor := item.CreatedAt, Author(item.Actor)
		// One upstream entry is one action, whatever it takes to record it: a
		// close and its reason are one thing that happened, not two.
		b.begin(at, login(item.Actor))
		switch item.Kind {
		case ghapi.Labeled:
			e := b.emit(at, actor, "label.add", entity.Str(item.Label), "", item.ID)
			labels.add(item.Label, e.ID)
		case ghapi.Unlabeled:
			if target, ok := labels.remove(item.Label); ok {
				b.emit(at, actor, "label.remove", entity.Value{}, target, item.ID)
				// A removal names the add it retracts rather than a value, so
				// the value is recorded here or it is not recoverable at all.
				b.removed[target] = item.Label
			}
		case ghapi.Assigned:
			e := b.emit(at, actor, "assignee.add", entity.Str(item.Assignee), "", item.ID)
			assignees.add(item.Assignee, e.ID)
		case ghapi.Unassigned:
			if target, ok := assignees.remove(item.Assignee); ok {
				b.emit(at, actor, "assignee.remove", entity.Value{}, target, item.ID)
				b.removed[target] = item.Assignee
			}
		case ghapi.Renamed:
			b.emit(at, actor, "title", entity.Str(item.CurrentTitle), "", item.ID)
		case ghapi.Closed:
			b.emit(at, actor, "status", entity.Str(StatusClosed), "", item.ID)
			if reason := Reason(item.Reason); reason != "" {
				b.emit(at, actor, "status.reason", entity.Str(reason), "", item.ID+":reason")
			}
		case ghapi.Reopened:
			b.emit(at, actor, "status", entity.Str(issue.StatusOpen), "", item.ID)
		case ghapi.Milestoned:
			b.emit(at, actor, "milestone", entity.Str(item.Milestone), "", item.ID)
		case ghapi.Demilestoned:
			b.emit(at, actor, "milestone", entity.Null(), "", item.ID)
		case ghapi.Locked:
			b.emit(at, actor, "locked", entity.Bool(true), "", item.ID)
			if reason := LockReason(item.Reason); reason != "" {
				b.emit(at, actor, "lock.reason", entity.Str(reason), "", item.ID+":reason")
			}
		case ghapi.Unlocked:
			b.emit(at, actor, "locked", entity.Bool(false), "", item.ID)
		case ghapi.Pinned:
			b.emit(at, actor, "pinned", entity.Bool(true), "", item.ID)
		case ghapi.Unpinned:
			b.emit(at, actor, "pinned", entity.Bool(false), "", item.ID)
		case ghapi.ParentAdded:
			b.link(item, issue.KindParent, rels)
		case ghapi.ParentRemoved:
			b.unlink(item, issue.KindParent, rels)
		case ghapi.BlockedByAdded:
			b.link(item, issue.KindBlockedBy, rels)
		case ghapi.BlockedByRemoved:
			b.unlink(item, issue.KindBlockedBy, rels)
		}
	}

	b.survivingLabels = labels.surviving()
	b.survivingAssignees = assignees.surviving()
	b.survivingRels = rels.surviving()
	b.sawClose = b.hasKind(ghapi.Closed, ghapi.Reopened)
	b.sawMilestone = b.hasKind(ghapi.Milestoned, ghapi.Demilestoned)
	b.sawLock = b.hasKind(ghapi.Locked, ghapi.Unlocked)
}

// link writes one relation the timeline reports, on the end that depends on it:
// the child names its parent, the blocked issue names what blocks it. The other
// end is derived by a reader and never stored, so GitHub's `SubIssueAddedEvent`
// and `BlockingAddedEvent` are not even requested (docs/issues.md).
//
// A relation's target is an *entity* id — the hash of that issue's own create
// event — which cannot be derived from a node id without fetching the issue it
// names. So a link whose target this clone does not hold is left unwritten
// rather than guessed at, which is also what the cross-repository case comes
// down to: an issue in another repository is not in this ledger.
func (b *builder) link(item ghapi.TimelineItem, kind string, rels *members) {
	target, ok := b.entity(item.Target)
	if !ok {
		return
	}
	e := b.emit(item.CreatedAt, Author(item.Actor), issue.RelAdd, entity.Str(kind), target, item.ID)
	rels.add(relKey(kind, target), e.ID)
}

// unlink retracts one. A removal names the add it retracts rather than a value,
// so it can only be written where this import produced that add — the same
// bookkeeping labels and assignees need.
func (b *builder) unlink(item ghapi.TimelineItem, kind string, rels *members) {
	target, ok := b.entity(item.Target)
	if !ok {
		return
	}
	add, ok := rels.remove(relKey(kind, target))
	if !ok {
		return
	}
	b.emit(item.CreatedAt, Author(item.Actor), issue.RelRemove, entity.Value{}, add, item.ID)
	// What the retracted add said, for the commit message: the pair is not
	// recoverable from the removal itself.
	b.removed[add] = kind + " " + target
}

// entity is the local entity an upstream issue is filed as, if this clone holds
// it at all, counting the links it could not answer for.
func (b *builder) entity(node string) (string, bool) {
	if node == "" {
		return "", false
	}
	var id string
	var ok bool
	if b.known != nil {
		id, ok = b.known.Entity(node)
	}
	if !ok {
		b.unresolved++
	}
	return id, ok
}

// relKey names one relation for the add/remove bookkeeping: the pair, which is
// what identifies a member (docs/blob-format.md).
func relKey(kind, target string) string {
	return issue.Relation{Kind: kind, Target: target}.Key()
}

func (b *builder) hasKind(kinds ...string) bool {
	for _, item := range b.gh.Timeline {
		for _, k := range kinds {
			if item.Kind == k {
				return true
			}
		}
	}
	return false
}

// reconcile fills in what the timeline did not account for.
//
// The timeline is authoritative where it speaks, but it does not always speak:
// an issue that predates a timeline entry type, or one whose history was lost
// to a transfer, can be labelled without ever having been `labeled`. Anything
// GitHub reports as currently true and the replay did not produce is added at
// creation time, which is the earliest moment it can be attributed to.
//
// Each such event derives its nonce from the issue's node id and the field it
// stands for, so a second import produces the same bytes and adds nothing.
func (b *builder) reconcile() {
	created, author := b.gh.CreatedAt, Author(b.gh.Author)

	// These are facts about the issue as filed, so they join the action that
	// filed it rather than becoming an action of their own with no upstream
	// event behind it.
	b.cur = b.create

	for _, name := range b.gh.Labels {
		if !b.survivingLabels[name] {
			b.emit(created, author, "label.add", entity.Str(name), "", b.gh.ID+":label.add:"+name)
		}
	}
	for _, login := range b.gh.Assignees {
		if !b.survivingAssignees[login] {
			b.emit(created, author, "assignee.add", entity.Str(login), "", b.gh.ID+":assignee.add:"+login)
		}
	}
	if !b.sawMilestone && b.gh.Milestone != "" {
		b.emitField(created, author, "milestone", entity.Str(b.gh.Milestone))
	}
	if !b.sawLock && b.gh.Locked {
		b.emitField(created, author, "locked", entity.Bool(true))
		if reason := LockReason(b.gh.LockReason); reason != "" {
			b.emitField(created, author, "lock.reason", entity.Str(reason))
		}
	}
	// A parent the replay did not account for: an issue transferred between
	// repositories, or one filed under another before GitHub raised an event
	// for it. A duplicate is always in this position — GitHub raises
	// `MarkedAsDuplicateEvent` on the canonical issue, naming the duplicate,
	// and the duplicate's own timeline says nothing at all — so `duplicateOf`
	// is not a fallback there but the only source, and the link imports without
	// attribution or a removal history, exactly as the body does.
	b.reconcileRelation(created, author, issue.KindParent, b.gh.Parent)
	b.reconcileRelation(created, author, issue.KindDuplicate, b.gh.DuplicateOf)

	if !b.sawClose && b.gh.State == ghapi.StateClosed {
		b.emit(created, author, "status", entity.Str(StatusClosed), "", b.gh.ID+":status:closed")
		if reason := Reason(b.gh.Reason); reason != "" {
			b.emit(created, author, "status.reason", entity.Str(reason), "", b.gh.ID+":status.reason")
		}
	}
}

// reconcileRelation writes a link GitHub currently reports and the timeline did
// not produce.
//
// Its nonce derives from the two node ids and the kind, which is upstream fact
// rather than local state: two clones that resolve the target differently still
// agree on what the event is called, and a second import adds nothing.
func (b *builder) reconcileRelation(at time.Time, author, kind, node string) {
	target, ok := b.entity(node)
	if !ok {
		return
	}
	if b.survivingRels[relKey(kind, target)] {
		return
	}
	b.emit(at, author, issue.RelAdd, entity.Str(kind), target, b.gh.ID+":"+issue.RelAdd+":"+kind+":"+node)
}

// emit builds one event and appends it.
//
// `c` is the upstream timestamp, not a position in the feed. That is forced:
// re-import must reproduce every event's bytes exactly, so `c` has to be a
// pure function of one upstream event and never of its neighbours. A counter
// over the feed fails that — deleting one comment shifts every later position,
// changing every later event's id, and the blob is a grow-only set, so the
// old lines stay and every comment after the deleted one appears twice.
//
// This does not weaken the ordering rule. The fold still orders by `(c, id)`
// and never by `ts`; it is the value put into `c` that comes from a clock, and
// on an imported issue that clock is GitHub's single server rather than the
// many unsynchronised ones the rule exists to defend against.
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

// emitField is emit for an event synthesised from the issue's current state
// rather than from an upstream event of its own. Its nonce derives from the
// issue's node id and the operation, which is stable across imports and
// distinct per field.
func (b *builder) emitField(at time.Time, author, op string, val entity.Value) entity.Event {
	return b.emit(at, author, op, val, "", b.gh.ID+":"+op)
}

// members tracks which addition is currently in force for each value of an
// OR-Set field, so that a removal can point at it.
//
// A value can be added, removed and added again, so the additions are a stack
// per value rather than a single id: the removal retracts the addition that is
// standing at that moment, which is the one an OR-Set says it retracts.
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

// Vocabulary is this bridge's contribution to the fold, and it is empty.
//
// It was not always: the bridge used to write github.origin and github.url as
// namespaced scalars. Both moved to the origin ledger, which is where a fact
// about this repository's relationship with one tracker belongs — and a scalar
// had room for exactly one tracker, so an issue could not be linked to an
// upstream and a fork at the same time.
//
// It stays declared rather than being deleted because the composition in
// cmd/git-issue's open() is the seam a bridge with genuine namespaced state
// would use, and because keeping it makes the absence deliberate rather than an
// oversight.
var Vocabulary = entity.Vocabulary{}

// Nonce derives an event's nonce from stable upstream identity: the first 16
// hex characters of its SHA-256, per docs/storage-model.md.
//
// Deriving rather than generating is what makes a re-import a no-op. A random
// nonce would change the event's bytes on every run, and with them its id —
// and for the create event, the whole entity's id.
func Nonce(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:16]
}

// login normalises a GitHub login, substituting GitHub's own name for a
// deleted account — the one its API returns where it substitutes rather than
// nulling.
func login(s string) string {
	if s == "" {
		return "ghost"
	}
	return s
}

// Author maps a GitHub login onto an event author.
//
// The scheme prefix matters as much as the login: `a` is part of the event's
// hashed bytes, so two implementations spelling the same author differently
// would produce different ids for the same event and stop converging.
func Author(l string) string { return "github:" + login(l) }

// Identity maps a GitHub login onto the author of a commit.
//
// GitHub exposes no email for most accounts, so the address is its own
// noreply form, which is what makes `git shortlog`, `git log --author` and a
// repository's .mailmap work on the tracker's history without a second
// mechanism. It is a bridge-asserted label and not proof of anything: the
// authoritative record of who wrote an event is the `a` field inside it, which
// no reordering or rewrite can touch (docs/storage-model.md).
//
// Unlike Author, nothing hashes this. Two bridges spelling it differently
// disagree about a commit's byline and about nothing else.
func Identity(l string, at time.Time) gitx.Identity {
	l = login(l)
	return gitx.Identity{Name: l, Email: l + "@users.noreply.github.com", When: at}
}

// StatusClosed is docs/issues.md's terminal status. GitHub's own spelling of
// the states lives in internal/bridge/github/api, with the rest of GitHub's vocabulary.
const StatusClosed = "closed"

// LockReason normalises GitHub's lock reason.
//
// docs/issues.md has bridges pass the upstream string through rather than map
// it onto a fixed set, since platform vocabularies differ. What is passed
// through is lowercased: GraphQL spells the same values in enum case that REST
// spells in lower, and an issue must not import differently depending on which
// door the bridge came in by.
func LockReason(s string) string { return strings.ToLower(s) }

// Reason maps GitHub's state reason onto docs/issues.md's vocabulary.
//
// REOPENED has no counterpart and must not become one: `status.reason` says
// why an issue reached a terminal status, and a reopened issue has not reached
// one. Anything unrecognised passes through lowercased rather than being
// dropped, since the vocabulary is open.
func Reason(stateReason string) string {
	switch strings.ToUpper(stateReason) {
	case "":
		return ""
	case "REOPENED":
		return ""
	default:
		return strings.ToLower(stateReason)
	}
}
