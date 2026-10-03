// Write-back: turning what a local issue says into Azure DevOps mutations.
//
// The comparison itself is not here and must not be — it is issue.Diff, named
// entirely in the issue vocabulary, so every bridge reuses it whole. What
// belongs here is the two ends: reading the tracker's current state, and
// mapping a delta onto the mutations of docs/bridge-ado.md.

package adoissue

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/hdweiss/git-issue/internal/bridge"
	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
)

// DefaultType is the work item type a created issue gets when nothing says
// otherwise. Every stock process has it, which is more than can be said for
// User Story, Product Backlog Item or Requirement — those are the same concept
// under three names, one per process, and picking one would fail on the other
// two.
const DefaultType = "Issue"

// Pusher writes local changes back to one Azure DevOps project.
type Pusher struct {
	Client *adoapi.Client
	Target adoapi.Target
	Format entity.ObjectFormat
	// Vocab folds both the local blob and a fresh import, so the two states are
	// always compared through the same lens.
	Vocab entity.Vocabulary
	// Known is the origin ledger, so that Plan's import resolves parent links
	// the same way the pull's did. An import that saw less would report a
	// difference that is not there and push it forever.
	Known bridge.Lookup
}

// NewPusher builds a Pusher for one project.
func NewPusher(client *adoapi.Client, target adoapi.Target, format entity.ObjectFormat, vocab entity.Vocabulary, known bridge.Lookup) *Pusher {
	return &Pusher{Client: client, Target: target, Format: format, Vocab: vocab, Known: known}
}

// Tracker names the project as the origin ledger spells it.
func (p *Pusher) Tracker() string { return p.Target.String() }

// Plan works out what to send, and writes nothing.
//
// The tracker's current state is read by replaying the importer against it —
// see "Pushing to an external tracker" in docs/storage-model.md. That is what
// makes a push need no record of its own: the events a faithful import would
// produce are exactly the events Azure DevOps already knows about.
func (p *Pusher) Plan(candidates []bridge.Candidate, mapped issue.Mapped) (bridge.Plan, error) {
	var plan bridge.Plan

	// One batch for everything already upstream, rather than a query per issue.
	var ids []int
	for _, c := range candidates {
		if id := WorkItemID(c.Upstream); id != 0 {
			ids = append(ids, id)
		}
	}
	upstream := map[int]adoapi.WorkItem{}
	if len(ids) > 0 {
		items, err := p.Client.FetchIDs(p.Target, ids, nil)
		if err != nil {
			return plan, err
		}
		for _, w := range items {
			upstream[w.ID] = w
		}
	}

	// What this run will file upstream, so a link between two work items that
	// are both new is writable rather than declined: the command orders a delta
	// after the ones it links to, so the target has an id by the time this one
	// is applied.
	pending := map[string]bool{}
	for _, c := range candidates {
		if WorkItemID(c.Upstream) == 0 {
			pending[c.ID] = true
		}
	}

	known := bridge.LinksOnly(p.Known)

	for _, c := range candidates {
		id := WorkItemID(c.Upstream)
		if id == 0 {
			d, conflicts := issue.Diff(c.ID, entity.State{}, c.State, issue.Upstream{}, mapped)
			d.Create = true
			plan.Skipped = append(plan.Skipped, p.unsupported(&d, pending)...)
			plan.Deltas = append(plan.Deltas, d)
			plan.Conflicts = append(plan.Conflicts, conflicts...)
			continue
		}

		w, ok := upstream[id]
		if !ok {
			// The ledger names a work item Azure DevOps will not return:
			// deleted, or in a project this token cannot see. Refiling it would
			// duplicate something that may well still exist, so say so instead.
			plan.Skipped = append(plan.Skipped, bridge.Skip{
				ID:     c.ID,
				Reason: fmt.Sprintf("%s is not readable in %s", c.Upstream, p.Target),
			})
			continue
		}

		imported, err := Import(p.Format, p.Target, w, known)
		if err != nil {
			return plan, fmt.Errorf("reading work item %d: %w", w.ID, err)
		}
		events := imported.Events()

		up := issue.Upstream{
			State:    entity.Fold(p.Vocab, events),
			Comments: map[string]string{},
		}
		for _, com := range w.Comments {
			up.Comments[CommentOrigin(p.Target, w.ID, com.ID)] = com.Text
		}

		d, conflicts := issue.Diff(c.ID, issue.Base(p.Vocab, c.Events, events), c.State, up, mapped)
		plan.Conflicts = append(plan.Conflicts, conflicts...)
		// A conflicted issue is skipped whole. Sending its uncontended fields
		// would leave the issue half-pushed and the report unable to say what
		// upstream now holds.
		if len(conflicts) > 0 {
			continue
		}
		if skips := p.unsupported(&d, pending); len(skips) > 0 {
			plan.Skipped = append(plan.Skipped, skips...)
		}
		if d.Empty() {
			continue
		}
		plan.Deltas = append(plan.Deltas, d)
	}
	return plan, nil
}

// unsupported names what this bridge declines to write, takes it out of the
// delta, and reports it — so a limitation is said out loud rather than silently
// dropped.
func (p *Pusher) unsupported(d *issue.Delta, pending map[string]bool) []bridge.Skip {
	out := p.unwritableLinks(d, pending)
	if _, ok := d.Set["type"]; ok {
		// Changing System.WorkItemType is not a plain field write: Azure DevOps
		// restricts which types a work item may become, and the rules are per
		// process and invisible to this client. Declaring it unsent beats
		// attempting it and half-failing.
		out = append(out, bridge.Skip{
			ID:     d.ID,
			Field:  "type",
			Reason: "the work item type cannot be changed through this bridge",
		})
		delete(d.Set, "type")
	}
	return out
}

// unwritableLinks names the links this delta carries that cannot be sent, and
// takes them back out of it. Two reasons, and they are different reasons:
//
//   - The kind has no Azure DevOps link type. Every kind docs/issues.md defines
//     has one — this is the more capable of the two bridges on links, since a
//     duplicate really can be marked here — so what is left is a kind another
//     bridge carried in. Guessing at a link type for it would be worse than
//     saying so.
//   - The target is not on this tracker. A relation names an entity and a patch
//     names a work item by URL, so an issue that has never been pushed cannot be
//     linked to. Pushing it first is the whole fix, and the message says so.
func (p *Pusher) unwritableLinks(d *issue.Delta, pending map[string]bool) []bridge.Skip {
	var out []bridge.Skip
	keep := func(rels []issue.Relation) []issue.Relation {
		var kept []issue.Relation
		for _, r := range rels {
			switch {
			case !hasLinkType(r.Kind):
				out = append(out, bridge.Skip{
					ID:     d.ID,
					Field:  "links",
					Reason: fmt.Sprintf("Azure DevOps has no link type for a %s link", r.Kind),
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

func hasLinkType(kind string) bool {
	_, ok := LinkType(kind)
	return ok
}

// mapped reports whether a link's target is something this tracker knows.
func (p *Pusher) mapped(entity string) bool {
	if p.Known == nil {
		return false
	}
	_, ok := p.Known.Upstream(entity)
	return ok
}

// links turns a delta's relations into the patch operations that write them.
//
// Plan has already declined every link this bridge cannot send, so a target
// without a work item here is a broken assumption rather than an ordinary
// state, and it is reported as an error rather than quietly skipped: the
// alternative is a push that says it wrote a link it did not.
func (p *Pusher) links(d issue.Delta) (adoapi.LinkOps, error) {
	var ops adoapi.LinkOps
	convert := func(rels []issue.Relation) ([]adoapi.Relation, error) {
		var out []adoapi.Relation
		for _, r := range rels {
			rel, ok := LinkType(r.Kind)
			if !ok {
				return nil, fmt.Errorf("%s %s: no Azure DevOps link type", r.Kind, short(r.Target))
			}
			id, err := p.workItem(r)
			if err != nil {
				return nil, err
			}
			out = append(out, adoapi.Relation{Rel: rel, URL: p.Target.WorkItemURL(id)})
		}
		return out, nil
	}

	var err error
	// Removals first, so that re-filing a work item is a detach and then an
	// attach: Azure DevOps allows one parent, and the two halves arrive as one
	// delta. UpdateWorkItem keeps that order in the document it builds.
	if ops.Remove, err = convert(d.RelationsRemoved); err != nil {
		return ops, err
	}
	ops.Add, err = convert(d.RelationsAdded)
	return ops, err
}

// workItem is the work item a link's target is filed as on this tracker.
func (p *Pusher) workItem(r issue.Relation) (int, error) {
	if p.Known != nil {
		if origin, ok := p.Known.Upstream(r.Target); ok {
			if id := WorkItemID(origin); id != 0 {
				return id, nil
			}
		}
	}
	return 0, fmt.Errorf("%s %s: the target is not on %s", r.Kind, short(r.Target), p.Target)
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
	id := WorkItemID(c.Upstream)
	if id == 0 {
		return result, fmt.Errorf("%s: %q is not a work item", d.ID, c.Upstream)
	}

	// The rev is what the patch's leading test names, and the relations array is
	// what a link removal indexes into, so both come from one read of the work
	// item as it stands.
	current, err := p.current(id)
	if err != nil {
		return result, err
	}

	links, err := p.links(d)
	if err != nil {
		return result, err
	}
	// Fields and links in the one patch. Two documents would be two revisions
	// for one thing that happened, and the next import would read them back as
	// two.
	if err := p.Client.UpdateWorkItem(p.Target, current, p.fields(d, current), links); err != nil {
		return result, err
	}

	comments, err := p.applyComments(id, d)
	result.Comments = comments
	return result, err
}

// fields maps a delta onto the fields of an existing work item.
//
// System.AreaPath is not among them, and never can be. An area is scope rather
// than state, so nothing local holds one and no delta can carry one — a push
// therefore leaves every work item exactly where it is filed. See "The area is
// scope, and only scope" in docs/bridge-ado.md.
func (p *Pusher) fields(d issue.Delta, current adoapi.WorkItem) []adoapi.Field {
	var fields []adoapi.Field

	if v, ok := d.Set["title"]; ok {
		fields = append(fields, adoapi.Field{Name: adoapi.FieldTitle, Value: v.Display()})
	}
	if v, ok := d.Set["description"]; ok {
		fields = append(fields, adoapi.Field{Name: adoapi.BodyField(current.Type), Value: v.Display()})
	}
	if v, ok := d.Set["status"]; ok {
		// The state is written as the issue holds it, because that is how it
		// was read: this bridge stores System.State verbatim rather than
		// mapping it onto open and closed. A state the process does not define
		// is refused by the server, which is the right place for that to fail.
		fields = append(fields, adoapi.Field{Name: adoapi.FieldState, Value: v.Display()})
	}
	if v, ok := d.Set["milestone"]; ok {
		fields = append(fields, adoapi.Field{Name: adoapi.FieldIteration, Value: p.iterationPath(v.Display())})
	}

	if tags, changed := p.tags(current.Tags, d); changed {
		fields = append(fields, adoapi.Field{Name: adoapi.FieldTags, Value: adoapi.JoinTags(tags)})
	}
	if who, changed := p.assignee(current.AssignedTo.UniqueName, d); changed {
		fields = append(fields, adoapi.Field{Name: adoapi.FieldAssignedTo, Value: who})
	}
	return fields
}

// tags folds a list delta back into the single string Azure DevOps stores.
//
// The whole field is rewritten because that is the only way to write it, which
// is why the current value has to be read first: sending only the additions
// would drop every tag the work item already had.
func (p *Pusher) tags(current []string, d issue.Delta) ([]string, bool) {
	if len(d.LabelsAdded) == 0 && len(d.LabelsRemoved) == 0 {
		return nil, false
	}
	out := append([]string(nil), current...)
	out = difference(out, d.LabelsRemoved)
	for _, tag := range d.LabelsAdded {
		if !containsFold(out, tag) {
			out = append(out, tag)
		}
	}
	return out, true
}

// assignee folds a list delta back onto the one identity Azure DevOps holds.
//
// A removal that leaves nobody clears the field; an addition takes the first
// name, and any beyond it was already reported as skipped by Plan.
func (p *Pusher) assignee(current string, d issue.Delta) (string, bool) {
	if len(d.AssigneesAdded) == 0 && len(d.AssigneesRemoved) == 0 {
		return "", false
	}
	if len(d.AssigneesAdded) > 0 {
		return d.AssigneesAdded[0], true
	}
	for _, gone := range d.AssigneesRemoved {
		if strings.EqualFold(gone, current) {
			return "", true
		}
	}
	return "", false
}

// iterationPath turns a milestone name back into an iteration path. A cleared
// milestone is the project root, which is Azure DevOps' way of saying a work
// item is not scheduled.
func (p *Pusher) iterationPath(milestone string) string {
	if milestone == "" {
		return p.Target.Project
	}
	return p.Target.Project + `\` + strings.ReplaceAll(strings.Trim(milestone, "/"), "/", `\`)
}

func (p *Pusher) create(d issue.Delta) (bridge.Result, error) {
	result := bridge.Result{ID: d.ID}

	workItemType := DefaultType
	if v, ok := d.Set["type"]; ok && v.Display() != "" {
		workItemType = v.Display()
	}

	fields := []adoapi.Field{
		{Name: adoapi.FieldTitle, Value: d.Set["title"].Display()},
	}
	if body := d.Set["description"].Display(); body != "" {
		fields = append(fields, adoapi.Field{Name: adoapi.BodyField(workItemType), Value: body})
	}
	if v, ok := d.Set["status"]; ok && v.Display() != "" {
		fields = append(fields, adoapi.Field{Name: adoapi.FieldState, Value: v.Display()})
	}
	if v, ok := d.Set["milestone"]; ok && v.Display() != "" {
		fields = append(fields, adoapi.Field{Name: adoapi.FieldIteration, Value: p.iterationPath(v.Display())})
	}
	if len(d.LabelsAdded) > 0 {
		fields = append(fields, adoapi.Field{Name: adoapi.FieldTags, Value: adoapi.JoinTags(d.LabelsAdded)})
	}
	if len(d.AssigneesAdded) > 0 {
		fields = append(fields, adoapi.Field{Name: adoapi.FieldAssignedTo, Value: d.AssigneesAdded[0]})
	}
	// The one place System.AreaPath is ever written: where a new work item is
	// filed. An empty area is the project root, which is where Azure DevOps
	// would have put it anyway.
	if area := p.Target.AreaPath(); area != "" {
		fields = append(fields, adoapi.Field{Name: adoapi.FieldAreaPath, Value: area})
	}

	// A created work item has nothing to detach from, so only the additions can
	// be here — and they go in the create call rather than after it, for the
	// reason the fields do.
	links, err := p.links(d)
	if err != nil {
		return result, err
	}

	// The whole item in one call. One created bare and then edited would
	// generate revisions for changes that never happened, and the next import
	// would read that invented history back.
	created, err := p.Client.CreateWorkItem(p.Target, workItemType, fields, links.Add)
	if err != nil {
		return result, err
	}
	result.Upstream = Origin(p.Target, created.ID)
	result.URL = p.Target.WebURL(created.ID)

	// Everything after this point can fail without losing the work item,
	// because the caller journals the mapping the moment this returns it. That
	// is why the identity is set before the rest is attempted.
	comments, err := p.applyComments(created.ID, d)
	result.Comments = comments
	return result, err
}

// applyComments posts, edits and deletes thread entries, returning the mapping
// for every entry it posted.
//
// The mappings are returned even on failure, which is deliberate: a comment
// posted and then not recorded is posted again on the next run, and the caller
// journals whatever comes back before it reports the error.
func (p *Pusher) applyComments(workItem int, d issue.Delta) ([]bridge.CommentOrigin, error) {
	var out []bridge.CommentOrigin

	for _, c := range d.CommentsNew {
		id, err := p.Client.AddComment(p.Target, workItem, c.Entry.Body.Display())
		if err != nil {
			return out, err
		}
		out = append(out, bridge.CommentOrigin{
			EventID:  c.Entry.ID(),
			Upstream: CommentOrigin(p.Target, workItem, id),
		})
	}
	for _, c := range d.CommentsEdited {
		id := commentID(c.Upstream)
		if id == 0 {
			continue
		}
		if err := p.Client.UpdateComment(p.Target, workItem, id, c.Entry.Body.Display()); err != nil {
			return out, err
		}
	}
	for _, c := range d.CommentsRemoved {
		id := commentID(c.Upstream)
		if id == 0 {
			continue
		}
		if err := p.Client.DeleteComment(p.Target, workItem, id); err != nil {
			return out, err
		}
	}
	return out, nil
}

// current re-reads one work item, for its revision number and the fields a
// whole-field write has to preserve.
func (p *Pusher) current(id int) (adoapi.WorkItem, error) {
	items, err := p.Client.FetchIDs(p.Target, []int{id}, nil)
	if err != nil {
		return adoapi.WorkItem{}, err
	}
	if len(items) == 0 {
		return adoapi.WorkItem{}, fmt.Errorf("work item %d is not readable in %s", id, p.Target)
	}
	return items[0], nil
}

// WorkItemID reads the work item number out of an identity string, returning 0
// for anything that is not one — an id belonging to another tracker, or a
// ledger line from a version that spelled them differently.
func WorkItemID(origin string) int {
	_, after, ok := strings.Cut(origin, "#")
	if !ok {
		return 0
	}
	// Anything past the number belongs to a comment or a revision, not to the
	// work item itself.
	if slash := strings.Index(after, "/"); slash >= 0 {
		after = after[:slash]
	}
	id, err := strconv.Atoi(after)
	if err != nil {
		return 0
	}
	return id
}

// commentID reads the comment number out of a comment identity string.
func commentID(origin string) int {
	_, after, ok := strings.Cut(origin, "/comments/")
	if !ok {
		return 0
	}
	id, err := strconv.Atoi(after)
	if err != nil {
		return 0
	}
	return id
}

func containsFold(values []string, want string) bool {
	for _, v := range values {
		if strings.EqualFold(v, want) {
			return true
		}
	}
	return false
}
