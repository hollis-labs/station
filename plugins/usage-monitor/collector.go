package usagemonitor

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/hollis-labs/libs/llm/core/usageledger"
	"github.com/hollis-labs/libs/security/secrets"
	llmgateway "github.com/hollis-labs/station/plugins/llm-gateway"
)

const maxResponse = 4 << 20

// Source authority comes only from local bootstrap configuration. The HTTP
// management API cannot choose executable paths, remote hosts or secret refs.
type Source struct {
	ID               string `yaml:"id" json:"id"`
	AccountID        string `yaml:"account_id" json:"account_id"`
	Collector        string `yaml:"collector" json:"collector"`
	CredentialRef    string `yaml:"credential_ref" json:"-"`
	CredentialFormat string `yaml:"credential_format" json:"-"` // token, claude, codex
	AccountHeader    string `yaml:"account_header" json:"-"`
	GatewayURL       string `yaml:"gateway_url" json:"-"`
	Executable       string `yaml:"executable" json:"-"`
	Principal        string `yaml:"principal" json:"principal,omitempty"`
}

func (s Source) validate() error {
	if !identifier.MatchString(s.ID) || !identifier.MatchString(s.AccountID) {
		return errors.New("invalid source identity")
	}
	switch s.Collector {
	case "push":
		if s.Principal == "" {
			return errors.New("push source requires a principal")
		}
	case "claude", "codex", "opencode-go", "gateway":
		if _, err := secrets.ParseRef(s.CredentialRef); err != nil {
			return errors.New("HTTP source requires a valid credential_ref")
		}
	case "agy":
		if !filepath.IsAbs(s.Executable) {
			return errors.New("agy source requires an absolute executable path")
		}
	default:
		return errors.New("unsupported collector")
	}
	switch s.CredentialFormat {
	case "", "token", "claude", "codex":
	default:
		return errors.New("unsupported credential format")
	}
	if strings.ContainsAny(s.AccountHeader, "\r\n") {
		return errors.New("invalid account header")
	}
	if s.Collector == "gateway" {
		u, err := url.Parse(s.GatewayURL)
		if err != nil || u.Scheme != "https" || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
			return errors.New("gateway_url must be HTTPS without credentials, query or fragment")
		}
	}
	return nil
}

type collector struct {
	source  Source
	store   *Store
	resolve func(context.Context, string) (string, error)
	client  *http.Client
	mu      sync.Mutex
}

func newID() string             { var b [16]byte; _, _ = rand.Read(b[:]); return hex.EncodeToString(b[:]) }
func number(v float64) *float64 { return &v }
func percentWindow(id string, used *float64, reset *time.Time) Window {
	w := Window{ID: id, Unit: "percent", Used: used, Limit: number(100), ResetsAt: reset}
	if used != nil {
		w.Remaining = number(100 - *used)
	}
	return w
}

// Collect keeps one in-flight read per source. Provider errors are persisted as
// safe categories, never response bodies, credential values or command output.
func (c *collector) Collect(ctx context.Context) error {
	if !c.mu.TryLock() {
		return errors.New("collection_busy")
	}
	defer c.mu.Unlock()
	a, err := c.store.Account(ctx, c.source.AccountID)
	if err != nil {
		return err
	}
	if !a.Enabled {
		return ErrDisabled
	}
	ctx, cancel := context.WithTimeout(ctx, 95*time.Second)
	defer cancel()
	at := time.Now().UTC()
	batch, code := c.fetch(ctx, a, at)
	if code != "" {
		batch = Batch{Observations: []Observation{{ID: newID(), ObservedAt: at, Status: "error", ErrorCode: code, Windows: []Window{}}}}
	}
	// A timed-out request still leaves a failure observation. Keep this write
	// bounded independently of the failed/cancelled upstream request.
	writeCtx, writeCancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer writeCancel()
	if err = c.store.Ingest(writeCtx, a.ID, c.source.ID, "collector:"+c.source.ID, batch); err != nil {
		return err
	}
	if code != "" {
		return errors.New(code)
	}
	return nil
}

func (c *collector) fetch(ctx context.Context, a Account, at time.Time) (Batch, string) {
	if c.source.Collector == "gateway" {
		return c.fetchGateway(ctx, a, at)
	}
	var raw []byte
	if c.source.Collector == "agy" {
		cmd := exec.CommandContext(ctx, c.source.Executable, "-p", "/usage", "--output-format", "json")
		cmd.WaitDelay = 2 * time.Second
		out := &boundedBuffer{limit: 1 << 20}
		cmd.Stdout = out
		cmd.Stderr = io.Discard
		if err := cmd.Run(); err != nil {
			return Batch{}, "command_failed"
		}
		raw = out.Bytes()
	} else {
		secret, err := c.resolve(ctx, c.source.CredentialRef)
		if err != nil {
			return Batch{}, "credential_unavailable"
		}
		token, account, err := credential(secret, c.source.CredentialFormat)
		if err != nil {
			return Batch{}, "credential_invalid"
		}
		endpoint := ""
		switch c.source.Collector {
		case "claude":
			endpoint = "https://api.anthropic.com/api/oauth/usage"
		case "codex":
			endpoint = "https://chatgpt.com/backend-api/wham/usage"
		case "opencode-go":
			endpoint = "https://opencode.ai/zen/go/v1/usage"
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return Batch{}, "request_invalid"
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "Station-Usage-Monitor/1")
		if c.source.Collector == "claude" {
			req.Header.Set("anthropic-beta", "oauth-2025-04-20")
		}
		if c.source.AccountHeader != "" {
			account = c.source.AccountHeader
		}
		if c.source.Collector == "codex" && account != "" {
			req.Header.Set("ChatGPT-Account-Id", account)
		}
		resp, err := c.client.Do(req)
		if err != nil {
			return Batch{}, "transport_error"
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			switch resp.StatusCode {
			case 401, 403:
				return Batch{}, "authentication_required"
			case 429:
				return Batch{}, "rate_limited"
			default:
				return Batch{}, "upstream_error"
			}
		}
		raw, err = io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		if err != nil || len(raw) > maxResponse {
			return Batch{}, "response_too_large_or_unreadable"
		}
	}
	windows, err := parseWindows(c.source.Collector, raw, at)
	if err != nil {
		return Batch{}, "unsupported_response"
	}
	o := Observation{ID: newID(), ObservedAt: at, Status: "ok", Windows: windows}
	known := false
	for _, w := range windows {
		if w.Used != nil || w.Remaining != nil {
			known = true
		}
	}
	if !known {
		o.Status = "unknown"
		o.ErrorCode = "quota_unavailable"
		o.Windows = []Window{}
	}
	if err = o.validate(); err != nil {
		return Batch{}, "invalid_measurement"
	}
	return Batch{Observations: []Observation{o}}, ""
}

func credential(raw, format string) (string, string, error) {
	token, account := strings.TrimSpace(raw), ""
	switch format {
	case "claude":
		var d struct {
			OAuth struct {
				Token string `json:"accessToken"`
			} `json:"claudeAiOauth"`
		}
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			return "", "", err
		}
		token = d.OAuth.Token
	case "codex":
		var d struct {
			Tokens struct {
				Token   string `json:"access_token"`
				Account string `json:"account_id"`
			} `json:"tokens"`
		}
		if err := json.Unmarshal([]byte(raw), &d); err != nil {
			return "", "", err
		}
		token, account = d.Tokens.Token, d.Tokens.Account
	}
	if token == "" || strings.ContainsAny(token+account, "\r\n") {
		return "", "", errors.New("invalid credential")
	}
	return token, account, nil
}

type boundedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *boundedBuffer) Write(p []byte) (int, error) {
	if b.Len()+len(p) > b.limit {
		return 0, errors.New("output limit")
	}
	return b.Buffer.Write(p)
}

func parseWindows(kind string, raw []byte, at time.Time) ([]Window, error) {
	out := []Window{}
	switch kind {
	case "claude":
		var d map[string]json.RawMessage
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		for _, key := range []string{"five_hour", "seven_day", "seven_day_sonnet", "seven_day_opus", "seven_day_oauth_apps", "seven_day_cowork"} {
			v, ok := d[key]
			if !ok || string(v) == "null" {
				continue
			}
			var w struct {
				Utilization *float64   `json:"utilization"`
				Reset       *time.Time `json:"resets_at"`
			}
			if err := json.Unmarshal(v, &w); err != nil {
				return nil, err
			}
			out = append(out, percentWindow(key, w.Utilization, w.Reset))
		}
	case "codex":
		type cw struct {
			Used  *float64 `json:"used_percent"`
			Reset *int64   `json:"reset_at"`
		}
		type limits struct {
			Primary   *cw `json:"primary_window"`
			Secondary *cw `json:"secondary_window"`
		}
		var d struct {
			Rate   *limits `json:"rate_limit"`
			Review *limits `json:"code_review_rate_limit"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		for i, l := range []*limits{d.Rate, d.Review} {
			if l == nil {
				continue
			}
			prefix := ""
			if i == 1 {
				prefix = "code_review_"
			}
			for j, w := range []*cw{l.Primary, l.Secondary} {
				if w == nil {
					continue
				}
				id := prefix + []string{"primary", "secondary"}[j]
				var reset *time.Time
				if w.Reset != nil {
					t := time.Unix(*w.Reset, 0).UTC()
					reset = &t
				}
				out = append(out, percentWindow(id, w.Used, reset))
			}
		}
	case "opencode-go":
		var d struct {
			Usage map[string]struct {
				Percent *float64   `json:"usagePercent"`
				Used    *float64   `json:"used"`
				Limit   *float64   `json:"limit"`
				Reset   *time.Time `json:"resetsAt"`
				ResetIn *int64     `json:"resetInSec"`
			} `json:"usage"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		for _, key := range []string{"rolling", "weekly", "monthly"} {
			w, ok := d.Usage[key]
			if !ok {
				continue
			}
			used := w.Percent
			if used == nil && w.Used != nil && w.Limit != nil && *w.Limit > 0 {
				used = number(*w.Used / *w.Limit * 100)
			}
			reset := w.Reset
			if reset == nil && w.ResetIn != nil {
				if *w.ResetIn < 0 || *w.ResetIn > 366*24*3600 {
					return nil, errors.New("invalid reset")
				}
				t := at.Add(time.Duration(*w.ResetIn) * time.Second)
				reset = &t
			}
			out = append(out, percentWindow(key, used, reset))
		}
	case "agy":
		var d struct {
			Status  string `json:"status"`
			Command struct {
				Name string `json:"name"`
				Data struct {
					Groups []struct {
						Name    string `json:"display_name"`
						Buckets []struct {
							ID        string     `json:"bucket_id"`
							Remaining *float64   `json:"remaining_fraction"`
							Reset     *time.Time `json:"reset_time"`
							Disabled  bool       `json:"disabled"`
						} `json:"buckets"`
					} `json:"groups"`
				} `json:"data"`
			} `json:"command"`
		}
		if err := json.Unmarshal(raw, &d); err != nil {
			return nil, err
		}
		if d.Status != "SUCCESS" || d.Command.Name != "usage" {
			return nil, errors.New("unsuccessful usage report")
		}
		for gi, g := range d.Command.Data.Groups {
			for _, b := range g.Buckets {
				if b.Disabled {
					continue
				}
				var used *float64
				if b.Remaining != nil {
					used = number((1 - *b.Remaining) * 100)
				}
				w := percentWindow(fmt.Sprintf("g%d:%s", gi, b.ID), used, b.Reset)
				w.Model = g.Name
				out = append(out, w)
			}
		}
	default:
		return nil, errors.New("unsupported collector")
	}
	// A changed/empty response is an unknown observation, never measured zero.
	return out, nil
}

// fetchGateway rescans retained history in bounded keyset pages. Replaying
// finalized entry IDs is safe, including after a failed scan or process restart.
// A rescan also picks up late writes and formerly reserved attempts; the gateway
// remains the authoritative ledger and this is an attributed read projection.
func (c *collector) fetchGateway(ctx context.Context, a Account, at time.Time) (Batch, string) {
	secret, err := c.resolve(ctx, c.source.CredentialRef)
	if err != nil {
		return Batch{}, "credential_unavailable"
	}
	token, _, err := credential(secret, c.source.CredentialFormat)
	if err != nil {
		return Batch{}, "credential_invalid"
	}
	cursor := ""
	seen := map[string]bool{}
	degraded := false
	for range 200 {
		endpoint := strings.TrimRight(c.source.GatewayURL, "/") + "/v1/usage/export"
		if cursor != "" {
			endpoint += "?before=" + url.QueryEscape(cursor)
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			return Batch{}, "request_invalid"
		}
		req.Header.Set("Authorization", "Bearer "+token)
		req.Header.Set("Accept", "application/json")
		resp, err := c.client.Do(req)
		if err != nil {
			return Batch{}, "transport_error"
		}
		raw, readErr := io.ReadAll(io.LimitReader(resp.Body, maxResponse+1))
		resp.Body.Close()
		if resp.StatusCode != 200 {
			return Batch{}, "gateway_export_unavailable"
		}
		if readErr != nil || len(raw) > maxResponse {
			return Batch{}, "response_too_large_or_unreadable"
		}
		var page llmgateway.UsageExport
		if json.Unmarshal(raw, &page) != nil || page.Entries == nil || len(page.Entries) > 500 {
			return Batch{}, "unsupported_response"
		}
		if page.Buffer != nil && (page.Buffer.Dropped > 0 || page.Buffer.Failed > 0) {
			degraded = true
		}
		batch := Batch{}
		for _, e := range page.Entries {
			if e.Row.Provider != a.Provider || e.Outcome == llmgateway.OutcomeReserved {
				continue
			}
			if a.Kind == "subscription" && e.CostUSD != nil {
				e.Row.CostKind = usageledger.CostKindSubscriptionEquivalent
			}
			batch.Events = append(batch.Events, Event{ID: e.ID, Row: e.Row, CostUSD: e.CostUSD, CostPartial: e.CostPartial, Outcome: e.Outcome})
		}
		if len(batch.Events) > 0 {
			if err = c.store.Ingest(ctx, a.ID, c.source.ID, "collector:"+c.source.ID, batch); err != nil {
				return Batch{}, "gateway_import_failed"
			}
		}
		if page.NextBefore == "" {
			observation := Observation{ID: newID(), ObservedAt: at, Status: "unknown", ErrorCode: "usage_only", Windows: []Window{}}
			if degraded {
				observation.Status = "error"
				observation.ErrorCode = "gateway_ledger_degraded"
			}
			return Batch{Observations: []Observation{observation}}, ""
		}
		if seen[page.NextBefore] || len(page.NextBefore) > 1024 {
			return Batch{}, "invalid_gateway_cursor"
		}
		seen[page.NextBefore] = true
		cursor = page.NextBefore
	}
	return Batch{}, "gateway_scan_limit"
}
