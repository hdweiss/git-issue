// The crash journal: mappings held locally between being earned and being
// committed to the ledger ref.

package origins

import (
	"os"
	"path/filepath"
	"strings"
)

// Journal is one tracker's uncommitted mappings.
//
// A push earns a mapping the moment an upstream object is created, and a lost
// mapping is the one failure here that nothing can undo: the next run sees an
// entity with no upstream object and files a second one. Committing per mapping
// would answer that with a commit per issue, which is exactly the log noise the
// ledger exists to avoid.
//
// So mappings are appended here as they are earned and committed to the ref once
// the run finishes. A run that dies leaves the journal behind; the next one
// commits it first and carries on. Local, derived and disposable in the sense
// docs/storage-model.md gives — its whole lifetime is one interrupted run.
type Journal struct {
	path  string
	lines []string
}

// OpenJournal loads any mappings a previous run left behind. An unreadable
// journal is an empty one: it is a crash-recovery aid, and refusing to push
// because it is malformed would be strictly worse than ignoring it.
func (s *Store) OpenJournal(tracker string) (*Journal, error) {
	dir, err := s.Repo.CommonDir()
	if err != nil {
		return nil, err
	}
	j := &Journal{path: filepath.Join(dir, "git-issue", journalDir, tracker)}
	body, err := os.ReadFile(j.path)
	if err != nil {
		return j, nil
	}
	for _, line := range strings.Split(string(body), "\n") {
		if strings.TrimSpace(line) != "" {
			j.lines = append(j.lines, line)
		}
	}
	return j, nil
}

// Append records one mapping and flushes it to disk immediately.
//
// Immediately is the entire point: a mapping buffered in memory when the process
// dies is a mapping lost, and the cost of losing one is a duplicate issue
// upstream. The file is small and the write is one syscall against an
// already-open path, which is cheap next to the API call that earned the line.
func (j *Journal) Append(kind string, fields ...string) error {
	j.lines = append(j.lines, Line(kind, fields...))
	return j.flush()
}

func (j *Journal) flush() error {
	if err := os.MkdirAll(filepath.Dir(j.path), 0o755); err != nil {
		return err
	}
	body := strings.Join(j.lines, "\n") + "\n"
	// Whole-file through a temporary, like the sync watermark: a half-written
	// journal that happened to parse would silently drop the mappings after the
	// tear, which is the failure this file exists to prevent.
	tmp := j.path + ".tmp"
	if err := os.WriteFile(tmp, []byte(body), 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, j.path); err != nil {
		os.Remove(tmp)
		return err
	}
	return nil
}

// Empty reports whether the journal holds nothing to commit.
func (j *Journal) Empty() bool { return len(j.lines) == 0 }

// Lines are the mappings held, oldest first.
func (j *Journal) Lines() []string { return j.lines }

// Into folds the journal's mappings into a ledger, ready to be saved.
func (j *Journal) Into(l *Ledger) {
	for _, line := range j.lines {
		l.insert(line)
	}
}

// Done deletes the journal, which is only correct once its mappings are on the
// ref. Failing to remove it is not an error worth reporting: the lines are
// already committed, and re-applying them next run is a no-op.
//
// The directories go too. A tracker name is a nested path, so a journal leaves
// a chain of directories behind it, and an empty chain that never gets cleared
// makes "is there a journal here" ambiguous to anything looking at the
// filesystem rather than at the file.
func (j *Journal) Done() {
	j.lines = nil
	os.Remove(j.path)

	root := journalRoot(j.path)
	for dir := filepath.Dir(j.path); strings.HasPrefix(dir, root) && dir != root; dir = filepath.Dir(dir) {
		// Only ever an empty one: Remove refuses a directory with anything in
		// it, which is exactly the guard wanted here.
		if os.Remove(dir) != nil {
			return
		}
	}
	os.Remove(root)
}

// clearJournals removes every tracker's journal, reporting whether there was
// anything to remove.
//
// Unlike Done, which retires a journal whose mappings reached the ref, this
// throws them away unapplied. Only correct when the entities they map are
// themselves being deleted: a mapping is a claim that some local entity is
// already filed upstream, and a claim about an entity that no longer exists
// cannot be applied to anything.
func clearJournals(dir string) (bool, error) {
	root := filepath.Join(dir, "git-issue", journalDir)
	if _, err := os.Stat(root); err != nil {
		return false, nil
	}
	return true, os.RemoveAll(root)
}

// journalRoot is the directory every tracker's journal hangs under.
func journalRoot(path string) string {
	for dir := filepath.Dir(path); dir != "/" && dir != "."; dir = filepath.Dir(dir) {
		if filepath.Base(dir) == journalDir {
			return dir
		}
	}
	return filepath.Dir(path)
}

const journalDir = "origins-journal"
