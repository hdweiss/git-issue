// Write-back: turning what a local review says into GitHub mutations.
//
// The comparison itself is not here and must not be — it is review.Diff, named
// entirely in the review vocabulary, so a second bridge reuses it whole. What
// belongs here is the two ends: reading the tracker's current state, and mapping
// a delta onto GitHub's pull request mutations.
//
// The plan and candidate types are this package's rather than internal/bridge's,
// and that is deliberate. internal/bridge is typed on issue.Delta; making it
// generic to hold a second delta shape would put the issue vocabulary on
// cmd/git-review's import graph for no gain, since there is exactly one review
// bridge. When a second arrives, these three types are what gets hoisted.

package ghreview

import (
	"fmt"
	"sort"
	"strings"

	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/review"
)

// The push pipeline's types are review's — the same Candidate, Plan, Result and
// Ledger every review bridge speaks, so cmd/git-review drives them all through
// one path. See internal/review/push.go.
type (
	Candidate = review.Candidate
	Plan      = review.Plan
	Skip      = review.Skip
	Result    = review.Result
	Ledger    = review.Ledger
)

// Pusher writes local changes back to one GitHub repository.
type Pusher struct {
	Client *ghapi.Client
	Target ghapi.Target
	Format entity.ObjectFormat
	// Vocab folds both the local blob and a fresh import, so the two states are
	// always compared through the same lens.
	Vocab entity.Vocabulary
	// Known is the origin ledger, so that Plan's import resolves links the same
	// way the pull's did. An import that saw less would report a difference that
	// is not there and push it forever.
	Known Ledger

	repoID     string
	milestones map[string]string
	labels     map[string]string
	// upstream is what Plan read, kept so Apply can state a whole reviewer set
	// without a second request. GitHub has no "remove one reviewer" mutation.
	upstream map[string]ghapi.PullRequest
}

// NewPusher builds a Pusher for one repository.
func NewPusher(client *ghapi.Client, target ghapi.Target, format entity.ObjectFormat, vocab entity.Vocabulary, known Ledger) *Pusher {
	return &Pusher{
		Client: client, Target: target, Format: format, Vocab: vocab, Known: known,
		upstream: map[string]ghapi.PullRequest{},
	}
}

// Tracker names the repository as the origin ledger spells it.
func (p *Pusher) Tracker() string { return p.Target.String() }

// Plan works out what to send, and writes nothing.
//
// The tracker's current state is read by replaying the importer against it — see
// "Pushing to an external tracker" in docs/storage-model.md. That is what makes
// a push need no record of its own: the events a faithful import would produce
// are exactly the ones GitHub already knows about.
func (p *Pusher) Plan(candidates []Candidate) (Plan, error) {
	var plan Plan

	var known []string
	for _, c := range candidates {
		if c.Upstream != "" {
			known = append(known, c.Upstream)
		}
	}
	if len(known) > 0 {
		pulls, err := p.Client.FetchPullNodes(known)
		if err != nil {
			return plan, err
		}
		for _, pr := range pulls {
			p.upstream[pr.ID] = pr
		}
	}

	for _, c := range candidates {
		if c.Upstream == "" {
			d, conflicts := review.Diff(c.ID, entity.State{}, c.State, review.Upstream{}, p.Known)
			d.Create = true
			plan.Skipped = append(plan.Skipped, p.unsupported(&d, c.State)...)
			plan.Deltas = append(plan.Deltas, d)
			plan.Conflicts = append(plan.Conflicts, conflicts...)
			continue
		}

		pr, ok := p.upstream[c.Upstream]
		if !ok {
			// The ledger names an object GitHub will not return: deleted, or in a
			// repository this token cannot see. Re-filing it would open a second
			// pull request for a branch that may well still have one.
			plan.Skipped = append(plan.Skipped, Skip{
				ID:     c.ID,
				Reason: fmt.Sprintf("%s is not readable in %s", c.Upstream, p.Target),
			})
			continue
		}

		// The import runs with comments unclaimed on purpose: this is the
		// tracker's state in full rather than a blob to write, and suppressing
		// what this clone posted would understate what is already there. Links
		// still resolve — see linksOnly.
		imported, err := Import(p.Format, pr, linksOnly{p.Known})
		if err != nil {
			return plan, fmt.Errorf("reading %s: %w", pr.URL, err)
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
// plus the three things keyed by upstream id that a fold cannot supply.
func (p *Pusher) state(pr ghapi.PullRequest, events []entity.Event) review.Upstream {
	up := review.Upstream{
		State:     entity.Fold(p.Vocab, events),
		Comments:  map[string]string{},
		Threads:   map[string]bool{},
		Dismissed: map[string]bool{},
	}
	for _, c := range pr.Comments {
		up.Comments[c.ID] = c.Body
	}
	// A review's body is a comment for the purpose of the comparison, keyed by
	// the review's own id — which is exactly how the import files it.
	for _, r := range pr.Reviews {
		if r.Body != "" {
			up.Comments[r.ID] = r.Body
		}
		if strings.EqualFold(r.State, ghapi.ReviewDismissed) {
			up.Dismissed[r.ID] = true
		}
	}
	for _, t := range pr.Threads {
		up.Threads[t.ID] = t.Resolved
		for _, c := range t.Comments {
			up.Comments[c.ID] = c.Body
		}
	}
	return up
}

// linksOnly narrows the ledger to the half a plan may use: where an upstream
// object is filed here, while claiming no comment. The two halves have to differ
// — links must resolve exactly as they did on the pull, while comments must not
// be suppressed, because a plan reads the tracker's state in full.
type linksOnly struct{ Ledger }

func (linksOnly) ClaimedComment(string) bool { return false }

func (linksOnly) ClaimedVerdict(string) (string, bool) { return "", false }

// unsupported names what this bridge cannot write, takes it out of the delta,
// and reports it — so a limitation is said out loud rather than half-attempted.
func (p *Pusher) unsupported(d *review.Delta, st entity.State) []Skip {
	var out []Skip
	drop := func(field, reason string) {
		if _, ok := d.Set[field]; !ok {
			return
		}
		out = append(out, Skip{ID: d.ID, Field: field, Reason: reason})
		delete(d.Set, field)
	}

	// A merge is a claim about the code, and docs/reviews.md lets only an
	// observer write it. Asking GitHub to merge is a different verb with
	// different consequences, so a local `merged` is reported unsent rather than
	// turned into a mergePullRequest nobody asked for.
	if v, ok := d.Set["status"]; ok && v.Display() == review.StatusMerged {
		out = append(out, Skip{ID: d.ID, Field: "status", Reason: "a merge is performed upstream, not pushed"})
		delete(d.Set, "status")
	}
	drop("status.reason", "GitHub records no close reason on a pull request")

	// A locally created review has to have a branch GitHub can see. Nothing here
	// pushes one — that is the git leg's job, and doing it silently would send
	// code as a side effect of syncing a comment.
	if d.Create && review.HeadRef(st) == "" {
		out = append(out, Skip{ID: d.ID, Reason: "no head branch recorded; set one with 'git review edit --head'"})
		d.Create = false
		*d = review.Delta{ID: d.ID, Set: map[string]entity.Value{}}
		return out
	}

	return append(out, p.unwritableLinks(d)...)
}

// unwritableLinks names the links a delta carries that GitHub will not take, and
// takes them back out of it.
//
// All of them, on a pull request, and that is not an oversight. `closes` is
// derived by GitHub from keywords in the body rather than set through the API,
// so writing one would mean editing somebody's prose; the sub-issue and
// blocked-by mutations are issue-only; and `related` is not a GitHub link at
// all. A bridge that guessed at any of these would produce a link the next
// import cannot read back.
func (p *Pusher) unwritableLinks(d *review.Delta) []Skip {
	var out []Skip
	report := func(rels []review.Relation, verb string) {
		for _, r := range rels {
			reason := fmt.Sprintf("GitHub has no way to %s a %s link on a pull request", verb, r.Kind)
			if r.Kind == review.KindCloses {
				reason = "GitHub derives closing links from the description; write 'Closes #n' in it"
			}
			out = append(out, Skip{ID: d.ID, Field: "links", Reason: reason})
		}
	}
	report(d.RelationsAdded, "write")
	report(d.RelationsRemoved, "remove")
	d.RelationsAdded, d.RelationsRemoved = nil, nil
	return out
}

// Apply sends one delta.
//
// The order within a review is load-bearing twice over. Comments come before
// resolutions, because a thread has to exist before it can be resolved; and a
// thread's root comes before its replies, which the delta already guarantees by
// carrying entries in the blob's own (c, id) order.
func (p *Pusher) Apply(c Candidate, d review.Delta) (Result, error) {
	result := Result{ID: d.ID}

	pullID := c.Upstream
	if d.Create {
		created, err := p.create(c, d)
		result.Upstream, result.URL = created.ID, created.URL
		if err != nil {
			return result, err
		}
		// Everything after this point can fail without losing the pull request,
		// because the caller journals the mapping the moment this returns it.
		pullID = created.ID
	} else if err := p.fields(pullID, d); err != nil {
		return result, err
	}

	if err := p.applyLabels(pullID, d); err != nil {
		return result, err
	}
	if err := p.reviewers(pullID, c, d); err != nil {
		return result, err
	}

	// Comments before verdicts: a verdict's own body goes up with the verdict,
	// and the threads it refers to should exist by the time somebody reads it.
	threads, err := p.comments(pullID, &result, d)
	if err != nil {
		return result, err
	}
	if err := p.verdicts(pullID, &result, c, d); err != nil {
		return result, err
	}
	return result, p.resolutions(threads, d)
}

// create files the pull request. Only what createPullRequest takes goes in the
// call; the rest is applied by Apply straight afterwards.
func (p *Pusher) create(c Candidate, d review.Delta) (ghapi.NewPull, error) {
	repoID, err := p.repository()
	if err != nil {
		return ghapi.NewPull{}, err
	}

	base := d.Set["base"].Display()
	if base == "" {
		base, err = p.Client.DefaultBranch(p.Target)
		if err != nil {
			return ghapi.NewPull{}, err
		}
	}

	created, err := p.Client.CreatePull(ghapi.NewPullInput{
		RepoID:      repoID,
		BaseRefName: base,
		HeadRefName: headRefName(review.HeadRef(c.State)),
		Title:       d.Set["title"].Display(),
		Body:        d.Set["description"].Display(),
		Draft:       d.Set["draft"].Truthy(),
	})
	if err != nil {
		return created, fmt.Errorf("%w\n       the head branch has to be on %s already: git push %s %s",
			err, p.Target, "<remote>", review.HeadRef(c.State))
	}

	// The status is applied after the fact rather than in the call, because
	// createPullRequest has no state input: a review that was closed before it
	// was ever pushed is filed and then closed.
	if v, ok := d.Set["status"]; ok && v.Display() != review.StatusOpen {
		if err := p.Client.ClosePull(created.ID); err != nil {
			return created, err
		}
	}
	if title := d.Set["milestone"].Display(); title != "" {
		id, err := p.milestoneID(title)
		if err != nil {
			return created, err
		}
		if err := p.Client.UpdatePull(created.ID, map[string]any{"milestoneId": id}); err != nil {
			return created, err
		}
	}
	return created, nil
}

// headRefName is the head as GitHub's createPullRequest spells it: `owner:branch`
// for a branch in a fork, a bare name for one in the target repository.
//
// The local form is `owner/branch`, which is what a person reads and what a
// fetch can be built from. The two differ in one character and in nothing else,
// so the conversion is here rather than in the vocabulary.
func headRefName(head string) string {
	owner, branch, ok := strings.Cut(head, "/")
	if !ok {
		return head
	}
	return owner + ":" + branch
}

// fields writes the scalars updatePullRequest covers, plus the two state axes
// that have mutations of their own.
func (p *Pusher) fields(pullID string, d review.Delta) error {
	fields := map[string]any{}
	if v, ok := d.Set["title"]; ok {
		fields["title"] = v.Display()
	}
	if v, ok := d.Set["description"]; ok {
		fields["body"] = v.Display()
	}
	if v, ok := d.Set["base"]; ok {
		fields["baseRefName"] = v.Display()
	}
	if v, ok := d.Set["milestone"]; ok {
		// A cleared milestone is a null, which is how updatePullRequest detaches
		// one; an empty string would be a title nothing matches.
		if title := v.Display(); title == "" {
			fields["milestoneId"] = nil
		} else {
			id, err := p.milestoneID(title)
			if err != nil {
				return err
			}
			fields["milestoneId"] = id
		}
	}
	if err := p.Client.UpdatePull(pullID, fields); err != nil {
		return err
	}

	if v, ok := d.Set["status"]; ok {
		var err error
		if v.Display() == review.StatusOpen {
			err = p.Client.ReopenPull(pullID)
		} else {
			err = p.Client.ClosePull(pullID)
		}
		if err != nil {
			return err
		}
	}
	if v, ok := d.Set["draft"]; ok {
		if err := p.Client.SetPullDraft(pullID, v.Truthy()); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pusher) applyLabels(pullID string, d review.Delta) error {
	add, err := p.labelIDs(d.LabelsAdded)
	if err != nil {
		return err
	}
	remove, err := p.labelIDs(d.LabelsRemoved)
	if err != nil {
		return err
	}
	if err := p.Client.AddLabels(pullID, add); err != nil {
		return err
	}
	return p.Client.RemoveLabels(pullID, remove)
}

// reviewers states the whole set of people asked to read the review.
//
// The whole set rather than a difference, because requestReviews is the only
// mutation there is and it replaces. So the set is rebuilt from what the tracker
// currently holds — not from what this clone last saw — which is what keeps a
// reviewer somebody else added from being dropped by an unrelated push.
//
// A name that resolves to no GitHub user is left out and reported rather than
// failing the push: a team is requested by slug and looks exactly like a login
// here, and losing a whole review's changes over one is the wrong trade.
func (p *Pusher) reviewers(pullID string, c Candidate, d review.Delta) error {
	if len(d.ReviewersAdded) == 0 && len(d.ReviewersRemoved) == 0 {
		return nil
	}

	want := map[string]bool{}
	for _, l := range p.upstream[c.Upstream].Reviewers {
		want[Login(l)] = true
	}
	for _, l := range d.ReviewersAdded {
		want[Login(l)] = true
	}
	for _, l := range d.ReviewersRemoved {
		delete(want, Login(l))
	}

	logins := make([]string, 0, len(want))
	for l := range want {
		logins = append(logins, l)
	}
	sort.Strings(logins)

	found, err := p.Client.UserIDs(logins)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(logins))
	for _, l := range logins {
		if id, ok := found[l]; ok {
			ids = append(ids, id)
		}
	}
	return p.Client.RequestReviews(pullID, ids, nil)
}

// comments posts, edits and deletes thread entries, returning the upstream
// thread id each local root now has.
//
// The mappings are recorded on the Result even when a later call fails, which is
// deliberate: a comment posted and not recorded is posted again on the next run,
// and the caller journals whatever comes back before it reports the error.
func (p *Pusher) comments(pullID string, result *Result, d review.Delta) (map[string]string, error) {
	// Seeded with the threads the ledger already knows, then extended as this
	// run opens new ones — which is what lets a root and its replies be pushed
	// in the same run.
	threads := map[string]string{}
	for _, c := range d.CommentsNew {
		if c.Root != "" && c.Thread != "" {
			threads[c.Root] = c.Thread
		}
	}
	for _, t := range append(append([]review.ThreadPush{}, d.ThreadsResolved...), d.ThreadsUnresolved...) {
		if t.Upstream != "" {
			threads[t.Root] = t.Upstream
		}
	}

	for _, c := range d.CommentsNew {
		body := c.Entry.Body.Display()
		switch {
		case c.Root != "":
			thread, ok := threads[c.Root]
			if !ok {
				// The root was never posted and is not being posted now — a reply
				// under an imported thread this clone has no mapping for. Posting
				// it at the top level would detach it from what it answers.
				return threads, fmt.Errorf("reply %s: its thread is not on %s", short(c.Entry.ID()), p.Target)
			}
			id, err := p.Client.AddThreadReply(thread, body)
			if err != nil {
				return threads, err
			}
			result.Comments = append(result.Comments, CommentOrigin{EventID: c.Entry.ID(), Upstream: id})

		case c.Anchored:
			threadID, commentID, err := p.Client.AddReviewThread(pullID, ghapi.NewThreadInput{
				Path:      c.Anchor.Path,
				Line:      c.Anchor.Last,
				StartLine: c.Anchor.First,
				Side:      c.Anchor.Side,
				Body:      body,
			})
			if err != nil {
				return threads, err
			}
			threads[c.Entry.ID()] = threadID
			result.Threads = append(result.Threads, CommentOrigin{EventID: c.Entry.ID(), Upstream: threadID})
			if commentID != "" {
				result.Comments = append(result.Comments, CommentOrigin{EventID: c.Entry.ID(), Upstream: commentID})
			}

		default:
			id, err := p.Client.AddComment(pullID, body)
			if err != nil {
				return threads, err
			}
			result.Comments = append(result.Comments, CommentOrigin{EventID: c.Entry.ID(), Upstream: id})
		}
	}

	for _, c := range d.CommentsEdited {
		var err error
		switch c.Kind {
		case review.CommentAnchored:
			err = p.Client.UpdateReviewComment(c.Upstream, c.Entry.Body.Display())
		case review.CommentVerdict:
			err = p.Client.UpdatePullReview(c.Upstream, c.Entry.Body.Display())
		default:
			err = p.Client.UpdateComment(c.Upstream, c.Entry.Body.Display())
		}
		if err != nil {
			return threads, err
		}
	}

	for _, c := range d.CommentsRemoved {
		var err error
		switch c.Kind {
		case review.CommentAnchored:
			err = p.Client.DeleteReviewComment(c.Upstream)
		case review.CommentVerdict:
			// A submitted review cannot be deleted, only dismissed — which is a
			// different act with a different meaning, so it is not substituted.
			continue
		default:
			err = p.Client.DeleteComment(c.Upstream)
		}
		if err != nil {
			return threads, err
		}
	}
	return threads, nil
}

// verdicts submits the positions this clone holds and withdraws the ones it
// retracted.
//
// A verdict's message goes up as the review's body, in the one call, because
// that is what it is upstream — and the local comment that carried it is
// recorded against the review's id, so the next import claims it instead of
// filing a second copy.
func (p *Pusher) verdicts(pullID string, result *Result, c Candidate, d review.Delta) error {
	for _, v := range d.VerdictsCast {
		event := Event(v.Verdict.Value)
		if event == "" {
			// A verdict word this bridge has no GitHub event for. The vocabulary
			// is open, so this is ordinary rather than an error.
			continue
		}
		id, err := p.Client.AddPullReview(pullID, event, v.Body, v.Verdict.Revision)
		if err != nil {
			return err
		}
		result.Verdicts = append(result.Verdicts, CommentOrigin{EventID: v.Verdict.ID, Upstream: id})
		if v.Comment != "" {
			result.Comments = append(result.Comments, CommentOrigin{EventID: v.Comment, Upstream: id})
		}
	}

	for _, v := range d.VerdictsDismissed {
		if err := p.Client.DismissPullReview(v.Upstream, v.Body); err != nil {
			return err
		}
		if v.Comment != "" {
			result.Comments = append(result.Comments, CommentOrigin{EventID: v.Comment, Upstream: v.Upstream})
		}
	}
	return nil
}

// resolutions marks threads resolved and reopened, after the comments that may
// have created them.
func (p *Pusher) resolutions(threads map[string]string, d review.Delta) error {
	for _, set := range []struct {
		pushes   []review.ThreadPush
		resolved bool
	}{{d.ThreadsResolved, true}, {d.ThreadsUnresolved, false}} {
		for _, t := range set.pushes {
			id := t.Upstream
			if id == "" {
				id = threads[t.Root]
			}
			if id == "" {
				// A thread with nothing upstream to resolve. Not an error: the
				// resolution is recorded locally and will be sent once the thread
				// itself is.
				continue
			}
			if err := p.Client.SetThreadResolved(id, set.resolved); err != nil {
				return err
			}
		}
	}
	return nil
}

func (p *Pusher) repository() (string, error) {
	if p.repoID != "" {
		return p.repoID, nil
	}
	id, err := p.Client.RepoID(p.Target)
	if err != nil {
		return "", err
	}
	p.repoID = id
	return id, nil
}

// labelIDs resolves label names, creating nothing — a label the repository does
// not have is an error rather than something to invent, for the reason
// docs/bridge-github.md gives: colour and description are repository-wide
// settings this tracker does not model.
func (p *Pusher) labelIDs(names []string) ([]string, error) {
	if len(names) == 0 {
		return nil, nil
	}
	if p.labels == nil {
		got, err := p.Client.Labels(p.Target)
		if err != nil {
			return nil, err
		}
		p.labels = got
	}
	out := make([]string, 0, len(names))
	var missing []string
	for _, name := range names {
		id, ok := p.labels[name]
		if !ok {
			missing = append(missing, fmt.Sprintf("%q", name))
			continue
		}
		out = append(out, id)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s has no label %s", p.Target, strings.Join(missing, ", "))
	}
	return out, nil
}

func (p *Pusher) milestoneID(title string) (string, error) {
	if p.milestones == nil {
		got, err := p.Client.Milestones(p.Target)
		if err != nil {
			return "", err
		}
		p.milestones = got
	}
	id, ok := p.milestones[title]
	if !ok {
		return "", fmt.Errorf("%s has no milestone %q", p.Target, title)
	}
	return id, nil
}

// Event maps a verdict onto the GitHub review event that casts it.
//
// `comment` is a real position — somebody read this and took none — and GitHub
// spells it COMMENT. An unknown word maps to nothing rather than to COMMENT: the
// vocabulary is open, and silently downgrading a verdict a build does not
// recognise would misreport somebody's position.
func Event(verdict string) string {
	switch verdict {
	case review.VerdictApprove:
		return ghapi.EventApprove
	case review.VerdictRequestChanges:
		return ghapi.EventRequestChanges
	case review.VerdictComment:
		return ghapi.EventComment
	}
	return ""
}

// Login strips the scheme from an author value. A list member written locally is
// a bare login; one that arrived through this bridge carries `github:`, because
// `a` is hashed into every event and the scheme is part of the spelling.
func Login(s string) string { return strings.TrimPrefix(s, "github:") }

func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}
