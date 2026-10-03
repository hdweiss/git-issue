// Ordering a listing as a forest, and drawing the spine that shows it.
//
// This file names no field. It is handed the edges — which row hangs under
// which — and knows only how to nest and draw them, so the vocabulary that
// decides what "under" means stays in the entity type's own package.

package render

import "slices"

// TreeMark is what a root carries when it hangs off something the listing does
// not contain. A plain character rather than a colour, so it survives a pipe:
// without it a filtered tree is indistinguishable from a flat one and quietly
// claims there is nothing above.
const TreeMark = "↑ "

// The spine. A child is drawn with a connector, and the levels above it with a
// bar when that ancestor has siblings still to come, blank when it does not.
const (
	treeBranch = "├─ "
	treeLast   = "└─ "
	treeBar    = "│  "
	treeGap    = "   "
)

// Tree orders rows as a forest and gives each one the prefix it is drawn with.
// It returns the rows in the order they should print, and the ids whose edge
// was dropped because it closed a cycle.
//
// parent maps a row's id onto the id it hangs under. An id naming something
// outside rows is a root: that is the rule Forest already applies to a comment
// thread, and it collapses every reason a parent can be missing — filtered out,
// archived, in another repository, never imported — into the one case a reader
// can actually see. Every row in, every row out, exactly once.
func Tree(rows []Row, parent map[string]string) ([]Row, []string) {
	shown := make(map[string]Row, len(rows))
	for _, r := range rows {
		shown[r.ID] = r
	}
	broken := cycleBreaks(rows, parent, shown)

	// under is the row each one is actually drawn beneath, "" for a root.
	under := make(map[string]string, len(rows))
	children := make(map[string][]Row, len(rows))
	for _, r := range rows {
		p := parent[r.ID]
		if _, ok := shown[p]; !ok || broken[r.ID] {
			p = ""
		}
		under[r.ID] = p
		children[p] = append(children[p], r)
	}

	out := make([]Row, 0, len(rows))
	var walk func(kids []Row, indent string, depth int)
	walk = func(kids []Row, indent string, depth int) {
		kids = sortRows(kids)
		for i, r := range kids {
			connector, below := treeBranch, treeBar
			if i == len(kids)-1 {
				connector, below = treeLast, treeGap
			}
			switch {
			case depth > 0:
				r.Prefix = indent + connector
			case parent[r.ID] != "":
				// A root that names a parent. It has one; this listing just
				// cannot show it.
				r.Prefix = TreeMark
			}
			out = append(out, r)

			next := indent
			if depth > 0 {
				next = indent + below
			}
			walk(children[r.ID], next, depth+1)
		}
	}
	walk(children[""], "", 0)

	return out, broken.ids()
}

// breaks records which rows lose their edge to a cycle.
type breaks map[string]bool

func (b breaks) ids() []string {
	out := make([]string, 0, len(b))
	for id := range b {
		out = append(out, id)
	}
	slices.Sort(out)
	return out
}

// cycleBreaks picks the edges to drop so that walking parents terminates.
//
// A cycle is not a corruption to be refused at read time: `parent` is
// last-write-wins, so two clones can each set the other's issue as its parent
// while offline, having both checked first, and the merge is a faithful record
// of what happened. Exactly one edge per cycle is dropped, the lowest id's, so
// every clone breaks the same cycle at the same place and the rest of the loop
// still nests.
func cycleBreaks(rows []Row, parent map[string]string, shown map[string]Row) breaks {
	dropped, settled := breaks{}, map[string]bool{}
	for _, r := range rows {
		if settled[r.ID] {
			continue
		}
		var path []string
		at := map[string]int{}
		for cur := r.ID; ; {
			if _, ok := shown[cur]; !ok || settled[cur] {
				break
			}
			if i, seen := at[cur]; seen {
				loop := path[i:]
				lowest := slices.Min(loop)
				dropped[lowest] = true
				break
			}
			at[cur] = len(path)
			path = append(path, cur)
			if cur = parent[cur]; cur == "" {
				break
			}
		}
		for _, id := range path {
			settled[id] = true
		}
	}
	return dropped
}
