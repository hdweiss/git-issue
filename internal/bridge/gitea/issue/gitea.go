// Package gitea maps Gitea issues onto the tracker's event vocabulary, per
// docs/bridge-gitea.md.
//
// It is the only package that knows both what an issue is and what Gitea is.
// internal/bridge/gitea/api below it knows Gitea and nothing else; internal/issue below
// that knows issues and nothing else. Keeping the seam here is what lets the
// mapping be tested end to end from recorded API responses, with no network and
// no repository.
//
// Everything here is a pure function of what Gitea returned. That is a
// correctness requirement, not a style: an import that depended on local state
// would produce different bytes on a second run, and since an event's id is the
// hash of its own bytes, a re-import would duplicate the entity instead of
// converging on it.
//
// The mapping is closest to internal/bridge/github/issue's: Gitea's issue data model
// — a repo-local number, a body with no history, comments with stable ids, and
// a timeline of typed entries — mirrors GitHub's. Where it is thinner (no issue
// types, no cross-issue hierarchy, no comment edit history) the bridge says so
// rather than inventing it.
package giteaissue

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/bridge"
	giteaapi "github.com/hdweiss/git-issue/internal/bridge/gitea/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/issue"
)

// StatusClosed is docs/issues.md's terminal status. Gitea's own spelling of the
// two states ("open"/"closed") lives in internal/bridge/gitea/api.
const StatusClosed = "closed"

// Entity is one imported issue: the id its create event hashes to, every action
// the import produced for it, oldest first, and the mappings it establishes.
//
// The mappings do not go into the blob. Which Gitea issue an entity corresponds
// to is not a fact about the issue — see "The origin ledger" in
// docs/storage-model.md — so it is reported here for the caller to record on the
// ledger ref.
type Entity struct {
	ID      string
	Actions []entity.Action

	// Origin is the issue's identity string and URL its web address, kept
	// separate because a transferred issue would get a new URL.
	Origin string
	URL    string

	Comments []bridge.CommentOrigin

	// Unresolved is how many relation events this import could not write because
	// the ledger does not hold the issue they name. It is what makes importing a
	// batch twice worth the second pass and no more.
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

// Import maps one Gitea issue onto actions, each a group of events that one
// person did at one moment: the issue being filed, a comment being posted, a
// label being added.
//
// known is the origin ledger, and answers the two questions the response
// cannot: which upstream comments are already present locally as entries this
// repository posted, and which local entity an upstream issue is filed as.
// Passing nil claims nothing and resolves nothing.
func Import(format entity.ObjectFormat, t giteaapi.Target, iss giteaapi.Issue, known bridge.Lookup) (Entity, error) {
	b := &builder{
		format:  format,
		target:  t,
		iss:     iss,
		known:   known,
		origin:  t.Origin(iss.Number),
		removed: map[string]string{},
	}
	b.build()
	if b.err != nil {
		return Entity{}, b.err
	}
	return Entity{
		ID:         b.id,
		Actions:    b.actions(),
		Origin:     b.origin,
		URL:        iss.HTMLURL,
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
	format entity.ObjectFormat
	target giteaapi.Target
	iss    giteaapi.Issue
	known  bridge.Lookup
	origin string

	groups     []*group
	cur        *group
	create     *group
	id         string
	err        error
	removed    map[string]string
	comments   []bridge.CommentOrigin
	unresolved int

	survivingLabels    map[string]bool
	survivingAssignees map[string]bool
	sawClose           bool
	sawMilestone       bool
}

func (b *builder) begin(at time.Time, login string) *group {
	g := &group{at: at, login: login, seq: len(b.groups)}
	b.groups = append(b.groups, g)
	b.cur = g
	return g
}

func (b *builder) build() {
	created := b.iss.CreatedAt
	author := Author(b.iss.User)

	b.create = b.begin(created, login(b.iss.User))

	// The create event first, and its id is the entity's. Its nonce is SHA-256
	// of the identity string exactly as docs/bridge-gitea.md publishes it.
	create := b.emit(created, author, "create", entity.Str(issue.Type), "", b.origin)
	b.id = create.ID

	// No gitea.origin or gitea.url event. The identity and the URL are recorded
	// on the origin ledger, which is where a fact about this repository's
	// relationship with one tracker belongs.

	b.emitField(created, author, "title", entity.Str(b.originalTitle()))
	if b.iss.Body != "" {
		b.emitField(created, author, "description", entity.Str(b.iss.Body))
	}
	// No `status: open` event. Open is the implied status of an issue that has
	// none (docs/issues.md), and writing it explicitly would tie against a
	// close reconciled from current state.

	for _, c := range b.iss.Comments {
		upstream := b.target.CommentOrigin(b.iss.Number, c.ID)
		if b.known != nil && b.known.ClaimedComment(upstream) {
			continue
		}
		b.begin(c.CreatedAt, login(c.User))
		e := b.emit(c.CreatedAt, Author(c.User), "comment", entity.Str(c.Body), "", upstream)
		b.comments = append(b.comments, bridge.CommentOrigin{EventID: e.ID, Upstream: upstream})
	}

	b.timeline()
	b.reconcile()
}

// originalTitle is the title the issue was created with. Gitea keeps the
// previous title on every rename, so the earliest rename names what the issue
// was called before it; with no renames, the current title is the original.
func (b *builder) originalTitle() string {
	earliest := ""
	var at time.Time
	for _, e := range b.iss.Timeline {
		if e.Type != giteaapi.TLTitle {
			continue
		}
		if at.IsZero() || e.CreatedAt.Before(at) {
			at, earliest = e.CreatedAt, e.OldTitle
		}
	}
	if !at.IsZero() {
		return earliest
	}
	return b.iss.Title
}

// timeline replays the upstream event feed.
//
// Every entry has an actor, a timestamp and a stable id, so a label removal can
// name the specific addition it retracts and the OR-Set is populated properly
// rather than reconstructed from a final state.
//
// Dependencies are the exception. Gitea writes an identical add_dependency
// entry to both issues of a blocked-by pair, so the timeline cannot say which
// end is blocked — the direction is recovered from the current dependency list
// in reconcile instead, unattributed, the way GitHub's duplicateOf is.
func (b *builder) timeline() {
	items := make([]giteaapi.TimelineEntry, len(b.iss.Timeline))
	copy(items, b.iss.Timeline)
	sort.SliceStable(items, func(i, j int) bool {
		if !items[i].CreatedAt.Equal(items[j].CreatedAt) {
			return items[i].CreatedAt.Before(items[j].CreatedAt)
		}
		return items[i].ID < items[j].ID
	})

	labels := newMembers()
	assignees := newMembers()

	for _, item := range items {
		if item.CreatedAt.IsZero() {
			continue
		}
		at, actor := item.CreatedAt, Author(item.User)
		b.begin(at, login(item.User))
		ref := b.entryRef(item)

		switch item.Type {
		case giteaapi.TLLabel:
			if item.Label == nil {
				continue
			}
			if item.Body == "1" {
				e := b.emit(at, actor, "label.add", entity.Str(item.Label.Name), "", ref)
				labels.add(item.Label.Name, e.ID)
			} else if target, ok := labels.remove(item.Label.Name); ok {
				b.emit(at, actor, "label.remove", entity.Value{}, target, ref)
				b.removed[target] = item.Label.Name
			}
		case giteaapi.TLAssignees:
			who := ""
			if item.Assignee != nil {
				who = item.Assignee.Name()
			}
			if who == "" {
				continue
			}
			if item.RemovedAssignee {
				if target, ok := assignees.remove(who); ok {
					b.emit(at, actor, "assignee.remove", entity.Value{}, target, ref)
					b.removed[target] = who
				}
			} else {
				e := b.emit(at, actor, "assignee.add", entity.Str(who), "", ref)
				assignees.add(who, e.ID)
			}
		case giteaapi.TLTitle:
			b.emit(at, actor, "title", entity.Str(item.NewTitle), "", ref)
		case giteaapi.TLClose:
			b.emit(at, actor, "status", entity.Str(StatusClosed), "", ref)
		case giteaapi.TLReopen:
			b.emit(at, actor, "status", entity.Str(issue.StatusOpen), "", ref)
		case giteaapi.TLMilestone:
			if item.Milestone != nil && item.Milestone.Title != "" {
				b.emit(at, actor, "milestone", entity.Str(item.Milestone.Title), "", ref)
			} else {
				b.emit(at, actor, "milestone", entity.Null(), "", ref)
			}
		case giteaapi.TLLock:
			b.emit(at, actor, "locked", entity.Bool(true), "", ref)
		case giteaapi.TLUnlock:
			b.emit(at, actor, "locked", entity.Bool(false), "", ref)
		case giteaapi.TLPin:
			b.emit(at, actor, "pinned", entity.Bool(true), "", ref)
		case giteaapi.TLUnpin:
			b.emit(at, actor, "pinned", entity.Bool(false), "", ref)
		}
	}

	b.survivingLabels = labels.surviving()
	b.survivingAssignees = assignees.surviving()
	b.sawClose = b.hasType(giteaapi.TLClose, giteaapi.TLReopen)
	b.sawMilestone = b.hasType(giteaapi.TLMilestone)
}

// entryRef is the stable nonce input for a timeline entry: the issue's identity
// and the entry's id.
func (b *builder) entryRef(e giteaapi.TimelineEntry) string {
	return fmt.Sprintf("%s/timeline/%d", b.origin, e.ID)
}

func (b *builder) hasType(types ...string) bool {
	for _, e := range b.iss.Timeline {
		for _, want := range types {
			if e.Type == want {
				return true
			}
		}
	}
	return false
}

// reconcile fills in what the timeline did not account for.
//
// The timeline is authoritative where it speaks, but an issue that predates an
// entry type, or one whose history was lost, can be labelled without ever
// having been `label`ed. Anything Gitea reports as currently true and the
// replay did not produce is added at creation time, the earliest moment it can
// be attributed to. Each such event's nonce derives from the issue's identity
// and the field, so a second import produces the same bytes and adds nothing.
func (b *builder) reconcile() {
	created, author := b.iss.CreatedAt, Author(b.iss.User)
	b.cur = b.create

	for _, l := range b.iss.Labels {
		if !b.survivingLabels[l.Name] {
			b.emit(created, author, "label.add", entity.Str(l.Name), "", b.origin+":label.add:"+l.Name)
		}
	}
	for _, u := range b.iss.Assignees {
		name := u.Name()
		if name != "" && !b.survivingAssignees[name] {
			b.emit(created, author, "assignee.add", entity.Str(name), "", b.origin+":assignee.add:"+name)
		}
	}
	if !b.sawMilestone && b.iss.Milestone != nil && b.iss.Milestone.Title != "" {
		b.emitField(created, author, "milestone", entity.Str(b.iss.Milestone.Title))
	}

	// blocked-by comes only from the current dependency list — see timeline().
	// The nonce keys on the upstream number rather than the resolved entity id,
	// so it does not change when the ledger learns the target on a second pass.
	seen := map[string]bool{}
	for _, dep := range b.iss.Dependencies {
		target, ok := b.entity(dep)
		if !ok {
			continue
		}
		key := relKey(issue.KindBlockedBy, target)
		if seen[key] {
			continue
		}
		seen[key] = true
		b.emit(created, author, issue.RelAdd, entity.Str(issue.KindBlockedBy), target,
			fmt.Sprintf("%s:%s:%s:%d", b.origin, issue.RelAdd, issue.KindBlockedBy, dep))
	}

	if !b.sawClose && b.iss.State == "closed" {
		b.emit(created, author, "status", entity.Str(StatusClosed), "", b.origin+":status:closed")
	}
}

// entity is the local entity a linked issue is filed as, if this clone holds
// it, counting the links it could not answer for.
func (b *builder) entity(number int64) (string, bool) {
	if number == 0 {
		return "", false
	}
	var id string
	var ok bool
	if b.known != nil {
		id, ok = b.known.Entity(b.target.Origin(number))
	}
	if !ok {
		b.unresolved++
	}
	return id, ok
}

func relKey(kind, target string) string {
	return issue.Relation{Kind: kind, Target: target}.Key()
}

// actions turns the collected groups into the units Apply commits, oldest
// first, each carrying the title the issue answered to at the time.
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
	if b.iss.HTMLURL != "" {
		trailers = []issue.Trailer{{Key: "Origin", Value: b.iss.HTMLURL}}
	}

	title := ""
	out := make([]entity.Action, 0, len(groups))
	for _, g := range groups {
		if len(g.events) == 0 {
			continue
		}
		out = append(out, entity.Action{
			Author: Identity(g.login, b.target.Host, g.at),
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

// emit builds one event and appends it. `c` is the upstream timestamp, not a
// position in the feed: re-import must reproduce every event's bytes exactly, so
// `c` has to be a pure function of one upstream event and never of its
// neighbours.
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
// rather than from an upstream event of its own.
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

// Vocabulary is this bridge's contribution to the fold, and it is empty. The
// identity and the URL live on the origin ledger; nothing Gitea holds needs a
// namespaced operation. It stays declared so the absence is deliberate and so
// the composition in cmd/git-issue's open() has a seam to use.
var Vocabulary = entity.Vocabulary{}

// Nonce derives an event's nonce from stable upstream identity: the first 16
// hex characters of its SHA-256, per docs/storage-model.md.
func Nonce(input string) string {
	sum := sha256.Sum256([]byte(input))
	return hex.EncodeToString(sum[:])[:16]
}

// login normalises a Gitea login, substituting Gitea's own name for a deleted
// account.
func login(u giteaapi.User) string {
	if n := u.Name(); n != "" {
		return n
	}
	return "ghost"
}

// Author maps a Gitea user onto an event author.
//
// The scheme prefix matters as much as the login: `a` is part of the event's
// hashed bytes, so two implementations spelling the same author differently
// would produce different ids for the same event and stop converging.
func Author(u giteaapi.User) string { return "gitea:" + login(u) }

// Identity maps a Gitea login onto the author of a commit.
//
// Gitea often exposes no usable email, so the address is synthesised from the
// login and the host — enough for `git shortlog` and `git log --author` to work
// on the tracker's history. It is a bridge-asserted label, not proof of
// anything: the authoritative record of who wrote an event is the `a` field
// inside it. Nothing hashes this.
func Identity(l, host string, at time.Time) gitx.Identity {
	if l == "" {
		l = "ghost"
	}
	return gitx.Identity{Name: l, Email: l + "@" + hostLabel(host), When: at}
}

func hostLabel(host string) string {
	if h, _, ok := strings.Cut(host, ":"); ok {
		return h
	}
	return host
}
