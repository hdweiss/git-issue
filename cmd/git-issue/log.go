package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/hdweiss/git-issue/internal/issue"
)

// cmdLog shows the notes ref's own history: one commit per action, in the
// words docs/storage-model.md's provenance section asks for.
//
// It is `git log` and not a renderer of its own. Everything git log can do —
// --oneline, -p, --author, --since, --grep, its pager and its colours — is
// worth more than any format this could invent, and the commits were given
// real authors and timestamps precisely so that those tools would work on
// them.
//
// What it is not is a source of truth. Commit messages are prose about events
// whose own bytes are authoritative, and unlike those bytes they do not
// converge: two clones that import the same issue independently write
// identical blobs and two sets of commits, so a merged log lists every action
// twice. `git issue show` is what says what an issue currently is.
func cmdLog(args []string) error {
	if len(args) > 0 && (args[0] == "-h" || args[0] == "--help") {
		fmt.Println(logUsage)
		return nil
	}

	s, err := open()
	if err != nil {
		return err
	}

	// A leading argument that is not a flag is an issue id. Everything else,
	// wherever it appears, belongs to git log.
	var paths []string
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		id, _, err := s.Find(args[0], issue.Type)
		if err != nil {
			return err
		}
		paths = notePathspecs(id)
		args = args[1:]
	}

	// --author-date-order first, so a later flag of the caller's own still
	// wins. The default matters: commits arrive in the order they were
	// written, and a resumed import writes events older than ones already on
	// the ref, so commit order is not the history's order.
	full := []string{"--author-date-order"}
	full = append(full, args...)
	full = append(full, s.FullRef())
	if len(paths) > 0 {
		full = append(full, "--")
		full = append(full, paths...)
	}
	// git log writes to this terminal and reports its own failures on it, so
	// its exit status is passed through rather than described a second time.
	if err := s.Repo.Log(full, os.Stdout, os.Stderr); err != nil {
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			return exitCode(exit.ExitCode())
		}
		return err
	}
	return nil
}

// notePathspecs is every path an entity's blob may sit at.
//
// git re-shards a notes tree as it grows, which renames every note in it —
// `abcd…` becomes `ab/cd…`. A pathspec of one spelling therefore loses the
// history written under the other, silently: confirmed empirically, limiting
// to the pre-reshard path shows the commits from before the reshard and none
// after. Naming every fanout depth costs nothing and cannot miss.
func notePathspecs(id string) []string {
	specs := []string{id}
	for _, depth := range []int{1, 2} {
		if len(id) <= 2*depth {
			break
		}
		var b strings.Builder
		for i := 0; i < depth; i++ {
			b.WriteString(id[i*2 : i*2+2])
			b.WriteString("/")
		}
		b.WriteString(id[depth*2:])
		specs = append(specs, b.String())
	}
	return specs
}

const logUsage = `usage: git issue log [<id>] [<git log options>]

Shows the tracker's own history: one commit per action, authored by whoever
did it, dated when they did it. A commit's subject says what happened — the
issue that was filed, the comment that was posted, the label that was added —
and its Issue trailer names the entity it happened to.

       <id>            limit the log to one issue
       any other flag  passed straight to git log

Because it is git log, git log's own flags work: --oneline, -p to see the
event lines each action wrote, --author, --since, --grep, -S. Commits come out
in author-date order by default, since a resumed import writes events older
than ones already on the ref.

The log is a second reading of the tracker, never the authority on it. Commit
messages describe events whose own bytes are the truth, and they do not
converge the way those bytes do: two clones that import the same issues
independently produce the same blobs and two sets of commits, so a merged log
lists each action twice. Use 'git issue show <id>' for what an issue is.`
