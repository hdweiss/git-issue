package main

import (
	"bufio"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/hdweiss/git-issue/internal/cli"
	"github.com/hdweiss/git-issue/internal/entity"
	"github.com/hdweiss/git-issue/internal/gitx"
	"github.com/hdweiss/git-issue/internal/origins"
	"github.com/hdweiss/git-issue/internal/remote"
	"github.com/hdweiss/git-issue/internal/render"
)

// pushUsage spells out the target syntax and what the two legs actually write
// to, which is the part of push that is not obvious: one of them talks to a git
// remote and the other to a forge's API, and they do not reach the same place.
const pushUsage = `usage: git issue push [--dry-run] [-y] [--refresh] [--token <token>] [[git:|github:|ado:|gitea:]<remote>] [<id>...]

       (no remote)          the current branch's own remote: its refs, and its
                             issues too if it is GitHub, Azure DevOps or Gitea
       origin, git:origin   send the notes ref and the origin ledger to another clone
       github:origin        write local changes back through the bridge
       github:owner/name    a repository this clone has no remote for — a fork
       ado:origin           write local changes back to Azure DevOps
       ado:origin#Web/Auth  file work items this run creates under that area
       gitea:origin         write local changes back to Gitea (or Forgejo)

       <id>...              push only these issues
       --dry-run            print what would be pushed and stop
   -y, --yes                do not ask before writing to a bridge
       --refresh            re-import what was pushed, to see it as upstream now has it
       --token <token>      use this token instead of the ones git and the platform CLIs hold

The two legs write to different places. 'push github:origin' writes to GitHub and
records what it created in the local origin ledger; 'push origin' is what shares
the notes ref and that ledger with other clones.

An area only says where a work item this push *creates* is filed. A work item
that already exists keeps the area it has, always; move it in Azure DevOps.

A bridge push asks before it writes anything, and prints the plan first. Nothing
is forced: a remote holding changes this clone has not seen is refused, and the
remedy is 'git issue pull' and then push again.`

// cmdPush writes issues outward.
//
// The target grammar is pull's, and deliberately so — the same word names the
// same place in both directions. What differs is only that one of these reads
// and the other writes, which is why this one confirms and pull does not.
func cmdPush(args []string) error {
	opts, rest, err := parsePushFlags(args)
	if errors.Is(err, flag.ErrHelp) {
		fmt.Println(pushUsage)
		return nil
	}
	if err != nil {
		return err
	}

	// A target is a word that names a remote or a scheme; anything else is an
	// issue id. Only the first positional can be a target, so an id in that
	// position is told apart by not resolving as one.
	target := ""
	if len(rest) > 0 && looksLikeTarget(rest[0]) {
		target, rest = rest[0], rest[1:]
	}
	opts.ids = rest

	s, err := open()
	if err != nil {
		return err
	}
	spec := remote.Parse(s.Repo, target)
	if target == "" {
		return pushDefault(s, spec, opts)
	}
	switch spec.Scheme {
	case remote.SchemeGit:
		return pushGit(s, spec, opts)
	case remote.SchemeGitHub, remote.SchemeADO, remote.SchemeGitea:
		return pushBridge(s, spec, opts)
	default:
		return fmt.Errorf("unknown destination '%s'", spec.Scheme)
	}
}

// looksLikeTarget distinguishes `push origin` from `push 4f2a1c9`.
//
// A scheme is decisive. Otherwise the word is a target only if it is not a
// plausible object-name prefix, so an id can never be mistaken for a remote that
// does not exist — and a remote actually named in hex is still recognised,
// because it resolves.
func looksLikeTarget(arg string) bool {
	if scheme, _, ok := strings.Cut(arg, ":"); ok {
		for _, known := range remote.Schemes {
			if scheme == known {
				return true
			}
		}
	}
	return !isHexPrefix(arg)
}

func isHexPrefix(s string) bool {
	if s == "" {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

// pushOptions are the flags shared by both legs, plus the ones only a bridge
// uses.
type pushOptions struct {
	dryRun  bool
	yes     bool
	refresh bool
	token   string
	ids     []string
}

func parsePushFlags(args []string) (pushOptions, []string, error) {
	var opts pushOptions
	fs := flag.NewFlagSet("push", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.BoolVar(&opts.dryRun, "dry-run", false, "print what would be pushed and stop")
	fs.BoolVar(&opts.yes, "yes", false, "do not ask before writing to a bridge")
	fs.BoolVar(&opts.yes, "y", false, "do not ask before writing to a bridge")
	fs.BoolVar(&opts.refresh, "refresh", false, "re-import what was pushed")
	fs.StringVar(&opts.token, "token", "", "GitHub token to use instead of the ones git and gh hold")

	rest, err := parsePermuted(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return opts, nil, err
		}
		return opts, nil, fmt.Errorf("%s\n%s", err, pushUsage)
	}
	return opts, rest, nil
}

// pushDefault is what a bare `git issue push` does, mirroring pullDefault.
//
// The git leg always runs: it is the native path and needs no credentials. A
// bridge leg runs only when the resolved remote is that bridge's, and asks
// before it writes — which is what makes the symmetry with pull safe. A plain
// git remote has no API behind it to write to, and guessing one from a URL is
// exactly the surprise an explicit scheme exists to avoid.
func pushDefault(s *entity.Store, spec remote.Spec, opts pushOptions) error {
	// At most one bridge can be the remote's, since no URL is both.
	scheme := ""
	switch {
	case spec.LooksLikeGitHub():
		scheme = remote.SchemeGitHub
	case spec.LooksLikeADO():
		scheme = remote.SchemeADO
	}

	if err := pushGitRefs(s, spec, opts); err != nil {
		// A forge remote is rarely sent a notes ref at all — that is what the
		// bridge is for — so a refusal there is not fatal while there is still
		// a bridge leg to redeem the push.
		if scheme == "" {
			return err
		}
		fmt.Fprintf(os.Stderr, "git-issue: %s\n", err)
	}
	if scheme == "" {
		return nil
	}
	fmt.Println()
	bridged := spec
	bridged.Scheme = scheme
	return pushBridge(s, bridged, opts)
}

// pushGit sends the tracker's refs to another clone.
func pushGit(s *entity.Store, spec remote.Spec, opts pushOptions) error {
	// An id narrows which issues a bridge writes. A git push sends a ref, whole;
	// accepting the argument silently would imply it had narrowed something.
	if len(opts.ids) > 0 {
		return fmt.Errorf("naming issues applies to a bridge push; a git push sends %s entire", s.FullRef())
	}
	return pushGitRefs(s, spec, opts)
}

// pushGitRefs is the git half of a push: the notes ref and the origin ledger.
//
// The ledger goes first, and the order is load-bearing. A clone that receives
// issues without their mappings will re-create every one of them upstream on its
// next bridge push, and nothing undoes that; a clone that receives mappings for
// issues it does not yet have simply looks them up and misses. So if only one of
// the two lands, it must be the ledger.
func pushGitRefs(s *entity.Store, spec remote.Spec, opts pushOptions) error {
	if !spec.IsGitRemote() {
		return fmt.Errorf("no remote named '%s'; git issue push needs a configured remote", spec.Target)
	}

	led := origins.NewStore(s.Repo)
	ledgerMoved, err := pushLedger(led, spec.Name, opts.dryRun)
	if err != nil {
		return err
	}

	if opts.dryRun {
		return dryRunGit(s, spec)
	}

	push, err := s.PushGit(spec.Name)
	if err != nil {
		return pushGitError(spec, s.FullRef(), err)
	}
	if push.UpToDate() && !ledgerMoved {
		fmt.Println("Everything up-to-date")
		return nil
	}

	// Same filter a pull applies: an entity whose blob grew without its folded
	// state moving is an echo of an earlier bridge push, not news.
	changes, err := sync.Changes(push)
	if err != nil {
		return err
	}

	fmt.Println("To " + spec.URL)
	if ledgerMoved {
		fmt.Println(refLine(led.Ref, led.Ref, "", s.Repo.RefSHA(led.Ref)))
	}
	if !push.UpToDate() {
		fmt.Println(refLine(push.Ref, push.Ref, push.Old, push.New))
	}
	if len(changes) == 0 {
		fmt.Println(sync.NothingChanged())
		return nil
	}
	render.WriteChanges(os.Stdout, changes, uniqueIDs(s))
	fmt.Println(sync.Tally(changes))
	return nil
}

// pushLedger sends the origin ledger, reporting whether it moved. A repository
// that has never talked to a bridge has no ledger, which is not a failure.
func pushLedger(led *origins.Store, remoteName string, dryRun bool) (bool, error) {
	local := led.Repo.RefSHA(led.Ref)
	if local == "" {
		return false, nil
	}

	// A dry run fetches but does not merge. Moving a remote-tracking ref changes
	// nothing about this clone's own state, so it is fair game for a comparison;
	// unioning the remote's mappings into the live ledger is a real write, and a
	// command that says it is doing nothing must do nothing.
	if dryRun {
		if err := led.Fetch(remoteName); err != nil {
			return false, err
		}
		return local != led.Repo.RefSHA(led.TrackingRef(remoteName)), nil
	}

	if err := led.Pull(remoteName); err != nil {
		return false, err
	}
	// The merge may have moved the ref, so re-read: what gets pushed is the
	// union of both sides, never one side over the other.
	local = led.Repo.RefSHA(led.Ref)
	if remoteSHA := led.Repo.RefSHA(led.TrackingRef(remoteName)); remoteSHA == local {
		return false, nil
	}
	if err := led.Repo.Push(remoteName, led.Refspec()); err != nil {
		return false, err
	}
	return true, led.Repo.UpdateRef(led.TrackingRef(remoteName), local)
}

// dryRunGit reports what a push would send without sending it.
func dryRunGit(s *entity.Store, spec remote.Spec) error {
	tracking := s.TrackingRef(spec.Name)
	if err := s.Repo.Fetch(spec.Name, s.FullRef()+":"+tracking); err != nil {
		if !strings.Contains(err.Error(), "couldn't find remote ref") {
			return pushGitError(spec, s.FullRef(), err)
		}
	}
	old, now := s.Repo.RefSHA(tracking), s.Repo.RefSHA(s.FullRef())
	if now == "" || old == now {
		fmt.Println("Everything up-to-date")
		return nil
	}
	if !s.Repo.IsAncestor(old, now) {
		return pushGitError(spec, s.FullRef(), fmt.Errorf("%s: %w", spec.Name, entity.ErrNonFastForward))
	}

	changes, err := s.Repo.DiffTree(old, now)
	if err != nil {
		return err
	}
	fmt.Println("To " + spec.URL)
	fmt.Println(refLine(s.FullRef(), s.FullRef(), old, now))
	fmt.Printf("%d issues would be pushed\n", len(changes))
	return nil
}

// refLine is git push's own ref-update line.
func refLine(src, dst, old, new string) string {
	if old == "" {
		return fmt.Sprintf(" * %-17s %s -> %s", "[new reference]", cli.PrettyRef(src), cli.PrettyRef(dst))
	}
	return fmt.Sprintf("   %-17s %s -> %s", cli.ShortSHA(old)+".."+cli.ShortSHA(new), cli.PrettyRef(src), cli.PrettyRef(dst))
}

// pushGitError explains a refused push, and points at the bridge when the
// remote is a forge.
//
// A non-fast-forward gets the remedy spelled out rather than a bare refusal:
// both refs here are grow-only unions, so pull-then-push always works and there
// is never a reason to reach for a force.
func pushGitError(spec remote.Spec, ref string, err error) error {
	if errors.Is(err, entity.ErrNonFastForward) {
		return fmt.Errorf("%s holds issues this clone has not seen; %s was not pushed\n"+
			"       run: git issue pull %s\n"+
			"       then push again — the two merge, and nothing is lost",
			spec.Target, ref, spec.Target)
	}
	if spec.LooksLikeGitHub() {
		return fmt.Errorf("%s\n       issues hosted on GitHub are written through the bridge: git issue push github:%s", err, spec.Target)
	}
	if spec.LooksLikeADO() {
		return fmt.Errorf("%s\n       issues hosted on Azure DevOps are written through the bridge: git issue push ado:%s", err, spec.Target)
	}
	return err
}

// confirm asks before an outward-facing write.
//
// Off a terminal there is nobody to ask, so an unattended run must say --yes
// rather than be assumed to mean it. Answering for the user is exactly the
// mistake this prompt exists to prevent, and a push creates things upstream that
// nothing here can take back.
func confirm(prompt string) (bool, error) {
	if !gitx.IsTerminal(os.Stdin) {
		return false, fmt.Errorf("not a terminal; re-run with --yes to push without asking")
	}
	fmt.Printf("%s [y/N] ", prompt)
	line, err := bufio.NewReader(os.Stdin).ReadString('\n')
	// The prompt is on stdout with no newline of its own; the progress bar that
	// follows a "yes" is on stderr and redraws from column zero. Close the
	// prompt line on stderr too, so the first frame does not land on top of it.
	fmt.Fprintln(os.Stderr)
	if err != nil && line == "" {
		return false, nil
	}
	answer := strings.ToLower(strings.TrimSpace(line))
	return answer == "y" || answer == "yes", nil
}
