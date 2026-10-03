// Relations: every link from one entity to another, in one op family.
//
// The family and its mechanism are internal/issue's, unchanged — `rel.add`
// carries the kind in val and the target in ref, `rel.remove` retracts one by
// naming the add it undoes, `rel.note` annotates one. What this file adds is
// the two kinds a review has that an issue does not, and the rule that keeps
// `closes` from closing anything.

package review

import (
	"errors"
	"slices"
	"strings"

	"github.com/hdweiss/git-issue/internal/entity"
)

var errRelationSyntax = errors.New(`a relation is spelled <kind>:<id>, as in "closes:4b0755a3"`)

// The ops a relation is written with. RelField is the list field they belong
// to, which is the name the fold keys members under.
const (
	RelField  = "rel"
	RelAdd    = "rel.add"
	RelRemove = "rel.remove"
	RelNote   = "rel.note"
)

// The kinds this client knows. The vocabulary is open — an unknown kind is
// preserved and displayed verbatim, never coerced — so these are the kinds that
// get a phrase, a header and an inverse, not the kinds that are allowed.
const (
	KindParent       = "parent"
	KindBlockedBy    = "blocked-by"
	KindCloses       = "closes"
	KindSupersededBy = "superseded-by"
	KindDuplicate    = "duplicate-of"
	KindRelated      = "related"
)

// Kind is what this client knows about one relation kind. Direction is the
// load-bearing part: a relation is stored on one end only, and the other end's
// reading of it is derived by inverting the field across the entities a reader
// holds.
type Kind struct {
	Name           string
	Label, Inverse string
	// Single is whether a writer keeps at most one. Policy, not a rule the
	// format can enforce: two clones can each add one and merge.
	Single bool
	// Symmetric kinds have no dependent end — either may write one, and a
	// reader treats the unordered pair as one relation.
	Symmetric bool
}

// Kinds are the known relation kinds, in the order a rendering lists them.
//
// `closes` is written on the review: the end that knows, and the end whose own
// completion the link describes. That reads slightly against the general
// dependent-end rule — the issue is arguably the constrained party — and it is
// deliberate, because the alternative is a review write reaching into the issue
// refs. See Closes.
//
// `superseded-by` is written on the *older* review, which is the dependent end
// in the ordinary sense: the review that has been replaced is the one whose
// state the link constrains, and it is the one a reader arrives at wondering
// what happened to it.
var Kinds = []Kind{
	{Name: KindParent, Label: "Parent", Inverse: "Children", Single: true},
	{Name: KindCloses, Label: "Closes", Inverse: "Closed by"},
	{Name: KindBlockedBy, Label: "Blocked by", Inverse: "Blocks"},
	{Name: KindSupersededBy, Label: "Superseded by", Inverse: "Supersedes", Single: true},
	{Name: KindDuplicate, Label: "Duplicate of", Inverse: "Duplicated by", Single: true},
	{Name: KindRelated, Label: "Related", Inverse: "Related", Symmetric: true},
}

// KnownKind reports what this client knows about a kind, and whether it knows
// it at all. An unknown kind still folds, still renders and still round-trips.
func KnownKind(name string) (Kind, bool) {
	for _, k := range Kinds {
		if k.Name == name {
			return k, true
		}
	}
	return Kind{Name: name}, false
}

// Relation is one surviving link, as folded state holds it.
type Relation struct {
	// ID is the `rel.add` that put it there, which is what a removal must name
	// and what an annotation addresses.
	ID     string
	Kind   string
	Target string
	Note   string
}

// Key identifies a relation the way the fold does: the pair, never the add's
// id.
func (r Relation) Key() string { return r.Kind + "\x00" + r.Target }

// Relations are the review's links, in (c, id) order.
func Relations(st entity.State) []Relation {
	members := st.Distinct(RelField)
	out := make([]Relation, 0, len(members))
	for _, m := range members {
		if m.Ref == "" {
			continue
		}
		out = append(out, Relation{
			ID:     m.ID,
			Kind:   m.Val.Display(),
			Target: m.Ref,
			Note:   m.Note.Display(),
		})
	}
	return out
}

// RelationsOf are the review's links of one kind, in (c, id) order.
func RelationsOf(st entity.State, kind string) []Relation {
	var out []Relation
	for _, r := range Relations(st) {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// Parent is the entity this review is filed under, or "" for one filed under
// nothing. Where a merge left several, the survivor with the highest (c, id) —
// which every replica agrees on, and which a rendering shows alongside the
// others rather than instead of them.
//
// The id is not promised to name anything this repository holds.
func Parent(st entity.State) string {
	parents := RelationsOf(st, KindParent)
	if len(parents) == 0 {
		return ""
	}
	return parents[len(parents)-1].Target
}

// Closes are the entities this review says it closes.
//
// It records intent and nothing more. Nothing in this package, and nothing that
// calls it, folds the link into the target's status: an entity's state is a
// function of its own events, and a cascade would make folding one blob depend
// on another — so a clone holding the review but not the issue would fold a
// different answer from the same data. A client may offer to close the target;
// the close is then an ordinary write on that entity, by a person, as its own
// action.
//
// The targets are frequently in another repository, where an id means nothing
// at all. That is the cross-repository question docs/issues.md records, reached
// through the same single field.
func Closes(st entity.State) []Relation { return RelationsOf(st, KindCloses) }

// ParseRelation reads the `<kind>:<target>` spelling a flag uses. The kind is
// not checked against the known ones: the vocabulary is open, and refusing a
// kind this build has no phrase for would make a client unable to write what it
// can already read and carry.
func ParseRelation(arg string) (kind, target string, err error) {
	kind, target, ok := strings.Cut(arg, ":")
	kind = strings.ToLower(strings.TrimSpace(kind))
	target = strings.TrimSpace(target)
	if !ok || kind == "" || target == "" {
		return "", "", errRelationSyntax
	}
	return kind, target, nil
}

// SortKinds orders kinds for display: the ones this client knows in the order
// Kinds gives, then anything else alphabetically, so a rendering is stable
// however the events arrived.
func SortKinds(kinds []string) []string {
	rank := func(kind string) int {
		if i := slices.IndexFunc(Kinds, func(k Kind) bool { return k.Name == kind }); i >= 0 {
			return i
		}
		return len(Kinds)
	}
	out := slices.Clone(kinds)
	slices.SortFunc(out, func(a, b string) int {
		if ra, rb := rank(a), rank(b); ra != rb {
			return ra - rb
		}
		return strings.Compare(a, b)
	})
	return out
}
