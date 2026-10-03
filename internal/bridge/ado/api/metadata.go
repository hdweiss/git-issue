// Project metadata: the canonical project name, and the area tree a caller
// offers to choose from.

package adoapi

import (
	"net/url"
	"strings"
)

// Area is one node of a project's area tree.
type Area struct {
	// Name is the node's own label.
	Name string
	// Path is the node's position below the project root, "/"-separated, and
	// empty for the root itself. It is what a target's `#` scope holds.
	Path     string
	Children []Area
}

// Areas reads the project's area tree.
//
// The whole tree comes in one request, which is what lets a caller render it
// and ask rather than making somebody go and find their area path by hand. The
// depth is bounded because the API requires a bound; ten is far past any real
// project's nesting.
func (c *Client) Areas(t Target) (Area, error) {
	var node classificationNode
	params := url.Values{"$depth": {"10"}}
	if err := c.get(t.API()+"/wit/classificationnodes/areas", params, &node); err != nil {
		return Area{}, err
	}
	return node.area(""), nil
}

// Project resolves the project's canonical name.
//
// Azure DevOps compares project names case-insensitively, so a remote URL
// spelling a project "myproj" and another spelling it "MyProj" name one
// project — but they would be two paths in the origin ledger and two watermark
// keys. Asking the server which spelling is real is what keeps a project from
// forking into two trackers.
func (c *Client) Project(t Target) (string, error) {
	var response struct {
		Name string `json:"name"`
	}
	endpoint := t.Base + "/" + t.Collection + "/_apis/projects/" + url.PathEscape(t.Project)
	if err := c.get(endpoint, nil, &response); err != nil {
		return "", err
	}
	if response.Name == "" {
		return t.Project, nil
	}
	return response.Name, nil
}

type classificationNode struct {
	Name     string               `json:"name"`
	Children []classificationNode `json:"children"`
}

func (n classificationNode) area(parent string) Area {
	a := Area{Name: n.Name, Path: parent}
	for _, child := range n.Children {
		path := child.Name
		if parent != "" {
			path = parent + "/" + child.Name
		}
		a.Children = append(a.Children, child.area(path))
	}
	return a
}

// Walk visits the tree depth-first, root first, reporting each node's depth.
//
// The order is the order a rendered tree reads in, so a caller can number the
// nodes as it draws them and index straight back into the same walk.
func (a Area) Walk(visit func(node Area, depth int)) {
	a.walk(visit, 0)
}

func (a Area) walk(visit func(node Area, depth int), depth int) {
	visit(a, depth)
	for _, child := range a.Children {
		child.walk(visit, depth+1)
	}
}

// Find returns the node at a path, reporting whether the tree holds one.
// Matching folds case, because Azure DevOps does.
func (a Area) Find(path string) (Area, bool) {
	path = strings.Trim(strings.ReplaceAll(path, `\`, "/"), "/")
	var found Area
	ok := false
	a.Walk(func(node Area, _ int) {
		if !ok && strings.EqualFold(node.Path, path) {
			found, ok = node, true
		}
	})
	return found, ok
}
