// Package client is a small HTTP client for the botchat API, shared by the
// command-line helpers that talk to a running server (`botchat hook stop`,
// `botchat listen`).
package client

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// DefaultURL is the default botchat server URL.
const DefaultURL = "http://127.0.0.1:7878"

// MaxWait is the longest timeout the server accepts for one wait (API-7).
const MaxWait = 120 * time.Second

// PollTimeout is the timeout helpers use for each long poll that should keep
// an agent online. The server marks the agent active only when a request
// starts, and presence lasts 60 seconds (AGT-4), so it must be shorter than
// that. A variable so tests can shorten it.
var PollTimeout = 45 * time.Second

// Message is the subset of a botchat message the helpers use.
type Message struct {
	ID       uint64 `json:"id"`
	Channel  string `json:"channel"`
	ThreadID uint64 `json:"thread_id"`
	From     string `json:"from"`
	Text     string `json:"text"`
}

// StatusError is returned for a non-200, non-404 response.
type StatusError struct {
	Path   string
	Code   int
	Status string
	Body   string
}

func (e *StatusError) Error() string {
	return fmt.Sprintf("GET %s: %s: %s", e.Path, e.Status, e.Body)
}

// Client talks to one botchat server.
type Client struct {
	URL   string // without a trailing slash
	Token string
	HTTP  *http.Client
}

// New returns a client for the server at baseURL.
func New(baseURL, token string) *Client {
	return &Client{URL: strings.TrimRight(baseURL, "/"), Token: token, HTTP: &http.Client{}}
}

// Get fetches path and decodes the JSON body into v. A 404 returns the status
// and no error, leaving v untouched.
func (c *Client) Get(ctx context.Context, path string, q url.Values, v any) (int, error) {
	u := c.URL + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return 0, err
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	res, err := c.HTTP.Do(req)
	if err != nil {
		return 0, fmt.Errorf("cannot reach botchat at %s: %w", c.URL, err)
	}
	defer res.Body.Close()
	if res.StatusCode == http.StatusNotFound {
		return res.StatusCode, nil
	}
	if res.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(res.Body, 512))
		return res.StatusCode, &StatusError{Path: path, Code: res.StatusCode, Status: res.Status, Body: strings.TrimSpace(string(body))}
	}
	return res.StatusCode, json.NewDecoder(res.Body).Decode(v)
}

// LastMessageID returns the ID of the agent's most recent message (AGT-6), or
// 0 if the agent is unknown or has never posted.
func (c *Client) LastMessageID(ctx context.Context, agent string) (uint64, error) {
	var a struct {
		LastMessageID uint64 `json:"last_message_id"`
	}
	status, err := c.Get(ctx, "/api/v1/agents/"+url.PathEscape(agent), nil, &a)
	if err != nil || status == http.StatusNotFound {
		return 0, err
	}
	return a.LastMessageID, nil
}

// WaitQuery describes one long-poll of GET /api/v1/wait (API-7).
type WaitQuery struct {
	Agent   string // excluded as a sender, marked active, and mentioned unless All
	All     bool
	After   *uint64 // nil: the server's latest message
	Timeout time.Duration
}

// WaitResult is the response of GET /api/v1/wait.
type WaitResult struct {
	Messages []Message `json:"messages"`
	Cursor   uint64    `json:"cursor"`
}

// Wait long-polls once. Timeout is clamped to [0, MaxWait].
func (c *Client) Wait(ctx context.Context, w WaitQuery) (WaitResult, error) {
	q := url.Values{
		"timeout": {strconv.FormatFloat(max(0, min(w.Timeout, MaxWait)).Seconds(), 'f', 3, 64)},
	}
	if w.After != nil {
		q.Set("after", strconv.FormatUint(*w.After, 10))
	}
	if w.Agent != "" {
		q.Set("exclude_from", w.Agent)
		q.Set("agent", w.Agent)
		if !w.All {
			q.Set("mention", w.Agent)
		}
	}
	var res WaitResult
	_, err := c.Get(ctx, "/api/v1/wait", q, &res)
	return res, err
}
