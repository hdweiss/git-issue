package issue

import (
	"testing"
	"time"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/render"
)

const (
	epic  = "1111111111111111111111111111111111111111"
	other = "2222222222222222222222222222222222222222"
)

func rel(t *testing.T, c int64, kind, target, nonce string) entity.Event {
	t.Helper()
	return mkEvent(t, c, RelAdd, entity.Str(kind), target, nonce)
}

// A relation is the pair, so one kind pointing at two entities is two links —
// the case the scalar this replaced could not hold at all.
func TestRelationsAreThePair(t *testing.T) {
	st := state(t,
		mkStr(t, 1, "create", Type, "aa"),
		rel(t, 2, KindBlockedBy, epic, "bb"),
		rel(t, 3, KindBlockedBy, other, "cc"),
		rel(t, 4, KindRelated, epic, "dd"),
	)

	got := Relations(st)
	if len(got) != 3 {
		t.Fatalf("got %d relations, want 3: %v", len(got), got)
	}
	if blocked := RelationsOf(st, KindBlockedBy); len(blocked) != 2 {
		t.Errorf("got %d blocked-by relations, want both targets", len(blocked))
	}
	if related := RelationsOf(st, KindRelated); len(related) != 1 || related[0].Target != epic {
		t.Errorf("related = %v, want one pointing at the epic", related)
	}
}

// Two clones can each file one issue under a different epic and merge, and no
// writer can stop them. The reader's rule is the highest (c, id), which every
// replica agrees on — and the rest stay visible rather than being dropped.
func TestParentPicksTheHighestClock(t *testing.T) {
	st := state(t,
		mkStr(t, 1, "create", Type, "aa"),
		rel(t, 2, KindParent, epic, "bb"),
		rel(t, 3, KindParent, other, "cc"),
	)

	if got := Parent(st); got != other {
		t.Errorf("Parent = %s, want the highest (c, id) survivor %s", got, other)
	}
	if got := RelationsOf(st, KindParent); len(got) != 2 {
		t.Errorf("kept %d parent relations, want both: a reader must be able to show the conflict", len(got))
	}
}

// A retracted relation is gone from folded state, and the remove names the add
// it undoes rather than the link's value.
func TestRelationRetraction(t *testing.T) {
	add := rel(t, 2, KindBlockedBy, epic, "bb")
	st := state(t,
		mkStr(t, 1, "create", Type, "aa"), add,
		mkEvent(t, 3, RelRemove, entity.Value{}, add.ID, "cc"),
	)
	if got := Relations(st); len(got) != 0 {
		t.Errorf("relations = %v, want none left", got)
	}

	// Add-wins: a concurrent add the remove never saw is a different member and
	// survives, which is the whole point of naming the add rather than the pair.
	concurrent := rel(t, 3, KindBlockedBy, epic, "dd")
	st = state(t,
		mkStr(t, 1, "create", Type, "aa"), add, concurrent,
		mkEvent(t, 3, RelRemove, entity.Value{}, add.ID, "cc"),
	)
	if got := Relations(st); len(got) != 1 || got[0].Target != epic {
		t.Errorf("relations = %v, want the concurrent add to survive", got)
	}
}

// An annotation says why a link is there. It addresses the add, so it follows
// that link and not the target.
func TestRelationNote(t *testing.T) {
	add := rel(t, 2, KindBlockedBy, epic, "bb")
	st := state(t,
		mkStr(t, 1, "create", Type, "aa"), add,
		mkEvent(t, 3, RelNote, entity.Str("waiting on the rewrite"), add.ID, "cc"),
	)

	got := Relations(st)
	if len(got) != 1 {
		t.Fatalf("got %d relations, want 1", len(got))
	}
	if got[0].Note != "waiting on the rewrite" {
		t.Errorf("note = %q, want the annotation", got[0].Note)
	}
}

// A relation with no target is not a link to nowhere: there is nothing to
// resolve and nothing to retract, so it folds to no relation at all. The event
// itself stays in the blob, as every unrecognised one does.
func TestRelationWithoutATargetIsNotALink(t *testing.T) {
	st := state(t,
		mkStr(t, 1, "create", Type, "aa"),
		mkEvent(t, 2, RelAdd, entity.Str(KindParent), "", "bb"),
	)
	if got := Relations(st); len(got) != 0 {
		t.Errorf("relations = %v, want none", got)
	}
	if len(st.Events) != 2 {
		t.Errorf("kept %d events, want both preserved", len(st.Events))
	}
}

// An unknown kind is carried and displayed as itself. Coercing it to `related`
// would assert something nobody wrote, and dropping it would lose a link this
// tracker is perfectly able to store.
func TestUnknownKindSurvives(t *testing.T) {
	st := state(t,
		mkStr(t, 1, "create", Type, "aa"),
		rel(t, 2, "ado.affects", epic, "bb"),
	)

	got := Relations(st)
	if len(got) != 1 || got[0].Kind != "ado.affects" {
		t.Fatalf("relations = %v, want the kind kept verbatim", got)
	}
	if k, known := KnownKind("ado.affects"); known || k.Name != "ado.affects" {
		t.Errorf("KnownKind claimed to know %q", k.Name)
	}
	// It renders on the end that stored it, under its own name.
	d := Detail("4f2a1c9", st, time.UTC, Links{})
	if !hasHeader(d, "ado.affects") {
		t.Errorf("no header for the unknown kind:\n%v", d.Headers)
	}
}

// The two ends of one kind print together, and a symmetric kind prints once
// however many ends stored it.
func TestRelationHeaders(t *testing.T) {
	st := state(t,
		mkStr(t, 1, "create", Type, "aa"),
		rel(t, 2, KindParent, epic, "bb"),
		rel(t, 3, KindRelated, other, "cc"),
	)
	links := Links{
		Titles: map[string]string{epic: "The epic", other: "The other one"},
		Inverse: map[string][]string{
			KindParent:  {other},
			KindRelated: {other}, // the same link, written from the far end
		},
	}

	d := Detail("4f2a1c9", st, time.UTC, links)
	for _, key := range []string{"Parent", "Children", "Related"} {
		if !hasHeader(d, key) {
			t.Errorf("no %q header:\n%v", key, d.Headers)
		}
	}
	for _, h := range d.Headers {
		if h.Key == "Related" && len(h.More) != 0 {
			t.Errorf("Related printed twice for one link: %v %v", h.Value, h.More)
		}
	}
}

// hasHeader reports whether a rendering printed a header with anything in it.
// An empty header is not printed at all, which is what an absent kind produces.
func hasHeader(d render.Detail, key string) bool {
	for _, h := range d.Headers {
		if h.Key == key && h.Value != "" {
			return true
		}
	}
	return false
}

func TestParseRelation(t *testing.T) {
	kind, target, err := ParseRelation(" Blocked-By : 4b07 ")
	if err != nil || kind != "blocked-by" || target != "4b07" {
		t.Errorf("ParseRelation = (%q, %q, %v)", kind, target, err)
	}
	for _, bad := range []string{"", "parent", "parent:", ":4b07"} {
		if _, _, err := ParseRelation(bad); err == nil {
			t.Errorf("ParseRelation(%q) was accepted", bad)
		}
	}
}
