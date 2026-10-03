// The HTTP layer: one request helper, one error type, the rate-limit wait, and
// the page walk.

package giteaapi

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Error is a failed API call, carrying enough to say something useful about
// why: an HTTP status, and the message Gitea returned in its body.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("%s (%d)", e.Message, e.Status)
	}
	switch e.Status {
	case http.StatusUnauthorized:
		return "not authenticated (401)"
	case http.StatusForbidden:
		return "forbidden (403)"
	case http.StatusNotFound:
		return "not found (404)"
	}
	return fmt.Sprintf("http %d", e.Status)
}

// Unauthorized reports whether the failure is about credentials rather than
// about the request. Gitea answers a request for a private repository the token
// cannot see with 404, so that is in here too.
func (e *Error) Unauthorized() bool {
	return e.Status == http.StatusUnauthorized ||
		e.Status == http.StatusForbidden ||
		e.Status == http.StatusNotFound
}

// Retryable reports whether the same request could succeed on a second attempt.
func (e *Error) Retryable() bool { return e.Status >= 500 }

// do sends one request and unmarshals the response into out.
//
// A saturated token answers with 429 and a Retry-After, which is waited out
// rather than failed on, up to a bounded number of attempts. A 5xx is retried
// on a short backoff. Everything else is returned.
func (c *Client) do(method, endpoint string, body, out any) error {
	var payload []byte
	if body != nil {
		var err error
		if payload, err = json.Marshal(body); err != nil {
			return err
		}
	}

	const attempts = 5
	server := 0
	for attempt := 0; ; attempt++ {
		var reader io.Reader
		if payload != nil {
			reader = bytes.NewReader(payload)
		}
		req, err := http.NewRequest(method, endpoint, reader)
		if err != nil {
			return err
		}
		if payload != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "git-issue")
		if c.Token != "" {
			req.Header.Set("Authorization", "token "+c.Token)
		}

		resp, err := c.HTTP.Do(req)
		if err != nil {
			return err
		}
		data, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			return readErr
		}

		if wait, ok := retryAfter(resp, c.now()); ok && attempt < attempts-1 {
			c.wait(wait)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			e := apiError(resp.StatusCode, data)
			if e.Retryable() && server < 2 && attempt < attempts-1 {
				server++
				c.wait(time.Duration(1<<server) * time.Second)
				continue
			}
			return e
		}
		if out == nil {
			return nil
		}
		if len(data) == 0 {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("unparseable response: %w", err)
		}
		return nil
	}
}

// get is do for a GET with a query string.
func (c *Client) get(path string, params url.Values, out any) error {
	endpoint := c.API + path
	if len(params) > 0 {
		endpoint += "?" + params.Encode()
	}
	return c.do(http.MethodGet, endpoint, nil, out)
}

// pageSize is what every paged walk asks for. 50 is Gitea's default maximum for
// an unprivileged token; asking for more is silently clamped to it, so a walk
// that assumed a larger page would stop one page early.
const pageSize = 50

// page reads one page of a list endpoint into out (a pointer to a slice) and
// reports the server's total when it gave one through X-Total-Count, and
// whether this was the last page.
//
// "Last" is three tests rather than one, because **not every Gitea endpoint
// paginates**. `/issues/{index}/comments` ignores `page` and `limit` outright
// and answers every request with the whole thread — confirmed empirically
// against Gitea 1.27.3 — so a walk that only stopped on a short page asked for
// page 2, 3, 4 … forever and appended the same comments each time. That is an
// unbounded read of a bounded thread, and on an issue with a few hundred
// comments it is what got the process OOM-killed. A short page still ends a
// walk; so does a page longer than the limit that was asked for, which can only
// mean the server ignored it and sent everything; so does having reached the
// total the server itself reported.
func (c *Client) page(path string, params url.Values, num int, out any) (total int, last bool, err error) {
	if params == nil {
		params = url.Values{}
	}
	params.Set("page", strconv.Itoa(num))
	params.Set("limit", strconv.Itoa(pageSize))

	req, err := http.NewRequest(http.MethodGet, c.API+path+"?"+params.Encode(), nil)
	if err != nil {
		return 0, true, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "git-issue")
	if c.Token != "" {
		req.Header.Set("Authorization", "token "+c.Token)
	}

	resp, err := c.HTTP.Do(req)
	if err != nil {
		return 0, true, err
	}
	data, readErr := io.ReadAll(resp.Body)
	if v := resp.Header.Get("X-Total-Count"); v != "" {
		total, _ = strconv.Atoi(v)
	}
	resp.Body.Close()
	if readErr != nil {
		return 0, true, readErr
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return 0, true, apiError(resp.StatusCode, data)
	}

	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return 0, true, fmt.Errorf("unparseable response: %w", err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return 0, true, fmt.Errorf("unparseable response: %w", err)
	}
	return total, lastPage(len(raw), num, total), nil
}

// lastPage decides whether a walk is over, given what one page returned.
func lastPage(got, num, total int) bool {
	return got != pageSize || (total > 0 && num*pageSize >= total)
}

// apiError turns a failure body into an Error. Gitea answers with a JSON
// envelope carrying a "message", but a reverse proxy or a login redirect can
// answer with anything, so an unparseable body degrades to the status alone.
func apiError(status int, body []byte) *Error {
	e := &Error{Status: status}
	var envelope struct {
		Message string `json:"message"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		e.Message = strings.TrimSpace(envelope.Message)
		if e.Message == "" && len(envelope.Errors) > 0 {
			e.Message = envelope.Errors[0].Message
		}
	}
	return e
}

// retryAfter reports how long to wait before retrying, when the response is a
// 429 saying a rate limit was hit. The cap keeps a distant reset from turning
// into a long silent stall. A 5xx is not handled here — do's own server-error
// backoff covers it.
func retryAfter(resp *http.Response, now time.Time) (time.Duration, bool) {
	const limit = 5 * time.Minute
	if resp.StatusCode != http.StatusTooManyRequests {
		return 0, false
	}
	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return min(time.Duration(secs)*time.Second, limit), true
		}
	}
	if secs, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
		if wait := time.Unix(secs, 0).Sub(now); wait > 0 {
			return min(wait, limit), true
		}
	}
	return 30 * time.Second, true
}
