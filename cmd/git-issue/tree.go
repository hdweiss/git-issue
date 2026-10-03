package main

import (
	"fmt"
	"os"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/issue"
	"github.com/hdweiss/git-issue/internal/render"
)

// graph is every issue on the ref, reduced to what a rendering needs to
// talk about the links between them.
type graph struct {
	// rows is the line each issue would print as, by id.
	rows map[string]render.Row
	// parents is the one issue each is nested under, by id. Where a merge left
	// several, this is the winner issue.Parent picks; rels keeps them all.
	parents map[string]string
	// rels is every relation each issue stores, by id. Inverting it is what
	// answers "what points at this one", which no single blob knows.
	rels map[string][]issue.Relation
}

// survey folds every issue on the ref once and keeps what a graph of them
// needs from each.
//
// Every issue, not only the ones a filter kept, because a listing has to know
// about issues it is not printing — the parent to nest under, the title to
// resolve a link to. keep is called for each issue in turn, so a caller that
// also wants folded state gets it without a second pass.
func survey(s *entity.Store, notes []gitx.Note, keep func(id string, st entity.State)) (graph, error) {
	h := graph{
		rows:    make(map[string]render.Row, len(notes)),
		parents: make(map[string]string, len(notes)),
		rels:    make(map[string][]issue.Relation, len(notes)),
	}
	err := s.Each(notes, func(id string, st entity.State) error {
		h.rows[id] = issue.Row(id, st)
		h.parents[id] = issue.Parent(st)
		if rels := issue.Relations(st); len(rels) > 0 {
			h.rels[id] = rels
		}
		if keep != nil {
			keep(id, st)
		}
		return nil
	})
	return h, err
}

// The hierarchy side of a listing: which issues a positional argument selects,
// and what `show` says about the ends of a link.
//
// An issue's parent is one id in its own blob; everything else here — what that
// id is called, what hangs underneath it — is a question about other entities,
// answered by folding the ref. There is no index yet, so that is a full pass;
// see "Measured facts" in AGENTS.md for what it costs.

// filedUnder is the set of issues a listing's positional argument selects.
//
// `list none` and `list <id>` are the same operation, because the blob spells
// "filed under nothing" as an empty parent: one selects the issues filed under
// "", the other the issues filed under that id.
//
// For a named issue that is the whole subtree beneath it — every descendant,
// however deep. The selection does not depend on how the listing will be drawn:
// a tree can draw that depth and a flat listing cannot, but both are about the
// same issues, so piping a listing changes the shape of its rows and never
// which rows it holds. The named issue itself is not in the set — it is not
// filed under itself — though a tree draws it as the root the branches hang
// from; listIssues adds it back for that.
//
// `none` is the exception: "filed under nothing" taken recursively is every
// issue, which nobody means by it, so the empty root selects one level — the
// issues with no parent at all.
func filedUnder(parents map[string]string, root string) map[string]bool {
	children := map[string][]string{}
	for id, parent := range parents {
		children[parent] = append(children[parent], id)
	}

	keep := map[string]bool{}
	queue := append([]string(nil), children[root]...)
	for _, id := range queue {
		keep[id] = true
	}
	if root == "" {
		return keep
	}

	// Breadth-first, and a node already kept is never queued again — which is
	// also what stops a parent cycle from walking forever.
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		for _, child := range children[id] {
			if !keep[child] {
				keep[child] = true
				queue = append(queue, child)
			}
		}
	}
	return keep
}

// nest orders rows as a forest and warns about any cycle it had to break.
//
// The warning goes to stderr rather than into the listing: a cycle is a real
// thing that happened to the data — two clones filing each other's issue under
// the other while offline — and the reader needs to know the tree they are
// looking at is missing an edge, without the listing itself growing a row that
// is not an issue.
func nest(rows []render.Row, parents map[string]string) []render.Row {
	nested, cycles := render.Tree(rows, parents)
	for _, id := range cycles {
		fmt.Fprintf(os.Stderr, "warning: %s is filed under one of its own descendants; showing it at the top level\n", render.Abbrev(id))
	}
	return nested
}

// links is the hierarchy indexed once: every issue's title, and every issue's
// relations read from the other end.
//
// Built once rather than per rendering, because `list --format medium` renders
// every issue on the ref and rebuilding this for each of them would be
// quadratic in the size of the tracker.
type links struct {
	titles map[string]string
	// inverse is, per target, the issues pointing at it, keyed by the kind the
	// pointing end stored. inverse[epic]["parent"] is that epic's children.
	inverse map[string]map[string][]string
}

// indexLinks inverts the relation field. docs/issues.md stores an edge on one
// end only, so this is the whole of the other end's reading of it — derived on
// every listing, never written anywhere.
//
// The issues under one key come out in a listing's own order, newest first,
// because they are read as a listing: a reader moving between `show <epic>` and
// `list <epic>` sees the same issues in the same order.
func indexLinks(h graph) links {
	l := links{
		titles:  make(map[string]string, len(h.rows)),
		inverse: map[string]map[string][]string{},
	}
	pointing := map[string]map[string][]render.Row{}
	for id, row := range h.rows {
		l.titles[id] = row.Title
		for _, r := range h.rels[id] {
			if pointing[r.Target] == nil {
				pointing[r.Target] = map[string][]render.Row{}
			}
			pointing[r.Target][r.Kind] = append(pointing[r.Target][r.Kind], row)
		}
	}
	for target, kinds := range pointing {
		l.inverse[target] = make(map[string][]string, len(kinds))
		for kind, rows := range kinds {
			for _, row := range render.SortRows(rows) {
				l.inverse[target][kind] = append(l.inverse[target][kind], row.ID)
			}
		}
	}
	return l
}

// of is what a full rendering says about one issue's links: what the entities
// it points at are called, and what points back at it.
func (l links) of(id string) issue.Links {
	return issue.Links{Titles: l.titles, Inverse: l.inverse[id]}
}
