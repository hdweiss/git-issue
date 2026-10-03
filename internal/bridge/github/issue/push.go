// Write-back: turning what a local issue says into GitHub mutations.
//
// The comparison itself is not here and must not be — it is issue.Diff, named
// entirely in the issue vocabulary, so a second bridge reuses it whole. What
// belongs here is the two ends: reading the tracker's current state, and
// mapping a delta onto the mutations of docs/bridge-github.md.

package ghissue

import (
	"fmt"
	"strings"

	"github.com/hdweiss/git-issue/internal/bridge"
	ghapi "github.com/hdweiss/git-issue/internal/bridge/github/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

// Pusher writes local changes back to one GitHub repository.
type Pusher struct {
	Client *ghapi.Client
	Target ghapi.Target
	Format entity.ObjectFormat
	// Vocab folds both the local blob and a fresh import, so the two states are
	// always compared through the same lens.
	Vocab entity.Vocabulary
	// Known is the origin ledger, so that Plan's import resolves relations the
	// same way the pull's did. An import that saw less would report a
	// difference that is not there and push it forever.
	Known bridge.Lookup

	// Resolved repository ids, fetched once and only when something needs them.
	repoID     string
	labels     map[string]string
	milestones map[string]string
}

// New builds a Pusher for one repository.
func NewPusher(client *ghapi.Client, target ghapi.Target, format entity.ObjectFormat, vocab entity.Vocabulary, known bridge.Lookup) *Pusher {
	return &Pusher{Client: client, Target: target, Format: format, Vocab: vocab, Known: known}
}

// Tracker names the repository as the origin ledger spells it.
func (p *Pusher) Tracker() string { return p.Target.String() }

// Plan works out what to send, and writes nothing.
//
// The tracker's current state is read by replaying the importer against it —
// see "Pushing to an external tracker" in docs/storage-model.md. That is what
// makes a push need no record of its own: the events a faithful import would
// produce are exactly the events GitHub already knows about.
func (p *Pusher) Plan(candidates []bridge.Candidate, mapped issue.Mapped) (bridge.Plan, error) {
	var plan bridge.Plan

	// One nodes(ids:) round for everything already upstream, rather than a
	// request per issue.
	var known []string
	for _, c := range candidates {
		if c.Upstream != "" {
			known = append(known, c.Upstream)
		}
	}
	// What this run will file upstream, so a link between two issues that are
	// both new is writable rather than declined: the command orders a delta
	// after the ones it links to, so the target has a node id by the time this
	// one is applied.
	pending := map[string]bool{}
	for _, c := range candidates {
		if c.Upstream == "" {
			pending[c.ID] = true
		}
	}

	upstream := map[string]ghapi.Issue{}
	if len(known) > 0 {
		issues, err := p.Client.FetchNodes(known)
		if err != nil {
			return plan, err
		}
		for _, gh := range issues {
			upstream[gh.ID] = gh
		}
	}

	for _, c := range candidates {
		if c.Upstream == "" {
			d, conflicts := issue.Diff(c.ID, entity.State{}, c.State, issue.Upstream{}, mapped)
			d.Create = true
			plan.Skipped = append(plan.Skipped, p.unsupported(&d, pending)...)
			plan.Deltas = append(plan.Deltas, d)
			plan.Conflicts = append(plan.Conflicts, conflicts...)
			continue
		}

		gh, ok := upstream[c.Upstream]
		if !ok {
			// The ledger names an object GitHub will not return: deleted, or
			// transferred somewhere this token cannot see. Refiling it would
			// duplicate an issue that may well still exist, so say so instead.
			plan.Skipped = append(plan.Skipped, bridge.Skip{
				ID:     c.ID,
				Reason: fmt.Sprintf("%s is not readable in %s", c.Upstream, p.Target),
			})
			continue
		}

		// The import runs unclaimed here on purpose: this is the tracker's state
		// in full, not a blob to write, and suppressing the comments this clone
		// posted would understate what is already there. Links still resolve —
		// see bridge.LinksOnly.
		imported, err := Import(p.Format, gh, bridge.LinksOnly(p.Known))
		if err != nil {
			return plan, fmt.Errorf("reading %s: %w", gh.URL, err)
		}
		events := imported.Events()

		up := issue.Upstream{
			State:    entity.Fold(p.Vocab, events),
			Comments: map[string]string{},
		}
		for _, com := range gh.Comments {
			up.Comments[com.ID] = com.Body
		}

		d, conflicts := issue.Diff(c.ID, issue.Base(p.Vocab, c.Events, events), c.State, up, mapped)
		plan.Conflicts = append(plan.Conflicts, conflicts...)
		// A conflicted issue is skipped whole. Sending its uncontended fields
		// would leave the issue half-pushed and the report unable to say what
		// upstream now holds.
		if len(conflicts) > 0 {
			continue
		}
		// Whatever this bridge cannot write is taken out of the delta and
		// reported, rather than left in to be half-sent.
		plan.Skipped = append(plan.Skipped, p.unsupported(&d, pending)...)
		if d.Empty() {
			continue
		}
		plan.Deltas = append(plan.Deltas, d)
	}
	return plan, nil
}

// unsupported names the fields this bridge cannot write, takes them out of the
// delta, and reports them — so a limitation is said out loud rather than
// silently dropped or half-attempted.
//
// `type` is GitHub's issue types, which the read path already degrades around
// because Enterprise servers of a certain age reject the field outright. Writing
// one needs a repository-level type id that those servers cannot supply either,
// so a type change is declared unsent rather than attempted and half-failed.
func (p *Pusher) unsupported(d *issue.Delta, pending map[string]bool) []bridge.Skip {
	var out []bridge.Skip
	if _, ok := d.Set["type"]; ok {
		out = append(out, bridge.Skip{
			ID:     d.ID,
			Field:  "type",
			Reason: "GitHub issue types cannot be set through this bridge yet",
		})
		delete(d.Set, "type")
	}
	return append(out, p.unwritableLinks(d, pending)...)
}

// unwritableLinks names the links this delta carries that GitHub will not take,
// and takes them back out of it. Three reasons, and they are different reasons:
//
//   - The kind has no mutation. GitHub offers `unmarkIssueAsDuplicate` and no
//     `markIssueAsDuplicate` at all, so a duplicate can be undone through the
//     API and never made. `related` is not a GitHub link in the first place.
//     An unknown kind — one a bridge carried in from another tracker — is in the
//     same position, and guessing at a mutation for it would be worse than
//     saying so.
//   - The target is not on this tracker. A relation names an entity and a
//     mutation names a node, so an issue that has never been pushed cannot be
//     linked to. Pushing it first is the whole fix, and the message says so.
//   - Removing a duplicate would be writable, but is not written: a bridge that
//     can undo a link it cannot make would let a mirror drift one way only.
func (p *Pusher) unwritableLinks(d *issue.Delta, pending map[string]bool) []bridge.Skip {
	var out []bridge.Skip
	keep := func(rels []issue.Relation) []issue.Relation {
		var kept []issue.Relation
		for _, r := range rels {
			switch {
			case r.Kind != issue.KindParent && r.Kind != issue.KindBlockedBy:
				out = append(out, bridge.Skip{
					ID:     d.ID,
					Field:  "links",
					Reason: fmt.Sprintf("GitHub has no way to write a %s link", r.Kind),
				})
			case !p.mapped(r.Target) && !pending[r.Target]:
				out = append(out, bridge.Skip{
					ID:     d.ID,
					Field:  "links",
					Reason: fmt.Sprintf("%s %s is not on %s yet — push it first", r.Kind, short(r.Target), p.Target),
				})
			default:
				kept = append(kept, r)
			}
		}
		return kept
	}
	d.RelationsAdded, d.RelationsRemoved = keep(d.RelationsAdded), keep(d.RelationsRemoved)
	return out
}

// mapped reports whether a link's target is something this tracker knows.
func (p *Pusher) mapped(entity string) bool {
	if p.Known == nil {
		return false
	}
	_, ok := p.Known.Upstream(entity)
	return ok
}

// Apply sends one delta.
//
// Order matters within an issue. A create carries its labels, assignees and
// milestone in the one call, because an issue created bare and then edited
// generates timeline events for changes that never happened — and the next
// import would read that invented history back.
func (p *Pusher) Apply(c bridge.Candidate, d issue.Delta) (bridge.Result, error) {
	if d.Create {
		return p.create(d)
	}

	result := bridge.Result{ID: d.ID}
	issueID := c.Upstream

	fields := map[string]any{}
	if v, ok := d.Set["title"]; ok {
		fields["title"] = v.Display()
	}
	if v, ok := d.Set["description"]; ok {
		fields["body"] = v.Display()
	}
	if v, ok := d.Set["milestone"]; ok {
		// A cleared milestone is a null, which is how updateIssue detaches one;
		// an empty string would be a title nothing matches.
		if title := v.Display(); title == "" {
			fields["milestoneId"] = nil
		} else {
			id, err := p.milestoneID(title)
			if err != nil {
				return result, err
			}
			fields["milestoneId"] = id
		}
	}
	if err := p.Client.UpdateIssue(issueID, fields); err != nil {
		return result, err
	}

	if v, ok := d.Set["status"]; ok {
		if v.Display() == issue.StatusOpen {
			if err := p.Client.ReopenIssue(issueID); err != nil {
				return result, err
			}
		} else {
			// The reason rides on the close rather than being a second write:
			// closeIssue records both in the one ClosedEvent the timeline replay
			// reads back.
			if err := p.Client.CloseIssue(issueID, d.Set["status.reason"].Display()); err != nil {
				return result, err
			}
		}
	}

	if err := p.applyLabels(issueID, d); err != nil {
		return result, err
	}
	if err := p.applyAssignees(issueID, d); err != nil {
		return result, err
	}
	if err := p.applyRelations(issueID, d); err != nil {
		return result, err
	}

	comments, err := p.applyComments(issueID, d)
	result.Comments = comments
	return result, err
}

func (p *Pusher) create(d issue.Delta) (bridge.Result, error) {
	result := bridge.Result{ID: d.ID}

	repoID, err := p.repository()
	if err != nil {
		return result, err
	}
	labelIDs, err := p.labelIDs(d.LabelsAdded)
	if err != nil {
		return result, err
	}
	assigneeIDs, err := p.assigneeIDs(d.AssigneesAdded)
	if err != nil {
		return result, err
	}
	milestoneID := ""
	if title := d.Set["milestone"].Display(); title != "" {
		if milestoneID, err = p.milestoneID(title); err != nil {
			return result, err
		}
	}

	// The parent goes in the create call rather than after it, for the reason
	// the labels do: a sub-issue added afterwards raises an event for a link
	// the issue was born with, and the next import reads that back as history.
	parentID, err := p.parentOf(d.RelationsAdded)
	if err != nil {
		return result, err
	}

	created, err := p.Client.CreateIssue(ghapi.NewIssueInput{
		RepoID:      repoID,
		Title:       d.Set["title"].Display(),
		Body:        d.Set["description"].Display(),
		LabelIDs:    labelIDs,
		AssigneeIDs: assigneeIDs,
		MilestoneID: milestoneID,
		ParentID:    parentID,
	})
	if err != nil {
		return result, err
	}
	result.Upstream, result.URL = created.ID, created.URL

	// Everything after this point can fail without losing the issue, because the
	// caller journals the mapping the moment this function returns it. That is
	// why the identity is set before the rest is attempted.
	if v, ok := d.Set["status"]; ok && v.Display() != issue.StatusOpen {
		if err := p.Client.CloseIssue(created.ID, d.Set["status.reason"].Display()); err != nil {
			return result, err
		}
	}
	// Every link except the parent the create already carried.
	if err := p.applyRelations(created.ID, issue.Delta{
		RelationsAdded: notParent(d.RelationsAdded),
	}); err != nil {
		return result, err
	}
	comments, err := p.applyComments(created.ID, d)
	result.Comments = comments
	return result, err
}

// applyRelations writes the links a delta carries, removals first so that
// re-filing an issue is a detach and then an attach — GitHub allows one parent,
// and the two halves arrive as one delta.
func (p *Pusher) applyRelations(issueID string, d issue.Delta) error {
	for _, r := range d.RelationsRemoved {
		target, err := p.upstream(r)
		if err != nil {
			return err
		}
		switch r.Kind {
		case issue.KindParent:
			err = p.Client.RemoveSubIssue(target, issueID)
		case issue.KindBlockedBy:
			err = p.Client.RemoveBlockedBy(issueID, target)
		}
		if err != nil {
			return err
		}
	}
	for _, r := range d.RelationsAdded {
		target, err := p.upstream(r)
		if err != nil {
			return err
		}
		switch r.Kind {
		case issue.KindParent:
			err = p.Client.AddSubIssue(target, issueID)
		case issue.KindBlockedBy:
			err = p.Client.AddBlockedBy(issueID, target)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// upstream is the object a link's target is filed as on this tracker.
//
// Plan has already declined every link whose target has none, so a miss here is
// a broken assumption rather than an ordinary state, and it is reported as an
// error rather than quietly skipped: the alternative is a push that says it
// wrote a link it did not.
func (p *Pusher) upstream(r issue.Relation) (string, error) {
	if p.Known != nil {
		if id, ok := p.Known.Upstream(r.Target); ok {
			return id, nil
		}
	}
	return "", fmt.Errorf("%s %s: the target is not on %s", r.Kind, short(r.Target), p.Target)
}

// parentOf is the parent among a delta's new links, resolved upstream.
func (p *Pusher) parentOf(rels []issue.Relation) (string, error) {
	for _, r := range rels {
		if r.Kind == issue.KindParent {
			return p.upstream(r)
		}
	}
	return "", nil
}

func notParent(rels []issue.Relation) []issue.Relation {
	var out []issue.Relation
	for _, r := range rels {
		if r.Kind != issue.KindParent {
			out = append(out, r)
		}
	}
	return out
}

// short abbreviates an entity id for a message, the way the CLI does.
func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func (p *Pusher) applyLabels(issueID string, d issue.Delta) error {
	add, err := p.labelIDs(d.LabelsAdded)
	if err != nil {
		return err
	}
	remove, err := p.labelIDs(d.LabelsRemoved)
	if err != nil {
		return err
	}
	if err := p.Client.AddLabels(issueID, add); err != nil {
		return err
	}
	return p.Client.RemoveLabels(issueID, remove)
}

func (p *Pusher) applyAssignees(issueID string, d issue.Delta) error {
	add, err := p.assigneeIDs(d.AssigneesAdded)
	if err != nil {
		return err
	}
	remove, err := p.assigneeIDs(d.AssigneesRemoved)
	if err != nil {
		return err
	}
	if err := p.Client.AddAssignees(issueID, add); err != nil {
		return err
	}
	return p.Client.RemoveAssignees(issueID, remove)
}

// applyComments posts, edits and deletes thread entries, returning the mapping
// for every entry it posted.
//
// The mappings are returned even on failure, which is deliberate: a comment
// posted and then not recorded is posted again on the next run, and the caller
// journals whatever comes back before it reports the error.
func (p *Pusher) applyComments(issueID string, d issue.Delta) ([]bridge.CommentOrigin, error) {
	var out []bridge.CommentOrigin

	for _, c := range d.CommentsNew {
		id, err := p.Client.AddComment(issueID, c.Entry.Body.Display())
		if err != nil {
			return out, err
		}
		out = append(out, bridge.CommentOrigin{EventID: c.Entry.ID(), Upstream: id})
	}
	for _, c := range d.CommentsEdited {
		if err := p.Client.UpdateComment(c.Upstream, c.Entry.Body.Display()); err != nil {
			return out, err
		}
	}
	for _, c := range d.CommentsRemoved {
		if err := p.Client.DeleteComment(c.Upstream); err != nil {
			return out, err
		}
	}
	return out, nil
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

// labelIDs resolves label names, creating nothing.
//
// A label the repository does not have is an error rather than something to
// invent: label colour and description are repository-wide settings this
// tracker does not model (docs/bridge-github.md), so a label created here would
// arrive with defaults nobody chose.
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
	return resolve(names, p.labels, "label", p.Target)
}

func (p *Pusher) milestoneID(title string) (string, error) {
	if p.milestones == nil {
		got, err := p.Client.Milestones(p.Target)
		if err != nil {
			return "", err
		}
		p.milestones = got
	}
	ids, err := resolve([]string{title}, p.milestones, "milestone", p.Target)
	if err != nil {
		return "", err
	}
	return ids[0], nil
}

// assigneeIDs resolves logins.
//
// Bridged authors carry a `github:` prefix, because `a` is hashed into every
// event and the scheme is part of the spelling. Assignee values are the bare
// login, but a value that arrived through another bridge may not be — so the
// prefix is stripped where present rather than assumed absent.
func (p *Pusher) assigneeIDs(logins []string) ([]string, error) {
	if len(logins) == 0 {
		return nil, nil
	}
	want := make([]string, 0, len(logins))
	for _, l := range logins {
		want = append(want, strings.TrimPrefix(l, "github:"))
	}
	found, err := p.Client.UserIDs(want)
	if err != nil {
		return nil, err
	}
	return resolve(want, found, "user", p.Target)
}

func resolve(names []string, index map[string]string, kind string, t ghapi.Target) ([]string, error) {
	out := make([]string, 0, len(names))
	var missing []string
	for _, name := range names {
		id, ok := index[name]
		if !ok {
			missing = append(missing, name)
			continue
		}
		out = append(out, id)
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("%s has no %s %s", t, kind, strings.Join(quoteAll(missing), ", "))
	}
	return out, nil
}

func quoteAll(names []string) []string {
	out := make([]string, 0, len(names))
	for _, n := range names {
		out = append(out, fmt.Sprintf("%q", n))
	}
	return out
}
