package issue

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/render"
)

// The commit message for one action, per docs/storage-model.md's "Provenance".
//
// A commit on a notes ref used to say "Import 40 issues from owner/name", or
// whatever `git notes append` puts there, which made `git log` on the tracker
// worth nothing. What it says now is what somebody did, in the words the issue
// vocabulary already uses, so that the ref's own history is a second reading of
// the tracker rather than a record that it was written to.
//
// The message is not authoritative and must never be parsed back into state.
// It is prose about events whose own bytes are the truth, and — unlike those
// bytes — it does not converge: two clones that import the same issue
// independently produce identical blobs and two sets of commits, so a merged
// log lists every action twice. Confirmed empirically. `Issue:` is a trailer so
// that a reader can get from a commit back to the entity, not so that anything
// can reconstruct the entity from commits.

// Trailer is one `Key: value` line at the foot of a commit message.
type Trailer struct{ Key, Value string }

// Action is what a commit message needs to know: which issue was changed, what
// it was called before the change, the events the change wrote, and anything a
// bridge wants to add at the foot.
type Action struct {
	ID string

	// Title is the issue's title as it stood before these events. A create
	// leaves it empty — the action is what gives the issue a title — and a
	// rename uses it for the "Previously" line.
	Title string

	Events []entity.Event

	// Removed maps a `.remove` event's ref onto the value the add it retracts
	// put there. A removal addresses an event id rather than a value
	// (docs/blob-format.md), so nothing else in the action says which label
	// went away. An absent entry degrades the phrase, never the commit.
	Removed map[string]string

	Trailers []Trailer
}

// titleBudget is how much of a title a subject line will spend. Git's own
// convention is a subject of roughly 50 columns and a hard preference for
// under 72; a phrase plus a title routinely exceeds both, so the title is the
// part that gives. The whole of it is in the blob either way.
const titleBudget = 60

// subjectBudget is how wide a subject may get before it stops being worth
// reading in `git log --oneline` and the detail is better off in the body.
const subjectBudget = 72

// Message renders the action's commit message: a subject saying what happened,
// a body carrying whatever prose the action introduced, and the trailers that
// join the commit back to the issue.
func (a Action) Message() string {
	s := a.summarize()
	subject, listed := a.subject(s)

	var b strings.Builder
	b.WriteString(subject)
	b.WriteString("\n")
	if body := a.body(s, listed); body != "" {
		b.WriteString("\n")
		b.WriteString(body)
		b.WriteString("\n")
	}
	b.WriteString("\n")
	b.WriteString("Issue: " + a.ID + "\n")
	for _, t := range a.Trailers {
		if t.Value != "" {
			b.WriteString(t.Key + ": " + t.Value + "\n")
		}
	}
	return b.String()
}

// summary is one action's events sorted into the shapes a sentence can be
// built from.
type summary struct {
	created    bool
	title      string // the new title, from a create or a rename
	renamed    bool
	desc       entity.Value
	status     entity.Value
	reason     string
	typ        entity.Value
	milepost   entity.Value
	locked     entity.Value
	lockReason string
	pinned     entity.Value
	comments   int
	comment    string

	// An edit and a retraction address an entry by id, so neither says which
	// comment it was in words a subject could use. The count is all the
	// message can honestly claim; the new text of a single edit is prose the
	// action introduced, and goes in the body.
	commentEdits    int
	commentEdit     string
	commentRemovals int

	labelAdd, labelDrop       []string
	assigneeAdd, assigneeDrop []string

	// Relations, as (kind, target) pairs. A removal only knows its own pair
	// through Removed, since the event addresses the add it retracts.
	relAdd, relDrop []Relation

	// other is every op this client has no phrase for, including ops it has
	// never heard of. They still get a commit, because a commit nobody can
	// describe is better than an event nobody committed.
	other []string
}

func (a Action) summarize() summary {
	var s summary
	for _, e := range a.Events {
		switch e.Op {
		case "create":
			s.created = true
		case "title":
			s.title, s.renamed = e.Val.Display(), true
		case "description":
			s.desc = e.Val
		case "status":
			s.status = e.Val
		case "status.reason":
			s.reason = e.Val.Display()
		case "type":
			s.typ = e.Val
		case "milestone":
			s.milepost = e.Val
		case "locked":
			s.locked = e.Val
		case "lock.reason":
			s.lockReason = e.Val.Display()
		case "pinned":
			s.pinned = e.Val
		case "comment":
			s.comments++
			s.comment = e.Val.Display()
		case "comment.edit":
			s.commentEdits++
			s.commentEdit = e.Val.Display()
		case "comment.remove":
			s.commentRemovals++
		case "label.add":
			s.labelAdd = append(s.labelAdd, e.Val.Display())
		case "label.remove":
			s.labelDrop = append(s.labelDrop, a.Removed[e.Ref])
		case "assignee.add":
			s.assigneeAdd = append(s.assigneeAdd, e.Val.Display())
		case "assignee.remove":
			s.assigneeDrop = append(s.assigneeDrop, a.Removed[e.Ref])
		case RelAdd:
			s.relAdd = append(s.relAdd, Relation{Kind: e.Val.Display(), Target: e.Ref})
		case RelRemove:
			kind, target, _ := strings.Cut(a.Removed[e.Ref], " ")
			s.relDrop = append(s.relDrop, Relation{Kind: kind, Target: target})
		default:
			s.other = append(s.other, e.Op)
		}
	}
	return s
}

// phrase is one clause of a subject: `frag` for when it is joined with others,
// `full` for when it stands alone and can spend the sentence on the title.
type phrase struct {
	frag string
	full string // %s is the quoted title
}

// subject is the one line `git log --oneline` shows. It also reports the
// clauses it could not fit, which the body then lists instead.
//
// A create names the issue it creates and nothing else — it is the only action
// for which the title is the news. Everything else names what changed and then
// which issue it changed, because a log of many issues interleaved is unusable
// without the second half.
//
// An edit that touches four fields at once has more to say than a subject
// should carry, and spelling all of it out produces a line nobody reads. Past
// the budget the subject shrinks to the issue and the clauses move down into
// the body, where a list of them is easier to read than a sentence would have
// been anyway.
func (a Action) subject(s summary) (string, []phrase) {
	if s.created {
		title := s.title
		if title == "" {
			title = a.Title
		}
		return "Create issue " + quote(title), nil
	}

	phrases := s.phrases()
	switch len(phrases) {
	case 0:
		// Nothing this client can name — an entity written entirely by a newer
		// one. Say so plainly rather than inventing a change.
		return "Update " + quote(a.Title), nil
	case 1:
		return upper(strings.Replace(phrases[0].full, "%s", quote(a.Title), 1)), nil
	}

	frags := make([]string, len(phrases))
	for i, p := range phrases {
		frags[i] = p.frag
	}
	line := upper(join(frags)) + " on " + quote(a.Title)
	if utf8.RuneCountInString(line) <= subjectBudget {
		return line, nil
	}
	return "Update " + quote(a.Title), phrases
}

// phrases builds the clauses, in a fixed order so that the same action always
// reads the same way.
func (s summary) phrases() []phrase {
	var out []phrase
	add := func(frag, full string) { out = append(out, phrase{frag, full}) }

	if s.renamed {
		// The old title is in the body, so the subject spends its room on the
		// new one — which is also the name the issue answers to from here on.
		add("rename to "+quote(s.title), "rename to "+quote(s.title))
	}
	if s.desc.Present {
		if s.desc.Truthy() {
			add("update the description", "update the description of %s")
		} else {
			add("clear the description", "clear the description of %s")
		}
	}
	if s.status.Present {
		switch {
		case s.status.Display() == StatusOpen:
			add("reopen", "reopen %s")
		case Terminal(s.status.Display()):
			verb := "close"
			if s.reason != "" {
				verb += " as " + s.reason
			}
			// The reason belongs after the issue, not after the verb: "Close
			// X as completed" is the sentence, "Close as completed X" is not.
			full := "close %s"
			if s.reason != "" {
				full += " as " + s.reason
			}
			add(verb, full)
		default:
			// An unrecognised status is not terminal and must not be reported
			// as an ending (docs/issues.md).
			add("set status "+quote(s.status.Display()), "set status "+quote(s.status.Display())+" on %s")
		}
	}
	if s.typ.Present {
		if s.typ.Truthy() {
			add("set type "+quote(s.typ.Display()), "set type "+quote(s.typ.Display())+" on %s")
		} else {
			add("clear the type", "clear the type of %s")
		}
	}
	if s.milepost.Present {
		if s.milepost.Truthy() {
			add("set milestone "+quote(s.milepost.Display()), "set milestone "+quote(s.milepost.Display())+" on %s")
		} else {
			add("clear the milestone", "clear the milestone of %s")
		}
	}
	if len(s.labelAdd) > 0 {
		what := labels(s.labelAdd)
		add("add "+what, "add "+what+" to %s")
	}
	if len(s.labelDrop) > 0 {
		what := labels(s.labelDrop)
		add("remove "+what, "remove "+what+" from %s")
	}
	if len(s.assigneeAdd) > 0 {
		who := people(s.assigneeAdd)
		add("assign "+who, "assign "+who+" to %s")
	}
	if len(s.assigneeDrop) > 0 {
		who := people(s.assigneeDrop)
		add("unassign "+who, "unassign "+who+" from %s")
	}
	s.relations(add)
	if s.locked.Present {
		if s.locked.Truthy() {
			// The reason follows the issue, not the verb: "Lock X as spam".
			tail := ""
			if s.lockReason != "" {
				tail = " as " + s.lockReason
			}
			add("lock"+tail, "lock %s"+tail)
		} else {
			add("unlock", "unlock %s")
		}
	}
	if s.pinned.Present {
		if s.pinned.Truthy() {
			add("pin", "pin %s")
		} else {
			add("unpin", "unpin %s")
		}
	}
	if s.comments > 0 {
		add("comment", "comment on %s")
	}
	if s.commentEdits > 0 {
		what := "edit " + several(s.commentEdits, "comment")
		add(what, what+" on %s")
	}
	if s.commentRemovals > 0 {
		what := "retract " + several(s.commentRemovals, "comment")
		add(what, what+" on %s")
	}
	for _, op := range s.other {
		add("write "+op, "write "+op+" on %s")
	}
	return out
}

// relations adds the clauses for links this action wrote.
//
// Where an issue is filed gets its own words — "file under", "detach" — because
// that is the relation people read a log for, and a re-file writes a remove and
// an add of which only the add is news. Every other kind reads as linking and
// unlinking, in the kind's own spelling, so a kind this client has no phrase for
// still produces a sentence.
func (s summary) relations(add func(frag, full string)) {
	if filed := ofKind(s.relAdd, KindParent); len(filed) > 0 {
		where := render.Abbrev(filed[0].Target)
		add("file under "+where, "file %s under "+where)
	} else if len(ofKind(s.relDrop, KindParent)) > 0 {
		add("detach", "detach %s")
	}

	if linked := notOfKind(s.relAdd, KindParent); len(linked) > 0 {
		what := links(linked)
		add("link "+what, "link "+what+" to %s")
	}
	if unlinked := notOfKind(s.relDrop, KindParent); len(unlinked) > 0 {
		what := links(unlinked)
		add("unlink "+what, "unlink "+what+" from %s")
	}
}

func ofKind(rels []Relation, kind string) []Relation {
	var out []Relation
	for _, r := range rels {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

func notOfKind(rels []Relation, kind string) []Relation {
	var out []Relation
	for _, r := range rels {
		if r.Kind != kind {
			out = append(out, r)
		}
	}
	return out
}

// links names the relations a change wrote: `blocked-by 4b0755a3`, or a count.
//
// A removal addresses the add event it retracts (docs/blob-format.md), so
// neither kind nor target is always recoverable; when it is not, the phrase
// counts rather than inventing one.
func links(rels []Relation) string {
	if len(rels) != 1 {
		return strconv.Itoa(len(rels)) + " relations"
	}
	switch r := rels[0]; {
	case r.Kind == "":
		return "a relation"
	case r.Target == "":
		return r.Kind
	default:
		return r.Kind + " " + render.Abbrev(r.Target)
	}
}

// body is the prose the action introduced, so that plain `git log` reads as the
// tracker rather than as a list of headlines.
//
// It duplicates bytes that are already in the blob, deliberately: the blob's
// copy is inside a JSON line and only `git log -p` shows it, while this is what
// makes a comment thread readable in the log at all.
func (a Action) body(s summary, listed []phrase) string {
	var parts []string

	// The clauses the subject could not fit, one per line — the shape a reader
	// can scan, which a sentence of four clauses was not.
	if len(listed) > 0 {
		var b strings.Builder
		for _, p := range listed {
			b.WriteString("- " + p.frag + "\n")
		}
		parts = append(parts, strings.TrimRight(b.String(), "\n"))
	}

	switch {
	case s.created && s.desc.Truthy():
		parts = append(parts, s.desc.Display())
	case s.comments == 1 && s.comment != "":
		parts = append(parts, s.comment)
	case s.commentEdits == 1 && s.commentEdit != "":
		parts = append(parts, s.commentEdit)
	case !s.created && s.desc.Truthy():
		parts = append(parts, s.desc.Display())
	}

	// A rename says what the issue used to be called. Without it the old name
	// is only recoverable by folding the blob, which is exactly the work a log
	// is meant to save — but when the subject shrank to the old title it has
	// already said so, and the list above names the new one.
	if s.renamed && !s.created && a.Title != "" && len(listed) == 0 {
		parts = append(parts, "Previously: "+quote(a.Title))
	}
	return strings.Join(parts, "\n\n")
}

// quote wraps a value in the double quotes the subject grammar uses, with any
// line breaks flattened — a commit subject is one line, and a title carrying a
// newline would otherwise turn the rest of the sentence into the body.
func quote(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return `""`
	}
	return `"` + render.Trim(s, titleBudget) + `"`
}

// several counts things the way a sentence does: "a comment", "3 comments".
func several(n int, noun string) string {
	if n == 1 {
		return "a " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

// labels names the labels a change added or removed: `label "bug"`, or
// `labels "bug", "design"`.
func labels(vals []string) string {
	named, ok := known(vals, true)
	switch {
	case !ok && len(vals) == 1:
		return "a label"
	case !ok:
		return strconv.Itoa(len(vals)) + " labels"
	case len(named) == 1:
		return "label " + named[0]
	}
	return "labels " + strings.Join(named, ", ")
}

// people names the assignees a change added or removed. Logins go unquoted:
// they are names, and a name reads as one.
func people(vals []string) string {
	named, ok := known(vals, false)
	switch {
	case !ok && len(vals) == 1:
		return "an assignee"
	case !ok:
		return strconv.Itoa(len(vals)) + " assignees"
	}
	return strings.Join(named, ", ")
}

// known reports the values a phrase can name, and whether all of them were
// nameable.
//
// A removal addresses the add event it retracts rather than a value
// (docs/blob-format.md), so the value is not always recoverable. When any of
// them is missing the caller falls back to counting, which says less than the
// values would but says nothing false.
func known(vals []string, quoted bool) ([]string, bool) {
	named := make([]string, 0, len(vals))
	for _, v := range vals {
		if v == "" {
			continue
		}
		if quoted {
			v = quote(v)
		}
		named = append(named, v)
	}
	return named, len(named) == len(vals)
}

// join reads a list of clauses as a sentence: "a", "a and b", "a, b and c".
func join(frags []string) string {
	switch len(frags) {
	case 0:
		return ""
	case 1:
		return frags[0]
	case 2:
		return frags[0] + " and " + frags[1]
	}
	return strings.Join(frags[:len(frags)-1], ", ") + " and " + frags[len(frags)-1]
}

// upper capitalises the first letter without touching the rest, so a subject
// that opens on a quoted value or a login keeps that value's own spelling.
func upper(s string) string {
	if s == "" {
		return s
	}
	r := []rune(s)
	if r[0] >= 'a' && r[0] <= 'z' {
		r[0] -= 'a' - 'A'
	}
	return string(r)
}
