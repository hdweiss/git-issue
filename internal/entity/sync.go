// Sync against another copy of the same notes ref, per docs/storage-model.md:
// an ordinary fetch, then a set union of lines. Entity-agnostic, like the rest
// of this package — the caller supplies the ref.

package entity

import (
	"bytes"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/gitx"
)

// ErrNoRemoteRef reports that the remote has no copy of this ref at all. It is
// worth distinguishing from a transport failure: for a remote that is really a
// forge, it usually means the issues live there under a bridge rather than in
// git refs, and the caller can say so.
var ErrNoRemoteRef = errors.New("remote has no copy of this ref")

// ChangeKind is git's own status letter for a path that differs between two
// trees, which is exactly the granularity a notes tree gives: one path per
// entity.
type ChangeKind byte

const (
	Added   ChangeKind = 'A'
	Updated ChangeKind = 'M'
	Removed ChangeKind = 'D'
)

// Change is one entity whose blob differs between the ref before a pull and
// the ref after it.
type Change struct {
	ID   string
	Kind ChangeKind

	// spec is where this entity's blob can still be read from: the new ref for
	// an add or an update, the old one for a removal. Both are `<rev>:<path>`,
	// which cat-file --batch resolves like any other object name — so a
	// removed entity is still renderable without a second mechanism.
	spec string

	// prevSpec is the same entity as it stood before, set only for an update.
	// A blob can grow without anything a reader sees changing — a bridge
	// re-importing a change this clone pushed writes the tracker's own event
	// for it, saying what the blob already said — so a report needs both sides
	// to tell a real change from an echo.
	prevSpec string
}

// Pull is the outcome of one sync, including where the ref stood before and
// after so a caller can report it the way git reports a fetch.
type Pull struct {
	Remote   string
	Ref      string // the local ref, in full
	Tracking string // the remote-tracking ref it was fetched into
	Old, New string // the local ref's commit, before and after
	Changes  []Change

	// FF is whether the ref advanced by fast-forward rather than by a merge
	// commit — what tells a report to print git's "Updating x..y" and
	// "Fast-forward" pair rather than its merge line. A first pull, with no
	// prior commit to move from, is neither.
	FF bool

	// Commits is how many this write added to the ref, one per action. A git
	// pull adds none of its own — it merges what the remote already wrote — so
	// this is zero there and nonzero only for Apply.
	Commits int

	store *Store
}

// UpToDate reports that the pull moved nothing.
func (p Pull) UpToDate() bool { return p.Old == p.New }

// Count returns how many changes are of the given kind.
func (p Pull) Count(kind ChangeKind) int {
	n := 0
	for _, c := range p.Changes {
		if c.Kind == kind {
			n++
		}
	}
	return n
}

// Each folds every changed entity and calls fn, in one batched read.
//
// A removal is folded from the ref as it stood before the pull, so it can be
// described in the same terms as everything else rather than reported as a
// bare id.
func (p Pull) Each(fn func(Change, State) error) error {
	if len(p.Changes) == 0 {
		return nil
	}
	byID := make(map[string]Change, len(p.Changes))
	notes := make([]gitx.Note, 0, len(p.Changes))
	for _, c := range p.Changes {
		byID[c.ID] = c
		notes = append(notes, gitx.Note{Blob: c.spec, Entity: c.ID})
	}
	return p.store.Each(notes, func(id string, st State) error {
		return fn(byID[id], st)
	})
}

// Previous folds each updated entity as it stood before, keyed by id, in one
// batched read. Added and removed entities have no previous state and are
// absent.
func (p Pull) Previous() (map[string]State, error) {
	var notes []gitx.Note
	for _, c := range p.Changes {
		if c.Kind == Updated && c.prevSpec != "" {
			notes = append(notes, gitx.Note{Blob: c.prevSpec, Entity: c.ID})
		}
	}
	out := make(map[string]State, len(notes))
	if len(notes) == 0 {
		return out, nil
	}
	err := p.store.Each(notes, func(id string, st State) error {
		out[id] = st
		return nil
	})
	return out, err
}

// FullRef is the store's ref as a complete ref name. A notes ref may be named
// either way; git's own --ref accepts both.
func (s *Store) FullRef() string {
	if strings.HasPrefix(s.Ref, "refs/") {
		return s.Ref
	}
	return "refs/notes/" + s.Ref
}

// TrackingRef is where a remote's copy of this ref is kept locally.
//
// It lives under refs/remotes/ rather than inside refs/notes/ deliberately:
// a wildcard `git push origin refs/notes/*:refs/notes/*` is a thing people do,
// and it must not publish one clone's idea of what another remote holds.
func (s *Store) TrackingRef(remote string) string {
	return "refs/remotes/" + remote + "/notes/" + strings.TrimPrefix(s.FullRef(), "refs/notes/")
}

// PullGit fetches a remote's copy of this ref and unions it into the local
// one, reporting what changed.
//
// The fetch lands on a remote-tracking ref rather than on the live ref, which
// is what makes the before/after diff — and therefore the summary — possible
// at all.
func (s *Store) PullGit(remote string) (Pull, error) {
	p := Pull{
		Remote:   remote,
		Ref:      s.FullRef(),
		Tracking: s.TrackingRef(remote),
		store:    s,
	}
	p.Old = s.Repo.RefSHA(p.Ref)

	if err := s.Repo.Fetch(remote, p.Ref+":"+p.Tracking); err != nil {
		if strings.Contains(err.Error(), "couldn't find remote ref") {
			return p, fmt.Errorf("%s: %w", remote, ErrNoRemoteRef)
		}
		return p, err
	}
	if err := s.Repo.NotesMerge(s.Ref, p.Tracking); err != nil {
		return p, err
	}
	if err := p.diff(); err != nil {
		return p, err
	}

	// git notes merge fast-forwards exactly when the local ref was behind the
	// fetched one, which leaves the local ref pointing straight at it; a
	// divergent merge leaves it on a new two-parent commit instead.
	p.FF = p.Old != "" && s.Repo.RefSHA(p.Tracking) == p.New
	return p, nil
}

// ErrNonFastForward reports that the remote holds entities this clone has not
// seen, so pushing would have to discard them.
//
// The remedy is always the same and always terminates: pull, which unions the
// two, then push. There is deliberately no force — both refs this program
// pushes are grow-only sets of lines, so there is no case where overwriting the
// remote is the right answer rather than a way to lose somebody's write.
var ErrNonFastForward = errors.New("remote has changes this clone does not")

// PushGit sends this ref to a remote and reports what the remote gained, in the
// same terms a pull reports what this clone gained.
//
// It fetches first. That is not caution but arithmetic: the summary is a diff
// against what the remote actually holds, and without fetching there is nothing
// local to diff against — the tracking ref could be arbitrarily stale, and a
// push that reported the wrong set of entities would be worse than one that
// reported none.
func (s *Store) PushGit(remote string) (Pull, error) {
	p := Pull{
		Remote:   remote,
		Ref:      s.FullRef(),
		Tracking: s.TrackingRef(remote),
		store:    s,
	}

	if err := s.Repo.Fetch(remote, p.Ref+":"+p.Tracking); err != nil {
		// A remote with no copy of this ref is the ordinary first push, not a
		// failure. Anything else — no such remote, no network, no credential —
		// is.
		if !strings.Contains(err.Error(), "couldn't find remote ref") {
			return p, err
		}
	}

	// Old is what the remote holds and New is what this clone holds: the
	// mirror image of a pull, which is exactly what makes the summary read the
	// same way round.
	p.Old = s.Repo.RefSHA(p.Tracking)
	p.New = s.Repo.RefSHA(p.Ref)
	if p.New == "" || p.Old == p.New {
		p.New = p.Old
		return p, nil
	}
	if !s.Repo.IsAncestor(p.Old, p.New) {
		return p, fmt.Errorf("%s: %w", remote, ErrNonFastForward)
	}

	changes, err := s.Repo.DiffTree(p.Old, p.New)
	if err != nil {
		return p, err
	}
	for _, c := range changes {
		from := p.New
		if ChangeKind(c.Status) == Removed {
			from = p.Old
		}
		prev := ""
		if ChangeKind(c.Status) == Updated {
			prev = p.Old + ":" + c.Path
		}
		p.Changes = append(p.Changes, Change{
			ID:       strings.ReplaceAll(c.Path, "/", ""),
			Kind:     ChangeKind(c.Status),
			spec:     from + ":" + c.Path,
			prevSpec: prev,
		})
	}

	if err := s.Repo.Push(remote, p.Ref+":"+p.Ref); err != nil {
		return p, err
	}
	// git updates refs/remotes/* only for refs a configured refspec covers, and
	// no default one covers a notes ref, so the tracking ref is moved by hand.
	// Left behind, it would make the next push re-report everything just sent.
	if err := s.Repo.UpdateRef(p.Tracking, p.New); err != nil {
		return p, err
	}
	p.FF = p.Old != ""
	return p, nil
}

// diff reads back what moved between Old and New.
//
// The diff is the whole reporting story and it is nearly free: at 20,000
// entities it takes 3ms and names exactly the entities that changed, so
// neither a pull nor an import has to track what it touched.
func (p *Pull) diff() error {
	p.New = p.store.Repo.RefSHA(p.Ref)
	changes, err := p.store.Repo.DiffTree(p.Old, p.New)
	if err != nil {
		return err
	}
	for _, c := range changes {
		// Notes trees are sharded by git into 00-ff subtrees as they grow, so
		// a path is the entity id with fanout separators in it.
		from := p.New
		if ChangeKind(c.Status) == Removed {
			from = p.Old
		}
		prev := ""
		if ChangeKind(c.Status) == Updated {
			prev = p.Old + ":" + c.Path
		}
		p.Changes = append(p.Changes, Change{
			ID:       strings.ReplaceAll(c.Path, "/", ""),
			Kind:     ChangeKind(c.Status),
			spec:     from + ":" + c.Path,
			prevSpec: prev,
		})
	}
	return nil
}

// Action is one thing somebody did: the events it produced, who did it and
// when, and what a commit should say about it.
//
// It is the unit of a commit. Not the event — an issue filed with a title, a
// type and two labels is four events and one action — and not the sync run,
// which would collapse everyone's changes onto whoever happened to run it.
// docs/storage-model.md asks for exactly this granularity so that a commit
// carries a real author and timestamp rather than a bot's.
type Action struct {
	Author  gitx.Identity
	Message string
	Events  []Event
}

// Incoming is one entity a write produced: its id, and the actions to file
// under it, oldest first.
type Incoming struct {
	ID      string
	Actions []Action
}

// step is one action that turned out to carry events the blob does not already
// have — that is, one commit to write.
type step struct {
	entity  int // index into incoming
	action  int // index into that entity's actions
	when    time.Time
	novel   []Event
	message string
	author  gitx.Identity
}

// Apply writes entities onto the ref, one commit per action, reporting what
// changed in the same terms a pull does.
//
// Union, not overwrite. An entity blob is a grow-only set of lines
// (docs/blob-format.md), so an import that already ran adds nothing the second
// time, and lines written by a client that knows operations this one does not
// survive untouched — which is the preservation invariant, and the reason
// nothing here re-serializes a line it did not itself create.
//
// The union is also what keeps a resumed import from writing empty commits: an
// action whose events are all present already produces no commit at all, so a
// re-import moves the ref exactly as far as it moves the blobs, which is not at
// all.
func (s *Store) Apply(source string, incoming []Incoming) (Pull, error) {
	p := Pull{Remote: source, Ref: s.FullRef(), store: s}
	p.Old = s.Repo.RefSHA(p.Ref)
	if len(incoming) == 0 {
		p.New = p.Old
		return p, nil
	}

	paths, err := s.Repo.NotePaths(p.Ref)
	if err != nil {
		return p, err
	}

	// Read every existing blob these entities already have, in one batch.
	existing := make(map[string][]byte, len(incoming))
	var known []gitx.Note
	for _, in := range incoming {
		if path, ok := paths[in.ID]; ok {
			known = append(known, gitx.Note{Blob: p.Old + ":" + path, Entity: in.ID})
		}
	}
	if err := s.Repo.ReadNotes(known, func(b gitx.Blob) error {
		existing[b.Entity] = append([]byte(nil), b.Body...)
		return nil
	}); err != nil {
		return p, err
	}

	steps, fresh := plan(incoming, existing)
	if len(steps) == 0 {
		p.New = p.Old
		return p, nil
	}

	// Where a new entity's blob goes depends on how big the tree is about to
	// be, so it is decided once here rather than per commit.
	total := len(paths) + fresh
	place := make(map[string]string, len(incoming))
	for _, in := range incoming {
		place[in.ID] = gitx.NotePath(paths, in.ID, total)
	}

	// The blob as it stands at each point in its life, grown one action at a
	// time. Only the final versions are held: each commit's body is handed to
	// the stream and forgotten.
	body := make(map[string][]byte, len(incoming))
	for _, in := range incoming {
		body[in.ID] = terminated(existing[in.ID])
	}

	err = s.Repo.WriteNotes(p.Ref, func(stream *gitx.Stream) error {
		for _, st := range steps {
			id := incoming[st.entity].ID
			b := body[id]
			for _, e := range st.novel {
				b = append(b, e.Raw...)
				b = append(b, '\n')
			}
			body[id] = b
			// Commit consumes the body before it returns, so the growing
			// buffer is handed over as it is. Copying instead would allocate a
			// whole blob per commit, which at import scale is the write
			// amplification all over again, in memory.
			if err := stream.Commit(gitx.Commit{
				Author:  st.author,
				Message: st.message,
				Writes:  []gitx.FileWrite{{Path: place[id], Body: b}},
			}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return p, err
	}
	if err := p.diff(); err != nil {
		return p, err
	}
	// Commits stacked on the ref's own tip: a fast-forward, unless there was
	// no tip to stack on.
	p.FF = p.Old != ""
	p.Commits = len(steps)
	return p, nil
}

// plan works out which actions have anything new to say, and in which order to
// commit them. It also reports how many entities the ref does not hold yet,
// which is what decides where their blobs are placed.
//
// Commits come out in upstream chronological order across every entity, not
// entity by entity. Both cost the same to write — measured: 5.8s against 5.2s
// for 50,000 commits, with identical packed size — and only one of them
// produces a log that reads as a history of the tracker rather than as a list
// of issues each replayed from birth.
func plan(incoming []Incoming, existing map[string][]byte) ([]step, int) {
	var steps []step
	fresh := 0

	for i, in := range incoming {
		blob, held := existing[in.ID]
		if !held {
			fresh++
		}
		present := make(map[string]bool)
		for _, line := range bytes.Split(blob, []byte("\n")) {
			if len(line) > 0 {
				present[string(line)] = true
			}
		}

		for j, act := range in.Actions {
			// Comparison is on whole canonical lines because that is what
			// identity means here: an event is its bytes, and two lines that
			// differ anywhere are two events.
			var novel []Event
			for _, e := range act.Events {
				if present[string(e.Raw)] {
					continue
				}
				present[string(e.Raw)] = true
				novel = append(novel, e)
			}
			if len(novel) == 0 {
				continue
			}
			steps = append(steps, step{
				entity:  i,
				action:  j,
				when:    act.Author.When,
				novel:   novel,
				message: act.Message,
				author:  act.Author,
			})
		}
	}

	// Ties break on the entity and then on position within it, so an entity's
	// own actions never come out reordered — several of them commonly share a
	// timestamp, and a create must not land after the comment that follows it.
	sort.SliceStable(steps, func(a, b int) bool {
		x, y := steps[a], steps[b]
		if !x.when.Equal(y.when) {
			return x.when.Before(y.when)
		}
		if x.entity != y.entity {
			return incoming[x.entity].ID < incoming[y.entity].ID
		}
		return x.action < y.action
	})
	return steps, fresh
}

// terminated is a blob safe to append a line to: existing bytes untouched, with
// the newline the last line may be missing.
func terminated(b []byte) []byte {
	out := bytes.Clone(b)
	if len(out) > 0 && !bytes.HasSuffix(out, []byte("\n")) {
		out = append(out, '\n')
	}
	return out
}
