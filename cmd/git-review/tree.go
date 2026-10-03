package main

import (
	"fmt"
	"os"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/render"
	"github.com/hdweiss/git-issue/internal/review"
)

// graph is every review on the ref, reduced to what a rendering needs to talk
// about the links between them.
type graph struct {
	rows    map[string]render.Row
	parents map[string]string
	rels    map[string][]review.Relation
}

// survey folds every review on the ref once and keeps what a graph of them
// needs from each.
//
// Every review, not only the ones a filter kept, because a listing has to know
// about reviews it is not printing — the parent to nest under, the title to
// resolve a link to.
func survey(s *entity.Store, notes []gitx.Note, runs map[string][]review.Check, keep func(id string, st entity.State)) (graph, error) {
	h := graph{
		rows:    make(map[string]render.Row, len(notes)),
		parents: make(map[string]string, len(notes)),
		rels:    make(map[string][]review.Relation, len(notes)),
	}
	err := s.Each(notes, func(id string, st entity.State) error {
		h.rows[id] = review.Row(id, st, review.Summarize(runs[review.Head(st)]))
		h.parents[id] = review.Parent(st)
		if rels := review.Relations(st); len(rels) > 0 {
			h.rels[id] = rels
		}
		if keep != nil {
			keep(id, st)
		}
		return nil
	})
	return h, err
}

// filedUnder is the set of reviews a listing's positional argument selects: the
// whole subtree beneath a named one, or one level under `none`.
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
func nest(rows []render.Row, parents map[string]string) []render.Row {
	nested, cycles := render.Tree(rows, parents)
	for _, id := range cycles {
		fmt.Fprintf(os.Stderr, "warning: %s is filed under one of its own descendants; showing it at the top level\n", render.Abbrev(id))
	}
	return nested
}

// links is the hierarchy indexed once: every review's title, and every review's
// relations read from the other end.
type links struct {
	titles  map[string]string
	inverse map[string]map[string][]string
}

// indexLinks inverts the relation field. An edge is stored on one end only, so
// this is the whole of the other end's reading of it — derived on every
// listing, never written anywhere.
//
// Note what it cannot see. A review's `closes` targets are issues, which live
// on another ref entirely, so their titles are not here and the link renders as
// a bare id. Resolving across the two namespaces means folding both refs, which
// is a cost worth paying in `show` and not in every listing; see issueTitles.
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

// of is what a full rendering says about one review's links.
func (l links) of(id string) review.Links {
	return review.Links{Titles: l.titles, Inverse: l.inverse[id]}
}
