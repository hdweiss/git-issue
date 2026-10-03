// Write-back: turning what a local issue says into Gitea mutations.
//
// The comparison itself is not here and must not be — it is issue.Diff, named
// entirely in the issue vocabulary, so every bridge reuses it whole. What
// belongs here is the two ends: reading the tracker's current state, and
// mapping a delta onto the mutations of docs/bridge-gitea.md.

package giteaissue

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hdweiss/git-issue/internal/bridge"
	giteaapi "github.com/hdweiss/git-issue/internal/bridge/gitea/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

// Pusher writes local changes back to one Gitea repository.
type Pusher struct {
	Client *giteaapi.Client
	Target giteaapi.Target
	Format entity.ObjectFormat
	// Vocab folds both the local blob and a fresh import, so the two states are
	// always compared through the same lens.
	Vocab entity.Vocabulary
	// Known is the origin ledger, so that Plan's import resolves links the same
	// way the pull's did.
	Known bridge.Lookup

	labels     map[string]int64
	milestones map[string]int64
	assignable map[string]bool
}

// NewPusher builds a Pusher for one repository.
func NewPusher(client *giteaapi.Client, target giteaapi.Target, format entity.ObjectFormat, vocab entity.Vocabulary, known bridge.Lookup) *Pusher {
	return &Pusher{Client: client, Target: target, Format: format, Vocab: vocab, Known: known}
}

// Tracker names the repository as the origin ledger spells it.
func (p *Pusher) Tracker() string { return p.Target.String() }

// outcome is Plan's verdict on one candidate: the delta to send if any, and the
// skips and conflicts to report. Gathered per issue as the upstream reads
// stream in, then folded into the plan in candidate order.
type outcome struct {
	delta     issue.Delta
	hasDelta  bool
	skips     []bridge.Skip
	conflicts []issue.Conflict
}

// Plan works out what to send, and writes nothing.
//
// The tracker's current state is read by replaying the importer against it —
// see "Pushing to an external tracker" in docs/storage-model.md.
func (p *Pusher) Plan(candidates []bridge.Candidate, mapped issue.Mapped) (bridge.Plan, error) {
	var plan bridge.Plan

	// Confirm the repository is there and takes issues before planning anything.
	// An all-creates push otherwise makes no API call until the first Apply, and
	// a missing repo or a disabled tracker then surfaces as a per-issue error
	// with some issues already filed.
	if err := p.checkRepo(); err != nil {
		return plan, err
	}

	// Split the candidates: creates need nothing from upstream, mapped issues
	// each need their current state read back. pending lets a link to another
	// issue this same run is creating resolve.
	pending := map[string]bool{}
	var numbers []int64
	byNumber := map[int64]bridge.Candidate{}
	for _, c := range candidates {
		if n := parseNumber(c.Upstream); n != 0 {
			numbers = append(numbers, n)
			byNumber[n] = c
			continue
		}
		pending[c.ID] = true
	}

	known := bridge.LinksOnly(p.Known)

	// One per candidate, filled as each upstream issue streams in, then drained
	// in candidate order below so the plan reads the same however the feeds
	// happened to interleave.
	outcomes := make(map[string]*outcome, len(candidates))

	for _, c := range candidates {
		if parseNumber(c.Upstream) != 0 {
			continue
		}
		d, conflicts := issue.Diff(c.ID, entity.State{}, c.State, issue.Upstream{}, mapped)
		d.Create = true
		skips, err := p.unsupported(&d, pending)
		if err != nil {
			return plan, err
		}
		outcomes[c.ID] = &outcome{delta: d, hasDelta: true, skips: skips, conflicts: conflicts}
	}

	// Read the mapped issues one at a time rather than materialising the whole
	// set: a large mirror maps thousands, and holding every one with its feeds
	// in memory to diff them is what an OOM kill was landing on.
	if len(numbers) > 0 {
		err := p.Client.IssuesEach(p.Target, numbers, func(iss giteaapi.Issue) error {
			c, ok := byNumber[iss.Number]
			if !ok {
				return nil
			}
			o, err := p.planOne(c, iss, mapped, known, pending)
			if err != nil {
				return err
			}
			outcomes[c.ID] = o
			return nil
		})
		if err != nil {
			return plan, err
		}
	}

	for _, c := range candidates {
		o := outcomes[c.ID]
		if o == nil {
			// A mapped issue the server would not return — deleted upstream, or
			// moved out of this token's sight.
			plan.Skipped = append(plan.Skipped, bridge.Skip{
				ID:     c.ID,
				Reason: fmt.Sprintf("%s is not readable in %s", c.Upstream, p.Target),
			})
			continue
		}
		plan.Conflicts = append(plan.Conflicts, o.conflicts...)
		plan.Skipped = append(plan.Skipped, o.skips...)
		if o.hasDelta {
			plan.Deltas = append(plan.Deltas, o.delta)
		}
	}
	return plan, nil
}

// planOne diffs one mapped issue against its upstream state. It is the body of
// Plan's per-issue loop, lifted out so the streaming reader can call it as each
// issue arrives.
func (p *Pusher) planOne(c bridge.Candidate, iss giteaapi.Issue, mapped issue.Mapped, known bridge.Lookup, pending map[string]bool) (*outcome, error) {
	imported, err := Import(p.Format, p.Target, iss, known)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", iss.HTMLURL, err)
	}
	events := imported.Events()

	up := issue.Upstream{
		State:    entity.Fold(p.Vocab, events),
		Comments: map[string]string{},
	}
	for _, com := range iss.Comments {
		up.Comments[p.Target.CommentOrigin(iss.Number, com.ID)] = com.Body
	}

	d, conflicts := issue.Diff(c.ID, issue.Base(p.Vocab, c.Events, events), c.State, up, mapped)
	o := &outcome{conflicts: conflicts}
	if len(conflicts) > 0 {
		return o, nil
	}
	skips, err := p.unsupported(&d, pending)
	if err != nil {
		return nil, err
	}
	o.skips = skips
	if !d.Empty() {
		o.delta, o.hasDelta = d, true
	}
	return o, nil
}

// checkRepo turns a repository that is missing, private to this token, or has
// its issue tracker switched off into one clear error before the push starts.
func (p *Pusher) checkRepo() error {
	info, err := p.Client.Repo(p.Target)
	if err != nil {
		if e, ok := err.(*giteaapi.Error); ok && (e.Status == 404 || e.Status == 403) {
			return fmt.Errorf("%s: no such repository, or the token cannot see it — create it in Gitea, or check GITEA_TOKEN", p.Target)
		}
		return err
	}
	if info.Permissions != nil && !info.Permissions.Push {
		return fmt.Errorf("%s: the token has no write access to this repository", p.Target)
	}
	if !info.HasIssues {
		return fmt.Errorf("%s: the issue tracker is disabled on this repository", p.Target)
	}
	return nil
}

// unsupported names what this bridge declines to write, takes it out of the
// delta, and reports it.
//
// `type` is Gitea's absence of issue types: there is nothing to write a type
// change to, so it is declared unsent rather than attempted.
func (p *Pusher) unsupported(d *issue.Delta, pending map[string]bool) ([]bridge.Skip, error) {
	var out []bridge.Skip
	if _, ok := d.Set["type"]; ok {
		out = append(out, bridge.Skip{
			ID:     d.ID,
			Field:  "type",
			Reason: "Gitea has no issue types",
		})
		delete(d.Set, "type")
	}
	out = append(out, p.unwritableLinks(d, pending)...)

	labelSkips, err := p.unknownLabels(d)
	if err != nil {
		return out, err
	}
	out = append(out, labelSkips...)

	assigneeSkips, err := p.unknownAssignees(d)
	if err != nil {
		return out, err
	}
	out = append(out, assigneeSkips...)

	milestoneSkip, err := p.unknownMilestone(d)
	if err != nil {
		return out, err
	}
	return append(out, milestoneSkip...), nil
}

// unknownMilestone drops a milestone this delta sets that the repository does
// not have, and reports it — the same "seeded from another tracker" reasoning
// as labels and assignees. Detaching a milestone (an empty title) is always
// writable and is left alone.
func (p *Pusher) unknownMilestone(d *issue.Delta) ([]bridge.Skip, error) {
	v, ok := d.Set["milestone"]
	if !ok || v.Display() == "" {
		return nil, nil
	}
	title := v.Display()
	if p.milestones == nil {
		got, err := p.Client.Milestones(p.Target)
		if err != nil {
			return nil, err
		}
		p.milestones = got
	}
	if _, ok := p.milestones[title]; ok {
		return nil, nil
	}
	delete(d.Set, "milestone")
	return []bridge.Skip{{
		ID:     d.ID,
		Field:  "milestone",
		Reason: fmt.Sprintf("%s has no milestone %q — create it in Gitea first", p.Target, title),
	}}, nil
}

// unknownAssignees drops an assignee this delta adds who cannot be assigned to
// an issue in this repository, and reports it — Gitea rejects the whole write
// with a 422 otherwise, and an assignee who is not a member of the target is a
// warning, not a reason to abandon a push.
//
// Only additions are checked. A removal names someone to take *off* the issue,
// which is exactly the case where a since-departed collaborator most needs the
// change to go through, so removals are left alone.
func (p *Pusher) unknownAssignees(d *issue.Delta) ([]bridge.Skip, error) {
	if len(d.AssigneesAdded) == 0 {
		return nil, nil
	}
	if err := p.loadAssignable(); err != nil {
		return nil, err
	}

	var out []bridge.Skip
	var kept []string
	for _, a := range d.AssigneesAdded {
		if p.assignable[assigneeKey(a)] {
			kept = append(kept, a)
			continue
		}
		out = append(out, bridge.Skip{
			ID:     d.ID,
			Field:  "assignee",
			Reason: fmt.Sprintf("%s cannot be assigned to an issue in %s", assigneeLogin(a), p.Target),
		})
	}
	d.AssigneesAdded = kept
	return out, nil
}

func (p *Pusher) loadAssignable() error {
	if p.assignable != nil {
		return nil
	}
	got, err := p.Client.Assignees(p.Target)
	if err != nil {
		return err
	}
	p.assignable = got
	return nil
}

// assigneeLogin strips a bridge scheme prefix ("gitea:", or "github:" on a value
// that arrived through another bridge) from an assignee value.
func assigneeLogin(a string) string {
	if _, rest, ok := strings.Cut(a, ":"); ok {
		return rest
	}
	return a
}

func assigneeKey(a string) string { return strings.ToLower(assigneeLogin(a)) }

// unknownLabels drops a label this delta adds that the repository (and its
// organization) has no such label for, and reports it — creating one would give
// it a colour and description nobody chose, so an unknown label is a warning
// rather than a failed push. An unknown label being *removed* is simply dropped:
// the issue cannot be carrying a label that does not exist, so there is nothing
// to do and nothing to say.
func (p *Pusher) unknownLabels(d *issue.Delta) ([]bridge.Skip, error) {
	if len(d.LabelsAdded) == 0 && len(d.LabelsRemoved) == 0 {
		return nil, nil
	}
	if p.labels == nil {
		got, err := p.Client.Labels(p.Target)
		if err != nil {
			return nil, err
		}
		p.labels = got
	}

	var out []bridge.Skip
	keep := func(names []string, report bool) []string {
		var kept []string
		for _, name := range names {
			if _, ok := p.labels[name]; ok {
				kept = append(kept, name)
				continue
			}
			if report {
				out = append(out, bridge.Skip{
					ID:     d.ID,
					Field:  "label",
					Reason: fmt.Sprintf("%s has no label %q — create it in Gitea first", p.Target, name),
				})
			}
		}
		return kept
	}
	d.LabelsAdded = keep(d.LabelsAdded, true)
	d.LabelsRemoved = keep(d.LabelsRemoved, false)
	return out, nil
}

// unwritableLinks names the links this delta carries that Gitea will not take.
//
// Only blocked-by is writable, through the issue-dependencies API. Gitea has no
// cross-issue hierarchy and no duplicate marking, `related` is not a Gitea link,
// and a kind another bridge carried in has no mutation to guess at — all of
// those are reported and dropped. A blocked-by whose target has never been
// pushed is dropped too, with the fix named.
func (p *Pusher) unwritableLinks(d *issue.Delta, pending map[string]bool) []bridge.Skip {
	var out []bridge.Skip
	keep := func(rels []issue.Relation) []issue.Relation {
		var kept []issue.Relation
		for _, r := range rels {
			switch {
			case r.Kind != issue.KindBlockedBy:
				out = append(out, bridge.Skip{
					ID:     d.ID,
					Field:  "links",
					Reason: fmt.Sprintf("Gitea has no way to write a %s link", r.Kind),
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

func (p *Pusher) mapped(entity string) bool {
	if p.Known == nil {
		return false
	}
	_, ok := p.Known.Upstream(entity)
	return ok
}

// short abbreviates an entity id for a message, the way the CLI does.
func short(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

// Apply sends one delta.
func (p *Pusher) Apply(c bridge.Candidate, d issue.Delta) (bridge.Result, error) {
	if d.Create {
		return p.create(d)
	}

	result := bridge.Result{ID: d.ID}
	number := parseNumber(c.Upstream)
	if number == 0 {
		return result, fmt.Errorf("%s: %q is not a Gitea issue", d.ID, c.Upstream)
	}

	// Only the current scalar fields and assignee list are needed here, not the
	// comment, timeline and dependency feeds — Gitea replaces the whole assignee
	// list on write, so it has to be read first.
	current, err := p.Client.IssueBare(p.Target, number)
	if err != nil {
		return result, err
	}

	edit, err := p.edit(d, current)
	if err != nil {
		return result, err
	}
	if err := p.Client.EditIssue(p.Target, number, edit); err != nil {
		return result, err
	}

	if err := p.applyLabels(number, d); err != nil {
		return result, err
	}
	if err := p.applyRelations(number, d); err != nil {
		return result, err
	}

	comments, err := p.applyComments(number, d)
	result.Comments = comments
	return result, err
}

// edit maps a delta onto the fields EditIssue covers, reading the current
// assignee list off `current` because Gitea replaces the whole list rather than
// patching it.
func (p *Pusher) edit(d issue.Delta, current giteaapi.Issue) (giteaapi.EditIssueOption, error) {
	var opt giteaapi.EditIssueOption

	if v, ok := d.Set["title"]; ok {
		s := v.Display()
		opt.Title = &s
	}
	if v, ok := d.Set["description"]; ok {
		s := v.Display()
		opt.Body = &s
	}
	if v, ok := d.Set["status"]; ok {
		state := "closed"
		if v.Display() == issue.StatusOpen {
			state = "open"
		}
		opt.State = &state
	}
	if v, ok := d.Set["milestone"]; ok {
		if title := v.Display(); title == "" {
			zero := int64(0)
			opt.Milestone = &zero
		} else {
			id, err := p.milestoneID(title)
			if err != nil {
				return opt, err
			}
			opt.Milestone = &id
		}
	}

	if len(d.AssigneesAdded) > 0 || len(d.AssigneesRemoved) > 0 {
		opt.Assignees = p.assignees(current.Assignees, d)
	}
	return opt, nil
}

// assignees folds a list delta back into the whole list Gitea wants.
//
// Plan has already dropped every addition that names someone this repository
// cannot assign, so the add loop trusts its input; the current list is Gitea's
// own and is kept as-is, minus whoever the delta retracts.
func (p *Pusher) assignees(current []giteaapi.User, d issue.Delta) []string {
	out := []string{}
	drop := map[string]bool{}
	for _, a := range d.AssigneesRemoved {
		drop[assigneeKey(a)] = true
	}
	for _, u := range current {
		name := u.Name()
		if name != "" && !drop[strings.ToLower(name)] {
			out = append(out, name)
		}
	}
	for _, a := range d.AssigneesAdded {
		name := assigneeLogin(a)
		if !containsFold(out, name) {
			out = append(out, name)
		}
	}
	return out
}

func (p *Pusher) applyLabels(number int64, d issue.Delta) error {
	add, err := p.labelIDs(d.LabelsAdded)
	if err != nil {
		return err
	}
	remove, err := p.labelIDs(d.LabelsRemoved)
	if err != nil {
		return err
	}
	if err := p.Client.AddLabels(p.Target, number, add); err != nil {
		return err
	}
	for _, id := range remove {
		if err := p.Client.RemoveLabel(p.Target, number, id); err != nil {
			return err
		}
	}
	return nil
}

// applyRelations writes the blocked-by links a delta carries. Plan has already
// declined every other kind and every target with no issue here, so a miss in
// upstreamNumber is a broken assumption and is reported as an error rather than
// quietly skipped.
func (p *Pusher) applyRelations(number int64, d issue.Delta) error {
	for _, r := range d.RelationsRemoved {
		target, err := p.upstreamNumber(r)
		if err != nil {
			return err
		}
		if err := p.Client.RemoveDependency(p.Target, number, target); err != nil {
			return err
		}
	}
	for _, r := range d.RelationsAdded {
		target, err := p.upstreamNumber(r)
		if err != nil {
			return err
		}
		if err := p.Client.AddDependency(p.Target, number, target); err != nil {
			return err
		}
	}
	return nil
}

func (p *Pusher) upstreamNumber(r issue.Relation) (int64, error) {
	if p.Known != nil {
		if origin, ok := p.Known.Upstream(r.Target); ok {
			if n := parseNumber(origin); n != 0 {
				return n, nil
			}
		}
	}
	return 0, fmt.Errorf("%s %s: the target is not on %s", r.Kind, short(r.Target), p.Target)
}

// applyComments posts, edits and deletes thread entries, returning the mapping
// for every entry it posted — even on failure, so a comment posted and then not
// recorded is not posted again.
func (p *Pusher) applyComments(number int64, d issue.Delta) ([]bridge.CommentOrigin, error) {
	var out []bridge.CommentOrigin

	for _, c := range d.CommentsNew {
		created, err := p.Client.AddComment(p.Target, number, c.Entry.Body.Display())
		if err != nil {
			return out, err
		}
		out = append(out, bridge.CommentOrigin{
			EventID:  c.Entry.ID(),
			Upstream: p.Target.CommentOrigin(number, created.ID),
		})
	}
	for _, c := range d.CommentsEdited {
		id := commentID(c.Upstream)
		if id == 0 {
			continue
		}
		if err := p.Client.EditComment(p.Target, id, c.Entry.Body.Display()); err != nil {
			return out, err
		}
	}
	for _, c := range d.CommentsRemoved {
		id := commentID(c.Upstream)
		if id == 0 {
			continue
		}
		if err := p.Client.DeleteComment(p.Target, id); err != nil {
			return out, err
		}
	}
	return out, nil
}

func (p *Pusher) create(d issue.Delta) (bridge.Result, error) {
	result := bridge.Result{ID: d.ID}

	labelIDs, err := p.labelIDs(d.LabelsAdded)
	if err != nil {
		return result, err
	}
	milestoneID := int64(0)
	if title := d.Set["milestone"].Display(); title != "" {
		if milestoneID, err = p.milestoneID(title); err != nil {
			return result, err
		}
	}
	assignees := make([]string, 0, len(d.AssigneesAdded))
	for _, a := range d.AssigneesAdded {
		assignees = append(assignees, assigneeLogin(a))
	}

	closed := false
	if v, ok := d.Set["status"]; ok && v.Display() != issue.StatusOpen && v.Display() != "" {
		closed = true
	}

	created, err := p.Client.CreateIssue(p.Target, giteaapi.CreateIssueOption{
		Title:     d.Set["title"].Display(),
		Body:      d.Set["description"].Display(),
		Assignees: assignees,
		Milestone: milestoneID,
		Labels:    labelIDs,
		Closed:    closed,
	})
	if err != nil {
		return result, err
	}
	result.Upstream = p.Target.Origin(created.Number)
	result.URL = created.HTMLURL

	// Everything after this point can fail without losing the issue: the caller
	// journals the mapping the moment this returns it.
	if closed && created.State != "closed" {
		state := "closed"
		if err := p.Client.EditIssue(p.Target, created.Number, giteaapi.EditIssueOption{State: &state}); err != nil {
			return result, err
		}
	}
	for _, r := range d.RelationsAdded {
		target, err := p.upstreamNumber(r)
		if err != nil {
			return result, err
		}
		if err := p.Client.AddDependency(p.Target, created.Number, target); err != nil {
			return result, err
		}
	}
	comments, err := p.applyComments(created.Number, d)
	result.Comments = comments
	return result, err
}

// labelIDs resolves label names to ids, creating nothing.
//
// Plan has already reported and dropped every label this repository has no such
// name for, so a name that still does not resolve here is one deleted upstream
// between the plan and the apply. It is skipped rather than failing the push —
// the same "a missing label is a warning, not an error" rule Plan applies.
func (p *Pusher) labelIDs(names []string) ([]int64, error) {
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
	out := make([]int64, 0, len(names))
	for _, name := range names {
		if id, ok := p.labels[name]; ok {
			out = append(out, id)
		}
	}
	return out, nil
}

func (p *Pusher) milestoneID(title string) (int64, error) {
	if p.milestones == nil {
		got, err := p.Client.Milestones(p.Target)
		if err != nil {
			return 0, err
		}
		p.milestones = got
	}
	id, ok := p.milestones[title]
	if !ok {
		return 0, fmt.Errorf("%s has no milestone %q", p.Target, title)
	}
	return id, nil
}

// parseNumber reads the issue number out of an identity string, returning 0 for
// anything that is not one — an id belonging to another tracker, or a ledger
// line from a version that spelled them differently.
func parseNumber(origin string) int64 {
	_, after, ok := strings.Cut(origin, "#")
	if !ok {
		return 0
	}
	if slash := strings.IndexByte(after, '/'); slash >= 0 {
		after = after[:slash]
	}
	n, err := strconv.ParseInt(after, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

// commentID reads the comment id out of a comment identity string.
func commentID(origin string) int64 {
	_, after, ok := strings.Cut(origin, "/comments/")
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(after, 10, 64)
	if err != nil {
		return 0
	}
	return n
}

func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
