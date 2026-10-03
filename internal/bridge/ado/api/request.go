// The HTTP layer: one request helper, one error type, and the rate-limit wait.

package adoapi

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
// why: an HTTP status, and the message Azure DevOps returned in its body.
type Error struct {
	Status  int
	Message string
	// TypeKey is Azure DevOps' own error identifier, which is how a specific
	// failure is recognised without matching on prose that varies by locale.
	TypeKey string
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
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
// about the request.
//
// A 203 is in here because Azure DevOps answers an unauthenticated browser
// request with a sign-in page and a 203 rather than a 401, and a client that
// treats that as success parses HTML as JSON and reports something baffling.
func (e *Error) Unauthorized() bool {
	return e.Status == http.StatusUnauthorized ||
		e.Status == http.StatusForbidden ||
		e.Status == http.StatusNonAuthoritativeInfo
}

// TooManyResults reports whether the failure is WIQL's cap on how many work
// items one query may return. It is the one API error a caller can act on, by
// narrowing the area or asking for a shorter window.
func (e *Error) TooManyResults() bool { return e.TypeKey == "VS402337" }

// do sends one request and unmarshals the response into out.
//
// A saturated account answers with Retry-After, and that is waited out rather
// than failed on, up to a bounded number of attempts: an import of a large
// project will legitimately hit the limit, and failing there would mean
// starting over.
func (c *Client) do(method, endpoint string, body any, out any) error {
	var payload []byte
	contentType := "application/json"
	switch b := body.(type) {
	case nil:
	case patchDocument:
		// JSON-Patch has its own media type, and Azure DevOps rejects a patch
		// document sent as plain JSON.
		contentType = "application/json-patch+json"
		var err error
		if payload, err = json.Marshal(b); err != nil {
			return err
		}
	default:
		var err error
		if payload, err = json.Marshal(b); err != nil {
			return err
		}
	}

	const attempts = 5
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
			req.Header.Set("Content-Type", contentType)
		}
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "git-issue")
		if auth := authorization(c.Token); auth != "" {
			req.Header.Set("Authorization", auth)
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
			time.Sleep(wait)
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 || resp.StatusCode == http.StatusNonAuthoritativeInfo {
			return apiError(resp.StatusCode, data)
		}
		if out == nil {
			return nil
		}
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("unparseable response: %w", err)
		}
		return nil
	}
}

// get is do for the common case, appending the api-version every call needs.
func (c *Client) get(endpoint string, params url.Values, out any) error {
	return c.do(http.MethodGet, withVersion(endpoint, params, APIVersion), nil, out)
}

// withVersion adds the query string and the api-version Azure DevOps requires
// on every request.
func withVersion(endpoint string, params url.Values, version string) string {
	if params == nil {
		params = url.Values{}
	}
	params.Set("api-version", version)
	sep := "?"
	if strings.Contains(endpoint, "?") {
		sep = "&"
	}
	return endpoint + sep + params.Encode()
}

// apiError turns a failure body into an Error. Azure DevOps answers with a
// JSON envelope carrying a message and a typeKey, but an install behind a
// proxy or a sign-in redirect can answer with anything at all, so an
// unparseable body degrades to the status alone rather than to an error about
// the error.
func apiError(status int, body []byte) error {
	e := &Error{Status: status}
	var envelope struct {
		Message string `json:"message"`
		TypeKey string `json:"typeKey"`
	}
	if err := json.Unmarshal(body, &envelope); err == nil {
		e.Message, e.TypeKey = envelope.Message, envelope.TypeKey
	}
	return e
}

func (c *Client) now() time.Time {
	if c.Now != nil {
		return c.Now()
	}
	return time.Now()
}

// retryAfter reports how long to wait before retrying, if the response says a
// rate limit was hit. The cap keeps a reset an hour away from turning into an
// hour-long silent stall.
func retryAfter(resp *http.Response, now time.Time) (time.Duration, bool) {
	const limit = 5 * time.Minute

	if v := resp.Header.Get("Retry-After"); v != "" {
		if secs, err := strconv.Atoi(v); err == nil && secs >= 0 {
			return min(time.Duration(secs)*time.Second, limit), true
		}
	}
	if resp.Header.Get("X-RateLimit-Remaining") == "0" {
		if secs, err := strconv.ParseInt(resp.Header.Get("X-RateLimit-Reset"), 10, 64); err == nil {
			if wait := time.Unix(secs, 0).Sub(now); wait > 0 {
				return min(wait, limit), true
			}
			return 0, true
		}
	}
	return 0, false
}

// patchDocument is a JSON-Patch body. Its own type, so that do can recognise
// it and send the media type Azure DevOps requires for one.
type patchDocument []patchOp

// patchOp is one operation in a JSON-Patch document.
type patchOp struct {
	Op    string `json:"op"`
	Path  string `json:"path"`
	Value any    `json:"value,omitempty"`
}
