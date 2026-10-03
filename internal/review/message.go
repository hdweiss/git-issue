package review

import (
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/render"
)

// The commit message for one action, per docs/storage-model.md's "Provenance".
// The grammar is internal/issue's, said in the review vocabulary: what somebody
// did, in the words the type already uses, so the ref's own history is a second
// reading of the tracker rather than a record that it was written to.
//
// The message is not authoritative and must never be parsed back into state.

// Trailer is one `Key: value` line at the foot of a commit message.
type Trailer struct{ Key, Value string }

// Action is what a commit message needs to know: which review was changed, what
// it was called before, the events the change wrote, and anything a bridge
// wants to add at the foot.
type Action struct {
	ID string

	// Title is the review's title as it stood before these events. A create
	// leaves it empty, and a rename uses it for the "Previously" line.
	Title string

	Events []entity.Event

	// Removed maps a `.remove` event's ref onto the value the add it retracts
	// put there. A removal addresses an event id rather than a value, so
	// nothing else in the action says what went away.
	Removed map[string]string

	Trailers []Trailer
}

const (
	titleBudget   = 60
	subjectBudget = 72
)

// Message renders the action's commit message.
//
// The trailer is `Review:`, which is what joins a commit back to the entity.
// A checks commit carries `Commit:` instead — the checks ref is keyed by a
// commit sha and holds no entity, so calling that key a review would be a lie
// the log tells every time anyone reads it.
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
	if s.checks > 0 {
		b.WriteString("Commit: " + a.ID + "\n")
	} else {
		b.WriteString("Review: " + a.ID + "\n")
	}
	for _, t := range a.Trailers {
		if t.Value != "" {
			b.WriteString(t.Key + ": " + t.Value + "\n")
		}
	}
	return b.String()
}

// summary is one action's events sorted into the shapes a sentence can be built
// from.
type summary struct {
	created  bool
	title    string
	renamed  bool
	desc     entity.Value
	status   entity.Value
	reason   string
	base     entity.Value
	head     entity.Value
	headSHA  entity.Value
	milepost entity.Value
	locked   entity.Value
	draft    entity.Value

	comments int
	comment  string

	commentEdits    int
	commentEdit     string
	commentRemovals int

	// Anchors and resolutions ride along with the comment they annotate, so
	// they are counted rather than given a clause of their own: "comment on X"
	// already says what happened, and "comment and anchor a comment on X" says
	// it twice.
	anchored  int
	resolved  int
	unresolve int

	verdicts  []string
	dismissed []string

	checks     int
	checkNames []string

	labelAdd, labelDrop       []string
	assigneeAdd, assigneeDrop []string

	relAdd, relDrop []Relation

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
		case "base":
			s.base = e.Val
		case "head":
			s.head = e.Val
		case "head.sha":
			s.headSHA = e.Val
		case "milestone":
			s.milepost = e.Val
		case "locked":
			s.locked = e.Val
		case "draft":
			s.draft = e.Val
		case "comment":
			s.comments++
			s.comment = e.Val.Display()
		case "comment.edit":
			s.commentEdits++
			s.commentEdit = e.Val.Display()
		case "comment.remove":
			s.commentRemovals++
		case AnchorOp:
			s.anchored++
		case ResolveOp:
			if e.Val.Truthy() {
				s.resolved++
			} else {
				s.unresolve++
			}
		case VerdictAdd:
			s.verdicts = append(s.verdicts, e.Val.Display())
		case VerdictRemove:
			s.dismissed = append(s.dismissed, a.Removed[e.Ref])
		case CheckAdd:
			s.checks++
			if c, err := ParseCheck(e.Val.Display()); err == nil {
				s.checkNames = append(s.checkNames, c.Name+" "+c.Conclusion)
			}
		case CheckRemove:
			// The add it supersedes. Reporting a re-run as a removal would
			// describe bookkeeping rather than what happened.
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

// subject is the one line `git log --oneline` shows, and the clauses it could
// not fit.
func (a Action) subject(s summary) (string, []phrase) {
	if s.created {
		title := s.title
		if title == "" {
			title = a.Title
		}
		return "Create review " + quote(title), nil
	}

	// A checks commit names no review, because the ref it lands on holds none.
	if s.checks > 0 && len(s.checkNames) > 0 {
		if len(s.checkNames) == 1 {
			return "Check " + s.checkNames[0] + " on " + render.Abbrev(a.ID), nil
		}
		return strconv.Itoa(s.checks) + " checks on " + render.Abbrev(a.ID), nil
	}

	phrases := s.phrases()
	switch len(phrases) {
	case 0:
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
		switch v := s.status.Display(); {
		case v == StatusOpen:
			add("reopen", "reopen %s")
		case v == StatusMerged:
			add("merge", "merge %s")
		case Terminal(v):
			verb := "close"
			if s.reason != "" {
				verb += " as " + s.reason
			}
			full := "close %s"
			if s.reason != "" {
				full += " as " + s.reason
			}
			add(verb, full)
		default:
			// An unrecognised status is not terminal and must not be reported
			// as an ending.
			add("set status "+quote(v), "set status "+quote(v)+" on %s")
		}
	}
	if s.base.Present {
		add("retarget onto "+quote(s.base.Display()), "retarget %s onto "+quote(s.base.Display()))
	}
	if s.head.Present {
		add("set head "+quote(s.head.Display()), "set head "+quote(s.head.Display())+" on %s")
	}
	if s.headSHA.Present {
		where := render.Abbrev(s.headSHA.Display())
		add("update to "+where, "update %s to "+where)
	}
	if s.draft.Present {
		if s.draft.Truthy() {
			add("mark draft", "mark %s draft")
		} else {
			add("mark ready", "mark %s ready")
		}
	}
	if s.milepost.Present {
		if s.milepost.Truthy() {
			add("set milestone "+quote(s.milepost.Display()), "set milestone "+quote(s.milepost.Display())+" on %s")
		} else {
			add("clear the milestone", "clear the milestone of %s")
		}
	}
	for _, v := range s.verdicts {
		switch v {
		case VerdictApprove:
			add("approve", "approve %s")
		case VerdictRequestChanges:
			add("request changes", "request changes on %s")
		case VerdictComment:
			add("review", "review %s")
		default:
			add("record verdict "+quote(v), "record verdict "+quote(v)+" on %s")
		}
	}
	if len(s.dismissed) > 0 {
		what := "dismiss " + several(len(s.dismissed), "verdict")
		if len(s.dismissed) == 1 && s.dismissed[0] != "" {
			what = "dismiss " + s.dismissed[0]
		}
		add(what, what+" on %s")
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
		add("request review from "+who, "request review from "+who+" on %s")
	}
	if len(s.assigneeDrop) > 0 {
		who := people(s.assigneeDrop)
		add("unassign "+who, "unassign "+who+" from %s")
	}
	s.relations(add)
	if s.locked.Present {
		if s.locked.Truthy() {
			add("lock", "lock %s")
		} else {
			add("unlock", "unlock %s")
		}
	}
	if s.comments > 0 {
		// An anchored comment says where it sits, since that is the difference
		// between a review comment and any other kind.
		what := "comment"
		if s.anchored > 0 && s.comments == s.anchored {
			what = "comment on the diff"
		}
		add(what, what+" on %s")
	}
	if s.commentEdits > 0 {
		what := "edit " + several(s.commentEdits, "comment")
		add(what, what+" on %s")
	}
	if s.commentRemovals > 0 {
		what := "retract " + several(s.commentRemovals, "comment")
		add(what, what+" on %s")
	}
	if s.resolved > 0 {
		what := "resolve " + several(s.resolved, "thread")
		add(what, what+" on %s")
	}
	if s.unresolve > 0 {
		what := "reopen " + several(s.unresolve, "thread")
		add(what, what+" on %s")
	}
	for _, op := range s.other {
		add("write "+op, "write "+op+" on %s")
	}
	return out
}

// relations adds the clauses for links this action wrote.
func (s summary) relations(add func(frag, full string)) {
	if filed := ofKind(s.relAdd, KindParent); len(filed) > 0 {
		where := render.Abbrev(filed[0].Target)
		add("file under "+where, "file %s under "+where)
	} else if len(ofKind(s.relDrop, KindParent)) > 0 {
		add("detach", "detach %s")
	}

	// What a review closes reads as its own verb: it is the link people open a
	// log looking for, and "link closes 4b0755a3" buries it.
	if closes := ofKind(s.relAdd, KindCloses); len(closes) > 0 {
		what := targets(closes)
		add("close "+what, "close "+what+" with %s")
	}
	if opened := ofKind(s.relDrop, KindCloses); len(opened) > 0 {
		what := targets(opened)
		add("stop closing "+what, "stop closing "+what+" with %s")
	}

	if linked := notOfKinds(s.relAdd, KindParent, KindCloses); len(linked) > 0 {
		what := links(linked)
		add("link "+what, "link "+what+" to %s")
	}
	if unlinked := notOfKinds(s.relDrop, KindParent, KindCloses); len(unlinked) > 0 {
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

func notOfKinds(rels []Relation, kinds ...string) []Relation {
	var out []Relation
	for _, r := range rels {
		if !strings.Contains(" "+strings.Join(kinds, " ")+" ", " "+r.Kind+" ") {
			out = append(out, r)
		}
	}
	return out
}

// targets names what a `closes` clause points at, without repeating the kind.
func targets(rels []Relation) string {
	if len(rels) != 1 {
		return strconv.Itoa(len(rels)) + " issues"
	}
	if rels[0].Target == "" {
		return "an issue"
	}
	return render.Abbrev(rels[0].Target)
}

// links names the relations a change wrote: `blocked-by 4b0755a3`, or a count.
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
func (a Action) body(s summary, listed []phrase) string {
	var parts []string

	if len(listed) > 0 {
		var b strings.Builder
		for _, p := range listed {
			b.WriteString("- " + p.frag + "\n")
		}
		parts = append(parts, strings.TrimRight(b.String(), "\n"))
	}

	// Every check the action wrote, one per line: a subject can only count
	// them, and which gate failed is the whole reason to read this.
	if s.checks > 1 {
		var b strings.Builder
		for _, name := range s.checkNames {
			b.WriteString("- " + name + "\n")
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

	if s.renamed && !s.created && a.Title != "" && len(listed) == 0 {
		parts = append(parts, "Previously: "+quote(a.Title))
	}
	return strings.Join(parts, "\n\n")
}

func quote(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if s == "" {
		return `""`
	}
	return `"` + render.Trim(s, titleBudget) + `"`
}

func several(n int, noun string) string {
	if n == 1 {
		return "a " + noun
	}
	return strconv.Itoa(n) + " " + noun + "s"
}

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

func people(vals []string) string {
	named, ok := known(vals, false)
	switch {
	case !ok && len(vals) == 1:
		return "a reviewer"
	case !ok:
		return strconv.Itoa(len(vals)) + " reviewers"
	}
	return strings.Join(named, ", ")
}

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
