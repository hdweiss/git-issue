// Package giteaapi is a Gitea client: enough of the API to import and write
// back issues, and nothing else.
//
// It is a leaf, like internal/gitx, internal/bridge/github/api and internal/bridge/ado/api. It
// knows what Gitea returns and has never heard of an event, a fold, or an
// issue — mapping what it returns onto the tracker's vocabulary is
// internal/bridge/gitea/issue's job, and keeping the two apart is what lets the
// mapping be tested against recorded responses with no network at all.
//
// It speaks REST, because Gitea has no GraphQL API. Its shape is close to
// GitHub's v3 REST API but not identical, and one package covers Gitea,
// Forgejo and Codeberg alike — the differences that matter to an issue import
// are cosmetic.
//
// A Gitea install lives on an arbitrary host, sometimes under a path prefix,
// and there is no canonical one — so unlike ghapi and adoapi there is no
// public-host default, and a target that names no host is refused.
package giteaapi

import (
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/hdweiss/git-issue/internal/gitx"
)

// Target is the repository an import reads from.
type Target struct {
	// Scheme is https unless the target was written as an http URL. A local
	// Gitea reachable only over plain http is common in development.
	Scheme string
	// Host carries a port when the URL had one.
	Host string
	// Prefix is the path segment a subpath-hosted install lives under, with a
	// leading slash and no trailing one ("/gitea"), or "" for a root install.
	Prefix string
	Owner  string
	Repo   string
}

// Base is scheme://host, plus the path prefix when the install has one.
func (t Target) Base() string {
	scheme := t.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + t.Host + t.Prefix
}

// API is the root every REST call hangs off.
func (t Target) API() string { return t.Base() + "/api/v1" }

// Repos is the per-repository API root.
func (t Target) Repos() string {
	return t.API() + "/repos/" + t.Owner + "/" + t.Repo
}

// String is the tracker name: the ledger path, and the key a watermark is
// filed under. It drops the scheme so that the same repository reached over
// http in a test and https in life is one tracker.
func (t Target) String() string {
	return t.Host + t.Prefix + "/" + t.Owner + "/" + t.Repo
}

// Origin is the identity string for an issue: the nonce input for its create
// event, and the upstream id recorded on the origin ledger. The two are the
// same string on purpose, so the ledger and the hash cannot drift apart.
//
// It keys on the repository and the issue's repo-local number. A number is
// stable for the life of an issue in one repository; an issue moved to another
// repository is renumbered and imports as a new entity there, which is
// documented in docs/bridge-gitea.md rather than worked around.
func (t Target) Origin(number int64) string {
	return fmt.Sprintf("gitea:%s#%d", t.String(), number)
}

// CommentOrigin is the identity string for one comment on an issue.
func (t Target) CommentOrigin(number, comment int64) string {
	return fmt.Sprintf("%s/comments/%d", t.Origin(number), comment)
}

// WebURL is where an issue can be opened in a browser.
func (t Target) WebURL(number int64) string {
	return fmt.Sprintf("%s/%s/%s/issues/%d", t.Base(), t.Owner, t.Repo, number)
}

// ParseTarget resolves a repository from a remote URL.
//
// It accepts what a git remote can actually hold — https, http, ssh, scp-style,
// with or without a .git suffix. A subpath-hosted install is supported the way
// adoapi supports a virtual directory: the last two path segments are the owner
// and the repository, and whatever precedes them belongs to the base URL.
//
// A bare "owner/repo" slug is refused: Gitea has no canonical host to attach it
// to, so there is nothing to resolve it against.
func ParseTarget(s string) (Target, error) {
	raw := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(s), "/"), ".git")
	if raw == "" {
		return Target{}, fmt.Errorf("empty repository")
	}

	scheme, authority, path := "", "", raw
	switch {
	case strings.Contains(raw, "://"):
		i := strings.Index(raw, "://")
		s0, rest := raw[:i], raw[i+3:]
		authority, path, _ = strings.Cut(rest, "/")
		// ssh:// and git:// say nothing about how the API is reached; only an
		// explicit http URL does.
		if s0 == "http" {
			scheme = "http"
		}
	case strings.Contains(raw, ":") && !strings.HasPrefix(raw, "/") && !looksHostPort(raw):
		// scp-style: git@host:owner/repo
		authority, path, _ = strings.Cut(raw, ":")
	default:
		// host/owner/repo or owner/repo
		authority, path, _ = strings.Cut(raw, "/")
	}

	// Strip any userinfo: ssh and https URLs carry git@ before the host.
	if _, after, ok := strings.Cut(authority, "@"); ok {
		authority = after
	}
	host := authority

	segments := splitNonEmpty(path)
	if host == "" || !isHost(host) || len(segments) < 2 {
		return Target{}, fmt.Errorf("%q is not a Gitea repository URL; pass gitea:<remote> or a full https URL", s)
	}

	owner := segments[len(segments)-2]
	repo := segments[len(segments)-1]
	prefix := ""
	if extra := segments[:len(segments)-2]; len(extra) > 0 {
		prefix = "/" + strings.Join(extra, "/")
	}
	return Target{Scheme: scheme, Host: host, Prefix: prefix, Owner: owner, Repo: repo}, nil
}

// isHost reports whether a string is plausibly a hostname rather than the first
// segment of a bare owner/repo slug. A host has a dot (a domain), a colon (a
// port), or is exactly "localhost".
func isHost(s string) bool {
	return strings.Contains(s, ".") || strings.Contains(s, ":") || s == "localhost"
}

// looksHostPort reports whether "host:1234/..." is a bare host:port rather than
// an scp-style git@host:path — the part after the colon starts with a digit and
// a slash follows before any other colon.
func looksHostPort(raw string) bool {
	_, after, ok := strings.Cut(raw, ":")
	if !ok || after == "" || after[0] < '0' || after[0] > '9' {
		return false
	}
	return strings.Contains(after, "/")
}

func splitNonEmpty(path string) []string {
	var out []string
	for _, s := range strings.Split(path, "/") {
		if s != "" {
			out = append(out, s)
		}
	}
	return out
}

// TokenSource reports where a token came from, for error messages that can
// tell "you have no token" apart from "your token is not allowed to see this".
type TokenSource string

const (
	FromFlag       TokenSource = "--token"
	FromCredential TokenSource = "git credential"
	FromNowhere    TokenSource = ""
)

// Token finds a token for a host, in the order a user would expect one to win:
// what they typed, what their environment says, and finally git's credential
// helpers.
//
// There is no Gitea CLI in the ghapi/adoapi sense — `tea` exists but is rarely
// installed and stores its config in its own file rather than answering a
// query — so the environment and git's helpers are the whole of it.
func Token(repo *gitx.Repo, host, flag string) (string, TokenSource, *gitx.Credential) {
	if flag != "" {
		return flag, FromFlag, nil
	}
	for _, key := range []string{"GITEA_TOKEN", "FORGEJO_TOKEN"} {
		if v := os.Getenv(key); v != "" {
			return v, TokenSource(key), nil
		}
	}
	if cred, err := repo.CredentialFill("https", hostOnly(host)); err == nil {
		return cred.Password, FromCredential, &cred
	}
	return "", FromNowhere, nil
}

// hostOnly drops a port from a host, which is what git's credential helpers key
// on.
func hostOnly(host string) string {
	if h, _, ok := strings.Cut(host, ":"); ok {
		return h
	}
	return host
}

// Client talks to one host.
type Client struct {
	API   string
	Token string
	HTTP  *http.Client

	// Now is overridable so rate-limit waiting can be tested without one.
	Now func() time.Time
	// Sleep stands in for time.Sleep between attempts.
	Sleep func(time.Duration)

	// Workers bounds how many per-issue detail fetches run at once. Gitea has no
	// batch endpoint, so a large import is thousands of small requests, and
	// running them one at a time is the whole of why a first pull is slow. Zero
	// means the default.
	Workers int

	// depsOff records that this repository's dependency tracking is switched
	// off, so the per-issue dependencies call is made once and then skipped.
	depsOff atomic.Bool
}

// New builds a client for a target's API root.
func New(api, token string) *Client {
	return &Client{
		API:   api,
		Token: token,
		HTTP:  &http.Client{Timeout: 60 * time.Second},
		Now:   time.Now,
	}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

func (c *Client) wait(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}
