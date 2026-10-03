package review

import "time"

// CommitCheck is one check run as a bridge import produces it, before it is
// written to the checks ref.
//
// It is not a review event and never reaches a review blob: a check is a fact
// about a commit, so it is carried out of the importer keyed by the commit sha
// it belongs to, and the caller groups the run onto RefChecks. The type lives
// here rather than in a bridge because every bridge that reads checks produces
// the same shape, and `Run` is already this package's own on-ref form.
type CommitCheck struct {
	// Commit is the sha the run belongs to — the key it is stored under.
	Commit string
	// Run is the check itself, in the vocabulary WriteChecks writes.
	Run Check
	// At is when the run was reported, for the action's timestamp.
	At time.Time
	// Author is the event author the run is attributed to.
	Author string
}
