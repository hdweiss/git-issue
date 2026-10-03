package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/issue"
	"github.com/hdweiss/git-issue/internal/origins"
)

// destroyUsage is printed only for `git issue destroy --help`; the command is
// absent from helpText and from docs/ on purpose.
const destroyUsage = `usage: git issue destroy --yes

Deletes every issue in this repository by removing the notes refs they live on,
` + "`" + `refs/notes/issues/open` + "`" + ` and ` + "`" + `refs/notes/issues/archived` + "`" + `, in one step. There is
no undo: the refs and their whole history are gone, and unlike ` + "`" + `remove` + "`" + ` a
clone that already fetched them keeps its copies until it deletes them too.

The bridge state that described those issues goes too, since it now describes
nothing: the import watermark, and the origin ledger on ` + "`" + `refs/git-issue/origins` + "`" + `
saying which upstream object each issue was. Both are local and derived, and the
next pull rebuilds them — but a watermark left behind would tell that pull that
everything up to it was already imported, and it would fetch only what has
changed since.

--yes is mandatory, so this can never be the result of a typo. Remote-tracking
refs under refs/remotes/ are left alone; delete the remote's refs to be rid of
those.

For tearing down a test repository or abandoning a botched import — not part of
the everyday vocabulary, which is why ` + "`" + `git issue help` + "`" + ` does not list it.`

// cmdDestroy removes both issue notes refs outright. It is the one command that
// deletes rather than appends, so --yes is required and the refs it will touch
// are named in the refusal.
func cmdDestroy(args []string) error {
	fs := flag.NewFlagSet("destroy", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	yes := fs.Bool("yes", false, "confirm deletion of every issue ref")

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
	refs := []*entity.Store{
		s,
		entity.NewStore(s.Repo, issue.RefArchived, s.Vocab),
	}

	if !*yes {
		return fmt.Errorf("destroy deletes every issue in this repository — %s and %s — with no undo; pass --yes to confirm",
			refs[0].FullRef(), refs[1].FullRef())
	}

	destroyed := 0
	for _, ref := range refs {
		if s.Repo.RefSHA(ref.FullRef()) == "" {
			continue
		}
		notes, err := ref.Notes()
		if err != nil {
			return err
		}
		if err := ref.Destroy(); err != nil {
			return err
		}
		fmt.Fprintf(os.Stdout, "destroyed %s (%s)\n", ref.FullRef(), plural(len(notes), "issue"))
		destroyed++
	}

	// The notes refs are not the whole of it. Everything a bridge accumulated
	// about this repository describes entities that no longer exist, and the
	// watermark is the one with teeth: it says everything up to a point is
	// already imported, so the next pull asks only for what changed since and
	// refills the repository with a slice of the tracker.
	cleared, err := clearBridgeState(s)
	if err != nil {
		return err
	}
	destroyed += cleared

	if destroyed == 0 {
		fmt.Println("nothing to destroy")
	}
	return nil
}

// clearBridgeState removes what a bridge left outside the notes refs: how far
// each tracker has been read, and which upstream object each entity was. Both
// are local and derived — the next import rebuilds them — so dropping them
// costs one full re-import and nothing else.
func clearBridgeState(s *entity.Store) (int, error) {
	cleared := 0

	state, err := s.SyncState()
	if err != nil {
		return cleared, err
	}
	had, err := state.Clear()
	if err != nil {
		return cleared, err
	}
	if had {
		fmt.Println("cleared the import watermark; the next pull reads each tracker in full")
		cleared++
	}

	had, err = origins.NewStore(s.Repo).Destroy()
	if err != nil {
		return cleared, err
	}
	if had {
		fmt.Printf("destroyed %s (which upstream object each issue was)\n", origins.Ref)
		cleared++
	}

	// The saved area a pull was scoped to. Not a watermark and not a mapping,
	// but the same kind of leftover: it describes a tracker this repository is
	// no longer holding anything from.
	if keys := s.Repo.ConfigKeys(`^issue\..*\.area$`); len(keys) > 0 {
		forgetAreas(s.Repo)
		fmt.Printf("forgot %s; the next pull asks again\n", plural(len(keys), "saved area"))
		cleared++
	}
	return cleared, nil
}
