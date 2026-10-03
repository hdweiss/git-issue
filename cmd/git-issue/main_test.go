package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/hdweiss/git-issue/internal/origins"
)

// The timezone the goldens in testdata/golden were captured under. Dates are
// rendered in local time, so parity is only meaningful against a fixed zone.
const goldenTZ = "Europe/Berlin"

// build compiles the command once per test binary and returns its path.
func build(t *testing.T) string {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "git-issue")
	out, err := exec.Command("go", "build", "-o", bin, ".").CombinedOutput()
	if err != nil {
		t.Fatalf("build failed: %v\n%s", err, out)
	}
	return bin
}

// fixtureRepo creates a repo holding the note blobs in testdata/fixture. Their
// filenames are the entity ids they are filed under, and every nonce and
// timestamp in them is fixed, so the folded output is reproducible.
func fixtureRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	run := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
		}
	}
	run("init", "-q")
	run("config", "user.email", "hdweiss@gmail.com")
	run("config", "user.name", "Henning Weiss")

	blobs, err := filepath.Glob("../../testdata/fixture/*")
	if err != nil || len(blobs) == 0 {
		t.Fatalf("no fixtures: %v", err)
	}
	for _, path := range blobs {
		id := filepath.Base(path)
		if id == "index.json" {
			continue
		}
		abs, err := filepath.Abs(path)
		if err != nil {
			t.Fatal(err)
		}
		run("notes", "--ref=issues/open", "add", "-F", abs, id)
	}
	return dir
}

type result struct {
	stdout, stderr string
	code           int
}

func gitIssue(t *testing.T, bin, dir string, args ...string) result {
	t.Helper()
	return gitIssueEnv(t, bin, dir, nil, args...)
}

func gitIssueEnv(t *testing.T, bin, dir string, env []string, args ...string) result {
	t.Helper()
	return gitIssueIO(t, bin, dir, env, nil, args...)
}

// gitIssueIO is gitIssueEnv with stdin wired to r, for the -F - / piped-input
// paths. A nil r leaves stdin as os/exec's default (an empty pipe).
func gitIssueIO(t *testing.T, bin, dir string, env []string, r io.Reader, args ...string) result {
	t.Helper()
	cmd := exec.Command(bin, args...)
	cmd.Dir = dir
	cmd.Env = append(append(os.Environ(), "TZ="+goldenTZ), env...)
	cmd.Stdin = r

	var out, errb strings.Builder
	cmd.Stdout, cmd.Stderr = &out, &errb
	err := cmd.Run()

	code := 0
	var exit *exec.ExitError
	if err != nil {
		if !asExitError(err, &exit) {
			t.Fatalf("run %v: %v", args, err)
		}
		code = exit.ExitCode()
	}
	return result{stdout: out.String(), stderr: errb.String(), code: code}
}

func asExitError(err error, target **exec.ExitError) bool {
	e, ok := err.(*exec.ExitError)
	if ok {
		*target = e
	}
	return ok
}

// noEditor is an environment with no editor chosen. An empty value counts as
// unset here exactly as it does in git, so this leaves a command that needs an
// editor with nothing to open — which is the deterministic case to assert on,
// since a test process has no terminal to fall back to either.
// The config files are neutralised alongside them, since core.editor in
// whatever ~/.gitconfig the test host happens to have would count as a choice.
var noEditor = []string{
	"GIT_EDITOR=", "VISUAL=", "EDITOR=",
	"GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null",
}

func golden(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("../../testdata/golden", name))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// The goldens were captured from the bash/python prototype this replaced.
// Matching them byte for byte is what let the prototype be deleted. With no
// id, `git issue` lists — there is no separate word for it.
func TestListMatchesGolden(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	got := gitIssue(t, bin, dir)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if want := golden(t, "list.txt"); got.stdout != want {
		t.Errorf("list output differs\n--- want ---\n%s\n--- got ---\n%s", want, got.stdout)
	}
}

// --format medium is the same listing rendered as git log renders commits: the
// header block show prints, then the title, and no body.
func TestListMediumMatchesGolden(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	for _, args := range [][]string{{"--format", "medium"}, {"--format=medium"}} {
		got := gitIssue(t, bin, dir, args...)
		if got.code != 0 {
			t.Fatalf("%v: exit %d: %s", args, got.code, got.stderr)
		}
		if want := golden(t, "list-medium.txt"); got.stdout != want {
			t.Errorf("%v differs\n--- want ---\n%s\n--- got ---\n%s", args, want, got.stdout)
		}
	}
}

// --oneline is the default, and saying it twice over is not an error; naming
// two different formats is.
func TestListFormatSelection(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	want := golden(t, "list.txt")
	for _, args := range [][]string{{"--oneline"}, {"--format", "oneline"},
		{"--oneline", "--format", "oneline"}} {
		got := gitIssue(t, bin, dir, args...)
		if got.code != 0 || got.stdout != want {
			t.Errorf("%v: exit %d, stdout %q", args, got.code, got.stdout)
		}
	}
	for _, args := range [][]string{{"--oneline", "--format", "medium"}, {"--format", "bogus"}} {
		if got := gitIssue(t, bin, dir, args...); got.code == 0 {
			t.Errorf("%v was accepted: %q", args, got.stdout)
		}
	}
	// The flag-parse form of --help, reached mid-args rather than as the whole
	// invocation, prints the same usage as the top-level `help` word does.
	if got := gitIssue(t, bin, dir, "--oneline", "--help"); got.code != 0 || !strings.HasPrefix(got.stdout, "usage:") {
		t.Errorf("--oneline --help: exit %d, stdout %q", got.code, got.stdout)
	}
}

// `git issue list` and `git issue show <id>` are the explicit spellings of
// what the bare and one-id shortcuts already do.
func TestExplicitListAndShow(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	if got := gitIssue(t, bin, dir, "list"); got.code != 0 || got.stdout != golden(t, "list.txt") {
		t.Errorf("list: exit %d, stdout %q", got.code, got.stdout)
	}
	// An id after `list` scopes the listing to what is filed under it — the
	// same slot `add <id>` uses — rather than showing that issue. Nothing in
	// the fixture is filed under this one, so the listing is empty.
	if got := gitIssue(t, bin, dir, "list", "e9037839d7f8"); got.code != 0 || got.stdout != "" {
		t.Errorf("list under an issue with no children: exit %d, stdout %q", got.code, got.stdout)
	}
	// It is still an id, so a prefix that names nothing is refused rather than
	// listing everything or nothing.
	if got := gitIssue(t, bin, dir, "list", "deadbeef"); got.code == 0 {
		t.Errorf("list under an unknown id was accepted: %q", got.stdout)
	}

	id := "e9037839d7f8"
	direct := gitIssue(t, bin, dir, id)
	explicit := gitIssue(t, bin, dir, "show", id)
	if explicit.code != 0 || explicit.stdout != direct.stdout {
		t.Errorf("show %s differs from the bare id\n--- bare ---\n%s\n--- show ---\n%s", id, direct.stdout, explicit.stdout)
	}
	// show takes no format — that's what list is for.
	if got := gitIssue(t, bin, dir, "show", id, "--oneline"); got.code == 0 {
		t.Errorf("show --oneline was accepted: %q", got.stdout)
	}
}

// The list filters — mirrored from add's flags — narrow the listing, combine
// with AND, and fold case. They work on the `git issue` shortcut too.
func TestListFilters(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	const (
		rich   = "885797fb2d9b" // type bug, labels bug+design, assignees hdweiss+rev, open
		closed = "0bcd4859d4fe" // no type, closed
		thread = "45cf71414905"
		plain  = "e9037839d7f8"
		degen  = "f55f8e0bfdc1"
	)
	all := []string{closed, thread, rich, plain, degen}

	for _, tc := range []struct {
		args []string
		want []string
	}{
		{[]string{"--type", "bug"}, []string{rich}},
		{[]string{"--type", "BUG"}, []string{rich}},
		{[]string{"--type", "feature"}, nil},
		{[]string{"--label", "design"}, []string{rich}},
		{[]string{"--label", "Design"}, []string{rich}},
		{[]string{"--label", "bug", "--label", "design"}, []string{rich}},
		{[]string{"--label", "bug,design"}, []string{rich}}, // comma-split, like add
		{[]string{"--label", "bug", "--label", "missing"}, nil},
		{[]string{"-a", "rev@example.com"}, []string{rich}},
		{[]string{"--assignee", "REV@EXAMPLE.COM"}, []string{rich}},
		{[]string{"--label", "none"}, []string{closed, thread, plain, degen}},
		{[]string{"--label", "None"}, []string{closed, thread, plain, degen}},
		{[]string{"--assignee", "none"}, []string{closed, thread, plain, degen}},
		{[]string{"--label", "none", "--state", "closed"}, []string{closed}},
		{[]string{"--label", "none", "--assignee", "none"}, []string{closed, thread, plain, degen}},
		{[]string{"--state", "closed"}, []string{closed}},
		{[]string{"--state", "open"}, []string{thread, rich, plain, degen}},
		{[]string{"--state", "all"}, all},
		{[]string{"--author", "hdweiss"}, all},
		{[]string{"--author", "NOBODY"}, nil},
		{[]string{"--type", "bug", "--state", "open"}, []string{rich}},
		{[]string{"--type", "bug", "--state", "closed"}, nil},
	} {
		for _, prefix := range [][]string{{"list"}, nil} { // `list` and the bare shortcut
			args := append(append([]string{}, prefix...), tc.args...)
			got := gitIssue(t, bin, dir, args...)
			if got.code != 0 {
				t.Errorf("%v: exit %d: %s", args, got.code, got.stderr)
				continue
			}
			if ids := listedIDs(got.stdout); !equalIDs(ids, tc.want) {
				t.Errorf("%v listed %v, want %v", args, ids, tc.want)
			}
		}
	}

	// --state takes a tracker's own state as well as open and closed, because
	// a bridge passes its platform's spelling through rather than coercing it
	// and that vocabulary is open. So an unrecognised state is not an error:
	// it matches nothing, exactly as an unrecognised --type or --label does.
	if got := gitIssue(t, bin, dir, "list", "--state", "bogus"); got.code != 0 || strings.TrimSpace(got.stdout) != "" {
		t.Errorf("--state bogus: exit %d, stdout %q", got.code, got.stdout)
	}
	// 'closed' still means terminal rather than the literal word, and folds
	// case so that Azure DevOps' Agile "Closed" answers to it too.
	if got := gitIssue(t, bin, dir, "list", "--state", "CLOSED"); got.code != 0 {
		t.Errorf("--state CLOSED: exit %d, stderr %q", got.code, got.stderr)
	}
	// 'none' is a whole-list sentinel: it cannot sit next to a real name.
	if got := gitIssue(t, bin, dir, "list", "--label", "none", "--label", "bug"); got.code == 0 || !strings.Contains(got.stderr, "not both") {
		t.Errorf("--label none + name: exit %d, stderr %q", got.code, got.stderr)
	}
	// medium format filters the same way.
	if got := gitIssue(t, bin, dir, "list", "--format", "medium", "--type", "bug"); got.code != 0 ||
		!strings.Contains(got.stdout, "issue "+rich) || strings.Contains(got.stdout, "issue "+plain) {
		t.Errorf("medium --type bug: exit %d, stdout %q", got.code, got.stdout)
	}
	// Filters describe a listing, so they cannot ride along with `git issue <id>`.
	if got := gitIssue(t, bin, dir, plain, "--type", "bug"); got.code == 0 {
		t.Errorf("git issue <id> --type bug was accepted: %q", got.stdout)
	}
}

func listedIDs(out string) []string {
	var ids []string
	for _, line := range strings.Split(strings.TrimRight(out, "\n"), "\n") {
		if line != "" {
			ids = append(ids, strings.Fields(line)[0])
		}
	}
	return ids
}

func equalIDs(got, want []string) bool {
	g, w := append([]string{}, got...), append([]string{}, want...)
	sort.Strings(g)
	sort.Strings(w)
	if len(g) != len(w) {
		return false
	}
	for i := range g {
		if g[i] != w[i] {
			return false
		}
	}
	return true
}

// The top-level help is the command overview: what list/show/edit/remove/
// pull/add do, and the two shortcuts, with none of the per-command flag
// detail — that lives under each command's own --help.
func TestTopLevelHelp(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	for _, args := range [][]string{{"help"}, {"--help"}, {"-h"}} {
		got := gitIssue(t, bin, dir, args...)
		if got.code != 0 || got.stderr != "" {
			t.Errorf("%v: exit %d, stderr %q", args, got.code, got.stderr)
		}
		for _, want := range []string{"add", "list", "show", "comment", "edit", "remove", "pull", "git issue <id>"} {
			if !strings.Contains(got.stdout, want) {
				t.Errorf("%v: missing %q\n%s", args, want, got.stdout)
			}
		}
		for _, absent := range []string{"--oneline", "--format", "--edit", "version"} {
			if strings.Contains(got.stdout, absent) {
				t.Errorf("%v: top-level help should not detail flags, found %q\n%s", args, absent, got.stdout)
			}
		}
	}
}

// list, show, comment, edit and remove each answer their own --help with the
// flag detail the top-level help omits.
func TestSubcommandHelp(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	for _, tc := range []struct {
		args []string
		want string
	}{
		{[]string{"list", "--help"}, "--oneline"},
		{[]string{"show", "--help"}, "git issue edit <id>"},
		{[]string{"comment", "--help"}, "-m, --message"},
		{[]string{"edit", "--help"}, "one event per field"},
		{[]string{"remove", "--help"}, "Unlists"},
	} {
		got := gitIssue(t, bin, dir, tc.args...)
		if got.code != 0 || !strings.HasPrefix(got.stdout, "usage:") {
			t.Errorf("%v: exit %d, stdout %q", tc.args, got.code, got.stdout)
		}
		if !strings.Contains(got.stdout, tc.want) {
			t.Errorf("%v: missing %q\n%s", tc.args, tc.want, got.stdout)
		}
	}
}

// version has no word of its own, only the flag: "version" is just an
// unknown issue id now, and --version still works.
func TestVersionHasNoWord(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	if got := gitIssue(t, bin, dir, "version"); got.code == 0 {
		t.Errorf("bare 'version' was accepted as a command: %q", got.stdout)
	}
	if got := gitIssue(t, bin, dir, "--version"); got.code != 0 || !strings.HasPrefix(got.stdout, "git-issue ") {
		t.Errorf("--version: exit %d, stdout %q", got.code, got.stdout)
	}
}

// One fixture per branch of the fold: scalars and both list fields, a terminal
// status with a reason, a nested comment forest with an edit, a tombstone with
// a surviving reply, a dangling parent, entity- and comment-level reactions,
// and a blob of things a reader must survive rather than understand.
func TestShowMatchesGolden(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	files, err := filepath.Glob("../../testdata/golden/show-*.txt")
	if err != nil || len(files) == 0 {
		t.Fatalf("no show goldens: %v", err)
	}
	for _, f := range files {
		name := filepath.Base(f)
		id := strings.TrimSuffix(strings.TrimPrefix(name, "show-"), ".txt")
		t.Run(id, func(t *testing.T) {
			got := gitIssue(t, bin, dir, id)
			if got.code != 0 {
				t.Fatalf("exit %d: %s", got.code, got.stderr)
			}
			if want := golden(t, name); got.stdout != want {
				t.Errorf("differs\n--- want ---\n%s\n--- got ---\n%s", want, got.stdout)
			}
		})
	}
}

// errors.txt records the prototype's exact usage strings and exit codes.
func TestErrorsMatchGolden(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	var b strings.Builder
	for _, args := range [][]string{
		{"deadbeef"}, {"bogus"}, {"remove", "nosuch"}, {"add"},
		{"pull", "nosuchremote"}, {"--format", "detailed"}, {"--edit"},
	} {
		b.WriteString("### git issue " + strings.Join(args, " ") + " \n")
		// noEditor, because a bare `add` opens one otherwise, and whether the
		// machine running the tests has EDITOR set is not what is under test.
		got := gitIssueEnv(t, bin, dir, noEditor, args...)
		b.WriteString("rc=" + strconv.Itoa(got.code) + "\n" + strings.TrimRight(got.stderr, "\n") + "\n")
	}

	// The capture script printed a trailing space after the subcommand list;
	// normalise both sides rather than encode that quirk in the command.
	norm := func(s string) string {
		var out []string
		for _, line := range strings.Split(s, "\n") {
			out = append(out, strings.TrimRight(line, " "))
		}
		return strings.Join(out, "\n")
	}
	if want, got := norm(golden(t, "errors.txt")), norm(b.String()); want != got {
		t.Errorf("differs\n--- want ---\n%s\n--- got ---\n%s", want, got)
	}
}

// A second id shows one entry of the thread and the replies under it. The
// entry is rendered exactly as the whole issue rendered it — same headers,
// same reaction line, same nesting — only starting at column zero, so what a
// reader sees zoomed in is what they saw in place.
func TestShowComment(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	const thread = "comment 98df1a846e6fa1e1ed22741d9e85402a6482e6c0\n" +
		"Author: rev@example.com\n" +
		"Date:   Mon Aug 17 22:58:20 2026 +0200\n" +
		"\n" +
		"    Confirmed: absent in a fresh clone — and after gc.\n" +
		"\n" +
		"    Reactions: heart (1)\n" +
		"\n" +
		"    comment 4a15086c24f6656d47d4b04deb88ee5216ec2ab4\n" +
		"    Author: hdweiss@gmail.com\n" +
		"    Date:   Mon Aug 17 22:59:20 2026 +0200\n" +
		"\n" +
		"        Only after gc, though.\n" +
		"\n" +
		"        comment e24599f9125b9285edf627c007832681cf3670f2\n" +
		"        Author: rev@example.com\n" +
		"        Date:   Mon Aug 17 23:00:20 2026 +0200\n" +
		"\n" +
		"            Depth is a display decision.\n"

	// A retracted entry keeps its reply: the tombstone hides one body, and
	// nothing below it.
	const retracted = "comment 6d2dcb8e5f4872dd82aac57f91218e03e08fe1c0\n" +
		"Author: spam@example.com\n" +
		"Date:   Mon Aug 17 23:00:40 2026 +0200\n" +
		"\n" +
		"    (comment retracted)\n" +
		"\n" +
		"    comment f9e1fa4b04e8aa20c39e3568204c29432856e07f\n" +
		"    Author: hdweiss@gmail.com\n" +
		"    Date:   Mon Aug 17 23:00:50 2026 +0200\n" +
		"\n" +
		"        Reply beneath a retracted parent.\n"

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		// Both spellings, since `git issue <id>` is show's shorthand and the
		// shorthand reaching a comment is the whole point of it being one.
		{"show", []string{"show", "45cf71414905", "98df1a846e6f"}, thread},
		{"shorthand", []string{"45cf71414905", "98df1a846e6f"}, thread},
		// The prefix competes only with this issue's own comments, so four
		// characters name one.
		{"abbreviated", []string{"show", "45cf", "98df"}, thread},
		{"retracted", []string{"show", "45cf", "6d2d"}, retracted},
		// A leaf is the same rendering with nothing under it.
		{"leaf", []string{"show", "45cf", "e245"}, "comment e24599f9125b9285edf627c007832681cf3670f2\n" +
			"Author: rev@example.com\n" +
			"Date:   Mon Aug 17 23:00:20 2026 +0200\n" +
			"\n" +
			"    Depth is a display decision.\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := gitIssue(t, bin, dir, tc.args...)
			if got.code != 0 {
				t.Fatalf("exit %d: %s", got.code, got.stderr)
			}
			if got.stdout != tc.want {
				t.Errorf("differs\n--- want ---\n%s\n--- got ---\n%s", tc.want, got.stdout)
			}
		})
	}
}

// A comment prefix is resolved exactly as edit and remove resolve it, and a
// prefix that names nothing is refused rather than answered with the whole
// issue instead.
func TestShowCommentRefusals(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{"unknown", []string{"show", "45cf", "ffffff"}, "unknown comment"},
		{"empty", []string{"show", "45cf", ""}, "no comment given"},
		{"third id", []string{"show", "45cf", "98df", "4a15"}, "takes an issue and at most one comment"},
		// A comment of another issue is not in this thread's scope.
		{"other issue", []string{"show", "885797fb2d9b", "98df"}, "unknown comment"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := gitIssue(t, bin, dir, tc.args...)
			if got.code == 0 {
				t.Fatalf("accepted %v:\n%s", tc.args, got.stdout)
			}
			if !strings.Contains(got.stderr, tc.want) {
				t.Errorf("stderr %q does not mention %q", got.stderr, tc.want)
			}
		})
	}
}

// An abbreviated id that matches more than one entity must be refused, not
// resolved arbitrarily.
func TestAmbiguousPrefix(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)
	if got := gitIssue(t, bin, dir, ""); got.code == 0 {
		t.Fatal("bare invocation with an empty id succeeded")
	}
	if got := gitIssue(t, bin, dir, "remove", ""); got.code == 0 {
		t.Fatal("remove with an empty id succeeded")
	}

	// Two fixtures share no one-character prefix, so add issues until some
	// prefix collides, then assert the message.
	for i := 0; i < 20; i++ {
		if r := gitIssue(t, bin, dir, "add", "-t", "collide "+string(rune('a'+i))); r.code != 0 {
			t.Fatalf("add failed: %s", r.stderr)
		}
		for _, hex := range "0123456789abcdef" {
			r := gitIssue(t, bin, dir, string(hex))
			if strings.Contains(r.stderr, "ambiguous issue") {
				if !strings.Contains(r.stderr, "matches ") || r.code != 1 {
					t.Errorf("unexpected ambiguity report: %q (exit %d)", r.stderr, r.code)
				}
				return
			}
		}
	}
	t.Skip("no ambiguous prefix arose; ids are random")
}

// A round trip through add proves the write path: the id printed must be the
// hash of the create event actually stored, which is what makes entity
// identity self-verifying.
func TestAddIsSelfVerifying(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	added := gitIssue(t, bin, dir, "add", "-t", `Reject "short" ids <&> — ünïcode ✓`, "-d", "line one\nline two")
	if added.code != 0 {
		t.Fatalf("add failed: %s", added.stderr)
	}
	id := strings.TrimSpace(added.stdout)
	if len(id) != 12 {
		t.Fatalf("add printed %q, want a 12-character abbreviation", id)
	}

	// Re-hash the create line straight out of the note, with stock git.
	show := exec.Command("git", "notes", "--ref=issues/open", "show", fullID(t, dir, id))
	show.Dir = dir
	body, err := show.Output()
	if err != nil {
		t.Fatal(err)
	}
	createLine := strings.SplitN(string(body), "\n", 2)[0]

	hash := exec.Command("git", "hash-object", "--stdin")
	hash.Dir = dir
	hash.Stdin = strings.NewReader(createLine + "\n")
	sum, err := hash.Output()
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.TrimSpace(string(sum)); !strings.HasPrefix(got, id) {
		t.Errorf("entity id %s is not the hash of its own create line (%s)", id, got)
	}

	// And it must be readable back.
	shown := gitIssue(t, bin, dir, id)
	if shown.code != 0 {
		t.Fatalf("show failed: %s", shown.stderr)
	}
	for _, want := range []string{
		`    Reject "short" ids <&> — ünïcode ✓`, "    line one", "    line two",
	} {
		if !strings.Contains(shown.stdout, want) {
			t.Errorf("show output missing %q\n%s", want, shown.stdout)
		}
	}
}

func fullID(t *testing.T, dir, prefix string) string {
	t.Helper()
	cmd := exec.Command("git", "notes", "--ref=issues/open", "list")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(out), "\n") {
		_, entity, ok := strings.Cut(line, " ")
		if ok && strings.HasPrefix(entity, prefix) {
			return entity
		}
	}
	t.Fatalf("no entity with prefix %s", prefix)
	return ""
}

// remove unlists an entity without erasing it: the blob survives in the notes
// ref's own history.
func TestRemoveUnlistsButKeepsHistory(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	before := gitIssue(t, bin, dir)
	removed := gitIssue(t, bin, dir, "remove", "e9037839d7f8")
	if removed.code != 0 {
		t.Fatalf("remove failed: %s", removed.stderr)
	}
	if want := "removed e9037839d7f8  open  Anchor blobs are pruned by gc\n"; removed.stdout != want {
		t.Errorf("remove printed %q, want %q", removed.stdout, want)
	}

	after := gitIssue(t, bin, dir)
	if strings.Contains(after.stdout, "e9037839d7f8") {
		t.Error("removed issue still listed")
	}
	if len(strings.Split(after.stdout, "\n")) != len(strings.Split(before.stdout, "\n"))-1 {
		t.Error("remove changed more than one row")
	}

	log := exec.Command("git", "log", "--oneline", "refs/notes/issues/open")
	log.Dir = dir
	out, err := log.Output()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "Notes removed") {
		t.Error("removal is not recorded in the notes ref history")
	}
}

// destroy is the one command that deletes rather than appends: it removes both
// issue notes refs outright, refuses without --yes, and is kept out of the
// command list.
func TestDestroy(t *testing.T) {
	bin, dir := build(t), fixtureRepo(t)

	// It is hidden: help does not mention it.
	if help := gitIssue(t, bin, dir, "help"); strings.Contains(help.stdout, "destroy") {
		t.Errorf("destroy is listed in help:\n%s", help.stdout)
	}

	// Without --yes it names the refs it would touch and changes nothing.
	refused := gitIssue(t, bin, dir, "destroy")
	if refused.code == 0 {
		t.Fatal("destroy without --yes was accepted")
	}
	if !strings.Contains(refused.stderr, "--yes") || !strings.Contains(refused.stderr, "refs/notes/issues/open") {
		t.Errorf("destroy refusal unhelpful: %q", refused.stderr)
	}
	if gitIssue(t, bin, dir).stdout == "" {
		t.Fatal("destroy without --yes still emptied the tracker")
	}

	done := gitIssue(t, bin, dir, "destroy", "--yes")
	if done.code != 0 {
		t.Fatalf("destroy --yes failed: %s", done.stderr)
	}
	if !strings.Contains(done.stdout, "destroyed refs/notes/issues/open") {
		t.Errorf("destroy did not report the ref it removed:\n%s", done.stdout)
	}

	if refs := gitIn(t, dir, "for-each-ref", "refs/notes/issues"); strings.TrimSpace(refs) != "" {
		t.Errorf("issue notes refs survived destroy:\n%s", refs)
	}
	if listed := gitIssue(t, bin, dir); listed.code != 0 || listed.stdout != "" {
		t.Errorf("tracker not empty after destroy: exit %d, stdout %q", listed.code, listed.stdout)
	}

	// A second run has nothing to delete and says so, without failing.
	again := gitIssue(t, bin, dir, "destroy", "--yes")
	if again.code != 0 || !strings.Contains(again.stdout, "nothing to destroy") {
		t.Errorf("repeat destroy: exit %d, stdout %q", again.code, again.stdout)
	}
}

// An empty tracker is not an error: the notes ref simply does not exist yet.
func TestEmptyRepo(t *testing.T) {
	bin := build(t)
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"config", "user.email", "x@y.z"}} {
		cmd := exec.Command("git", args...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, out)
		}
	}
	got := gitIssue(t, bin, dir)
	if got.code != 0 || got.stdout != "" {
		t.Errorf("empty repo: exit %d, stdout %q, stderr %q", got.code, got.stdout, got.stderr)
	}
}

// gitIn runs git in a directory, failing the test on error.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %s: %v\n%s", strings.Join(args, " "), err, out)
	}
	return string(out)
}

// clonePair returns a bare origin and two clones of it, each configured to
// write as a different person. Sync is only meaningful between real
// repositories, so the pull tests use real ones rather than a fake transport.
func clonePair(t *testing.T) (alice, bob string) {
	t.Helper()
	root := t.TempDir()
	up := filepath.Join(root, "up")
	gitIn(t, root, "init", "-q", "--bare", up)

	for who, dir := range map[string]*string{"alice": &alice, "bob": &bob} {
		*dir = filepath.Join(root, who)
		gitIn(t, root, "clone", "-q", up, *dir)
		gitIn(t, *dir, "config", "user.email", who+"@example.com")
		gitIn(t, *dir, "config", "user.name", who)
	}
	return alice, bob
}

// The whole git-mode pull: fetch the remote's copy of the notes ref, union it
// into the local one, and report what moved.
func TestPullGit(t *testing.T) {
	bin := build(t)
	alice, bob := clonePair(t)

	first := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "First issue", "-d", "body").stdout)
	second := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "Second issue").stdout)
	gitIn(t, alice, "push", "-q", "origin", "refs/notes/issues/open")

	got := gitIssue(t, bin, bob, "pull", "origin")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{
		// The report is shaped like `git pull`: a fetch ref-update line under
		// the "From" header, then the changed entities, then a git-style tally.
		"* [new ref]         refs/notes/issues/open -> origin/notes/issues/open",
		" A " + first,
		" A " + second,
		"2 issues changed, 2 added(+)",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("pull output missing %q\n%s", want, got.stdout)
		}
	}
	if listed := gitIssue(t, bin, bob).stdout; !strings.Contains(listed, first) {
		t.Errorf("pulled issue is not listed\n%s", listed)
	}

	// The remote-tracking ref belongs under refs/remotes/, so that a wildcard
	// push of refs/notes/* cannot publish it.
	if sha := gitIn(t, bob, "rev-parse", "refs/remotes/origin/notes/issues/open"); strings.TrimSpace(sha) == "" {
		t.Error("no remote-tracking ref after pull")
	}

	// A second pull moves nothing, and says so the way git does: "Already up
	// to date." alone, with no "From" header above it.
	again := gitIssue(t, bin, bob, "pull")
	if again.code != 0 || strings.TrimSpace(again.stdout) != "Already up to date." {
		t.Errorf("second pull: exit %d, stdout %q", again.code, again.stdout)
	}
}

// An update and a removal upstream are reported as M and D. A removed entity
// is folded from the ref as it stood before the pull, so it still renders with
// its title instead of degrading to a bare id.
func TestPullGitReportsUpdatesAndRemovals(t *testing.T) {
	bin := build(t)
	alice, bob := clonePair(t)

	kept := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "Kept issue").stdout)
	doomed := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "Doomed issue").stdout)
	gitIn(t, alice, "push", "-q", "origin", "refs/notes/issues/open")
	if r := gitIssue(t, bin, bob, "pull", "origin"); r.code != 0 {
		t.Fatalf("initial pull: %s", r.stderr)
	}

	// Close one and remove the other, so the next pull reports an M and a D.
	if r := gitIssue(t, bin, alice, "close", kept); r.code != 0 {
		t.Fatalf("close: %s", r.stderr)
	}
	gitIn(t, alice, "notes", "--ref=issues/open", "remove", fullID(t, alice, doomed))
	gitIn(t, alice, "push", "-q", "--force", "origin", "refs/notes/issues/open")

	got := gitIssue(t, bin, bob, "pull", "origin")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{
		// The second pull moves an existing ref, so the fetch line names both
		// ends and the merge reports as a fast-forward.
		"  refs/notes/issues/open -> origin/notes/issues/open",
		"Fast-forward",
		" M " + kept + "  closed  Kept issue",
		" D " + doomed + "  open    Doomed issue",
		"2 issues changed, 1 updated, 1 removed(-)",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("pull output missing %q\n%s", want, got.stdout)
		}
	}
}

// When both sides committed since the common point, the notes merge lands on a
// two-parent commit rather than fast-forwarding, and the report says so in
// git's words instead of printing "Updating x..y / Fast-forward".
func TestPullGitDivergentMergeLine(t *testing.T) {
	bin := build(t)
	alice, bob := clonePair(t)

	shared := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "Shared issue").stdout)
	gitIn(t, alice, "push", "-q", "origin", "refs/notes/issues/open")
	if r := gitIssue(t, bin, bob, "pull", "origin"); r.code != 0 {
		t.Fatalf("initial pull: %s", r.stderr)
	}

	// A label event on each side, of the same issue: neither ref is an
	// ancestor of the other by the time bob pulls.
	label := func(dir, who, nonce, val string) {
		t.Helper()
		ev := `{"a":"` + who + `@example.com","c":1,"n":"` + nonce + `","op":"label","ts":1756200000,"v":1,"val":"` + val + `"}`
		cmd := exec.Command("git", "notes", "--ref=issues/open", "append", "-F", "-", fullID(t, dir, shared))
		cmd.Dir, cmd.Stdin = dir, strings.NewReader(ev+"\n")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("append: %v\n%s", err, out)
		}
	}
	label(alice, "alice", "aaaaaaaaaaaaaaaa", "from-alice")
	gitIn(t, alice, "push", "-q", "--force", "origin", "refs/notes/issues/open")
	label(bob, "bob", "bbbbbbbbbbbbbbbb", "from-bob")

	got := gitIssue(t, bin, bob, "pull", "origin")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "Merge made by the 'cat_sort_uniq' strategy.") {
		t.Errorf("divergent pull did not report a merge:\n%s", got.stdout)
	}
	if strings.Contains(got.stdout, "Fast-forward") {
		t.Errorf("divergent pull reported a fast-forward:\n%s", got.stdout)
	}
}

// A remote with no notes ref is not a crash, and when it is a forge the error
// names the bridge that would work instead.
func TestPullGitWithoutRemoteRef(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)

	got := gitIssue(t, bin, bob, "pull", "origin")
	if got.code == 0 {
		t.Fatalf("pull from an empty remote succeeded: %q", got.stdout)
	}
	if !strings.Contains(got.stderr, "has no refs/notes/issues/open") {
		t.Errorf("unexpected error: %q", got.stderr)
	}
	if strings.Contains(got.stderr, "github") {
		t.Errorf("bridge hint offered for a non-forge remote: %q", got.stderr)
	}

	gitIn(t, bob, "remote", "set-url", "origin", "https://github.com/hdweiss/git-issue.git")
	got = gitIssue(t, bin, bob, "pull", "origin")
	if !strings.Contains(got.stderr, "git issue pull github:origin") {
		t.Errorf("no bridge hint for a github remote: %q", got.stderr)
	}
}

// The whole bridge path from the command line: resolve the target, fetch,
// map, union into the notes ref, report.
//
// The API is a local server replaying the same recorded response the mapping
// tests use, reached through an http target — an Enterprise install on plain
// http takes the identical path, so nothing test-only is wired in to make this
// work.
func TestPullGitHub(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)

	body, err := os.ReadFile("../../testdata/github/issues.json")
	if err != nil {
		t.Fatal(err)
	}
	requests := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if got := r.Header.Get("Authorization"); got != "bearer secret" {
			t.Errorf("Authorization = %q", got)
		}
		w.Write(body)
	}))
	defer srv.Close()
	target := "github:" + srv.URL + "/hdweiss/git-issue"

	got := gitIssueEnv(t, bin, bob, []string{"GITHUB_TOKEN=secret"}, "pull", target)
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	for _, want := range []string{
		"Notes merge reorders lines permanently",
		"Anchor blobs are pruned by gc",
		"3 issues changed, 3 added(+)",
	} {
		if !strings.Contains(got.stdout, want) {
			t.Errorf("pull output missing %q\n%s", want, got.stdout)
		}
	}
	// A bridge import fetches no ref, so it has no git fetch ref-update line.
	if strings.Contains(got.stdout, "->") {
		t.Errorf("bridge pull printed a fetch ref-update line:\n%s", got.stdout)
	}
	// The fetch progress is git-shaped: a percentage against GitHub's match
	// count, closed with ", done.".
	if !strings.Contains(got.stderr, "Reading issues: 100% (3/3), done.") {
		t.Errorf("progress line not git-shaped:\n%s", got.stderr)
	}

	// One commit per upstream action, as docs/storage-model.md asks: the
	// issue being filed, each comment, each timeline entry. Not one per run,
	// which would collapse everyone's changes onto whoever ran the import.
	log := gitIn(t, bob, "log", "--oneline", "refs/notes/issues/open")
	lines := strings.Split(strings.TrimSpace(log), "\n")
	if len(lines) != 14 {
		t.Errorf("import wrote %d commits, want 14:\n%s", len(lines), log)
	}
	for _, want := range []string{
		`Create issue "merge reorders stuff"`,
		`Add label "needs-triage" to "merge reorders stuff"`,
		`Assign hdweiss to "merge reorders stuff"`,
		`Comment on "merge reorders stuff"`,
		`Rename to "Notes merge reorders lines"`,
		`Remove label "needs-triage" from "Notes merge reorders lines"`,
		`Set milestone "v1" on "Notes merge reorders lines"`,
		`Lock "Notes merge reorders lines permanently" as resolved`,
		`Close "Notes merge reorders lines permanently" as completed`,
	} {
		if !strings.Contains(log, want) {
			t.Errorf("log missing subject %q:\n%s", want, log)
		}
	}

	// Each commit is authored by whoever did the thing, at the moment they did
	// it, and committed by whoever ran the import. That split is the whole
	// point of the metadata: `git shortlog` on the tracker names contributors
	// rather than the person who last synced.
	authors := gitIn(t, bob, "log", "--format=%an <%ae>|%cn <%ce>|%at", "refs/notes/issues/open")
	seen := map[string]bool{}
	for _, line := range strings.Split(strings.TrimSpace(authors), "\n") {
		parts := strings.Split(line, "|")
		seen[parts[0]] = true
		if parts[1] != "bob <bob@example.com>" {
			t.Errorf("committer is %s, want the local user", parts[1])
		}
	}
	for _, want := range []string{
		"hdweiss <hdweiss@users.noreply.github.com>",
		"octocat <octocat@users.noreply.github.com>",
		"ghost <ghost@users.noreply.github.com>",
	} {
		if !seen[want] {
			t.Errorf("no commit authored by %s, saw %v", want, seen)
		}
	}

	// Commits come out oldest first across every issue, not issue by issue,
	// so the log reads as a history of the tracker.
	stamps := gitIn(t, bob, "log", "--reverse", "--author-date-order", "--format=%at", "refs/notes/issues/open")
	prev := ""
	for _, at := range strings.Split(strings.TrimSpace(stamps), "\n") {
		if prev != "" && len(at) == len(prev) && at < prev {
			t.Errorf("commit dated %s follows %s; log is not chronological", at, prev)
		}
		prev = at
	}

	// Every commit names the entity it wrote, so a reader can get from the log
	// back to the issue.
	trailers := gitIn(t, bob, "log", "--format=%(trailers:key=Issue,valueonly)", "refs/notes/issues/open")
	if n := len(strings.Fields(trailers)); n != 14 {
		t.Errorf("%d commits carry an Issue trailer, want 14:\n%s", n, trailers)
	}

	// The imported issues are readable, and carry where they came from.
	listed := gitIssue(t, bin, bob).stdout
	if !strings.Contains(listed, "closed") || !strings.Contains(listed, "\U0001f4ac 2") {
		t.Errorf("imported issues do not list as expected:\n%s", listed)
	}

	// A second import converges: the events hash to the same ids, so the union
	// adds nothing and the ref does not move.
	again := gitIssueEnv(t, bin, bob, []string{"GITHUB_TOKEN=secret"}, "pull", target)
	if again.code != 0 {
		t.Fatalf("exit %d: %s", again.code, again.stderr)
	}
	if !strings.Contains(again.stdout, "Already up to date.") {
		t.Errorf("re-import was not a no-op:\n%s", again.stdout)
	}
	if requests != 2 {
		t.Errorf("made %d API requests for two imports, want 2", requests)
	}
}

// show --web sends the reader to the issue's page on the tracker it came from,
// which the import recorded on the origin ledger. The browser is git's, so the
// test configures a stub one and reads back the URL it was handed.
func TestShowWeb(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)

	body, err := os.ReadFile("../../testdata/github/issues.json")
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	}))
	defer srv.Close()
	target := "github:" + srv.URL + "/hdweiss/git-issue"

	if got := gitIssueEnv(t, bin, bob, []string{"GITHUB_TOKEN=secret"}, "pull", target); got.code != 0 {
		t.Fatalf("pull: exit %d: %s", got.code, got.stderr)
	}

	// An id and the URL the import filed for it, straight off the origin ledger.
	tracker := strings.SplitN(strings.TrimSpace(
		gitIn(t, bob, "ls-tree", "-r", "--name-only", "refs/git-issue/origins")), "\n", 2)[0]
	var id, want string
	for _, line := range strings.Split(gitIn(t, bob, "show", "refs/git-issue/origins:"+tracker), "\n") {
		if f := strings.Fields(line); len(f) == 3 && f[0] == "url" {
			id, want = f[1], f[2]
			break
		}
	}
	if id == "" {
		t.Fatal("no url line on the origin ledger")
	}

	// A stub browser that records the URL git-web--browse hands it.
	opened := filepath.Join(t.TempDir(), "opened")
	script := filepath.Join(t.TempDir(), "browser")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s' \"$1\" > \""+opened+"\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	gitIn(t, bob, "config", "browser.stub.cmd", script)
	gitIn(t, bob, "config", "web.browser", "stub")

	got := gitIssue(t, bin, bob, "show", id, "--web")
	if got.code != 0 {
		t.Fatalf("show --web: exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stderr, "Opening "+want) {
		t.Errorf("stderr = %q, want it to name the URL being opened", got.stderr)
	}
	if b, _ := os.ReadFile(opened); strings.TrimSpace(string(b)) != want {
		t.Errorf("browser opened %q, want %q", strings.TrimSpace(string(b)), want)
	}

	// The shortcut form takes the flag too.
	if got := gitIssue(t, bin, bob, id, "--web"); got.code != 0 {
		t.Errorf("git issue <id> --web: exit %d: %s", got.code, got.stderr)
	}

	// An issue that has never touched a bridge has no page.
	local := strings.TrimSpace(gitIssue(t, bin, bob, "add", "-t", "Filed here").stdout)
	if got := gitIssue(t, bin, bob, "show", local, "--web"); got.code == 0 ||
		!strings.Contains(got.stderr, "no upstream page") {
		t.Errorf("show --web on a local issue: exit %d, stderr %q", got.code, got.stderr)
	}

	// A comment has no page of its own.
	if got := gitIssue(t, bin, bob, "show", id, "0000", "--web"); got.code == 0 ||
		!strings.Contains(got.stderr, "a comment has no page") {
		t.Errorf("show --web with a comment id: exit %d, stderr %q", got.code, got.stderr)
	}
}

// The default imports open issues only; --all lifts that. Asserted on the
// GraphQL variables actually sent, since that is where the decision lands.
func TestPullGitHubStates(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)

	var states any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		json.Unmarshal(body, &req)
		states = req.Variables["states"]
		io.WriteString(w, `{"data":{"repository":{"issues":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}`)
	}))
	defer srv.Close()
	target := "github:" + srv.URL + "/o/n"

	if got := gitIssueEnv(t, bin, bob, []string{"GITHUB_TOKEN=x"}, "pull", target); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if list, ok := states.([]any); !ok || len(list) != 1 || list[0] != "OPEN" {
		t.Errorf("default pull asked for states %#v, want [OPEN]", states)
	}

	if got := gitIssueEnv(t, bin, bob, []string{"GITHUB_TOKEN=x"}, "pull", "--all", target); got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if states != nil {
		t.Errorf("--all asked for states %#v, want null", states)
	}
}

// --all narrows nothing in git mode, so it is refused rather than accepted and
// quietly ignored.
func TestPullGitRejectsAll(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)

	got := gitIssue(t, bin, bob, "pull", "--all", "origin")
	if got.code != 1 || !strings.Contains(got.stderr, "--all applies to a bridge import") {
		t.Errorf("exit %d, stderr %q", got.code, got.stderr)
	}
}

// ghServer replays a canned page and records every request's variables, so a
// sequence of pulls can be asserted on what each one asked for.
type ghServer struct {
	*httptest.Server
	vars []map[string]any
}

func newGHServer(t *testing.T, body string) *ghServer {
	t.Helper()
	gh := &ghServer{}
	gh.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Variables map[string]any `json:"variables"`
		}
		json.Unmarshal(raw, &req)
		gh.vars = append(gh.vars, req.Variables)
		io.WriteString(w, body)
	}))
	t.Cleanup(gh.Close)
	return gh
}

func (g *ghServer) target() string { return "github:" + g.URL + "/hdweiss/git-issue" }

// last returns the variables of the most recent request.
func (g *ghServer) last(t *testing.T) map[string]any {
	t.Helper()
	if len(g.vars) == 0 {
		t.Fatal("no requests were made")
	}
	return g.vars[len(g.vars)-1]
}

// A second import resumes from where the first finished, rather than re-reading
// the repository. The watermark is the newest updatedAt of what was imported,
// which is the same clock the since filter compares against.
func TestPullGitHubResumes(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)

	body, err := os.ReadFile("../../testdata/github/issues.json")
	if err != nil {
		t.Fatal(err)
	}
	gh := newGHServer(t, string(body))
	env := []string{"GITHUB_TOKEN=secret"}

	// Nothing known yet, so the first run asks for everything.
	if got := gitIssueEnv(t, bin, bob, env, "pull", gh.target()); got.code != 0 {
		t.Fatalf("first pull: %s", got.stderr)
	}
	if since := gh.last(t)["since"]; since != nil {
		t.Errorf("first pull asked for since %#v, want null", since)
	}

	// The fixture's newest updatedAt. Everything older has already been read.
	const newest = "2026-03-07T09:30:00Z"
	second := gitIssueEnv(t, bin, bob, env, "pull", gh.target())
	if second.code != 0 {
		t.Fatalf("second pull: %s", second.stderr)
	}
	if since := gh.last(t)["since"]; since != newest {
		t.Errorf("second pull asked for since %#v, want %s", since, newest)
	}
	if !strings.Contains(second.stderr, "updated since") {
		t.Errorf("resuming was not reported: %q", second.stderr)
	}

	// --full ignores the watermark; --since overrides it.
	if got := gitIssueEnv(t, bin, bob, env, "pull", "--full", gh.target()); got.code != 0 {
		t.Fatalf("--full: %s", got.stderr)
	}
	if since := gh.last(t)["since"]; since != nil {
		t.Errorf("--full asked for since %#v, want null", since)
	}
	if got := gitIssueEnv(t, bin, bob, env, "pull", "--since", "2020-01-01", gh.target()); got.code != 0 {
		t.Fatalf("--since: %s", got.stderr)
	}
	// A plain date is read in the local zone, which the harness pins to
	// Europe/Berlin, and sent to GitHub as the UTC instant it names.
	if since := gh.last(t)["since"]; since != "2019-12-31T23:00:00Z" {
		t.Errorf("--since asked for %#v", since)
	}

	// An --all run has seen every open issue too, so it advances both
	// watermarks; an open-only run must not advance the --all one.
	if got := gitIssueEnv(t, bin, bob, env, "pull", "--all", gh.target()); got.code != 0 {
		t.Fatalf("--all: %s", got.stderr)
	}
	if since := gh.last(t)["since"]; since != nil {
		t.Errorf("first --all asked for since %#v, want null", since)
	}
	if got := gitIssueEnv(t, bin, bob, env, "pull", "--all", gh.target()); got.code != 0 {
		t.Fatalf("second --all: %s", got.stderr)
	}
	if since := gh.last(t)["since"]; since != newest {
		t.Errorf("second --all asked for since %#v, want %s", since, newest)
	}

	// The watermark is a cache in the git directory, never a ref: deleting it
	// costs a full re-read and nothing else.
	state := filepath.Join(bob, ".git", "git-issue", "sync.json")
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("no sync state at %s: %v", state, err)
	}
	refs := gitIn(t, bob, "for-each-ref", "--format=%(refname)")
	if strings.Contains(refs, "sync") {
		t.Errorf("sync state leaked into a ref:\n%s", refs)
	}
	// The origin ledger is the one thing here that is synced on purpose: it
	// records where each issue lives upstream, which is false nowhere, and a
	// clone without it re-creates every issue upstream on its first push.
	if !strings.Contains(refs, "refs/git-issue/origins") {
		t.Errorf("the import recorded no origin ledger:\n%s", refs)
	}
	if err := os.Remove(state); err != nil {
		t.Fatal(err)
	}
	if got := gitIssueEnv(t, bin, bob, env, "pull", gh.target()); got.code != 0 {
		t.Fatalf("after forgetting: %s", got.stderr)
	}
	if since := gh.last(t)["since"]; since != nil {
		t.Errorf("after deleting the state, since = %#v, want null", since)
	}
}

// destroy clears the bridge state too, not just the notes refs.
//
// The watermark is the one that bites: it survives the issues it describes, so
// a pull after a destroy asks only for what changed since the last import and
// refills the repository with a slice of the tracker instead of the tracker.
func TestDestroyClearsBridgeState(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)

	body, err := os.ReadFile("../../testdata/github/issues.json")
	if err != nil {
		t.Fatal(err)
	}
	gh := newGHServer(t, string(body))
	env := []string{"GITHUB_TOKEN=secret"}

	if got := gitIssueEnv(t, bin, bob, env, "pull", gh.target()); got.code != 0 {
		t.Fatalf("pull: %s", got.stderr)
	}
	state := filepath.Join(bob, ".git", "git-issue", "sync.json")
	if _, err := os.Stat(state); err != nil {
		t.Fatalf("the pull recorded no watermark at %s: %v", state, err)
	}

	done := gitIssueEnv(t, bin, bob, env, "destroy", "--yes")
	if done.code != 0 {
		t.Fatalf("destroy: %s", done.stderr)
	}
	if !strings.Contains(done.stdout, "watermark") {
		t.Errorf("destroy did not report clearing the watermark:\n%s", done.stdout)
	}
	if !strings.Contains(done.stdout, origins.Ref) {
		t.Errorf("destroy did not report removing the ledger:\n%s", done.stdout)
	}

	if _, err := os.Stat(state); !os.IsNotExist(err) {
		t.Errorf("the watermark survived destroy: %v", err)
	}
	if refs := gitIn(t, bob, "for-each-ref", "--format=%(refname)"); strings.Contains(refs, origins.Ref) {
		t.Errorf("the origin ledger survived destroy:\n%s", refs)
	}

	// The symptom, stated as the reader would meet it: the next pull reads the
	// tracker in full rather than the window since a watermark for issues that
	// are no longer here.
	if got := gitIssueEnv(t, bin, bob, env, "pull", gh.target()); got.code != 0 {
		t.Fatalf("pull after destroy: %s", got.stderr)
	}
	if since := gh.last(t)["since"]; since != nil {
		t.Errorf("pull after destroy asked for since %#v, want null", since)
	}

	// And a destroy with nothing left to clear still says so, rather than
	// reporting the state it just removed a second time.
	again := gitIssueEnv(t, bin, bob, env, "destroy", "--yes")
	if again.code != 0 {
		t.Fatalf("repeat destroy: %s", again.stderr)
	}
}

// Flags are honoured wherever they appear, and --help is a request rather than
// an error: it prints usage on stdout and exits zero.
func TestPullFlagPlacementAndHelp(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)

	// `pull origin --all` must reach the same code as `pull --all origin`.
	// Go's flag package stops at the first non-flag argument, so this used to
	// drop the flag silently.
	for _, args := range [][]string{
		{"pull", "--all", "origin"},
		{"pull", "origin", "--all"},
	} {
		got := gitIssue(t, bin, bob, args...)
		if !strings.Contains(got.stderr, "--all applies to a bridge import") {
			t.Errorf("%v: --all was not seen: %q", args, got.stderr)
		}
	}

	for _, args := range [][]string{
		{"pull", "--help"},
		{"pull", "origin", "--help"},
		{"--help"},
		{"help"},
	} {
		got := gitIssue(t, bin, bob, args...)
		if got.code != 0 {
			t.Errorf("%v: exit %d, stderr %q", args, got.code, got.stderr)
		}
		if !strings.HasPrefix(got.stdout, "usage:") {
			t.Errorf("%v: stdout %q, want usage", args, got.stdout)
		}
		if got.stderr != "" {
			t.Errorf("%v: help went to stderr: %q", args, got.stderr)
		}
	}

	// Two ways of saying where to start, which would disagree.
	if got := gitIssue(t, bin, bob, "pull", "--full", "--since", "2020-01-01", "github:o/n"); got.code == 0 {
		t.Error("--full with --since was accepted")
	}
}

// A bare `git issue pull` reads from the *current branch's* own remote — the
// same key plain `git pull` resolves — rather than a remote named origin
// unconditionally.
func TestPullDefaultUsesBranchRemote(t *testing.T) {
	bin := build(t)
	alice, bob := clonePair(t)

	// A second remote, configured as the current branch's upstream instead of
	// origin.
	upstream := filepath.Join(t.TempDir(), "upstream.git")
	gitIn(t, t.TempDir(), "init", "-q", "--bare", upstream)
	gitIn(t, bob, "remote", "add", "upstream", upstream)
	branch := strings.TrimSpace(gitIn(t, bob, "symbolic-ref", "--short", "HEAD"))
	gitIn(t, bob, "config", "branch."+branch+".remote", "upstream")

	// alice pushes only to upstream, never to origin, so a pull that still
	// assumed origin would find nothing.
	issue := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "Tracked via upstream").stdout)
	gitIn(t, alice, "remote", "add", "upstream", upstream)
	gitIn(t, alice, "push", "-q", "upstream", "refs/notes/issues/open")

	got := gitIssue(t, bin, bob, "pull")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if !strings.Contains(got.stdout, "From "+upstream) {
		t.Errorf("pull did not report fetching from upstream:\n%s", got.stdout)
	}
	if listed := gitIssue(t, bin, bob).stdout; !strings.Contains(listed, issue) {
		t.Errorf("issue pushed only to upstream is not listed after a bare pull:\n%s", listed)
	}
}

// --all narrows what the bridge leg of a bare pull asks for; it must not make
// the git leg refuse to run the way it refuses on an explicit git-only
// target, or --all would be unusable on the one target that can actually use
// it.
func TestPullDefaultAcceptsAll(t *testing.T) {
	bin := build(t)
	alice, bob := clonePair(t)

	issue := strings.TrimSpace(gitIssue(t, bin, alice, "add", "-t", "First issue").stdout)
	gitIn(t, alice, "push", "-q", "origin", "refs/notes/issues/open")

	got := gitIssue(t, bin, bob, "pull", "--all")
	if got.code != 0 {
		t.Fatalf("bare pull --all: exit %d: %s", got.code, got.stderr)
	}
	if strings.Contains(got.stderr, "--all applies to a bridge import") {
		t.Errorf("bare pull rejected --all, which its bridge leg is free to use:\n%s", got.stderr)
	}
	if listed := gitIssue(t, bin, bob).stdout; !strings.Contains(listed, issue) {
		t.Errorf("git leg did not run under --all:\n%s", listed)
	}
}

// A bare pull against a plain git remote does only the git leg: no bridge
// hint, no attempt at an API it has no credentials for.
func TestPullDefaultSkipsBridgeForAPlainRemote(t *testing.T) {
	bin := build(t)
	alice, bob := clonePair(t)

	gitIssue(t, bin, alice, "add", "-t", "Only a git remote")
	gitIn(t, alice, "push", "-q", "origin", "refs/notes/issues/open")

	got := gitIssue(t, bin, bob, "pull")
	if got.code != 0 {
		t.Fatalf("exit %d: %s", got.code, got.stderr)
	}
	if strings.Contains(strings.ToLower(got.stdout+got.stderr), "github") {
		t.Errorf("bare pull against a plain remote mentioned GitHub:\nstdout:%s\nstderr:%s", got.stdout, got.stderr)
	}

	// And a second bare pull, now that the ref is up to date, still says so
	// plainly rather than going quiet because there was no bridge leg either.
	again := gitIssue(t, bin, bob, "pull")
	if again.code != 0 || strings.TrimSpace(again.stdout) != "Already up to date." {
		t.Errorf("second bare pull: exit %d, stdout %q", again.code, again.stdout)
	}
}

// An unusable target fails before any network call, and says what is wrong
// with it.
func TestPullGitHubBadTarget(t *testing.T) {
	bin := build(t)
	_, bob := clonePair(t)

	got := gitIssue(t, bin, bob, "pull", "github:notarepo")
	if got.code != 1 || !strings.Contains(got.stderr, "owner/name") {
		t.Errorf("exit %d, stderr %q", got.code, got.stderr)
	}
}
