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

const commentUsage = `usage: git review comment <id> [<comment>] [-m <message>] [-F <path>]
                          [--on <path>:<line>[-<line>]] [--side left|right]

       <comment>                     reply in that thread instead of starting one
   -m, --message <text>              the comment, without opening an editor
   -F, --file <path>                 read it from a file ('-' = stdin)
       --on <path>:<line>            anchor it to a place in the code
       --side left|right             which half of a split diff; right is the default

With no -m an editor opens, the way git commit does.

--on anchors the comment to a place in the code. The anchor records the review's
current revision alongside the path and the lines, so a reader can tell later
that the comment was written against an older commit — which is why an anchor is
never moved afterwards. A comment is part of what somebody said, and relocating
it would be editing their words to point somewhere they never looked.

A second id replies inside that thread. Replies inherit the thread's anchor, so
--on is refused on one: a thread is attached to one place in the code, and
letting replies wander produces a conversation whose entries disagree about what
they are discussing.`

func cmdComment(args []string) error {
	var (
		message string
		file    string
		on      string
		side    string
	)
	fs := flag.NewFlagSet("comment", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	for _, name := range []string{"m", "message"} {
		fs.StringVar(&message, name, "", "the comment's text")
	}
	for _, name := range []string{"F", "file"} {
		fs.StringVar(&file, name, "", "read the comment from a file, or '-' for stdin")
	}
	fs.StringVar(&on, "on", "", "anchor the comment to <path>:<line>")
	fs.StringVar(&side, "side", "", "which half of a split diff: left or right")

	targets, err := parseTargets(fs, "comment", args, commentUsage, 2)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(commentUsage)
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

	parent := ""
	if len(targets) == 2 {
		c, err := st.FindComment(targets[1])
		if err != nil {
			return err
		}
		if on != "" {
			return fmt.Errorf("a reply inherits its thread's anchor; --on belongs on the comment that starts one")
		}
		parent = c.ID()
	}

	anchor, err := buildAnchor(st, on, side)
	if err != nil {
		return err
	}

	body := message
	if body == "" {
		buf, fromInput, err := readBuffer(file, file != "", true)
		if err != nil {
			return err
		}
		if fromInput {
			body = strings.TrimSpace(ignoredBlock.ReplaceAllString(buf, ""))
		} else {
			if !s.Repo.CanEdit() {
				return fmt.Errorf("no message given, and no editor to open: use -m, -F, or set core.editor")
			}
			body, err = editText(s, "", commentIntro(id, anchor))
			if err != nil {
				return err
			}
		}
	}

	entry, err := review.Comment(s, who, time.Now().Unix(), id, st, body, parent, anchor)
	if err != nil {
		return err
	}
	fmt.Println(render.Abbrev(entry))
	return nil
}

// buildAnchor turns --on and --side into the anchor a comment carries, or nil
// where none was asked for.
//
// The revision comes from the review rather than from the flag: a person names
// a place in the code, and the command fills in the commit they are looking at.
// A review with no revision recorded cannot anchor anything — there would be
// nothing to say the comment was written against, and an anchor without one
// could never be told apart from a current one.
func buildAnchor(st entity.State, on, side string) (*review.Anchor, error) {
	if on == "" {
		if side != "" {
			return nil, fmt.Errorf("--side says which half of a diff --on names; give both or neither")
		}
		return nil, nil
	}

	path, first, last, err := review.ParseWhere(on)
	if err != nil {
		return nil, err
	}
	head := review.Head(st)
	if head == "" {
		return nil, fmt.Errorf("this review records no revision, so there is nothing to anchor a comment against; set one with 'git review edit <id> --sync'")
	}

	switch side {
	case "", review.SideRight, review.SideLeft:
	default:
		return nil, fmt.Errorf("--side is left or right, not '%s'", side)
	}

	return &review.Anchor{
		Revision: head,
		Path:     path,
		First:    first,
		Last:     last,
		Side:     side,
	}, nil
}

func commentIntro(id string, anchor *review.Anchor) string {
	if anchor == nil {
		return "Commenting on review " + render.Abbrev(id) + "."
	}
	return "Commenting on review " + render.Abbrev(id) + ", at " + anchor.Where() + "."
}

const removeUsage = `usage: git review remove <id> [<comment>]

Unlists the review; it does not erase it. The blob stays in the notes ref's own
history, so this is recoverable locally, and a clone that has not fetched the
removal still has the review in full. It does not converge: a concurrent edit
elsewhere resurrects it on the next merge.

A second id retracts one comment inside that review instead. That one does
converge — it is a tombstone event, not a missing tree entry — and it hides the
entry's body without erasing the text or touching any reply to it.

Retracting is not resolving. A tombstone says the author withdrew what they
wrote; 'git review close <id> <comment>' says the conversation was addressed.`

func cmdRemove(args []string) error {
	fs := flag.NewFlagSet("remove", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	targets, err := parseTargets(fs, "remove", args, removeUsage, 2)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(removeUsage)
		return nil
	}
	if err != nil {
		return err
	}

	s, err := open()
	if err != nil {
		return err
	}
	id, st, err := s.Find(targets[0], review.Type)
	if err != nil {
		return err
	}

	if len(targets) == 1 {
		if err := s.Remove(id); err != nil {
			return err
		}
		fmt.Printf("%s  removed\n", render.Abbrev(id))
		return nil
	}

	who, err := author(s)
	if err != nil {
		return err
	}
	c, err := st.FindComment(targets[1])
	if err != nil {
		return err
	}
	wrote, err := review.RemoveComment(s, who, time.Now().Unix(), id, st, c)
	if err != nil {
		return err
	}
	if !wrote {
		fmt.Println("nothing changed")
		return nil
	}
	fmt.Printf("%s  retracted\n", render.Abbrev(c.ID()))
	return nil
}
