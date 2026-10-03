// Package adoapi is an Azure DevOps client: enough of the API to import and
// write back work items, and nothing else.
//
// It is a leaf, like internal/gitx and internal/bridge/github/api. It knows what Azure
// DevOps returns and has never heard of an event, a fold, or an issue —
// mapping what it returns onto the tracker's vocabulary is
// internal/bridge/ado/issue's job, and keeping the two apart is what lets the
// mapping be tested against recorded responses with no network at all.
//
// It speaks REST, because Azure DevOps has no GraphQL API. The cost is one
// request per work item for history and another for comments, which is why the
// two batching opportunities the API does offer are taken: WIQL returns ids
// only, and workitemsbatch reads 200 of them at a time.
//
// One package handles both Azure DevOps Services and Azure DevOps Server. The
// difference is entirely in the base URL — an on-prem install is on its own
// host, often under a virtual directory — and ParseTarget resolves that from
// the clone URL, so nothing above here has to know which it is talking to.
package adoapi

import (
	"encoding/base64"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/hdweiss/git-issue/internal/gitx"
)

// PublicHost is where Azure DevOps Services lives. Anything else is either the
// legacy *.visualstudio.com form or somebody's own Azure DevOps Server.
const PublicHost = "dev.azure.com"

// APIVersion is the REST version every call names. Azure DevOps requires one
// on every request and changes behaviour without it.
const APIVersion = "7.1"

// gitSegment is the marker in an Azure DevOps clone URL that separates the
// project from the repository. Everything to its left locates the project, and
// finding it is what lets one rule cover a hosted organization and an on-prem
// collection under a virtual directory alike.
const gitSegment = "/_git/"

// Target is the project an import reads from, and the area within it.
type Target struct {
	// Base is scheme://host, plus the virtual directory when the install has
	// one: "https://dev.azure.com", or "https://corp.local/tfs".
	Base       string
	Collection string
	Project    string

	// Repo is the git repository named by the clone URL's "/_git/<repo>"
	// segment, or "" when the target was a bare collection/project slug. Work
	// items are a project-wide resource and never look at it; pull requests are
	// per-repository and cannot be read without it.
	Repo string

	// Area is the area path below the project root, "/"-separated and without
	// the project itself, or "" for the whole project. It is scope rather than
	// state: see "The area is scope, and only scope" in docs/bridge-ado.md.
	Area string
}

// API is the root every REST call hangs off.
func (t Target) API() string {
	return t.Base + "/" + t.Collection + "/" + t.Project + "/_apis"
}

// String is the tracker name: the ledger path, and the key a watermark is
// filed under. It drops the scheme so that the same project reached over http
// in a test and https in life is one tracker, and keeps the virtual directory
// because two collections can differ by nothing else.
func (t Target) String() string {
	return strings.TrimSuffix(host(t.Base)+"/"+t.Collection+"/"+t.Project, "/")
}

// Key identifies the collection, and is what every nonce is derived from.
//
// The collection rather than the project, because a work item id is unique per
// collection and survives being moved between projects — keying identity on
// the project would fork such an item into two entities. See "Identity, and
// where it is recorded" in docs/bridge-ado.md.
func (t Target) Key() string {
	return strings.ToLower(host(t.Base) + "/" + t.Collection)
}

// AreaPath is the area as Azure DevOps spells it: rooted at the project and
// separated by backslashes. Empty Area means the project root, which is the
// whole project.
func (t Target) AreaPath() string {
	if t.Area == "" {
		return t.Project
	}
	return t.Project + `\` + strings.ReplaceAll(strings.Trim(t.Area, "/"), "/", `\`)
}

// PullRequestAPI is the root every pull request REST call hangs off: the git
// repository's own _apis path. Empty Repo yields a path the server rejects,
// which is caught before any call is made.
func (t Target) PullRequestAPI() string {
	return t.API() + "/git/repositories/" + t.Repo
}

// The identity strings recorded on the origin ledger. Each is the nonce input
// for the event that creates the entity and the key the ledger files it under,
// so the two cannot drift.
//
// WorkItemOrigin keys on the collection alone, because a work item id is unique
// per collection and survives being moved between projects. A pull request id
// is unique only per repository, so its origin carries the repository too. A
// review that closes a work item resolves the link through WorkItemOrigin,
// which is why it lives here rather than in one bridge.

func (t Target) WorkItemOrigin(id int) string {
	return fmt.Sprintf("ado:%s#%d", t.Key(), id)
}

func (t Target) PullRequestOrigin(id int) string {
	return fmt.Sprintf("ado:%s/%s/pullRequests/%d", t.Key(), t.Repo, id)
}

func (t Target) PullRequestThreadOrigin(prID, threadID int) string {
	return fmt.Sprintf("%s/threads/%d", t.PullRequestOrigin(prID), threadID)
}

func (t Target) PullRequestCommentOrigin(prID, threadID, commentID int) string {
	return fmt.Sprintf("%s/threads/%d/comments/%d", t.PullRequestOrigin(prID), threadID, commentID)
}

// PullRequestWebURL is where a pull request can be opened in a browser.
func (t Target) PullRequestWebURL(id int) string {
	return fmt.Sprintf("%s/%s/%s/_git/%s/pullrequest/%d", t.Base, t.Collection, t.Project, t.Repo, id)
}

// WorkItemURL is how a link names the work item at its far end: an API address
// rather than a browser one. Azure DevOps returns links spelled against the
// collection and accepts them spelled against the project, and reading one only
// ever takes the id off the end, so the two forms are interchangeable.
func (t Target) WorkItemURL(id int) string {
	return fmt.Sprintf("%s/wit/workItems/%d", t.API(), id)
}

// WebURL is where a work item can be opened in a browser. It is a locator, not
// an identity: an item moved between projects keeps its id and gets a new URL.
func (t Target) WebURL(id int) string {
	return fmt.Sprintf("%s/%s/%s/_workitems/edit/%d", t.Base, t.Collection, t.Project, id)
}

// host strips the scheme from a base URL, keeping any virtual directory.
func host(base string) string {
	if _, after, ok := strings.Cut(base, "://"); ok {
		return after
	}
	return base
}

// ParseTarget resolves a project from a remote URL or a collection/project
// slug.
//
// Every accepted form is anchored on the same landmark. An Azure DevOps clone
// URL is <base>/<collection>/<project>/_git/<repo>, so splitting on "/_git/"
// gives the collection and project as the last two segments to its left and
// the base — virtual directory included — as everything before them. That one
// rule covers a hosted organization, an on-prem collection under a virtual
// directory, and the legacy *.visualstudio.com form, which differs only in
// carrying its collection in the hostname instead of the path.
//
// The scp-style SSH form has no "/_git/" and is recognised by its "v3/" marker
// instead.
func ParseTarget(s string) (Target, error) {
	raw := strings.TrimSpace(s)
	raw = strings.TrimSuffix(strings.TrimSuffix(raw, "/"), ".git")
	if raw == "" {
		return Target{}, fmt.Errorf("empty project")
	}

	if t, ok := parseSSH(raw); ok {
		return t, nil
	}

	scheme, rest := "https", raw
	if before, after, ok := strings.Cut(raw, "://"); ok {
		// ssh:// and git:// say nothing about how the API is reached; only an
		// explicit http URL does, and an on-prem install reachable only over
		// plain http is unusual but real.
		if before == "http" {
			scheme = before
		}
		rest = after
	}

	// A slug carries no host at all, so it means the hosted service.
	if !strings.Contains(rest, gitSegment) && !strings.Contains(rest, ".") {
		collection, project, ok := strings.Cut(strings.Trim(rest, "/"), "/")
		if !ok || collection == "" || project == "" || strings.Contains(project, "/") {
			return Target{}, fmt.Errorf("%q is not a collection/project", s)
		}
		return Target{Base: "https://" + PublicHost, Collection: collection, Project: project}, nil
	}

	left, right, ok := strings.Cut(rest, gitSegment)
	if !ok {
		return Target{}, fmt.Errorf("%q is not an Azure DevOps repository URL: no %s segment", s, strings.Trim(gitSegment, "/"))
	}
	repo := strings.Trim(right, "/")
	if slash := strings.IndexByte(repo, '/'); slash >= 0 {
		repo = repo[:slash]
	}
	segments := strings.Split(strings.Trim(left, "/"), "/")
	// Strip any userinfo; https clone URLs often carry the organization there.
	if _, after, cut := strings.Cut(segments[0], "@"); cut {
		segments[0] = after
	}
	if len(segments) < 2 {
		return Target{}, fmt.Errorf("%q names no project", s)
	}

	hostname := segments[0]
	switch len(segments) {
	case 2:
		// <host>/<project>/_git/<repo>: the *.visualstudio.com form, where the
		// collection is the hostname's first label rather than a path segment.
		collection, _, _ := strings.Cut(hostname, ".")
		if collection == "" {
			return Target{}, fmt.Errorf("%q names no collection", s)
		}
		return Target{Base: scheme + "://" + hostname, Collection: collection, Project: segments[1], Repo: repo}, nil
	default:
		// Everything else: the last two segments are the collection and the
		// project, and whatever precedes them is a virtual directory that
		// belongs to the base URL.
		project := segments[len(segments)-1]
		collection := segments[len(segments)-2]
		base := scheme + "://" + strings.Join(segments[:len(segments)-2], "/")
		return Target{Base: base, Collection: collection, Project: project, Repo: repo}, nil
	}
}

// parseSSH handles git@ssh.dev.azure.com:v3/<collection>/<project>/<repo>,
// which is the one form carrying no "/_git/" segment.
func parseSSH(raw string) (Target, bool) {
	rest := raw
	if _, after, ok := strings.Cut(rest, "://"); ok {
		rest = after
	}
	if _, after, ok := strings.Cut(rest, "@"); ok {
		rest = after
	}
	_, after, ok := strings.Cut(rest, ":v3/")
	if !ok {
		if _, a, o := strings.Cut(rest, "/v3/"); o {
			after, ok = a, true
		}
	}
	if !ok {
		return Target{}, false
	}
	segments := strings.Split(strings.Trim(after, "/"), "/")
	if len(segments) < 2 || segments[0] == "" || segments[1] == "" {
		return Target{}, false
	}
	repo := ""
	if len(segments) > 2 {
		repo = segments[2]
	}
	return Target{Base: "https://" + PublicHost, Collection: segments[0], Project: segments[1], Repo: repo}, true
}

// TokenSource reports where a token came from, for error messages that can
// tell "you have no token" apart from "your token is not allowed to see this".
type TokenSource string

const (
	FromFlag       TokenSource = "--token"
	FromCLI        TokenSource = "az"
	FromCredential TokenSource = "git credential"
	FromNowhere    TokenSource = ""
)

// adoResource is the Entra application id of Azure DevOps. An access token has
// to be issued for it specifically; a token for any other resource is rejected.
const adoResource = "499b84ac-1321-427f-aa17-267ca6975798"

// Token finds a credential for a host.
//
// The order matches ghapi's, and for the same reasons. The az CLI runs before
// git's credential helpers because those can fall back to an interactive
// terminal prompt, while an az that is not installed or not logged in simply
// returns nothing and lets the helpers have their turn. Nothing here stores a
// token; approving one that worked is the caller's business, and is the other
// half of the contract `git credential fill` starts.
func Token(repo *gitx.Repo, host, flag string) (string, TokenSource, *gitx.Credential) {
	if flag != "" {
		return flag, FromFlag, nil
	}
	// AZURE_DEVOPS_EXT_PAT is what the az devops extension itself reads;
	// SYSTEM_ACCESSTOKEN is what a pipeline exposes to its own steps.
	for _, key := range []string{"AZURE_DEVOPS_EXT_PAT", "AZURE_DEVOPS_PAT", "SYSTEM_ACCESSTOKEN"} {
		if v := os.Getenv(key); v != "" {
			return v, TokenSource(key), nil
		}
	}
	if token := azToken(); token != "" {
		return token, FromCLI, nil
	}
	if cred, err := repo.CredentialFill("https", host); err == nil {
		return cred.Password, FromCredential, &cred
	}
	return "", FromNowhere, nil
}

// azToken asks the Azure CLI for an access token scoped to Azure DevOps.
//
// It is invoked here rather than through gitx because gitx runs git and only
// git; a second binary is this package's business.
func azToken() string {
	out, err := exec.Command("az", "account", "get-access-token",
		"--resource", adoResource, "--query", "accessToken", "-o", "tsv").Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// authorization is the header value for a credential.
//
// Azure DevOps takes a personal access token as HTTP basic auth with an empty
// username, and an Entra access token as a bearer. The two are told apart by
// shape rather than by asking the caller to say which they have: an access
// token is a JWT, three base64 segments separated by dots, and a PAT is a
// single opaque string that never is.
func authorization(token string) string {
	if token == "" {
		return ""
	}
	if isJWT(token) {
		return "Bearer " + token
	}
	return "Basic " + base64.StdEncoding.EncodeToString([]byte(":"+token))
}

func isJWT(token string) bool {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return false
	}
	for _, p := range parts {
		if p == "" {
			return false
		}
	}
	// A JWT's header always decodes to JSON beginning with '{'.
	head, err := base64.RawURLEncoding.DecodeString(parts[0])
	return err == nil && strings.HasPrefix(string(head), "{")
}

// Client talks to one Azure DevOps collection.
type Client struct {
	Token string
	HTTP  *http.Client

	// Now is overridable so rate-limit waiting can be tested without one.
	Now func() time.Time
}

// New builds a client.
func New(token string) *Client {
	return &Client{
		Token: token,
		HTTP:  &http.Client{Timeout: 60 * time.Second},
		Now:   time.Now,
	}
}
