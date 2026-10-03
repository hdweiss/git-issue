package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
	"github.com/hdweiss/git-issue/internal/render"
)

// close and reopen are sugar over the one field edit writes as `--status`: a
// single `status` scalar event, resolved last-write-wins like any other. They
// exist because closing an issue is the write people reach for most, and
// `edit --status closed` is a clumsy way to spell it.
//
// The close reason is a second, independent scalar (`status.reason`). It is a
// hint for a reader, ignored while the status is non-terminal, and a reopen
// deliberately leaves it alone — see docs/issues.md on the stale pairing.

const closeUsage = `usage: git issue close <id> [--as <reason>] [<duplicate-of>]

       --as <reason>   why it is closing: completed, not-planned or duplicate

Closes the issue — one 'status' event, nothing else. 'git issue reopen' undoes
it.

--as records why, as a separate 'status.reason' event. 'completed' is a plain
resolution, 'not-planned' is closed without being done (won't fix, out of
scope, stale), and 'duplicate' defers to another issue: name that issue as a
second argument and a 'duplicate-of' link is written in the same step.

    git issue close 4b07 --as duplicate 9c1e

The reason only resolves while the status is terminal, so an issue reopened
after 'close --as not-planned' keeps that reason until it is closed again
rather than being retroactively cleared (docs/issues.md). For a status a bridge
uses that is neither open nor closed — Azure DevOps' 'Active', say — use
'git issue edit <id> --status'.`

const reopenUsage = `usage: git issue reopen <id>

Reopens a closed issue — one 'status' event setting it back to open. An earlier
close reason is left untouched: it only resolves while the status is terminal
(docs/issues.md).`

// cmdClose sets an issue's status to closed, and — with --as — records why.
func cmdClose(args []string) error {
	fs := flag.NewFlagSet("close", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var as string
	fs.StringVar(&as, "as", "", "why it is closing: completed, not-planned or duplicate")

	rest, err := parsePermuted(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(closeUsage)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s\n%s", err, closeUsage)
	}
	if len(rest) == 0 {
		return fmt.Errorf("%s", closeUsage)
	}
	if len(rest) > 2 {
		return fmt.Errorf("close takes an issue and, as a duplicate, the issue it duplicates; got '%s' as well\n%s", rest[2], closeUsage)
	}

	reason := ""
	if as != "" {
		reason = issue.NormalizeReason(as)
		if !issue.KnownReason(reason) {
			return fmt.Errorf("unknown close reason '%s'; try completed, not-planned or duplicate — or 'git issue edit <id> --status-reason' for a raw value\n%s", as, closeUsage)
		}
	}

	dupOf := ""
	if len(rest) == 2 {
		dupOf = rest[1]
		if reason != issue.ReasonDuplicate {
			return fmt.Errorf("a second issue is the one being duplicated, which needs '--as duplicate'\n%s", closeUsage)
		}
	}

	s, err := open()
	if err != nil {
		return err
	}
	who, err := author(s)
	if err != nil {
		return err
	}
	id, st, err := s.Find(rest[0], issue.Type)
	if err != nil {
		return err
	}

	fields := issue.FieldsOf(st)
	fields.Status = issue.StatusClosed
	if reason != "" {
		fields.StatusReason = reason
	}
	if dupOf != "" {
		target, _, err := s.Find(dupOf, issue.Type)
		if err != nil {
			return err
		}
		if target == id {
			return fmt.Errorf("an issue cannot be a duplicate of itself")
		}
		fields.Relations = append(issue.Relations(st), issue.Relation{Kind: issue.KindDuplicate, Target: target})
	}

	return writeStatus(s, who, id, st, "closed ", fields)
}

// cmdReopen sets an issue's status back to open, leaving any close reason where
// it is.
func cmdReopen(args []string) error {
	fs := flag.NewFlagSet("reopen", flag.ContinueOnError)
	fs.SetOutput(io.Discard)

	rest, err := parsePermuted(fs, args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(reopenUsage)
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s\n%s", err, reopenUsage)
	}
	if len(rest) != 1 {
		return fmt.Errorf("%s", reopenUsage)
	}

	s, err := open()
	if err != nil {
		return err
	}
	who, err := author(s)
	if err != nil {
		return err
	}
	id, st, err := s.Find(rest[0], issue.Type)
	if err != nil {
		return err
	}

	fields := issue.FieldsOf(st)
	fields.Status = issue.StatusOpen
	return writeStatus(s, who, id, st, "reopened ", fields)
}

// writeStatus applies a status change and reports it the way `edit` reports a
// field change: the one-line row, or "nothing changed" when the issue already
// said this. It is the tail of editIssue, shared because a close is an edit of
// one field with a friendlier name.
func writeStatus(s *entity.Store, who, id string, st entity.State, verb string, fields issue.Fields) error {
	changed, detached, err := issue.Update(s, who, time.Now().Unix(), id, st, fields)
	if err != nil {
		return err
	}
	if len(changed) == 0 && len(detached) == 0 {
		fmt.Println("nothing changed")
		return nil
	}
	for _, far := range detached {
		fmt.Printf("also detached from %s\n", short(far))
	}

	_, updated, err := s.Find(id, issue.Type)
	if err != nil {
		return err
	}
	render.WriteRow(os.Stdout, verb, issue.Row(id, updated), uniqueIDs(s))
	return nil
}
