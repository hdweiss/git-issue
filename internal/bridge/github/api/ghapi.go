// Package ghapi is a GitHub client: enough of the API to import issues, and
// nothing else.
//
// It is a leaf, like internal/gitx. It knows what GitHub returns and has never
// heard of an event, a fold, or an issue — mapping what it returns onto the
// tracker's vocabulary is internal/bridge/github/issue's job, and keeping the two
// apart is what lets the mapping be tested against recorded responses with no
// network at all.
//
// It speaks GraphQL rather than REST. That is not a stylistic preference: over
// REST an import costs one request per issue for comments and another for the
// timeline, so a thousand-issue repository spends three thousand requests
// against a five-thousand-per-hour budget. One GraphQL query carries a page of
// issues with their comments and timelines together.
package ghapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hdweiss/git-issue/internal/gitx"
)

// PublicHost is github.com; anything else is taken to be GitHub Enterprise.
const PublicHost = "github.com"

// Target is the repository an import reads from.
type Target struct {
	// Host carries a port when the URL had one, since for anything but
	// github.com the host is also where the API lives.
	Host string
	// Scheme is https unless the target was written as an http URL. An
	// Enterprise install reachable only over plain http is unusual but real,
	// and inventing https for it would fail with a confusing TLS error.
	Scheme string
	Owner  string
	Name   string
}

func (t Target) String() string { return t.Host + "/" + t.Owner + "/" + t.Name }

// Endpoint is the GraphQL endpoint for the target's host. github.com serves it
// from api.github.com; Enterprise serves it from the host itself.
func (t Target) Endpoint() string {
	if t.Host == PublicHost {
		return "https://api.github.com/graphql"
	}
	scheme := t.Scheme
	if scheme == "" {
		scheme = "https"
	}
	return scheme + "://" + t.Host + "/api/graphql"
}

// ParseTarget resolves a repository from a remote URL or an owner/name slug.
//
// It accepts what a git remote can actually hold — https, ssh, scp-style, with
// or without a .git suffix — because the common case is that the target came
// straight out of `git remote get-url`.
func ParseTarget(s string) (Target, error) {
	raw := strings.TrimSuffix(strings.TrimSuffix(strings.TrimSpace(s), "/"), ".git")
	if raw == "" {
		return Target{}, fmt.Errorf("empty repository")
	}

	host, scheme := PublicHost, ""
	path := raw
	switch {
	case strings.Contains(raw, "://"):
		i := strings.Index(raw, "://")
		scheme, path = raw[:i], raw[i+3:]
		host, path, _ = strings.Cut(path, "/")
		// Strip any userinfo: ssh URLs carry git@ before the host.
		if _, after, ok := strings.Cut(host, "@"); ok {
			host = after
		}
		// ssh:// and git:// say nothing about how the API is reached; only an
		// explicit http URL does.
		if scheme != "http" {
			scheme = ""
		}
	case strings.Contains(raw, ":") && !strings.HasPrefix(raw, "/"):
		// scp-style: git@github.com:owner/name
		hostPart, rest, _ := strings.Cut(raw, ":")
		if _, after, ok := strings.Cut(hostPart, "@"); ok {
			hostPart = after
		}
		host, path = hostPart, rest
	}

	owner, name, ok := strings.Cut(strings.Trim(path, "/"), "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return Target{}, fmt.Errorf("%q is not an owner/name repository", s)
	}
	return Target{Host: host, Scheme: scheme, Owner: owner, Name: name}, nil
}

// Client talks to one host.
type Client struct {
	Endpoint string
	Token    string
	HTTP     *http.Client

	// Now is overridable so rate-limit waiting can be tested without one.
	Now func() time.Time
	// Sleep stands in for time.Sleep between attempts, so a test can drive a
	// retry without waiting one out.
	Sleep func(time.Duration)
	// Notice, where set, is told about things worth saying out loud that are
	// not failures: a page retried smaller after GitHub declined to finish it.
	Notice func(string)

	// out serialises the callbacks. Detail batches are fetched concurrently and
	// both callbacks usually end up on the same terminal, where two goroutines
	// writing at once produce one unreadable line — and a caller's progress
	// display is not obliged to be safe for concurrent use.
	out sync.Mutex

	// noIssueTypes and noRelations record that this host's schema has never
	// heard of issue types, or of issue relationships, so queryDegrading sends
	// the degraded query straight away instead of probing for the same fields
	// again on every page. They are separate because a server can have one
	// without the other. Atomic, and the reason a Client is passed by pointer
	// everywhere: one client answers concurrent queries.
	noIssueTypes atomic.Bool
	noRelations  atomic.Bool
	noPullExtras atomic.Bool
}

// TokenSource reports where a token came from, for error messages that can
// tell "you have no token" apart from "your token is not allowed to see this".
type TokenSource string

const (
	FromFlag       TokenSource = "--token"
	FromCredential TokenSource = "git credential"
	FromCLI        TokenSource = "gh auth token"
	FromNowhere    TokenSource = ""
)

// Token finds a token for a host, in the order a user would expect one to win:
// what they typed, what their environment says, what the GitHub CLI holds, and
// finally git's credential helpers.
//
// gh comes before git credential fill because fill is interactive: when no
// helper can answer, git prompts on the terminal and reports success, so an
// otherwise-working `gh auth token` would never get a turn. A user who has run
// `gh auth login` should not be asked to type a token by hand.
//
// Nothing here ever writes a token anywhere. When one comes from git's
// credential helpers and then works, Approve records that fact through git,
// which is the helper's business rather than ours.
func Token(repo *gitx.Repo, host, flag string) (string, TokenSource, *gitx.Credential) {
	if flag != "" {
		return flag, FromFlag, nil
	}
	for _, key := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		if v := os.Getenv(key); v != "" {
			return v, TokenSource(key), nil
		}
	}
	if token := ghCLIToken(host); token != "" {
		return token, FromCLI, nil
	}
	if cred, err := repo.CredentialFill("https", host); err == nil {
		return cred.Password, FromCredential, &cred
	}
	return "", FromNowhere, nil
}

// ghCLIToken asks the GitHub CLI. It runs before git's credential helpers
// because those fall back to an interactive terminal prompt, but only if gh is
// installed and logged in; a missing or unauthenticated gh returns nothing and
// the credential helpers still get their turn.
//
// It is invoked here rather than through gitx because gitx runs git and only
// git; a second binary is this package's business.
func ghCLIToken(host string) string {
	out, err := exec.Command("gh", "auth", "token", "--hostname", host).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// New builds a client for a target.
func New(endpoint, token string) *Client {
	return &Client{
		Endpoint: endpoint,
		Token:    token,
		HTTP:     &http.Client{Timeout: 60 * time.Second},
		Now:      time.Now,
	}
}

// Error is a failed API call, carrying enough to say something useful about
// why: an HTTP status, or the messages GraphQL returned in a 200.
type Error struct {
	Status   int
	Messages []string
}

func (e *Error) Error() string {
	if len(e.Messages) > 0 {
		return strings.Join(e.Messages, "; ")
	}
	switch e.Status {
	case http.StatusUnauthorized:
		return "not authenticated (401)"
	case http.StatusForbidden:
		return "forbidden (403)"
	case http.StatusNotFound:
		return "not found (404)"
	case http.StatusBadGateway, http.StatusGatewayTimeout:
		return fmt.Sprintf("GitHub did not finish the query (%d), which is usually a server-side timeout", e.Status)
	}
	return fmt.Sprintf("http %d", e.Status)
}

// Unauthorized reports whether the failure is about credentials rather than
// about the request.
func (e *Error) Unauthorized() bool {
	return e.Status == http.StatusUnauthorized || e.Status == http.StatusForbidden
}

// Retryable reports whether the same request could succeed on a second attempt:
// anything the server blamed on itself. GitHub's GraphQL endpoint answers a
// query it could not finish in time with one of these rather than with data, so
// they are common on a large repository and are not failures of the request.
func (e *Error) Retryable() bool { return e.Status >= 500 }

// TooHeavy reports that GitHub is saying the query itself was more than it would
// execute, rather than that something went wrong on the way. A 502 is how a
// timed-out query comes back, and the same condition sometimes arrives as a
// message inside a 200.
//
// The remedy is a smaller query — fewer entities per page — rather than another
// attempt at the same one, which is why it is a question of its own.
func (e *Error) TooHeavy() bool {
	if e.Status == http.StatusBadGateway || e.Status == http.StatusGatewayTimeout {
		return true
	}
	return mentions(e.Messages, "timeout", "Something went wrong while executing your query")
}

// query posts one GraphQL query and unmarshals data into out.
//
// Secondary rate limits answer with 403 and a Retry-After, and the primary
// limit answers with a reset timestamp; both are waited out rather than
// failed, up to a bounded number of attempts, because an import of a large
// repository will legitimately hit them.
//
// A 5xx is retried too, on a short backoff. GitHub answers a GraphQL query it
// could not finish inside its own time budget with a 502, and that is often
// weather rather than climate: the same page asked for again a second later
// comes back. When it is climate — a repository whose pages are genuinely too
// heavy — the retries are exhausted quickly and the caller is told, so that it
// can ask for less instead.
func (c *Client) query(q string, vars map[string]any, out any) error {
	body, err := json.Marshal(map[string]any{"query": q, "variables": vars})
	if err != nil {
		return err
	}

	const attempts = 5
	// Kept low deliberately: a caller that can shrink its query gets there
	// sooner, and one that cannot is not made to wait a minute to be told.
	const serverAttempts = 3
	server := 0

	for attempt := 0; ; attempt++ {
		req, err := http.NewRequest(http.MethodPost, c.Endpoint, bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "git-issue")
		if c.Token != "" {
			req.Header.Set("Authorization", "bearer "+c.Token)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		payload, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}

		if wait, ok := retryAfter(resp, c.now()); ok && attempt < attempts-1 {
			c.wait(wait)
			continue
		}
		if resp.StatusCode != http.StatusOK {
			// A non-200 still carries GraphQL's own account of what went wrong
			// often enough to be worth reading: "http 502" says nothing, while
			// the message inside it says the query timed out.
			e := &Error{Status: resp.StatusCode, Messages: graphQLMessages(payload)}
			if e.Retryable() && server < serverAttempts-1 && attempt < attempts-1 {
				server++
				c.wait(time.Duration(1<<server) * time.Second)
				continue
			}
			return e
		}

		var envelope struct {
			Data   json.RawMessage `json:"data"`
			Errors []struct {
				Message string `json:"message"`
				Type    string `json:"type"`
			} `json:"errors"`
		}
		if err := json.Unmarshal(payload, &envelope); err != nil {
			return fmt.Errorf("unparseable response: %w", err)
		}
		if len(envelope.Errors) > 0 {
			e := &Error{Status: resp.StatusCode}
			for _, m := range envelope.Errors {
				e.Messages = append(e.Messages, m.Message)
			}
			return e
		}
		if len(envelope.Data) == 0 {
			return fmt.Errorf("empty response")
		}
		return json.Unmarshal(envelope.Data, out)
	}
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// wait sleeps between attempts, through the client's own hook so that a test
// can drive a retry without spending the wait.
func (c *Client) wait(d time.Duration) {
	if c.Sleep != nil {
		c.Sleep(d)
		return
	}
	time.Sleep(d)
}

// notice reports something a person watching the import should know but that is
// not a failure — a page being asked for again, smaller. Silent where the caller
// wired up nothing to say it with.
func (c *Client) notice(format string, args ...any) {
	if c.Notice == nil {
		return
	}
	c.out.Lock()
	defer c.out.Unlock()
	c.Notice(fmt.Sprintf(format, args...))
}

// report is a caller's progress callback, under the same lock as notice so the
// two cannot write over each other.
func (c *Client) report(progress func(fetched, total int), fetched, total int) {
	if progress == nil {
		return
	}
	c.out.Lock()
	defer c.out.Unlock()
	progress(fetched, total)
}

// graphQLMessages is whatever GraphQL said in a response body that was not a
// 200. A body that is HTML, empty, or anything else this cannot read yields
// none: the status is still the answer, and a parse failure here must never
// replace it.
func graphQLMessages(payload []byte) []string {
	var envelope struct {
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(payload, &envelope); err != nil {
		return nil
	}
	var out []string
	for _, e := range envelope.Errors {
		if e.Message != "" {
			out = append(out, e.Message)
		}
	}
	return out
}

// retryAfter reports how long to wait before retrying, if the response says a
// rate limit was hit. The cap keeps a reset an hour away from turning into an
// hour-long silent stall.
func retryAfter(resp *http.Response, now time.Time) (time.Duration, bool) {
	const cap = 5 * time.Minute

	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return min(time.Duration(secs)*time.Second, cap), true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if secs, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if wait := time.Unix(secs, 0).Sub(now); wait > 0 {
				return min(wait, cap), true
			}
			return 0, true
		}
	}
	return 0, false
}
