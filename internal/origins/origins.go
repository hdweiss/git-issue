// Package origins is the origin ledger of docs/storage-model.md: which upstream
// object each local entity corresponds to, in each external tracker.
//
// It is deliberately not part of the entity blobs. That correspondence is not a
// fact about the entity — stop caring about a fork and its mapping is worthless
// — so it lives on its own ref, one blob per tracker, where a tracker can be
// dropped by deleting one path instead of by rewriting every entity.
//
// A leaf, like gitx below it: this package knows entity ids and tracker names as
// opaque strings, and has never heard of an event, a fold, or a platform.
package origins

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"github.com/hdweiss/git-issue/internal/gitx"
)

// Ref is where the ledger lives.
//
// Not under refs/notes/: git resolves a notes ref by prefixing anything that
// does not already begin with refs/notes/, so a ledger placed there would be at
// the mercy of tooling that assumes object-name keys — and every `git notes`
// command re-shards the tree it touches into hex fanout, which would destroy a
// tree keyed by tracker name. See docs/storage-model.md.
const Ref = "refs/git-issue/origins"

// The line kinds this package understands. Anything else is preserved and
// ignored, which is what lets a new kind — an identity map, say — be added
// without a format change.
const (
	KindIssue   = "issue"   // <entity id> <upstream id>
	KindReview  = "review"  // <entity id> <upstream id>
	KindURL     = "url"     // <entity id> <url>
	KindComment = "comment" // <entity id> <local comment event id> <upstream id>
	KindThread  = "thread"  // <entity id> <local root comment event id> <upstream id>
	KindVerdict = "verdict" // <entity id> <local verdict.add event id> <upstream review id>
)

// entityKinds are the line kinds that map an entity onto an upstream object.
// They index into one namespace deliberately: ids are content hashes and are
// unique across entity types without coordination, so "which local entity is
// this upstream object" has one answer regardless of what type it turned out to
// be — which is what lets a review resolve the issue it closes through the same
// ledger.
var entityKinds = []string{KindIssue, KindReview}

// Store reads and writes the ledger ref.
type Store struct {
	Repo *gitx.Repo
	Ref  string
}

// NewStore opens the ledger on its usual ref.
func NewStore(repo *gitx.Repo) *Store { return &Store{Repo: repo, Ref: Ref} }

// Destroy removes the ledger ref and any journal a torn run left behind,
// reporting whether either was there.
//
// For when the entities being mapped are deleted outright. Every line here is a
// correspondence between a local entity and an upstream object, so once the
// local side is gone the line maps nothing — and worse, still claims an entity
// id that no longer exists, which a later import would file its issues under.
//
// The ledger is rebuilt by the next import at no cost, which is what makes this
// safe: the mappings are re-derived from what upstream says, not lost.
func (s *Store) Destroy() (bool, error) {
	cleared := false
	if s.Repo.RefSHA(s.Ref) != "" {
		if err := s.Repo.DeleteRef(s.Ref); err != nil {
			return false, err
		}
		cleared = true
	}

	dir, err := s.Repo.CommonDir()
	if err != nil {
		return cleared, err
	}
	journals, err := clearJournals(dir)
	return cleared || journals, err
}

// Ledger is one tracker's mappings.
//
// Held as a set of whole lines, because that is what the merge is: a union of
// lines, deduped. Everything else here is an index over that set, rebuilt on
// load and maintained on Add.
type Ledger struct {
	// Tracker is the path this ledger occupies in the tree, and the name the
	// bridge knows the upstream repository by — "github.com/owner/name".
	Tracker string

	lines map[string]bool

	entityUpstream map[string]string // entity id -> upstream id
	upstreamEntity map[string]string // upstream id -> entity id
	url            map[string]string // entity id -> web URL
	comment        map[string]string // entity id + comment event id -> upstream id
	commentClaimed map[string]bool   // upstream comment id -> present
	thread         map[string]string // entity id + thread root event id -> upstream thread id
	verdict        map[string]string // entity id + verdict.add event id -> upstream review id
	verdictClaimed map[string]string // upstream review id -> local verdict.add event id

	warnings []string
}

func newLedger(tracker string) *Ledger {
	return &Ledger{
		Tracker:        tracker,
		lines:          map[string]bool{},
		entityUpstream: map[string]string{},
		upstreamEntity: map[string]string{},
		url:            map[string]string{},
		comment:        map[string]string{},
		commentClaimed: map[string]bool{},
		thread:         map[string]string{},
		verdict:        map[string]string{},
		verdictClaimed: map[string]string{},
	}
}

// Load reads one tracker's ledger. A tracker never recorded is an empty ledger,
// not an error.
func (s *Store) Load(tracker string) (*Ledger, error) {
	l := newLedger(tracker)
	body, err := s.Repo.ReadPath(s.Repo.RefSHA(s.Ref), tracker)
	if err != nil {
		return nil, err
	}
	l.parse(body)
	return l, nil
}

// Trackers lists every tracker the ledger holds mappings for.
func (s *Store) Trackers() ([]string, error) {
	paths, err := s.Repo.ListTree(s.Ref)
	if err != nil {
		return nil, err
	}
	sort.Strings(paths)
	return paths, nil
}

// Save writes the ledger back in a single commit. A ledger that gained nothing
// writes nothing, so a run with no new mappings does not move the ref.
func (s *Store) Save(l *Ledger, message string) error {
	current, err := s.Repo.ReadPath(s.Repo.RefSHA(s.Ref), l.Tracker)
	if err != nil {
		return err
	}
	body := l.Bytes()
	if string(current) == string(body) {
		return nil
	}
	return s.Repo.WriteRef(s.Ref, func(stream *gitx.Stream) error {
		return stream.Commit(gitx.Commit{
			Message: message,
			Writes:  []gitx.FileWrite{{Path: l.Tracker, Body: body}},
		})
	})
}

// Drop removes every mapping under a path — one tracker, or a whole host.
//
// This does not converge: a merge with a clone that still holds the tracker
// brings it back, the same way an entity removal does and for the same reason.
// Here that is arguably right, since the other clone does still have the
// relationship, and unlike an entity removal this is cheap to repeat.
func (s *Store) Drop(prefix, message string) error {
	paths, err := s.Trackers()
	if err != nil {
		return err
	}
	found := false
	for _, p := range paths {
		if p == prefix || strings.HasPrefix(p, prefix+"/") {
			found = true
			break
		}
	}
	if !found {
		return fmt.Errorf("no origins recorded for %s", prefix)
	}
	return s.Repo.WriteRef(s.Ref, func(stream *gitx.Stream) error {
		return stream.Commit(gitx.Commit{Message: message, Deletes: []string{prefix}})
	})
}

// TrackingRef is where a remote's copy of the ledger is kept locally, alongside
// the notes refs' own tracking refs and for the same reason: a wildcard push
// must never publish one clone's idea of what another remote holds.
func (s *Store) TrackingRef(remote string) string {
	return "refs/remotes/" + remote + "/" + strings.TrimPrefix(s.Ref, "refs/")
}

// Refspec is the ledger's push and fetch refspec.
func (s *Store) Refspec() string { return s.Ref + ":" + s.Ref }

// Pull fetches a remote's copy of the ledger and merges it in.
//
// A remote with no ledger at all is not a failure: it is a clone that has never
// pushed one, or a remote that is really a forge and holds issues behind an API
// rather than in refs.
func (s *Store) Pull(remote string) error {
	if err := s.Fetch(remote); err != nil {
		return err
	}
	return s.Merge(s.TrackingRef(remote))
}

// Fetch updates the remote-tracking copy of the ledger and merges nothing.
//
// Separate from Pull because a dry run needs the comparison without the
// consequence: a tracking ref is a cache of what the remote holds, so moving it
// changes nothing about this clone's own state, while merging would.
func (s *Store) Fetch(remote string) error {
	err := s.Repo.Fetch(remote, s.Ref+":"+s.TrackingRef(remote))
	if err != nil && strings.Contains(err.Error(), "couldn't find remote ref") {
		return nil
	}
	return err
}

// Merge unions another copy of the ledger into this one, per path.
//
// Lines are facts, so a union is the whole merge: there is no value-level
// conflict to resolve, and merging twice changes nothing the second time.
//
// The result records the other side as a parent even when it contributed no
// line. A merge that kept only its own history would leave the other side
// unreachable from this ref, so the next push would be rejected as a
// non-fast-forward with nothing actually in conflict.
func (s *Store) Merge(other string) error {
	theirs := s.Repo.RefSHA(other)
	if theirs == "" {
		return nil
	}
	mine := s.Repo.RefSHA(s.Ref)
	if mine == theirs || s.Repo.IsAncestor(theirs, mine) {
		return nil
	}
	// Nothing of our own to keep, or ours is already contained in theirs: take
	// their commit wholesale rather than building an identical merge.
	if mine == "" || s.Repo.IsAncestor(mine, theirs) {
		return s.Repo.UpdateRef(s.Ref, theirs)
	}

	// Divergent. Only paths on the other side can differ: ours are already the
	// base this commit is written on top of.
	paths, err := s.Repo.ListTree(other)
	if err != nil {
		return err
	}
	var writes []gitx.FileWrite
	for _, path := range paths {
		ours, err := s.Repo.ReadPath(mine, path)
		if err != nil {
			return err
		}
		yours, err := s.Repo.ReadPath(theirs, path)
		if err != nil {
			return err
		}
		l := newLedger(path)
		l.parse(ours)
		l.parse(yours)
		body := l.Bytes()
		if string(body) == string(ours) {
			continue
		}
		writes = append(writes, gitx.FileWrite{Path: path, Body: body})
	}
	return s.Repo.WriteRef(s.Ref, func(stream *gitx.Stream) error {
		return stream.Commit(gitx.Commit{
			Message: fmt.Sprintf("Merge origin ledger from %s\n", other),
			Writes:  writes,
			Merges:  []string{theirs},
		})
	})
}

// parse folds a blob's lines into the set and the indexes. It is additive, so
// calling it twice with two copies of the ledger is the merge.
func (l *Ledger) parse(body []byte) {
	for _, line := range strings.Split(string(body), "\n") {
		line = strings.TrimRight(line, "\r")
		if strings.TrimSpace(line) == "" {
			continue
		}
		l.insert(line)
	}
}

// insert adds one whole line and indexes it if its kind is one this client
// knows. An unrecognised kind is kept in the set untouched — the same
// preservation rule the event format has, and what a future kind depends on.
func (l *Ledger) insert(line string) {
	if l.lines[line] {
		return
	}
	l.lines[line] = true

	f := strings.Fields(line)
	switch {
	case len(f) == 3 && slices.Contains(entityKinds, f[0]):
		l.link(f[1], f[2])
	case len(f) == 3 && f[0] == KindURL:
		l.url[f[1]] = f[2]
	// A thread and a comment index the same way: both tie a local event id to
	// an upstream object, and both make that object claimed so a push's own
	// write does not come back as a second one.
	case len(f) == 4 && (f[0] == KindComment || f[0] == KindThread):
		l.comment[f[1]+" "+f[2]] = f[3]
		l.commentClaimed[f[3]] = true
		if f[0] == KindThread {
			l.thread[f[1]+" "+f[2]] = f[3]
		}
	// A verdict indexes on its own, not into the comment map. The upstream
	// object is a *review*, and a review with a body is also a comment: writing
	// both into one index would let a verdict's mapping answer "which comment is
	// this" and post the body twice.
	case len(f) == 4 && f[0] == KindVerdict:
		l.verdict[f[1]+" "+f[2]] = f[3]
		l.verdictClaimed[f[3]] = f[2]
	}
}

// link records an entity's upstream object, warning rather than failing when
// the ledger already says something different.
//
// Both directions are worth checking, and both mean a duplicate exists
// somewhere that a human has to look at: one entity filed twice upstream, or two
// entities pointing at one upstream object.
func (l *Ledger) link(entity, upstream string) {
	if prev, ok := l.entityUpstream[entity]; ok && prev != upstream {
		l.warnf("%s is mapped to both %s and %s in %s", short(entity), prev, upstream, l.Tracker)
	}
	if prev, ok := l.upstreamEntity[upstream]; ok && prev != entity {
		l.warnf("%s in %s is mapped from both %s and %s", upstream, l.Tracker, short(prev), short(entity))
	}
	l.entityUpstream[entity] = upstream
	l.upstreamEntity[upstream] = entity
}

func (l *Ledger) warnf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	for _, have := range l.warnings {
		if have == msg {
			return
		}
	}
	l.warnings = append(l.warnings, msg)
}

// Warnings reports duplicate mappings found while reading. They are never
// errors: the ledger still resolves, and what they need is a person.
func (l *Ledger) Warnings() []string { return l.warnings }

// Upstream is the object this entity corresponds to in this tracker.
func (l *Ledger) Upstream(entity string) (string, bool) {
	v, ok := l.entityUpstream[entity]
	return v, ok
}

// Entity is the local entity an upstream object corresponds to. This is the
// question an import asks, and answering it from one blob rather than by
// folding every entity in the repository is why the ledger is a ledger.
func (l *Ledger) Entity(upstream string) (string, bool) {
	v, ok := l.upstreamEntity[upstream]
	return v, ok
}

// URL is the entity's web address in this tracker, a locator only.
func (l *Ledger) URL(entity string) (string, bool) {
	v, ok := l.url[entity]
	return v, ok
}

// Thread is the upstream review thread a local thread root corresponds to,
// which is what a push needs in order to resolve one.
func (l *Ledger) Thread(entity, root string) (string, bool) {
	v, ok := l.thread[entity+" "+root]
	return v, ok
}

// Comment is the upstream object a local thread entry was posted as.
func (l *Ledger) Comment(entity, event string) (string, bool) {
	v, ok := l.comment[entity+" "+event]
	return v, ok
}

// ClaimedComment reports whether an upstream comment is already present locally
// as an entry this repository authored, which is what keeps a pushed comment
// from coming back as a second one.
func (l *Ledger) ClaimedComment(upstream string) bool { return l.commentClaimed[upstream] }

// Verdict is the upstream review a local verdict was submitted as, which is
// what a push needs in order to dismiss one and in order not to cast it twice.
func (l *Ledger) Verdict(entity, event string) (string, bool) {
	v, ok := l.verdict[entity+" "+event]
	return v, ok
}

// ClaimedVerdict is the local verdict an upstream review was submitted as, and
// whether this repository submitted it.
//
// It answers with the event id rather than with a bare yes, because an import
// that skips writing the verdict again still has to be able to replay a
// *dismissal* of it — and a dismissal names the add it undoes.
func (l *Ledger) ClaimedVerdict(upstream string) (string, bool) {
	v, ok := l.verdictClaimed[upstream]
	return v, ok
}

// Forget drops every mapping about the entities that lines of the given kind
// name — `origins.KindReview` for the reviews, leaving the issues alone — and
// reports how many lines went.
//
// The unit is the entity rather than the line, because the lines about one
// entity are not independent. A `comment` line says an upstream comment is
// already here, which is what stops a push's own writes coming back as second
// copies; left behind after the entity it belongs to is gone, it tells the next
// import to skip a comment this clone no longer holds. A stale mapping is
// recoverable; a silently skipped comment is not.
//
// Lines of a kind this build does not recognise are kept. Their second field may
// not be an entity id at all, and guessing is what the preservation rule exists
// to prevent.
func (l *Ledger) Forget(kind string) int {
	ids := map[string]bool{}
	for line := range l.lines {
		if f := strings.Fields(line); len(f) >= 2 && f[0] == kind {
			ids[f[1]] = true
		}
	}
	if len(ids) == 0 {
		return 0
	}

	kept, removed := newLedger(l.Tracker), 0
	for line := range l.lines {
		f := strings.Fields(line)
		if len(f) >= 2 && ids[f[1]] && knownKind(f[0]) {
			removed++
			continue
		}
		kept.insert(line)
	}
	*l = *kept
	return removed
}

// knownKind reports whether this build understands a line kind well enough to
// say what it is about.
func knownKind(kind string) bool {
	return slices.Contains(entityKinds, kind) ||
		kind == KindURL || kind == KindComment || kind == KindThread || kind == KindVerdict
}

// Add records a mapping. Adding one that is already there changes nothing.
func (l *Ledger) Add(kind string, fields ...string) {
	l.insert(Line(kind, fields...))
}

// Line renders one mapping. Fields are whitespace-separated, so none of them
// may contain whitespace — entity ids, upstream ids and URLs do not.
func Line(kind string, fields ...string) string {
	return strings.Join(append([]string{kind}, fields...), " ")
}

// Len is how many mappings the ledger holds.
func (l *Ledger) Len() int { return len(l.lines) }

// Bytes renders the ledger, sorted.
//
// Sorted because the set is a map and Go randomises map order, which would make
// consecutive writes of identical content produce different blobs. Order carries
// no meaning either way — every line stands alone.
func (l *Ledger) Bytes() []byte {
	if len(l.lines) == 0 {
		return nil
	}
	out := make([]string, 0, len(l.lines))
	for line := range l.lines {
		out = append(out, line)
	}
	sort.Strings(out)
	return []byte(strings.Join(out, "\n") + "\n")
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
