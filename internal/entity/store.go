package entity

import (
	"bytes"
	"fmt"
	"strings"

	"github.com/hdweiss/git-issue/internal/gitx"
)

// Store is one notes ref: it reads the ref in a single batch, parses each
// blob's lines, and hands back folded state.
//
// Entity-agnostic — the caller supplies the ref and the vocabulary — and the
// only handle a command needs, so nothing above this layer talks to git
// directly.
type Store struct {
	Repo  *gitx.Repo
	Ref   string
	Vocab Vocabulary
}

func NewStore(repo *gitx.Repo, ref string, vocab Vocabulary) *Store {
	return &Store{Repo: repo, Ref: ref, Vocab: vocab}
}

// OpenStore opens the repository at dir ("" for the working directory) and
// returns a store over one of its notes refs.
func OpenStore(dir, ref string, vocab Vocabulary) (*Store, error) {
	repo, err := gitx.Open(dir)
	if err != nil {
		return nil, err
	}
	return NewStore(repo, ref, vocab), nil
}

// Format is the repository's hash algorithm, which fixes the width of every
// entity id: notes keys are object names (docs/storage-model.md).
func (s *Store) Format() ObjectFormat { return ObjectFormat(s.Repo.ObjectFormat) }

// Notes enumerates the ref.
func (s *Store) Notes() ([]gitx.Note, error) { return s.Repo.NotesList(s.Ref) }

// IDs is the entity id of each note, which is the set an abbreviation competes
// against in Resolve.
func IDs(notes []gitx.Note) []string {
	ids := make([]string, 0, len(notes))
	for _, n := range notes {
		ids = append(ids, n.Entity)
	}
	return ids
}

// Writing goes through Apply, in sync.go: it is the only path that produces a
// commit saying who did what, and the only one that decides where a new
// entity's blob is placed in the tree. There is deliberately no Add or Append
// here to go round it.

// Remove drops an entity's tree entry.
func (s *Store) Remove(id string) error { return s.Repo.NotesRemove(s.Ref, id) }

// Destroy deletes the ref outright: every entity on it, and its whole history,
// gone in one step that cannot be pulled back and does not converge. Nothing in
// the normal vocabulary does this — it is the hidden `destroy` command's only
// job, for tearing down a test repository or abandoning a botched import.
func (s *Store) Destroy() error { return s.Repo.DeleteRef(s.FullRef()) }

// Each folds the given notes and calls fn for each entity, in the order the
// batch returns them. An entity with no interpretable events is skipped: a
// blob written entirely by a newer client has nothing this one can say about
// it, and inventing a blank row would be worse than omitting it.
//
// Blobs are handed over one at a time rather than accumulated, so memory stays
// proportional to the largest entity rather than to the whole tracker.
func (s *Store) Each(notes []gitx.Note, fn func(id string, st State) error) error {
	return s.Repo.ReadNotes(notes, func(b gitx.Blob) error {
		events := s.parse(b.Body)
		if len(events) == 0 {
			return nil
		}
		return fn(b.Entity, Fold(s.Vocab, events))
	})
}

// Load folds a single entity.
func (s *Store) Load(note gitx.Note) (State, error) {
	var st State
	found := false
	err := s.Each([]gitx.Note{note}, func(_ string, folded State) error {
		st, found = folded, true
		return nil
	})
	if err != nil {
		return st, err
	}
	if !found {
		return st, fmt.Errorf("no readable events in %s", note.Entity)
	}
	return st, nil
}

func (s *Store) parse(body []byte) []Event {
	format := s.Format()
	var events []Event
	for _, line := range bytes.Split(body, []byte("\n")) {
		if len(line) == 0 {
			continue
		}
		if ev, ok := ParseEvent(format, line); ok {
			events = append(events, ev)
		}
	}
	return events
}

// Find resolves an abbreviated id and folds the entity it names, returning
// the full id alongside the state. Callers that only want one entity need
// nothing else from this package.
func (s *Store) Find(prefix, kind string) (string, State, error) {
	note, err := s.Resolve(prefix, kind)
	if err != nil {
		return "", State{}, err
	}
	st, err := s.Load(note)
	if err != nil {
		return "", State{}, err
	}
	return note.Entity, st, nil
}

// Resolve matches an abbreviated entity id against the ref.
//
// Ids abbreviate like object names do, so an ambiguous prefix is refused
// rather than resolved arbitrarily. kind names the entity type for the error
// message ("issue", later "pr").
func (s *Store) Resolve(prefix, kind string) (gitx.Note, error) {
	notes, err := s.Notes()
	if err != nil {
		return gitx.Note{}, err
	}

	var matches []gitx.Note
	for _, n := range notes {
		if strings.HasPrefix(n.Entity, prefix) {
			matches = append(matches, n)
		}
	}

	switch len(matches) {
	case 1:
		return matches[0], nil
	case 0:
		return gitx.Note{}, fmt.Errorf("unknown %s '%s'", kind, prefix)
	default:
		return gitx.Note{}, fmt.Errorf("ambiguous %s '%s': matches %d %ss", kind, prefix, len(matches), kind)
	}
}
