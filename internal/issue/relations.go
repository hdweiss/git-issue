// Relations: every link from one entity to another, in one op family.
//
// docs/issues.md is the specification. `rel.add` carries the kind in val and
// the target in ref, `rel.remove` retracts one by naming the add it undoes, and
// `rel.note` annotates one the way a comment edit rewrites an entry. Nothing
// here is a new field kind: it is the OR-Set docs/blob-format.md already
// defines, read through a vocabulary this type supplies.

package issue

import (
	"errors"
	"slices"
	"strings"

	"github.com/hdweiss/git-issue/internal/entity"
)

// errRelationSyntax is what a flag that did not name both halves gets back. A
// relation needs a kind and a target, and guessing either one would file a link
// nobody asked for.
var errRelationSyntax = errors.New(`a relation is spelled <kind>:<id>, as in "blocked-by:4b0755a3"`)

// The ops a relation is written with. RelField is the list field they belong
// to, which is the name the fold keys members under.
const (
	RelField  = "rel"
	RelAdd    = "rel.add"
	RelRemove = "rel.remove"
	RelNote   = "rel.note"
)

// The kinds this client knows. The vocabulary is open — an unknown kind is
// preserved and displayed verbatim, never coerced to one of these — so these
// are the kinds that get a phrase, a header and an inverse, not the kinds that
// are allowed.
const (
	KindParent    = "parent"
	KindBlockedBy = "blocked-by"
	KindDuplicate = "duplicate-of"
	KindRelated   = "related"
)

// Kind is what this client knows about one relation kind.
//
// Direction is the load-bearing part. A relation is stored on one end only —
// the end whose own state the link constrains — and the other end's reading of
// it is derived by inverting the field across the entities a reader holds. The
// stored end is the one a writer may write; Inverse is only ever displayed.
type Kind struct {
	Name string
	// Label heads this relation where it is stored, Inverse where it is
	// derived: an issue has a "Parent", the issue it names has "Children".
	Label, Inverse string
	// Single is whether a writer keeps at most one of these. It is policy, not
	// a rule the format can enforce: two clones can each add one and merge, so
	// a reader must still cope with several (docs/issues.md).
	Single bool
	// Symmetric kinds have no dependent end. Either end may write one, and a
	// reader treats the unordered pair as one relation, so the same link
	// written from both ends renders once.
	Symmetric bool
}

// Kinds are the known relation kinds, in the order a rendering lists them.
var Kinds = []Kind{
	{Name: KindParent, Label: "Parent", Inverse: "Children", Single: true},
	{Name: KindBlockedBy, Label: "Blocked by", Inverse: "Blocks"},
	{Name: KindDuplicate, Label: "Duplicate of", Inverse: "Duplicated by", Single: true},
	{Name: KindRelated, Label: "Related", Inverse: "Related", Symmetric: true},
}

// KnownKind reports what this client knows about a kind, and whether it knows
// it at all. An unknown kind still folds, still renders and still round-trips;
// it simply has no inverse to derive and no opinion about cardinality.
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
	// Note is the latest `rel.note`, or "".
	Note string
}

// Key identifies a relation the way the fold does: the pair, never the add's
// id. It is what a writer reconciles against and what tells two links apart.
func (r Relation) Key() string { return r.Kind + "\x00" + r.Target }

// Relations are the issue's links, in (c, id) order.
//
// A member carrying no target is skipped rather than reported as a link to
// nowhere: the event stays in the blob untouched, as everything unrecognised
// does, but there is nothing for a reader to resolve or a writer to retract.
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

// RelationsOf are the issue's links of one kind, in (c, id) order.
func RelationsOf(st entity.State, kind string) []Relation {
	var out []Relation
	for _, r := range Relations(st) {
		if r.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

// Parent is the entity this issue is filed under, or "" for one filed under
// nothing.
//
// A grow-only set cannot enforce at most one parent — two clones can each file
// the same issue and merge — so where there are several this is the survivor
// with the highest (c, id), which every replica agrees on. It is deliberately
// not the whole story: a rendering shows the others too, because two parents is
// a fact about the data and a reader that silently picks one teaches nobody
// that there is something to fix.
//
// The id it returns is not promised to name anything this repository holds: a
// parent can be filtered out of a listing, archived, in another repository, or
// simply not imported yet. Every caller has to be ready for that.
func Parent(st entity.State) string {
	parents := RelationsOf(st, KindParent)
	if len(parents) == 0 {
		return ""
	}
	return parents[len(parents)-1].Target
}

// ParseRelation reads the `<kind>:<target>` spelling a flag uses, and reports
// the kind and the target separately.
//
// The kind is not checked against the known ones. The vocabulary is open, and
// refusing a kind this build has no phrase for would make a client unable to
// write what it can already read and carry.
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
