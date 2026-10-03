// Write-back: turning what a local review says into Azure DevOps pull request
// mutations.
//
// The comparison itself is not here and must not be — it is review.Diff, named
// entirely in the review vocabulary, so this bridge reuses it whole exactly as
// internal/bridge/github/review does. What belongs here is the two ends: reading the
// pull request's current state, and mapping a delta onto the thread, comment,
// label and vote calls of docs/bridge-ado.md.

package adoreview

import (
	"fmt"
	"strconv"
	"strings"

	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/review"
)

// Pusher writes local changes back to one Azure DevOps repository's pull
// requests.
type Pusher struct {
	Client *adoapi.Client
	Target adoapi.Target
	Format entity.ObjectFormat
	// Vocab folds both the local blob and a fresh import, so the two states are
	// always compared through the same lens.
	Vocab entity.Vocabulary
	// Known is the origin ledger, so that Plan's import resolves links the same
	// way the pull's did. An import that saw less would report a difference that
	// is not there and push it forever.
	Known review.Ledger
	// Self is who the token authenticates as, from connectionData. A vote is cast
	// under this identity, and a pushed verdict is recorded on the ledger against
	// the key a re-import derives from it.
	Self adoapi.Identity

	upstream map[int]adoapi.PullRequest
}

// NewPusher builds a Pusher for one repository.
func NewPusher(client *adoapi.Client, target adoapi.Target, format entity.ObjectFormat, vocab entity.Vocabulary, known review.Ledger, self adoapi.Identity) *Pusher {
	return &Pusher{
		Client: client, Target: target, Format: format, Vocab: vocab, Known: known, Self: self,
		upstream: map[int]adoapi.PullRequest{},
	}
}

// Tracker names the repository as the origin ledger spells it.
func (p *Pusher) Tracker() string { return p.Target.String() }

// Plan works out what to send, and writes nothing.
//
// The pull request's current state is read by replaying the importer against it
// — see "Pushing to an external tracker" in docs/storage-model.md. That is what
// makes a push need no record of its own: the events a faithful import would
// produce are exactly the ones Azure DevOps already knows about.
func (p *Pusher) Plan(candidates []review.Candidate) (review.Plan, error) {
	var plan review.Plan

	for _, c := range candidates {
		id := pullID(c.Upstream)
		if id == 0 {
			continue
		}
		pr, err := p.Client.FetchPull(p.Target, id)
		if err != nil {
			// The ledger names a pull request Azure DevOps will not return:
			// deleted, or in a repository this token cannot see. Re-filing it
			// would open a second pull request for a branch that may still have
			// one.
			plan.Skipped = append(plan.Skipped, review.Skip{
				ID:     c.ID,
				Reason: fmt.Sprintf("%s is not readable in %s", c.Upstream, p.Target),
			})
			continue
		}
		p.upstream[id] = pr
	}

	for _, c := range candidates {
		id := pullID(c.Upstream)
		if id == 0 {
			d, conflicts := review.Diff(c.ID, entity.State{}, c.State, review.Upstream{}, p.Known)
			d.Create = true
			plan.Skipped = append(plan.Skipped, p.unsupported(&d, c.State)...)
			plan.Deltas = append(plan.Deltas, d)
			plan.Conflicts = append(plan.Conflicts, conflicts...)
			continue
		}
		pr, ok := p.upstream[id]
		if !ok {
			continue // FetchPull failed; already reported as skipped
		}

		// The import runs with comments unclaimed on purpose: this is the
		// tracker's state in full, and suppressing what this clone posted would
		// understate what is already there. Links still resolve.
		imported, err := Import(p.Format, p.Target, pr, linksOnly{p.Known})
		if err != nil {
			return plan, fmt.Errorf("reading pull request %d: %w", pr.ID, err)
		}
		events := imported.Events()

		d, conflicts := review.Diff(
			c.ID,
			review.Common(p.Vocab, c.Events, events),
			c.State,
			p.state(pr, events),
			p.Known,
		)
		plan.Conflicts = append(plan.Conflicts, conflicts...)
		// A conflicted review is skipped whole. Sending its uncontended fields
		// would leave it half-pushed and the report unable to say what upstream
		// now holds.
		if len(conflicts) > 0 {
			continue
		}
		plan.Skipped = append(plan.Skipped, p.unsupported(&d, c.State)...)
		if d.Empty() {
			continue
		}
		plan.Deltas = append(plan.Deltas, d)
	}

	return plan, nil
}

// state assembles the tracker's side of the comparison: the folded re-import,
// plus the comment bodies and thread resolutions a fold cannot supply.
func (p *Pusher) state(pr adoapi.PullRequest, events []entity.Event) review.Upstream {
	up := review.Upstream{
		State:     entity.Fold(p.Vocab, events),
		Comments:  map[string]string{},
		Threads:   map[string]bool{},
		Dismissed: map[string]bool{},
	}
	for _, th := range pr.Threads {
		up.Threads[p.Target.PullRequestThreadOrigin(pr.ID, th.ID)] = resolvedStatus(th.Status)
		for _, c := range th.Comments {
			up.Comments[p.Target.PullRequestCommentOrigin(pr.ID, th.ID, c.ID)] = c.Content
		}
	}
	return up
}

// linksOnly narrows the ledger to the half a plan may use: where an upstream
// object is filed here, while claiming no comment or verdict. A plan reads the
// tracker's state in full, so those must not be suppressed.
type linksOnly struct{ review.Lookup }

func (linksOnly) ClaimedComment(string) bool { return false }

func (linksOnly) ClaimedVerdict(string) (string, bool) { return "", false }

// unsupported names what this bridge cannot write, takes it out of the delta,
// and reports it — so a limitation is said out loud rather than half-attempted.
func (p *Pusher) unsupported(d *review.Delta, st entity.State) []review.Skip {
	var out []review.Skip
	drop := func(field, reason string) {
		if _, ok := d.Set[field]; !ok {
			return
		}
		out = append(out, review.Skip{ID: d.ID, Field: field, Reason: reason})
		delete(d.Set, field)
	}

	if v, ok := d.Set["status"]; ok {
		if _, pushable := adoStatus(v.Display()); !pushable {
			out = append(out, review.Skip{ID: d.ID, Field: "status", Reason: "a completion is performed upstream, not pushed"})
			delete(d.Set, "status")
		}
	}
	drop("status.reason", "an Azure DevOps pull request has no close reason")
	drop("milestone", "an Azure DevOps pull request has no milestone")

	// A locally created review has to have a branch Azure DevOps can see. Nothing
	// here pushes one — that is the git leg's job.
	if d.Create && review.HeadRef(st) == "" {
		out = append(out, review.Skip{ID: d.ID, Reason: "no head branch recorded; set one with 'git review edit --head'"})
		*d = review.Delta{ID: d.ID, Set: map[string]entity.Value{}}
		return out
	}

	if len(d.ReviewersAdded) > 0 || len(d.ReviewersRemoved) > 0 {
		out = append(out, review.Skip{ID: d.ID, Field: "reviewers", Reason: "reviewer changes are not pushed to Azure DevOps yet"})
		d.ReviewersAdded, d.ReviewersRemoved = nil, nil
	}

	for _, rels := range [][]review.Relation{d.RelationsAdded, d.RelationsRemoved} {
		for range rels {
			out = append(out, review.Skip{ID: d.ID, Field: "links", Reason: "a pull request links a work item with an artifact link, not pushed yet"})
		}
	}
	d.RelationsAdded, d.RelationsRemoved = nil, nil

	return out
}

// Apply sends one delta.
//
// The order within a review is load-bearing: comments come before resolutions,
// because a thread has to exist before it can be resolved; and a thread's root
// comes before its replies, which the delta already guarantees by carrying
// entries in the blob's own (c, id) order.
func (p *Pusher) Apply(c review.Candidate, d review.Delta) (review.Result, error) {
	result := review.Result{ID: d.ID}

	prID := pullID(c.Upstream)
	if d.Create {
		created, err := p.create(d, review.HeadRef(c.State))
		if created.ID != 0 {
			result.Upstream = p.Target.PullRequestOrigin(created.ID)
			result.URL = created.URL
			if result.URL == "" {
				result.URL = p.Target.PullRequestWebURL(created.ID)
			}
			prID = created.ID
		}
		if err != nil {
			return result, err
		}
	} else if err := p.fields(prID, d); err != nil {
		return result, err
	}

	if err := p.applyLabels(prID, d); err != nil {
		return result, err
	}
	threads, err := p.comments(prID, &result, d)
	if err != nil {
		return result, err
	}
	if err := p.verdicts(prID, &result, d); err != nil {
		return result, err
	}
	return result, p.resolutions(prID, threads, d)
}

// create files the pull request. Only what CreatePull takes goes in the call;
// the rest is applied straight afterwards.
func (p *Pusher) create(d review.Delta, head string) (adoapi.NewPull, error) {
	if strings.Contains(head, "/") {
		return adoapi.NewPull{}, fmt.Errorf("head %q is in a fork; this bridge does not file cross-repository pull requests", head)
	}
	base := d.Set["base"].Display()
	if base == "" {
		var err error
		if base, err = p.Client.DefaultBranch(p.Target); err != nil {
			return adoapi.NewPull{}, err
		}
	}

	created, err := p.Client.CreatePull(p.Target, adoapi.NewPullInput{
		SourceRef:   head,
		TargetRef:   base,
		Title:       d.Set["title"].Display(),
		Description: d.Set["description"].Display(),
		Draft:       d.Set["draft"].Truthy(),
	})
	if err != nil {
		return created, fmt.Errorf("%w\n       the head branch has to be on %s already: git push <remote> %s", err, p.Target, head)
	}

	// The state is applied after the fact rather than in the call, because
	// CreatePull has no state input: a review abandoned before it was ever
	// pushed is filed and then abandoned.
	if v, ok := d.Set["status"]; ok {
		if status, pushable := adoStatus(v.Display()); pushable && status != "active" {
			if err := p.Client.UpdatePull(p.Target, created.ID, map[string]any{"status": status}); err != nil {
				return created, err
			}
		}
	}
	return created, nil
}

// fields writes the scalars UpdatePull covers.
func (p *Pusher) fields(prID int, d review.Delta) error {
	fields := map[string]any{}
	if v, ok := d.Set["title"]; ok {
		fields["title"] = v.Display()
	}
	if v, ok := d.Set["description"]; ok {
		fields["description"] = v.Display()
	}
	if v, ok := d.Set["base"]; ok {
		fields["targetRefName"] = "refs/heads/" + v.Display()
	}
	if v, ok := d.Set["draft"]; ok {
		fields["isDraft"] = v.Truthy()
	}
	if v, ok := d.Set["status"]; ok {
		if status, pushable := adoStatus(v.Display()); pushable {
			fields["status"] = status
		}
	}
	return p.Client.UpdatePull(p.Target, prID, fields)
}

func (p *Pusher) applyLabels(prID int, d review.Delta) error {
	for _, name := range d.LabelsAdded {
		if err := p.Client.AddPullLabel(p.Target, prID, name); err != nil {
			return err
		}
	}
	for _, name := range d.LabelsRemoved {
		if err := p.Client.RemovePullLabel(p.Target, prID, name); err != nil {
			return err
		}
	}
	return nil
}

// comments posts, edits and deletes thread entries, returning the upstream
// thread id each local root now has.
//
// The mappings are recorded on the Result even when a later call fails: a
// comment posted and not recorded is posted again on the next run, and the
// caller journals whatever comes back before it reports the error.
func (p *Pusher) comments(prID int, result *review.Result, d review.Delta) (map[string]int, error) {
	threads := map[string]int{}
	seed := func(root, upstream string) {
		if root == "" || upstream == "" {
			return
		}
		if _, tid := threadRef(upstream); tid != 0 {
			threads[root] = tid
		}
	}
	for _, c := range d.CommentsNew {
		seed(c.Root, c.Thread)
	}
	for _, t := range append(append([]review.ThreadPush{}, d.ThreadsResolved...), d.ThreadsUnresolved...) {
		seed(t.Root, t.Upstream)
	}

	record := func(event string, tid, cid int) {
		if cid != 0 {
			result.Comments = append(result.Comments, review.CommentOrigin{
				EventID: event, Upstream: p.Target.PullRequestCommentOrigin(prID, tid, cid),
			})
		}
	}

	for _, c := range d.CommentsNew {
		body := c.Entry.Body.Display()
		switch {
		case c.Root != "":
			tid, ok := threads[c.Root]
			if !ok {
				return threads, fmt.Errorf("reply %s: its thread is not on %s", short(c.Entry.ID()), p.Target)
			}
			id, err := p.Client.AddPullComment(p.Target, prID, tid, 0, body)
			if err != nil {
				return threads, err
			}
			record(c.Entry.ID(), tid, id)

		case c.Anchored:
			side := ""
			if c.Anchor.Side == review.SideLeft {
				side = "left"
			}
			tid, cid, err := p.Client.AddPullThread(p.Target, prID, adoapi.NewThreadInput{
				Body: body, Path: c.Anchor.Path, First: c.Anchor.First, Last: c.Anchor.Last, Side: side,
			})
			if err != nil {
				return threads, err
			}
			threads[c.Entry.ID()] = tid
			result.Threads = append(result.Threads, review.CommentOrigin{
				EventID: c.Entry.ID(), Upstream: p.Target.PullRequestThreadOrigin(prID, tid),
			})
			record(c.Entry.ID(), tid, cid)

		default:
			tid, cid, err := p.Client.AddPullThread(p.Target, prID, adoapi.NewThreadInput{Body: body})
			if err != nil {
				return threads, err
			}
			threads[c.Entry.ID()] = tid
			result.Threads = append(result.Threads, review.CommentOrigin{
				EventID: c.Entry.ID(), Upstream: p.Target.PullRequestThreadOrigin(prID, tid),
			})
			record(c.Entry.ID(), tid, cid)
		}
	}

	for _, c := range d.CommentsEdited {
		_, tid, cid := commentRef(c.Upstream)
		if tid == 0 || cid == 0 {
			continue
		}
		if err := p.Client.UpdatePullComment(p.Target, prID, tid, cid, c.Entry.Body.Display()); err != nil {
			return threads, err
		}
	}
	for _, c := range d.CommentsRemoved {
		_, tid, cid := commentRef(c.Upstream)
		if tid == 0 || cid == 0 {
			continue
		}
		if err := p.Client.DeletePullComment(p.Target, prID, tid, cid); err != nil {
			return threads, err
		}
	}
	return threads, nil
}

// verdicts casts the positions this clone holds and withdraws the ones it
// retracted.
//
// A vote carries no body on Azure DevOps, so a verdict written with a message
// posts that message as its own discussion thread, and the local comment that
// carried it is recorded against the thread's comment so the next import claims
// it instead of filing a second copy.
func (p *Pusher) verdicts(prID int, result *review.Result, d review.Delta) error {
	postBody := func(event, body string) error {
		if event == "" || body == "" {
			return nil
		}
		tid, cid, err := p.Client.AddPullThread(p.Target, prID, adoapi.NewThreadInput{Body: body})
		if err != nil {
			return err
		}
		if cid != 0 {
			result.Comments = append(result.Comments, review.CommentOrigin{
				EventID: event, Upstream: p.Target.PullRequestCommentOrigin(prID, tid, cid),
			})
		}
		return nil
	}

	for _, v := range d.VerdictsCast {
		vote, ok := Vote(v.Verdict.Value)
		if !ok {
			// A verdict word with no Azure DevOps vote — `comment`. The vocabulary
			// is open, so this is ordinary rather than an error.
			continue
		}
		if err := p.Client.SetReviewerVote(p.Target, prID, p.Self.ID, vote); err != nil {
			return err
		}
		result.Verdicts = append(result.Verdicts, review.CommentOrigin{
			EventID:  v.Verdict.ID,
			Upstream: p.Target.PullRequestOrigin(prID) + ":vote:" + p.Self.EventAuthor(),
		})
		if err := postBody(v.Comment, v.Body); err != nil {
			return err
		}
	}

	for _, v := range d.VerdictsDismissed {
		if err := p.Client.SetReviewerVote(p.Target, prID, p.Self.ID, adoapi.VoteNone); err != nil {
			return err
		}
		if err := postBody(v.Comment, v.Body); err != nil {
			return err
		}
	}
	return nil
}

// resolutions marks threads resolved and reopened, after the comments that may
// have created them.
func (p *Pusher) resolutions(prID int, threads map[string]int, d review.Delta) error {
	for _, set := range []struct {
		pushes []review.ThreadPush
		status string
	}{
		{d.ThreadsResolved, "closed"},
		{d.ThreadsUnresolved, "active"},
	} {
		for _, t := range set.pushes {
			tid := 0
			if t.Upstream != "" {
				_, tid = threadRef(t.Upstream)
			}
			if tid == 0 {
				tid = threads[t.Root]
			}
			if tid == 0 {
				// Nothing upstream to resolve yet. Recorded locally, sent once
				// the thread itself is.
				continue
			}
			if err := p.Client.SetPullThreadStatus(p.Target, prID, tid, set.status); err != nil {
				return err
			}
		}
	}
	return nil
}

// Vote maps a verdict onto the Azure DevOps reviewer vote that casts it. An
// unknown word — `comment` included — maps to nothing rather than to a neutral
// vote: the vocabulary is open, and a `0` would misreport somebody's position as
// "no vote".
func Vote(verdict string) (int, bool) {
	switch verdict {
	case review.VerdictApprove:
		return adoapi.VoteApproved, true
	case review.VerdictRequestChanges:
		return adoapi.VoteRejected, true
	}
	return 0, false
}

// adoStatus maps a review status onto the Azure DevOps pull request status a
// push writes, and reports whether it is one a push may assert at all. A
// completion is the server's to perform, so `merged` / `Completed` are not
// pushable.
func adoStatus(v string) (status string, pushable bool) {
	switch v {
	case review.StatusOpen, "Active":
		return "active", true
	case review.StatusClosed, "Abandoned":
		return "abandoned", true
	}
	return "", false
}

// pullID reads the pull request number out of an origin string, returning 0 for
// anything that is not one.
func pullID(origin string) int {
	return scanRef(origin, "/pullRequests/")
}

// threadRef reads the pull request and thread numbers out of a thread origin.
func threadRef(origin string) (prID, threadID int) {
	return scanRef(origin, "/pullRequests/"), scanRef(origin, "/threads/")
}

// commentRef reads the pull request, thread and comment numbers out of a comment
// origin.
func commentRef(origin string) (prID, threadID, commentID int) {
	return scanRef(origin, "/pullRequests/"), scanRef(origin, "/threads/"), scanRef(origin, "/comments/")
}

// scanRef takes the integer that follows marker in origin, or 0.
func scanRef(origin, marker string) int {
	i := strings.Index(origin, marker)
	if i < 0 {
		return 0
	}
	rest := origin[i+len(marker):]
	if s := strings.IndexByte(rest, '/'); s >= 0 {
		rest = rest[:s]
	}
	n, err := strconv.Atoi(rest)
	if err != nil {
		return 0
	}
	return n
}

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
