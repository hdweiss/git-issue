package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	adoapi "github.com/hdweiss/git-issue/internal/bridge/ado/api"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/remote"
)

// The area a pull is scoped to, and where the choice is kept.
//
// The area is not state: it is never an event, never a ledger line and never
// shown. It is a question asked of the server — which work items to read — and
// a default for where a push files a new one. See "The area is scope, and only
// scope" in docs/bridge-ado.md.
//
// The point of asking once and remembering is that a real Azure DevOps project
// is organization-wide. Reading all of it is rarely what somebody wants and
// sometimes more than the query will return at all, so the alternative to
// choosing an area is not "no setup", it is "go and find your area path and
// type it every time".

// areaConfigKey is where one tracker's saved area lives.
//
// Git config rather than a file of our own: it is user-editable, per-clone,
// and it is what this tracker already does for everything configurable. The
// dots in the tracker name are fine — git splits a key on its first and last
// dot only, so the middle is one subsection however many dots it holds.
func areaConfigKey(tracker string) string { return "issue." + tracker + ".area" }

// savedArea is the area configured for a tracker, or "" for none.
func savedArea(repo *gitx.Repo, tracker string) string {
	return strings.TrimSpace(repo.ConfigDefault(areaConfigKey(tracker)))
}

// saveArea records the choice, or clears it when the choice was the whole
// project.
func saveArea(repo *gitx.Repo, tracker, area string) error {
	if area == "" {
		return repo.UnsetConfig(areaConfigKey(tracker))
	}
	return repo.SetConfig(areaConfigKey(tracker), area)
}

// forgetAreas clears every saved area. `git issue destroy` calls it for the
// same reason it clears the ledger and the watermarks: what is left behind
// should not describe a tracker this repository no longer holds anything from.
func forgetAreas(repo *gitx.Repo) {
	for _, key := range repo.ConfigKeys(`^issue\..*\.area$`) {
		repo.UnsetConfig(key)
	}
}

// resolveArea decides which area this run reads, and remembers a choice made
// interactively.
//
// Three inputs, in order. An area written on the command line wins and is not
// saved — it is a question, not a checkpoint, the same rule --since follows. A
// bare `#` forgets the saved choice and asks again. Otherwise the saved area
// stands, and only when there is none is anybody asked.
func resolveArea(repo *gitx.Repo, client *adoapi.Client, target adoapi.Target, spec remote.Spec, w io.Writer) (adoapi.Target, error) {
	tracker := target.String()

	if spec.ScopeSet && spec.Scope != "" {
		target.Area = normalizeArea(spec.Scope)
		return target, nil
	}
	if !spec.ScopeSet {
		if saved := savedArea(repo, tracker); saved != "" {
			target.Area = normalizeArea(saved)
			return target, nil
		}
	}

	// Nothing configured, or an explicit request to choose again.
	area, chose, err := pickArea(client, target, w)
	if err != nil {
		return target, err
	}
	if chose {
		if err := saveArea(repo, tracker, area); err != nil {
			return target, err
		}
	}
	target.Area = area
	return target, nil
}

// normalizeArea accepts either separator and drops the project root, so that
// pasting a path out of Azure DevOps works as well as typing one.
func normalizeArea(area string) string {
	return strings.Trim(strings.ReplaceAll(area, `\`, "/"), "/")
}

// pickArea renders the project's area tree and asks which one to track,
// reporting whether an answer was actually given.
//
// Nothing is asked when stdin is not a terminal. A pull from a script or a
// pipeline must not block on a prompt nobody can see, so it reads the whole
// project and says how to scope it — which is the right default anyway, being
// the one that omits nothing.
func pickArea(client *adoapi.Client, target adoapi.Target, w io.Writer) (string, bool, error) {
	if !gitx.IsTerminal(os.Stdin) {
		fmt.Fprintf(w, "Reading all of %s. To scope it to one area: %s\n",
			target.Project, "git issue pull ado:<remote>#<area>")
		return "", false, nil
	}

	root, err := client.Areas(target)
	if err != nil {
		return "", false, err
	}

	nodes := renderAreas(root, w)
	if len(nodes) <= 1 {
		// A project with no sub-areas has nothing to choose between.
		return "", false, nil
	}

	answer, err := bufio.NewReader(os.Stdin).ReadString('\n')
	if err != nil && answer == "" {
		return "", false, nil
	}
	answer = strings.TrimSpace(answer)
	if answer == "" {
		// The default is the root, which is the whole project. Saving that is
		// worth doing: it records that the question was asked and answered, so
		// the next pull does not ask again.
		return "", true, nil
	}

	n, err := strconv.Atoi(answer)
	if err != nil || n < 0 || n >= len(nodes) {
		return "", false, fmt.Errorf("no area numbered %q; run the pull again to choose", answer)
	}
	return nodes[n], true, nil
}

// renderAreas draws the tree and returns each numbered node's path, indexed by
// the number it was drawn with.
func renderAreas(root adoapi.Area, w io.Writer) []string {
	var paths []string
	var lines []string
	root.Walk(func(node adoapi.Area, depth int) {
		paths = append(paths, node.Path)
		lines = append(lines, strings.Repeat("  ", depth)+node.Name)
	})

	fmt.Fprintf(w, "\nPick an area to track in %s:\n\n", root.Name)
	for i, line := range lines {
		note := ""
		if i == 0 {
			note = "   everything"
		}
		fmt.Fprintf(w, "  %2d  %s%s\n", i, line, note)
	}
	fmt.Fprintf(w, "\nArea [0]: ")
	return paths
}
