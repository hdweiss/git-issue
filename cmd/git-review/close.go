package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/render"
	"github.com/hdweiss/git-issue/internal/review"
)

const closeUsage = `usage: git review close <id> [<comment>] [--as <reason>[:<id>]] [-m <message>]

       <comment>              resolve that thread instead of closing the review
       --as <reason>          why: not-planned, superseded:<id>, duplicate:<id>
   -m, --message <text>       post this alongside, in the same action

One positional closes the review — abandoned, not merged. Two resolve one
thread inside it, addressed by the id 'git review show' prints beside each
entry. That is the same slot 'edit' and 'remove' use for a comment, so the three
read alike.

Resolving is not retracting. A resolved thread still folds, still renders, and
still carries every word in it: the difference from 'git review remove <id>
<comment>' is the difference between addressed and withdrawn. Anyone may resolve
any thread, including the person who opened it — nothing here is enforced.

--as names another entity where the reason has one: 'superseded:4b0755a3' closes
this review in favour of that one and writes the link in the same action. The
target is a flag value rather than a second positional, so the second positional
always means a comment.

'merged' is not a close: it says the head reached the base, which only a client
that saw it happen may write. This verb is for a review nobody merged.

-m posts a comment in the same action, so a closure that needs a word is one
command and one commit.`

const reopenUsage = `usage: git review reopen <id> [<comment>] [-m <message>]

One positional reopens the review; two unresolve one thread inside it.

Reopening leaves --as's reason and any link it wrote alone. The pairing goes
stale — a reopened review keeps the reason it was closed with, because nothing
supersedes it — and that is deliberate: clearing it would lose why it was closed
the first time, and it would still race with a concurrent re-close.`

func cmdClose(args []string) error {
	return closeOrReopen(args, true)
}

func cmdReopen(args []string) error {
	return closeOrReopen(args, false)
}

func closeOrReopen(args []string, closing bool) error {
	usage := reopenUsage
	name := "reopen"
	if closing {
		usage, name = closeUsage, "close"
	}

	var as, message string
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	if closing {
		fs.StringVar(&as, "as", "", "why: not-planned, superseded:<id>, duplicate:<id>")
	}
	for _, n := range []string{"m", "message"} {
		fs.StringVar(&message, n, "", "post this alongside")
	}

	targets, err := parseTargets(fs, name, args, usage, 2)
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

	if len(targets) == 2 {
		if as != "" {
			return fmt.Errorf("--as gives a review's close reason; a thread is resolved or it is not\n%s", usage)
		}
		return setThreadResolved(s, who, id, st, targets[1], closing, message)
	}
	return setReviewStatus(s, who, id, st, closing, as, message, usage)
}

// setThreadResolved resolves or unresolves one thread, addressed by any entry
// in it.
//
// The annotation lands on the thread's *root*, which is what makes resolution a
// property of the conversation rather than of the last thing said in it — so
// naming a reply resolves the thread it belongs to, and the command says which
// one that was.
func setThreadResolved(s *entity.Store, who, id string, st entity.State, prefix string, resolved bool, message string) error {
	c, err := st.FindComment(prefix)
	if err != nil {
		return err
	}

	root := rootOf(st, c)
	wrote, err := review.SetResolved(s, who, time.Now().Unix(), id, st, root.ID(), resolved, message)
	if err != nil {
		return err
	}
	if !wrote {
		fmt.Println("nothing changed")
		return nil
	}

	word := "unresolved"
	if resolved {
		word = "resolved"
	}
	line := fmt.Sprintf("%s  %s", render.Abbrev(root.ID()), word)
	if root.ID() != c.ID() {
		line += fmt.Sprintf(" (the thread %s is in)", render.Abbrev(c.ID()))
	}
	fmt.Println(line)
	return nil
}

// rootOf walks up to the entry a thread hangs from.
//
// An entry whose parent this clone does not hold is itself a root, which is the
// same rule the fold applies when it renders such an entry at the top level: a
// parent goes missing routinely, and waiting for one would lose the thread.
func rootOf(st entity.State, c entity.Comment) entity.Comment {
	byID := make(map[string]entity.Comment, len(st.Thread))
	for _, e := range st.Thread {
		byID[e.ID()] = e
	}
	seen := map[string]bool{}
	for {
		parent, ok := byID[c.Parent()]
		if !ok || seen[c.ID()] {
			return c
		}
		seen[c.ID()] = true
		c = parent
	}
}

// setReviewStatus closes or reopens the review itself.
func setReviewStatus(s *entity.Store, who, id string, st entity.State, closing bool, as, message, usage string) error {
	want := review.FieldsOf(st)

	if closing {
		want.Status = review.StatusClosed
		if as != "" {
			reason, target, err := parseReason(as)
			if err != nil {
				return fmt.Errorf("%s\n%s", err, usage)
			}
			want.StatusReason = reason
			if target != "" {
				// Expanded before it is written: an abbreviation only means
				// something relative to the ref it was measured against, so
				// storing one would write a link nobody can follow.
				full, err := resolveTarget(s, target)
				if err != nil {
					return err
				}
				kind, _ := review.ReasonTakesTarget(reason)
				want.Relations = append(review.Relations(st), review.Relation{Kind: kind, Target: full})
			}
		}
	} else {
		// Reopening leaves the reason and any link it wrote alone: the stale
		// pairing docs/reviews.md accepts. Readers ignore status.reason while
		// the status is non-terminal.
		want.Status = review.StatusOpen
	}

	changed, _, err := review.Update(s, who, time.Now().Unix(), id, st, want)
	if err != nil {
		return err
	}

	posted := false
	if message != "" {
		// Reloaded, because the status events above moved the clock: an event
		// written from the stale state would collide rather than follow.
		_, fresh, err := s.Find(id, review.Type)
		if err != nil {
			return err
		}
		if _, err := review.Comment(s, who, time.Now().Unix(), id, fresh, message, "", nil); err != nil {
			return err
		}
		posted = true
	}

	if len(changed) == 0 && !posted {
		fmt.Println("nothing changed")
		return nil
	}
	if len(changed) == 0 {
		changed = []string{"comment"}
	} else if posted {
		changed = append(changed, "comment")
	}
	fmt.Printf("%s  %s\n", render.Abbrev(id), strings.Join(changed, ", "))
	return nil
}

// parseReason reads `--as <reason>` and `--as <reason>:<id>`.
//
// The target is a flag value rather than a second positional so that the second
// positional can always mean a comment. It is required for the reasons that
// have no meaning without one — closing "as superseded" by nothing says nothing
// — and refused for the one that does.
func parseReason(arg string) (reason, target string, err error) {
	reason, target, hasTarget := strings.Cut(arg, ":")
	reason = review.NormalizeReason(reason)
	target = strings.TrimSpace(target)

	if !review.KnownReason(reason) {
		return "", "", fmt.Errorf("unknown reason '%s'; try %s", reason, strings.Join(review.Reasons, ", "))
	}

	kind, needs := review.ReasonTakesTarget(reason)
	switch {
	case needs && (!hasTarget || target == ""):
		return "", "", fmt.Errorf("--as %s closes this review in favour of another; spell it %s:<id>", reason, reason)
	case !needs && hasTarget:
		return "", "", fmt.Errorf("--as %s names nothing else; drop the ':%s'", reason, target)
	}
	_ = kind
	return reason, target, nil
}
