package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/render"
	"github.com/hdweiss/git-issue/internal/review"
)

const approveUsage = `usage: git review approve <id> [-m <message>] [-F <path>]

Records that, as far as you are concerned, this should merge.

The verdict carries the revision it was cast against, so a later push makes it
stale — reported as stale, never dropped and never quietly re-pointed at the new
head. Whether a stale approval still counts is policy: upstream it is branch
protection's call, and here 'git review status' reports it and stops.

It supersedes your previous verdict rather than retracting it. Both stay in the
blob, so the record that you once asked for changes survives; only your current
position changes.

Approving does not resolve your own open threads. If you have any, this says so
and approves anyway — resolving them as a side effect would discard questions
other people are still waiting on.

-m posts a comment alongside, in the same action. Without it nothing is said and
no editor opens: approving silently is ordinary.`

const requestChangesUsage = `usage: git review request-changes <id> [-m <message>] [-F <path>]

Records that this should not merge as it stands.

Advisory, like every verdict here: nothing in a grow-only set can prevent a
merge, and a client that ignores this is not corrupting anything. Upstream,
branch protection is what turns it into a block; locally, 'git review status'
reports it.

Without -m an editor opens. A request for changes that does not say what they
are is not worth much, which is why this is the one verdict that asks.`

func cmdVerdict(value string, args []string) error {
	usage := approveUsage
	name := "approve"
	if value == review.VerdictRequestChanges {
		usage, name = requestChangesUsage, "request-changes"
	}

	var message, file string
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, n := range []string{"m", "message"} {
		fs.StringVar(&message, n, "", "post this alongside")
	}
	for _, n := range []string{"F", "file"} {
		fs.StringVar(&file, n, "", "read the message from a file, or '-' for stdin")
	}

	targets, err := parseTargets(fs, name, args, usage, 1)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(usage)
		return nil
	}
	if err != nil {
		return err
	}

	s, err := open()
	if err != nil {
		return err
	}
	who, err := author(s)
	if err != nil {
		return err
	}
	id, st, err := s.Find(targets[0], review.Type)
	if err != nil {
		return err
	}

	if message == "" {
		buf, fromInput, err := readBuffer(file, file != "", true)
		if err != nil {
			return err
		}
		switch {
		case fromInput:
			message = strings.TrimSpace(ignoredBlock.ReplaceAllString(buf, ""))
		// The editor opens where a message is required and missing, which is
		// request-changes and not approve. Approving silently is ordinary;
		// asking for changes without saying what they are is not.
		case value == review.VerdictRequestChanges:
			if !s.Repo.CanEdit() {
				return fmt.Errorf("request-changes needs a message: use -m, -F, or set core.editor")
			}
			message, err = editText(s, "", "Requesting changes on review "+render.Abbrev(id)+".")
			if err != nil {
				return err
			}
		}
	}

	if review.Terminal(review.Status(st)) {
		fmt.Fprintf(os.Stderr, "warning: this review is %s; the verdict is recorded but nothing is waiting on it\n", review.Status(st))
	}
	if review.Draft(st) {
		fmt.Fprintf(os.Stderr, "warning: this review is still a draft\n")
	}

	wrote, err := review.SetVerdict(s, who, time.Now().Unix(), id, st, value, message)
	if err != nil {
		return err
	}
	if !wrote {
		fmt.Println("nothing changed")
		return nil
	}

	// Approving over your own open threads is worth saying and not worth
	// preventing: resolving them here would discard questions other people are
	// still waiting on, and refusing would make the common case a two-step.
	if value == review.VerdictApprove {
		if n := openThreadsBy(st, who); n > 0 {
			fmt.Fprintf(os.Stderr, "warning: %d of your own %s still unresolved on this review\n",
				n, plural(n, "thread is", "threads are"))
		}
	}

	fmt.Printf("%s  %s\n", render.Abbrev(id), value)
	return nil
}

// openThreadsBy counts the conversations this person started and has not
// resolved.
//
// Their own, not everyone's: approving while somebody else's question is open
// is a normal thing to do — you have read it and you are content — while
// approving over your own is the case worth a word.
func openThreadsBy(st entity.State, who string) int {
	n := 0
	for _, t := range review.Unresolved(review.Threads(st, nil)) {
		if strings.EqualFold(t.Root.Event.A, who) {
			n++
		}
	}
	return n
}
