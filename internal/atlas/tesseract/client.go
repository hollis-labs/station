// Package tesseract is the Atlas server's read-side client for Tesseract's
// HTTP API. It is deliberately narrow: the two things Atlas reads (recall
// over the Atlas namespaces, and one current record by key) plus namespace
// listing and a health probe.
//
// The request shapes here are the HTTP door's and differ from the MCP tools'
// (see Tangent's internal/plugins/tesseract for the longer account): fields
// are real JSON values rather than JSON-encoded strings, and the recall route
// rejects fields it does not know. Recall goes through POST /v1/memory/recall
// and not GET /v1/recall because only the POST route returns a manifest with
// results_total and next_cursor; GET /v1/recall silently ignores a cursor, so a
// complete read through it can only guess a limit. The POST route also
// expands a trailing `/*` in a namespace, which GET does not.
package tesseract

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// DefaultBaseURL is where Tesseract's API service listens on this machine.
// It answers on loopback without a credential.
const DefaultBaseURL = "http://127.0.0.1:8089"

const (
	requestTimeout   = 20 * time.Second
	maxResponseBytes = 64 << 20
	defaultPageLimit = 500
)

var (
	// ErrNotFound is matched by errors.Is for a 404 / not_found answer.
	ErrNotFound = errors.New("tesseract: not found")
	// ErrUnavailable is matched by errors.Is when Tesseract could not be
	// reached or answered something unusable: a tool result can then say
	// "Tesseract is down" rather than "something failed".
	ErrUnavailable = errors.New("tesseract: unavailable")
)

// APIError is Tesseract's own {code, message} refusal, carried through so its
// validation messages (which name the offending field) reach the caller.
type APIError struct {
	Status  int
	Code    string
	Message string
	Path    string
}

func (e *APIError) Error() string {
	if e.Code != "" {
		return fmt.Sprintf("tesseract refused %s (%d): %s: %s", e.Path, e.Status, e.Code, e.Message)
	}
	return fmt.Sprintf("tesseract refused %s (%d): %s", e.Path, e.Status, e.Message)
}

// Is makes a not-found answer match ErrNotFound.
func (e *APIError) Is(target error) bool {
	return target == ErrNotFound && (e.Status == http.StatusNotFound || e.Code == "not_found")
}

// Revision is one Tesseract revision as the read routes return it. `data` and
// `consumer_state` stay raw: what they mean belongs to Atlas, not to this
// package.
type Revision struct {
	RevisionID string   `json:"revision_id"`
	ItemID     string   `json:"item_id"`
	Domain     string   `json:"domain"`
	Namespace  string   `json:"namespace"`
	MemoryKey  string   `json:"memory_key"`
	Status     string   `json:"status"`
	Supersedes string   `json:"supersedes"`
	CreatedAt  string   `json:"created_at"`
	SessionID  string   `json:"session_id"`
	Confidence float64  `json:"confidence"`
	Tags       []string `json:"tags"`
	Author     struct {
		AgentID      string `json:"agent_id"`
		AgentVersion string `json:"agent_version"`
	} `json:"author"`
	Payload struct {
		Summary string          `json:"summary"`
		Body    string          `json:"body"`
		Data    json.RawMessage `json:"data"`
	} `json:"payload"`
	Facets struct {
		Kind    string `json:"kind"`
		Source  string `json:"source"`
		Pointer *struct {
			Scheme  string `json:"scheme"`
			Locator string `json:"locator"`
		} `json:"pointer"`
	} `json:"facets"`
	ConsumerState json.RawMessage `json:"consumer_state"`
}

// RecallRequest is one recall as Atlas issues it.
type RecallRequest struct {
	Namespaces  []string `json:"namespaces"`
	Ranking     string   `json:"ranking,omitempty"`
	Query       string   `json:"query,omitempty"`
	SearchMode  string   `json:"search_mode,omitempty"`
	Limit       int      `json:"limit,omitempty"`
	PayloadMode string   `json:"payload_mode,omitempty"`
	Cursor      string   `json:"cursor,omitempty"`
}

// Manifest is recall's own account of what it did and did not return. Every
// field is always present in the response: truncated=false means everything
// matched came back, and completeness is never inferred from the length of
// the results.
type Manifest struct {
	ResultsTotal     int    `json:"results_total"`
	ResultsReturned  int    `json:"results_returned"`
	Truncated        bool   `json:"truncated"`
	TruncationReason string `json:"truncation_reason"`
	NextCursor       string `json:"next_cursor"`
}

// Result is one recall hit.
type Result struct {
	Revision Revision `json:"revision"`
	Score    *float64 `json:"score"`
}

// RecallPage is one page of a recall.
type RecallPage struct {
	Results  []Result `json:"results"`
	Manifest Manifest `json:"manifest"`
}

// Client talks to one Tesseract.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// New builds a client. An empty baseURL means DefaultBaseURL; an empty token
// means no Authorization header, which is the local service's normal state.
func New(baseURL, token string) *Client {
	if baseURL == "" {
		baseURL = DefaultBaseURL
	}
	return &Client{baseURL: strings.TrimSuffix(baseURL, "/"), token: token, http: &http.Client{Timeout: requestTimeout}}
}

// BaseURL is the Tesseract this client points at.
func (c *Client) BaseURL() string { return c.baseURL }

// Recall issues one recall and returns the page with its manifest.
func (c *Client) Recall(ctx context.Context, req RecallRequest) (RecallPage, error) {
	var page RecallPage
	err := c.do(ctx, http.MethodPost, "/v1/memory/recall", nil, req, &page)
	return page, err
}

// RecallAll pages a recall to exhaustion by following manifest.next_cursor,
// and reports whether it got everything. It stops at maxRecords rather than
// paging without bound; in that case complete is false and the caller must
// say so instead of presenting the result as the whole set.
func (c *Client) RecallAll(ctx context.Context, req RecallRequest, maxRecords int) (revisions []Revision, complete bool, err error) {
	if req.Limit == 0 {
		req.Limit = defaultPageLimit
	}
	for {
		page, err := c.Recall(ctx, req)
		if err != nil {
			return nil, false, err
		}
		for _, r := range page.Results {
			revisions = append(revisions, r.Revision)
		}
		if page.Manifest.NextCursor == "" {
			return revisions, true, nil
		}
		if len(revisions) >= maxRecords {
			return revisions, false, nil
		}
		req.Cursor = page.Manifest.NextCursor
	}
}

// GetCurrent returns the current revision of one knowledge record. A missing
// key is an error matching ErrNotFound.
func (c *Client) GetCurrent(ctx context.Context, namespace, key string) (Revision, error) {
	q := url.Values{"namespace": {namespace}, "key": {key}}
	var rev Revision
	err := c.do(ctx, http.MethodGet, "/v1/knowledge/current", q, nil, &rev)
	return rev, err
}

// ListNamespaces returns the registered namespaces under prefix.
func (c *Client) ListNamespaces(ctx context.Context, prefix string) ([]string, error) {
	var out []string
	cursor := ""
	for {
		q := url.Values{"prefix": {prefix}, "limit": {"200"}}
		if cursor != "" {
			q.Set("cursor", cursor)
		}
		var body struct {
			Items []struct {
				Namespace string `json:"namespace"`
			} `json:"items"`
			Truncated  bool   `json:"truncated"`
			NextCursor string `json:"next_cursor"`
		}
		if err := c.do(ctx, http.MethodGet, "/v1/namespaces/list", q, nil, &body); err != nil {
			return nil, err
		}
		for _, it := range body.Items {
			out = append(out, it.Namespace)
		}
		if !body.Truncated || body.NextCursor == "" {
			return out, nil
		}
		cursor = body.NextCursor
	}
}

// Health reports whether Tesseract answers at all. The readiness route is
// public, so this separates "the service is down" from "this caller is not
// authorized".
func (c *Client) Health(ctx context.Context) error {
	var ignored json.RawMessage
	return c.do(ctx, http.MethodGet, "/v1/health/readiness", nil, nil, &ignored)
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	var payload io.Reader
	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return fmt.Errorf("tesseract: encode request: %w", err)
		}
		payload = bytes.NewReader(encoded)
	}
	target := c.baseURL + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, target, payload)
	if err != nil {
		return fmt.Errorf("tesseract: build request: %w", err)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("%w: %s: %w", ErrUnavailable, c.baseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	if err != nil {
		return fmt.Errorf("%w: reading %s: %w", ErrUnavailable, path, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		apiErr := &APIError{Status: resp.StatusCode, Path: path}
		var parsed struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		}
		if json.Unmarshal(raw, &parsed) == nil && parsed.Message != "" {
			apiErr.Code, apiErr.Message = parsed.Code, parsed.Message
		} else if text := strings.TrimSpace(string(raw)); text != "" {
			apiErr.Message = truncate(text, 300)
		} else {
			apiErr.Message = http.StatusText(resp.StatusCode)
		}
		return apiErr
	}
	if out == nil {
		return nil
	}
	if err := json.Unmarshal(raw, out); err != nil {
		return fmt.Errorf("%w: decoding %s: %w", ErrUnavailable, path, err)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
