package main

import (
	"fmt"
	"os"
	"time"

	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/origins"
	"github.com/hdweiss/git-issue/internal/render"
	"github.com/hdweiss/git-issue/internal/review"
)

// showReview prints one review in full, or one thread inside it.
//
// It folds the whole ref, because resolving a relation to a title and deriving
// the inverse ends are questions about other entities that no single blob
// answers. That is the cost of the unbuilt index, paid here as it is in
// git-issue's own show.
func showReview(targets []string) error {
	s, err := open()
	if err != nil {
		return err
	}
	id, st, err := s.Find(targets[0], review.Type)
	if err != nil {
		return err
	}

	notes, err := s.Notes()
	if err != nil {
		return err
	}
	runs := loadAllChecks(s)
	h, err := survey(s, notes, runs, nil)
	if err != nil {
		return err
	}

	repo := review.GitRepo{Repo: s.Repo}
	detail := review.Detail(id, st, time.Local, indexLinks(h).of(id), runs[review.Head(st)], repo)
	uniq := render.Uniquify(entity.IDs(notes))

	p := cli.StartPager(s)
	defer p.Finish()

	if len(targets) == 2 {
		c, err := st.FindComment(targets[1])
		if err != nil {
			return err
		}
		render.WriteComment(p, detail, c)
		return nil
	}

	render.WriteDetail(p, detail, uniq)
	writeThreadIndex(p, st, repo)
	return nil
}

// writeThreadIndex lists the review's anchored threads under the rendering, so
// a reader sees at a glance where every conversation sits and how many are
// still open, without scrolling back through the forest for it.
//
// Each thread's own entry carries the same placement (review.Detail supplies it
// to the renderer), so this is a contents page rather than the only statement of
// it. The reply counts are what it adds.
func writeThreadIndex(w *cli.Pager, st entity.State, repo review.Repo) {
	threads := review.Threads(st, repo)
	var anchored []review.Thread
	for _, t := range threads {
		if t.Anchored || t.Resolved {
			anchored = append(anchored, t)
		}
	}
	if len(anchored) == 0 {
		return
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "Threads:")
	for _, t := range anchored {
		fmt.Fprintf(w, "    %s  %s\n", render.Abbrev(t.Root.ID()), describeThread(t))
	}
}

// describeThread is the one line that says where a conversation sits and what
// has become of it.
//
// Outdated and detached are reported as the different things they are: an
// anchor's commit stays an ancestor across every ordinary push, so a client
// that collapsed them would call a thread current long after the lines under it
// were replaced.
func describeThread(t review.Thread) string {
	out := "(no anchor)"
	if t.Anchored {
		out = t.Anchor.Where()
	}
	if label := t.Currency.Label(); label != "" && t.Anchored {
		out += "  " + label
	}
	if t.Resolved {
		out += "  resolved"
	}
	if t.Replies > 0 {
		out += fmt.Sprintf("  %d %s", t.Replies, plural(t.Replies, "reply", "replies"))
	}
	return out
}

func plural(n int, one, many string) string {
	if n == 1 {
		return one
	}
	return many
}

// openReviewWeb sends the reader to the review's page on the forge it came
// from. The address is on the origin ledger, so a review that has never touched
// a bridge has none — which is the ordinary state of a local review rather than
// a failure.
func openReviewWeb(targets []string) error {
	if len(targets) == 2 {
		return fmt.Errorf("show --web opens a review; a comment has no page of its own\n%s", showUsage)
	}
	s, err := open()
	if err != nil {
		return err
	}
	id, _, err := s.Find(targets[0], review.Type)
	if err != nil {
		return err
	}
	url, err := reviewWebURL(s, id)
	if err != nil {
		return err
	}
	if url == "" {
		return fmt.Errorf("no upstream page for this review: it has not been pulled from or pushed to a bridge")
	}
	fmt.Fprintf(os.Stderr, "Opening %s\n", url)
	return s.Repo.WebBrowse(url)
}

func reviewWebURL(s *entity.Store, id string) (string, error) {
	led := origins.NewStore(s.Repo)
	trackers, err := led.Trackers()
	if err != nil {
		return "", err
	}
	for _, tracker := range trackers {
		l, err := led.Load(tracker)
		if err != nil {
			return "", err
		}
		if url, ok := l.URL(id); ok {
			return url, nil
		}
	}
	return "", nil
}
