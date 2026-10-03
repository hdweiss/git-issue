// Package gitx is the whole surface this tracker has against git.
//
// It shells out to the git binary rather than linking a library. go-git has no
// notes support (go-git#915, open since 2023) and libgit2 would forfeit the
// static binary that is the reason this is Go at all. See AGENTS.md,
// "Decisions already made".
package gitx

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"time"
)

// Repo is a git repository reached through the git binary.
//
// This package is a leaf: it knows about git and nothing about the tracker, so
// ObjectFormat is the plain string git reports rather than a tracker type.
type Repo struct {
	Dir string // "" runs git in the process's working directory
	// ObjectFormat is the repository's hash algorithm, "sha1" or "sha256".
	ObjectFormat string
}

// Open detects the repository's hash algorithm, which fixes the width of every
// entity id: notes keys are object names, so a sha256 repo has 64-hex ids
// (docs/storage-model.md).
func Open(dir string) (*Repo, error) {
	r := &Repo{Dir: dir}
	out, err := r.run(nil, "rev-parse", "--show-object-format")
	if err != nil {
		return nil, err
	}
	r.ObjectFormat = strings.TrimSpace(string(out))
	return r, nil
}

func (r *Repo) git(stdin io.Reader, args ...string) *exec.Cmd {
	cmd := exec.Command("git", args...)
	cmd.Dir = r.Dir
	cmd.Stdin = stdin
	return cmd
}

func (r *Repo) run(stdin io.Reader, args ...string) ([]byte, error) {
	cmd := r.git(stdin, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = err.Error()
		}
		return nil, fmt.Errorf("git %s: %s", strings.Join(args, " "), msg)
	}
	return out, nil
}

// Config reads one config value.
func (r *Repo) Config(key string) (string, error) {
	out, err := r.run(nil, "config", key)
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// WebBrowse opens a URL in the user's browser via `git web--browse`, so the
// choice of browser is git's `web.browser` config and this program does not
// carry its own per-platform launcher.
func (r *Repo) WebBrowse(url string) error {
	_, err := r.run(nil, "web--browse", url)
	return err
}

// Note is one line of `git notes list`: the note blob, and the entity id it is
// filed under.
type Note struct {
	Blob   string
	Entity string
}

// NotesList enumerates a notes ref. A missing ref is not an error — it is an
// empty tracker.
func (r *Repo) NotesList(ref string) ([]Note, error) {
	cmd := r.git(nil, "notes", "--ref="+ref, "list")
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return nil, nil
	}

	var notes []Note
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		blob, entity, ok := strings.Cut(line, " ")
		if !ok {
			continue
		}
		notes = append(notes, Note{Blob: blob, Entity: entity})
	}
	return notes, nil
}

// Notes are never written with `git notes add` or `git notes append`.
//
// Both were used here once and both are wrong now, for the same reason: they
// write a commit whose message git chooses ("Notes added by 'git notes
// append'") and whose author is the local user, which throws away the record
// this tracker exists to keep. Everything goes through WriteNotes instead —
// see entity.Apply, which assembles the blob bytes itself and so also keeps
// the append-never-rewrite invariant those flags were being used for.
//
// Worth knowing about the id either way: it need not name an object that
// exists, because git notes keys are tree path strings rather than object
// pointers. That is why nothing here ever validates an entity id with
// `cat-file -e` — the check passes in a fresh repo and fails after the first
// gc, for every entity.

// NotesRemove drops the tree entry. It unlists the entity; it does not erase
// it, and it does not converge under concurrent edits — see cmd/git-issue.
func (r *Repo) NotesRemove(ref, id string) error {
	_, err := r.run(nil, "notes", "--ref="+ref, "remove", id)
	return err
}

// Blob is one object read back from a batch.
type Blob struct {
	Entity string
	Body   []byte
}

// ReadNotes reads the given notes in a single `cat-file --batch`, calling fn
// for each one as it arrives.
//
// Batching is the whole performance story: one batched read of a 20,000-entity
// ref takes 0.755s against 58.6s for a `notes show` per entity (AGENTS.md,
// "Measured facts"). %(rest) carries the entity id through the batch, so the
// caller needs no separate join, and bodies are handed over one at a time
// rather than accumulated.
func (r *Repo) ReadNotes(notes []Note, fn func(Blob) error) error {
	if len(notes) == 0 {
		return nil
	}

	var in bytes.Buffer
	for _, n := range notes {
		fmt.Fprintf(&in, "%s %s\n", n.Blob, n.Entity)
	}

	// exec copies a non-*os.File stdin on its own goroutine, so writing the
	// whole request up front cannot deadlock against reading the responses.
	cmd := r.git(&in, "cat-file", "--batch=%(rest) %(objectsize)")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}

	readErr := readBatch(bufio.NewReaderSize(stdout, 64*1024), fn)
	io.Copy(io.Discard, stdout)
	if err := cmd.Wait(); err != nil && readErr == nil {
		return fmt.Errorf("git cat-file: %s", strings.TrimSpace(stderr.String()))
	}
	return readErr
}

func readBatch(rd *bufio.Reader, fn func(Blob) error) error {
	for {
		header, err := rd.ReadString('\n')
		if err == io.EOF && header == "" {
			return nil
		}
		if err != nil && err != io.EOF {
			return err
		}
		header = strings.TrimSuffix(header, "\n")
		if header == "" {
			return nil
		}

		entity, sizeField, ok := lastField(header)
		if !ok {
			return fmt.Errorf("git cat-file: unparseable response %q", header)
		}
		// A note whose blob is gone or ambiguous answers with a status word
		// instead of a size, and sends no body. Skip it rather than abort:
		// one unreadable note must not take down a listing.
		size, err := strconv.Atoi(sizeField)
		if err != nil {
			continue
		}

		body := make([]byte, size)
		if _, err := io.ReadFull(rd, body); err != nil {
			return err
		}
		if _, err := rd.Discard(1); err != nil && err != io.EOF {
			return err
		}
		if err := fn(Blob{Entity: entity, Body: body}); err != nil {
			return err
		}
	}
}

// lastField splits "a b c" into "a b" and "c", matching Python's rsplit(" ", 1).
func lastField(s string) (head, tail string, ok bool) {
	i := strings.LastIndexByte(s, ' ')
	if i < 0 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// ConfigDefault reads one config value, returning "" when it is unset. An
// unset key is not an error to git either — `git config` just exits 1.
func (r *Repo) ConfigDefault(key string) string {
	v, err := r.Config(key)
	if err != nil {
		return ""
	}
	return v
}

// SetConfig writes one config value to this repository's own config.
//
// Local rather than global on purpose: everything written through here
// describes this clone's dealings with one remote, which is false of every
// other clone and of every other repository on the machine.
func (r *Repo) SetConfig(key, value string) error {
	_, err := r.run(nil, "config", "--local", key, value)
	return err
}

// UnsetConfig removes one config value, treating an already-absent key as
// success — `git config --unset` exits 5 for one, and a caller clearing
// something wants it gone rather than wants to know it was already gone.
func (r *Repo) UnsetConfig(key string) error {
	if r.ConfigDefault(key) == "" {
		return nil
	}
	_, err := r.run(nil, "config", "--local", "--unset", key)
	return err
}

// ConfigKeys lists the config keys matching a regular expression, which is how
// a caller enumerates a namespace of its own rather than guessing at names.
func (r *Repo) ConfigKeys(pattern string) []string {
	out, err := r.run(nil, "config", "--name-only", "--get-regexp", pattern)
	if err != nil {
		return nil
	}
	var keys []string
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if line = strings.TrimSpace(line); line != "" {
			keys = append(keys, line)
		}
	}
	return keys
}

// RemoteURL is the fetch URL configured for a named remote, or "" if no such
// remote exists. That distinction is how a target is classified: a name git
// knows is a remote, anything else is a URL or a slug.
func (r *Repo) RemoteURL(name string) string {
	out, err := r.run(nil, "remote", "get-url", name)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Toplevel is the root of the working tree, which is what a path stored in an
// entity is relative to. A bare repository has none and reports "".
func (r *Repo) Toplevel() string {
	out, err := r.run(nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Remotes are the names of this repository's remotes, in git's own order.
//
// A repository with none is not an error but an empty list: a clone that was
// never given a remote is the ordinary local case.
func (r *Repo) Remotes() []string {
	out, err := r.run(nil, "remote")
	if err != nil {
		return nil
	}
	var names []string
	for _, line := range strings.Split(string(out), "\n") {
		if name := strings.TrimSpace(line); name != "" {
			names = append(names, name)
		}
	}
	return names
}

// CurrentBranch is the branch HEAD points at, or "" in detached HEAD. HEAD is
// symbolic before the first commit too, so this still names the branch in a
// repository with no history yet.
func (r *Repo) CurrentBranch() string {
	cmd := r.git(nil, "symbolic-ref", "--short", "-q", "HEAD")
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// BranchRemote is the remote configured as the current branch's upstream —
// `branch.<name>.remote`, the same key `git pull` with no arguments reads —
// or "" if there is no current branch, or it has no remote configured.
func (r *Repo) BranchRemote() string {
	branch := r.CurrentBranch()
	if branch == "" {
		return ""
	}
	return r.ConfigDefault("branch." + branch + ".remote")
}

// RefSHA resolves a ref. A ref that does not exist is not an error — it is an
// empty tracker, or a remote this repository has never pulled from.
func (r *Repo) RefSHA(ref string) string {
	cmd := r.git(nil, "rev-parse", "--verify", "--quiet", ref)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// DeleteRef deletes a ref outright. `update-ref -d` is idempotent — a ref that
// is already gone reports success — so this is safe to call on a tracker that
// was only ever half-populated.
func (r *Repo) DeleteRef(ref string) error {
	_, err := r.run(nil, "update-ref", "-d", ref)
	return err
}

// Fetch retrieves one refspec from a remote.
//
// The refspec is always explicit: notes refs are outside the default fetch
// refspec, so nothing here relies on a remote's configured refs.
func (r *Repo) Fetch(remote, refspec string) error {
	_, err := r.run(nil, "fetch", remote, refspec)
	return err
}

// Push sends refspecs to a remote.
//
// Explicit refspecs for the same reason Fetch takes one: neither a notes ref nor
// the origin ledger is covered by a remote's default push refspec, so nothing
// here relies on one being configured.
//
// Never force. Both refs this program pushes are grow-only unions, so a rejected
// push means fetch-merge-push, which always terminates and never discards a
// line — and a force would silently drop whatever the remote had that this
// clone had not seen.
func (r *Repo) Push(remote string, refspecs ...string) error {
	_, err := r.run(nil, append([]string{"push", remote}, refspecs...)...)
	return err
}

// IsAncestor reports whether commit a is reachable from commit b, which is what
// distinguishes a push that fast-forwards from one that has to merge first.
//
// An empty a is an ancestor of anything: nothing to lose means nothing to
// refuse.
func (r *Repo) IsAncestor(a, b string) bool {
	if a == "" {
		return true
	}
	if b == "" {
		return false
	}
	cmd := r.git(nil, "merge-base", "--is-ancestor", a, b)
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// UpdateRef points a ref at a commit.
//
// This is how a remote-tracking ref follows a successful push: git updates
// refs/remotes/* on its own only for refs covered by a configured refspec, and
// neither the notes refs nor the ledger is.
func (r *Repo) UpdateRef(ref, sha string) error {
	_, err := r.run(nil, "update-ref", ref, sha)
	return err
}

// ReadPath reads one blob addressed as <rev>:<path>.
//
// A missing path is not an error but an empty result: a tracker the ledger has
// never recorded anything for reads as empty, exactly as a missing ref does.
func (r *Repo) ReadPath(rev, path string) ([]byte, error) {
	if rev == "" {
		return nil, nil
	}
	cmd := r.git(nil, "cat-file", "blob", rev+":"+path)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return nil, nil
	}
	return out, nil
}

// DefaultBranch is the branch a change is proposed against by default.
//
// Asked of the remote first, because that is where the answer actually lives:
// `refs/remotes/<remote>/HEAD` is what the server said its default was at clone
// time. Without a remote — a repository that has never been pushed, which is
// exactly the local-review case — it falls back to whichever of the usual two
// names this repository has, and to "" when it has neither, so a caller can
// tell "no answer" from a wrong guess.
func (r *Repo) DefaultBranch(remote string) string {
	if remote != "" {
		if ref := r.symbolicRef("refs/remotes/" + remote + "/HEAD"); ref != "" {
			if name := strings.TrimPrefix(ref, "refs/remotes/"+remote+"/"); name != ref {
				return name
			}
		}
	}
	for _, name := range []string{"main", "master", "trunk"} {
		if r.RefSHA("refs/heads/"+name) != "" {
			return name
		}
	}
	return ""
}

func (r *Repo) symbolicRef(ref string) string {
	cmd := r.git(nil, "symbolic-ref", "--quiet", ref)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Checkout checks out a commit or ref, leaving HEAD detached where it names a
// commit.
func (r *Repo) Checkout(rev string) error {
	_, err := r.run(nil, "checkout", rev)
	return err
}

// CheckoutBranch checks out a local branch at start, creating it if it is not
// there and switching to it if it is.
//
// `git checkout -B` rather than `-b`: picking up a review a second time after
// its head moved is the ordinary case, and refusing because the branch already
// exists would make the second checkout of every review a manual reset. The
// branch is a local name for somebody else's work, not somewhere local work
// accumulates.
func (r *Repo) CheckoutBranch(name, start string) error {
	args := []string{"checkout", "-B", name}
	if start != "" {
		args = append(args, start)
	}
	_, err := r.run(nil, args...)
	return err
}

// MergeBase is the best common ancestor of two commits, or "" where there is
// none or either side is unknown.
func (r *Repo) MergeBase(a, b string) string {
	if a == "" || b == "" {
		return ""
	}
	cmd := r.git(nil, "merge-base", a, b)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// Diff runs `git diff` with the given arguments, streaming to the writers
// given. It is the one place this tracker shells out to git's own diff rather
// than computing one: a review stores no diff, so showing one means asking git
// for it against the commits the review names.
func (r *Repo) Diff(args []string, stdout, stderr io.Writer) error {
	cmd := r.git(nil, append([]string{"diff"}, args...)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// PathSHA is the object name of one path at one revision, or "" where the
// revision or the path is not there.
//
// A missing answer is empty rather than an error, the way ReadPath's is: a
// review's anchor routinely names a commit this clone has not fetched, or a
// file that did not exist yet, and neither is a failure.
func (r *Repo) PathSHA(rev, path string) string {
	if rev == "" || path == "" {
		return ""
	}
	cmd := r.git(nil, "rev-parse", "--verify", "--quiet", rev+":"+path)
	cmd.Stderr = nil
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// HasCommit reports whether this repository holds an object at rev and that it
// is a commit.
func (r *Repo) HasCommit(rev string) bool {
	if rev == "" {
		return false
	}
	cmd := r.git(nil, "rev-parse", "--verify", "--quiet", rev+"^{commit}")
	cmd.Stderr = nil
	return cmd.Run() == nil
}

// ListTree lists every file path under a ref, recursively. A missing ref is
// empty rather than an error.
func (r *Repo) ListTree(ref string) ([]string, error) {
	sha := r.RefSHA(ref)
	if sha == "" {
		return nil, nil
	}
	out, err := r.run(nil, "ls-tree", "-r", "--name-only", sha)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, path := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if path != "" {
			paths = append(paths, path)
		}
	}
	return paths, nil
}

// NotesMerge unions another notes ref into ref.
//
// cat_sort_uniq is a set union of lines, which is exactly the blob format's
// merge semantics (docs/storage-model.md). It also permanently reorders the
// file alphabetically, which is why physical line order carries no meaning.
func (r *Repo) NotesMerge(ref, other string) error {
	_, err := r.run(nil, "notes", "--ref="+ref, "merge", "-s", "cat_sort_uniq", other)
	return err
}

// Change is one path that differs between two trees, with git's own status
// letter: A added, M modified, D deleted.
type Change struct {
	Status byte
	Path   string
}

// DiffTree lists the paths that differ between two commits. An empty old means
// everything in new is new, which is the first-pull case.
//
// This is what makes a fetch summary cheap: at 20,000 entities the diff takes
// 3ms and names exactly the entities that changed, so nothing has to re-read
// the ref to find out (AGENTS.md, "Measured facts").
func (r *Repo) DiffTree(old, new string) ([]Change, error) {
	if new == "" {
		return nil, nil
	}
	if old == "" {
		out, err := r.run(nil, "ls-tree", "-r", "--name-only", new)
		if err != nil {
			return nil, err
		}
		var changes []Change
		for _, path := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
			if path != "" {
				changes = append(changes, Change{Status: 'A', Path: path})
			}
		}
		return changes, nil
	}

	out, err := r.run(nil, "diff", "--name-status", old, new)
	if err != nil {
		return nil, err
	}
	var changes []Change
	for _, line := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		status, path, ok := strings.Cut(line, "\t")
		if !ok || status == "" {
			continue
		}
		changes = append(changes, Change{Status: status[0], Path: path})
	}
	return changes, nil
}

// Credential is what `git credential` exchanges: for a forge API, Password is
// the token.
type Credential struct {
	Protocol string
	Host     string
	Username string
	Password string
}

func (c Credential) request() string {
	return fmt.Sprintf("protocol=%s\nhost=%s\n\n", c.Protocol, c.Host)
}

// CredentialFill asks git for the credential it has for a host, running
// whatever helper is configured — a keychain, libsecret, or the GitHub CLI's
// own helper.
//
// Going through git rather than reading any of those directly is what makes
// this work on an Enterprise host, and what keeps this program from ever
// storing a token itself.
func (r *Repo) CredentialFill(protocol, host string) (Credential, error) {
	want := Credential{Protocol: protocol, Host: host}
	out, err := r.run(strings.NewReader(want.request()), "credential", "fill")
	if err != nil {
		return Credential{}, err
	}

	got := Credential{Protocol: protocol, Host: host}
	for _, line := range strings.Split(string(out), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch key {
		case "username":
			got.Username = value
		case "password":
			got.Password = value
		}
	}
	if got.Password == "" {
		return Credential{}, fmt.Errorf("git credential fill returned no password for %s", host)
	}
	return got, nil
}

// CredentialApprove tells git the credential worked, so a helper can store it.
// It completes the contract CredentialFill starts; a failure to record is not
// worth failing the operation over.
func (r *Repo) CredentialApprove(c Credential) {
	in := fmt.Sprintf("protocol=%s\nhost=%s\nusername=%s\npassword=%s\n\n", c.Protocol, c.Host, c.Username, c.Password)
	r.run(strings.NewReader(in), "credential", "approve")
}

// NotePaths maps each entity id to the path it occupies in a notes tree.
//
// The two differ once a tree is big enough that git shards it: an entity is
// filed at `e9/0378…` rather than `e90378…`, and a writer that ignored that
// would file a second entry for the same entity under a flat name.
func (r *Repo) NotePaths(ref string) (map[string]string, error) {
	sha := r.RefSHA(ref)
	if sha == "" {
		return map[string]string{}, nil
	}
	out, err := r.run(nil, "ls-tree", "-r", "--name-only", sha)
	if err != nil {
		return nil, err
	}
	paths := map[string]string{}
	for _, path := range strings.Split(strings.TrimRight(string(out), "\n"), "\n") {
		if path != "" {
			paths[strings.ReplaceAll(path, "/", "")] = path
		}
	}
	return paths, nil
}

// FileWrite is one blob to write: where it goes in the tree, and its body.
type FileWrite struct {
	Path string
	Body []byte
}

// Identity is a commit's author or committer: who, and when.
type Identity struct {
	Name  string
	Email string
	When  time.Time
}

// line renders the ident the way fast-import wants it, with the timestamp raw.
//
// Names and addresses are scrubbed of the bytes that would end the field
// early — the angle brackets and the line endings. They arrive from a forge
// rather than from git, so a login carrying one of them is a malformed stream,
// and a malformed stream aborts the whole import rather than looking untidy.
func (i Identity) line() string {
	name, email := scrubIdent(i.Name), scrubIdent(i.Email)
	if name == "" {
		name = email
	}
	when := i.When
	if when.IsZero() {
		when = time.Now()
	}
	_, offset := when.Zone()
	sign := "+"
	if offset < 0 {
		sign, offset = "-", -offset
	}
	return fmt.Sprintf("%s <%s> %d %s%02d%02d", name, email, when.Unix(), sign, offset/3600, (offset%3600)/60)
}

func scrubIdent(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '<', '>', '\n', '\r':
			return -1
		}
		return r
	}, s)
}

// Committer is the local user, which every commit this program writes is the
// committer of, whoever authored the events inside it.
func (r *Repo) Committer(when time.Time) (Identity, error) {
	email := r.ConfigDefault("user.email")
	if email == "" {
		return Identity{}, fmt.Errorf("set user.email first")
	}
	return Identity{Name: r.ConfigDefault("user.name"), Email: email, When: when}, nil
}

// Author is the local user as the author of a commit, under the address the
// events themselves are attributed to. Locally those are the same person; on a
// bridged import they are not, which is the whole reason the two are separate.
func (r *Repo) Author(email string, when time.Time) Identity {
	return Identity{Name: r.ConfigDefault("user.name"), Email: email, When: when}
}

// Commit is one commit on a ref: who did the thing, what to say about it, and
// the blobs it leaves behind or takes away.
type Commit struct {
	Author  Identity
	Message string
	Writes  []FileWrite
	// Deletes are paths to remove, recursively — naming a directory drops the
	// whole subtree, which is how one tracker leaves the origin ledger.
	Deletes []string
	// Merges are additional parents. A merge that records only its own side
	// leaves the other side unreachable, so the result does not fast-forward and
	// the next push is rejected — which is why a union merge still has to say
	// what it merged.
	Merges []string
}

// Stream accepts commits for one fast-import pass.
type Stream struct {
	w         io.Writer
	ref       string
	committer Identity
	started   bool
	parent    string
	err       error
}

// Commit appends one commit to the stream.
func (s *Stream) Commit(c Commit) error {
	if s.err != nil {
		return s.err
	}
	author := c.Author
	if author.Email == "" {
		author = s.committer
	}

	var b bytes.Buffer
	fmt.Fprintf(&b, "commit %s\n", s.ref)
	fmt.Fprintf(&b, "author %s\n", author.line())
	fmt.Fprintf(&b, "committer %s\n", s.committer.line())
	fmt.Fprintf(&b, "data %d\n%s\n", len(c.Message), c.Message)
	// Only the first commit needs `from`: fast-import stacks the rest on the
	// branch it is already building. Without it here, fast-import would replace
	// the ref's tree wholesale rather than adding to it, silently dropping
	// every entity not in this pass.
	if !s.started {
		s.started = true
		if s.parent != "" {
			fmt.Fprintf(&b, "from %s\n", s.parent)
		}
	}
	for _, merge := range c.Merges {
		fmt.Fprintf(&b, "merge %s\n", merge)
	}
	for _, path := range c.Deletes {
		fmt.Fprintf(&b, "D %s\n", path)
	}
	for _, w := range c.Writes {
		fmt.Fprintf(&b, "M 100644 inline %s\ndata %d\n", w.Path, len(w.Body))
		b.Write(w.Body)
		b.WriteByte('\n')
	}

	if _, err := s.w.Write(b.Bytes()); err != nil {
		s.err = err
	}
	return s.err
}

// WriteNotes writes a sequence of commits to a notes ref in one fast-import
// pass. gen is called once and appends commits to the stream in the order they
// should appear; nothing is written if it appends none.
//
// `git notes add` is one process, one commit and one ref update per note,
// which is fine for a person typing and hopeless for an import. fast-import
// writes every blob, tree and commit over a single pipe, so a commit per
// upstream action stays affordable — measured at 50,000 commits in 5.3s.
//
// The stream is written as it is generated rather than buffered. That is not
// tidiness: a commit per action carries a full copy of the note blob at that
// point in its life, so buffering an import of any size means holding the
// tracker's entire write history in memory at once.
//
// The one thing this must not do is let a generator error look like a clean
// import. gen's error is returned after the pipe is closed and git has been
// waited on, so a half-written stream ends as an aborted fast-import rather
// than as a ref update of whatever happened to arrive.
func (r *Repo) WriteNotes(ref string, gen func(*Stream) error) error {
	return r.WriteRef(ref, gen)
}

// WriteRef is WriteNotes without the notes: the same single fast-import pass
// against any ref at all.
//
// The origin ledger (docs/storage-model.md) is an ordinary ref holding an
// ordinary tree keyed by tracker name rather than by object name, so it cannot
// go through `git notes` — but it wants exactly this writer, including the
// author/committer split and the nested paths fast-import handles natively.
func (r *Repo) WriteRef(ref string, gen func(*Stream) error) error {
	committer, err := r.Committer(time.Now())
	if err != nil {
		return err
	}

	cmd := r.git(nil, "fast-import", "--quiet", "--date-format=raw", "--done")
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	buf := bufio.NewWriterSize(stdin, 64*1024)
	s := &Stream{w: buf, ref: ref, committer: committer, parent: r.RefSHA(ref)}
	genErr := gen(s)
	if genErr == nil {
		if _, err := buf.WriteString("done\n"); err != nil {
			genErr = err
		}
	}
	if genErr == nil {
		genErr = buf.Flush()
	}
	stdin.Close()

	waitErr := cmd.Wait()
	if genErr != nil {
		return genErr
	}
	if waitErr != nil {
		msg := strings.TrimSpace(stderr.String())
		if msg == "" {
			msg = waitErr.Error()
		}
		return fmt.Errorf("git fast-import: %s", msg)
	}
	return nil
}

// NotePath is where an entity's blob belongs in the notes tree.
//
// git shards a notes tree into 00-ff subtrees as it grows, and does so on its
// own heuristic — measured: it switches somewhere between 64 and 96 notes.
// Reproducing that heuristic exactly is neither possible nor necessary, since
// git reads a note at any fanout and re-shards the whole tree the next time one
// of its own commands touches the ref. What matters is only that a large tree
// is not written flat: a commit per action against a flat tree of 5,000
// entities rewrites a 5,000-entry root tree every time, which took an import
// from 5.3s to 47.5s.
//
// An entity already in the tree keeps the path it has, whatever depth that is;
// a mixed-depth tree is read correctly by git and settles on its next reshard.
func NotePath(existing map[string]string, id string, total int) string {
	if path, ok := existing[id]; ok {
		return path
	}
	if total > noteFanoutThreshold && len(id) > 2 {
		return id[:2] + "/" + id[2:]
	}
	return id
}

// noteFanoutThreshold is the low end of the range git's own heuristic switches
// in, so this tree is sharded no later than git would have sharded it.
const noteFanoutThreshold = 64

// Repack rewrites the object store into one pack, recomputing every delta.
//
// `-f` is the whole point and the reason `git gc` is not what runs here. gc
// repacks by reusing the deltas it already has, which leaves a bulk import's
// chains exactly as fast-import wrote them: oldest version of a note as the
// base, newest at the deep end, so every listing replays each entity's whole
// history. Measured on a 17,773-commit import, `git gc` moved a listing from
// 1.74s to 1.58s while `git repack -adf` moved it to 0.21s.
//
// `--aggressive` is deliberately not used. It widens the delta window and
// deepens the chains, which costs a great deal more time to gain nothing here:
// the problem is which end of the chain the current version sits at, not how
// well it compresses.
//
// Progress goes wherever the caller says, so git draws its own counters on a
// terminal exactly as it does for a fetch.
func (r *Repo) Repack(progress io.Writer) error {
	cmd := r.git(nil, "repack", "-adf")
	cmd.Stderr = progress
	return cmd.Run()
}

// Log runs `git log` against the given ref with stdout inherited, so git pages
// its own output exactly as it does anywhere else.
func (r *Repo) Log(args []string, stdout, stderr io.Writer) error {
	cmd := r.git(nil, append([]string{"log"}, args...)...)
	cmd.Stdout, cmd.Stderr = stdout, stderr
	return cmd.Run()
}

// CommonDir is the repository's shared git directory, as an absolute path.
//
// Shared rather than per-worktree: a linked worktree has its own .git
// directory, but anything derived from the object store — an index, a sync
// watermark — describes the repository, not the checkout, and should not
// silently reset when someone adds a worktree.
func (r *Repo) CommonDir() (string, error) {
	out, err := r.run(nil, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}
