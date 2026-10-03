package main

import (
	"errors"
	"flag"
	"fmt"
	"io"

	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/origins"
)

// destroyUsage is printed only for `git review destroy --help`; the command is
// absent from helpText and from docs/ on purpose.
const destroyUsage = `usage: git review destroy --yes

Deletes every review in this repository by removing the notes ref they live on,
` + "`" + `refs/notes/reviews/open` + "`" + `, together with the check runs on ` + "`" + `refs/notes/checks/runs` + "`" + `.
There is no undo: the refs and their whole history are gone, and unlike ` + "`" + `remove` + "`" + `
a clone that already fetched them keeps its copies until it deletes them too.

The bridge state that described those reviews goes too, since it now describes
nothing: the import watermark for each tracker's pull requests, and the lines on
` + "`" + `refs/git-issue/origins` + "`" + ` saying which upstream object each review was. Both are
local and derived, and the next pull rebuilds them — but a watermark left behind
would tell that pull everything up to it was already imported, and it would
fetch only what has changed since.

Only the reviews' share of that state goes. The issues keep their watermarks and
their mappings, which is the difference between destroying one entity type and
emptying the repository.

--yes is mandatory, so this can never be the result of a typo. Remote-tracking
refs under refs/remotes/ are left alone; delete the remote's refs to be rid of
those.

For tearing down a test repository or abandoning a botched import — not part of
the everyday vocabulary, which is why ` + "`" + `git review help` + "`" + ` does not list it.`

// cmdDestroy removes the reviews ref outright. It is the one command that
// deletes rather than appends, so --yes is required and what it will touch is
// named in the refusal.
func cmdDestroy(args []string) error {
	fs := flag.NewFlagSet("destroy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "confirm deletion of every review")

	rest, err := parsePermuted(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Println(destroyUsage)
			return nil
		}
		return fmt.Errorf("%s\n%s", err, destroyUsage)
	}
	if len(rest) > 0 {
		return fmt.Errorf("destroy takes no arguments, got '%s'\n%s", rest[0], destroyUsage)
	}

	s, err := open()
	if err != nil {
		return err
	}
	checks := openChecks(s.Repo)

	if !*yes {
		return fmt.Errorf("destroy deletes every review in this repository — %s and %s — with no undo; pass --yes to confirm",
			s.FullRef(), checks.FullRef())
	}

	destroyed := 0
	if s.Repo.RefSHA(s.FullRef()) != "" {
		notes, err := s.Notes()
		if err != nil {
			return err
		}
		if err := s.Destroy(); err != nil {
			return err
		}
		fmt.Printf("destroyed %s (%s)\n", s.FullRef(), cli.Plural(len(notes), "review"))
		destroyed++
	}
	// The check runs are keyed by commit rather than by review, so they outlive
	// any one review — but they are this command's writing, and a clone with no
	// reviews left has nothing to read them against.
	if s.Repo.RefSHA(checks.FullRef()) != "" {
		notes, err := checks.Notes()
		if err != nil {
			return err
		}
		if err := checks.Destroy(); err != nil {
			return err
		}
		fmt.Printf("destroyed %s (checks on %s)\n", checks.FullRef(), cli.Plural(len(notes), "commit"))
		destroyed++
	}

	cleared, err := clearReviewBridgeState(s)
	if err != nil {
		return err
	}
	destroyed += cleared

	if destroyed == 0 {
		fmt.Println("nothing to destroy")
	}
	return nil
}

// clearReviewBridgeState removes what a pull left outside the notes ref: how far
// each tracker's pull requests have been read, and which upstream object each
// review was.
//
// Both are local and derived — the next import rebuilds them — so dropping them
// costs one full re-import and nothing else. Leaving them costs more than it
// looks: the watermark is the one with teeth, since it says everything up to a
// point is already imported, so the next pull asks only for what changed since
// and refills the repository with a slice of the tracker.
//
// Both are also shared with the issues, which is why neither is cleared whole.
// The watermarks are keyed by scheme and the reviews' keys are their own; the
// ledger is keyed by entity, and only the entities its `review` lines name are
// forgotten. An issue's mapping deleted here would be re-created upstream by the
// next issue push, as a second copy of an issue that is already there.
func clearReviewBridgeState(s *entity.Store) (int, error) {
	cleared := 0

	state, err := s.SyncState()
	if err != nil {
		return cleared, err
	}
	n, err := state.Forget(syncScheme)
	if err != nil {
		return cleared, err
	}
	if n > 0 {
		fmt.Printf("cleared %s; the next pull reads each tracker's pull requests in full\n",
			cli.Plural(n, "import watermark"))
		cleared++
	}

	led := origins.NewStore(s.Repo)
	trackers, err := led.Trackers()
	if err != nil {
		return cleared, err
	}
	forgotten := 0
	for _, tracker := range trackers {
		ledger, err := led.Load(tracker)
		if err != nil {
			return cleared, err
		}
		gone := ledger.Forget(origins.KindReview)
		if gone == 0 {
			continue
		}
		forgotten += gone
		// A tracker with nothing left in it is dropped rather than left as an
		// empty path: an empty ledger is a ledger, and the next reader would
		// have to open it to find out it says nothing.
		if ledger.Len() == 0 {
			err = led.Drop(tracker, "git-review: forget "+tracker)
		} else {
			err = led.Save(ledger, "git-review: forget the reviews in "+tracker)
		}
		if err != nil {
			return cleared, err
		}
	}
	if forgotten > 0 {
		fmt.Printf("forgot %s on %s (which upstream object each review was)\n",
			cli.Plural(forgotten, "mapping"), origins.Ref)
		cleared++
	}

	return cleared, nil
}
