// How far a bridge has read, so the next import can ask for a window instead
// of the whole tracker. Local, derived and disposable, per the index rule in
// docs/storage-model.md.

package entity

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// SyncState records, per source, the point up to which everything has already
// been imported.
//
// It lives in the git directory and is never written to a ref, because it is
// not tracker state: it says what *this* clone has already read, which is true
// of nowhere else and is worthless to anybody it might be pushed to. Deleting
// it costs one full re-import and nothing else — every entity's real state is
// in the blobs, and a re-import converges on what is already there.
//
// Deliberately not derived from the notes ref's own last commit. That moves
// when anyone writes locally, which says nothing about how far upstream was
// read, and it carries the local clock rather than the source's.
type SyncState struct {
	path    string
	Sources map[string]string `json:"sources"`
}

const syncStateFile = "sync.json"

// SyncState loads the record, treating any unreadable or unparseable file as
// empty. A corrupt watermark must degrade to a full import, never to an error:
// the file is a cache, and refusing to sync because a cache is malformed would
// be strictly worse than ignoring it.
func (s *Store) SyncState() (*SyncState, error) {
	dir, err := s.Repo.CommonDir()
	if err != nil {
		return nil, err
	}
	state := &SyncState{
		path:    filepath.Join(dir, "git-issue", syncStateFile),
		Sources: map[string]string{},
	}
	body, err := os.ReadFile(state.path)
	if err != nil {
		return state, nil
	}
	var loaded SyncState
	if err := json.Unmarshal(body, &loaded); err == nil && loaded.Sources != nil {
		state.Sources = loaded.Sources
	}
	return state, nil
}

// Clear removes the record, reporting whether there was one, so that the next
// import reads the whole tracker instead of the window since a watermark.
//
// This is what has to happen whenever the entities the watermark describes are
// deleted rather than merely re-read. The file says "everything up to here is
// already imported", and once it is not, an import trusting it asks only for
// what changed since — and converges on a slice of the tracker rather than on
// the tracker.
func (m *SyncState) Clear() (bool, error) {
	m.Sources = map[string]string{}
	err := os.Remove(m.path)
	if os.IsNotExist(err) {
		return false, nil
	}
	return err == nil, err
}

// Forget drops the watermarks whose key begins with prefix, reporting how many
// went, and rewrites the file — or removes it once nothing is left.
//
// Clear's narrower form, for the same reason and one command at a time: the file
// is shared by every tracker this repository syncs with, so a command that has
// destroyed its own entities must not tell the next import of somebody else's
// to read them again from the beginning.
func (m *SyncState) Forget(prefix string) (int, error) {
	var gone []string
	for key := range m.Sources {
		if strings.HasPrefix(key, prefix) {
			gone = append(gone, key)
		}
	}
	if len(gone) == 0 {
		return 0, nil
	}
	for _, key := range gone {
		delete(m.Sources, key)
	}
	if len(m.Sources) == 0 {
		if err := os.Remove(m.path); err != nil && !os.IsNotExist(err) {
			return 0, err
		}
		return len(gone), nil
	}
	return len(gone), m.write()
}

// Since is how far the named source has been read, or the zero time if it
// never has been.
func (m *SyncState) Since(key string) time.Time {
	at, err := time.Parse(time.RFC3339, m.Sources[key])
	if err != nil {
		return time.Time{}
	}
	return at
}

// Record advances one or more sources to at, and writes the file.
//
// It never moves a watermark backwards. A narrower run — one asking for a
// later window than the last one covered — would otherwise erase the knowledge
// that the earlier window was already read.
func (m *SyncState) Record(at time.Time, keys ...string) error {
	if at.IsZero() {
		return nil
	}
	changed := false
	for _, key := range keys {
		if at.After(m.Since(key)) {
			m.Sources[key] = at.UTC().Format(time.RFC3339)
			changed = true
		}
	}
	if !changed {
		return nil
	}
	return m.write()
}

// write puts the record on disk, whole.
func (m *SyncState) write() error {
	if err := os.MkdirAll(filepath.Dir(m.path), 0o755); err != nil {
		return err
	}
	body, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	// Written whole through a temporary file: a half-written watermark that
	// happened to parse would silently skip everything before it.
	tmp := m.path + ".tmp"
	if err := os.WriteFile(tmp, append(body, '\n'), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, m.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}
