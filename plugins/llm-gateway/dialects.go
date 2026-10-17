package llmgateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hollis-labs/libs/llm/core/llmtypes"
)

// Usage is what an upstream reported. Input and Output are always present
// when a Usage exists; the optional cache dimensions are nil when the
// upstream did not report them, so "not reported" never reads as "zero".
// Input excludes cached tokens: the dimensions are disjoint.
type Usage struct {
	Input      int64
	Output     int64
	CacheRead  *int64
	CacheWrite *int64
	StopReason string
}

func ptr(n int64) *int64 { return &n }

// ---------------------------------------------------------------- OpenAI

// openaiDialect speaks OpenAI Chat Completions, and every server that copies
// it (llama.cpp, vLLM, Ollama's /v1, OpenRouter).
type openaiDialect struct{}

func (openaiDialect) Name() string { return "openai" }
func (openaiDialect) UpstreamPath(op Operation) string {
	switch op {
	case OpChat:
		return "/chat/completions"
	case OpResponses:
		return "/responses"
	case OpEmbed:
		return "/embeddings"
	}
	return ""
}

func (openaiDialect) Inspect(op Operation, body []byte) (Meta, error) {
	m, err := jsonObject(body)
	if err != nil {
		return Meta{}, err
	}
	if op == OpEmbed {
		var meta Meta
		if raw, ok := m["model"]; !ok {
			return meta, errNoModel
		} else if json.Unmarshal(raw, &meta.Model) != nil || meta.Model == "" {
			return meta, errBadModel
		}
		if raw, ok := m["input"]; !ok || len(bytes.TrimSpace(raw)) == 0 || string(bytes.TrimSpace(raw)) == "null" {
			return meta, errNoInput
		}
		return meta, nil
	}
	if op == OpResponses {
		return inspectResponses(m)
	}
	meta, err := commonMeta(m)
	if err != nil {
		return meta, err
	}
	if raw, ok := m["functions"]; ok { // the legacy spelling of tools
		var fns []json.RawMessage
		if json.Unmarshal(raw, &fns) == nil && len(fns) > 0 {
			meta.Needs.Tools = true
		}
	}
	return meta, nil
}

func inspectResponses(m map[string]json.RawMessage) (Meta, error) {
	var meta Meta
	raw, ok := m["model"]
	if !ok {
		return meta, errNoModel
	}
	if json.Unmarshal(raw, &meta.Model) != nil || meta.Model == "" {
		return meta, errBadModel
	}
	if raw, ok := m["stream"]; ok {
		if json.Unmarshal(raw, &meta.Stream) != nil {
			return meta, errBadStream
		}
	}
	inRaw, ok := m["input"]
	if !ok || len(bytes.TrimSpace(inRaw)) == 0 || string(bytes.TrimSpace(inRaw)) == "null" {
		return meta, errNoInput
	}
	trimmed := bytes.TrimSpace(inRaw)
	switch trimmed[0] {
	case '"':
		var s string
		if json.Unmarshal(trimmed, &s) != nil || s == "" {
			return meta, errNoInput
		}
	case '[':
		var items []struct {
			Type    string          `json:"type"`
			Content json.RawMessage `json:"content"`
		}
		if json.Unmarshal(trimmed, &items) != nil || len(items) == 0 {
			return meta, errNoInput
		}
		for _, it := range items {
			switch strings.ToLower(it.Type) {
			case "input_image", "image", "image_url":
				meta.Needs.Vision = true
			}
			meta.Needs.Vision = meta.Needs.Vision || hasImagePart(it.Content)
		}
	default:
		return meta, errNoInput
	}
	if raw, ok := m["tools"]; ok {
		var tools []json.RawMessage
		if json.Unmarshal(raw, &tools) == nil && len(tools) > 0 {
			meta.Needs.Tools = true
		}
	}
	return meta, nil
}

func (openaiDialect) Rewrite(op Operation, body []byte, meta Meta, upstreamModel string) ([]byte, bool) {
	m, err := jsonObject(body)
	if err != nil {
		return body, false
	}
	set := map[string]json.RawMessage{}
	injected := false
	if upstreamModel != meta.Model {
		set["model"], _ = json.Marshal(upstreamModel)
	}
	if op == OpChat && meta.Stream {
		opts := map[string]json.RawMessage{}
		if raw, ok := m["stream_options"]; ok {
			_ = json.Unmarshal(raw, &opts)
		}
		var already bool
		if raw, ok := opts["include_usage"]; ok {
			_ = json.Unmarshal(raw, &already)
		}
		if !already {
			opts["include_usage"] = json.RawMessage("true")
			set["stream_options"], _ = json.Marshal(opts)
			// Hide the extra usage chunk only when the client had no
			// stream_options of its own to expect one from.
			injected = true
		}
	}
	if len(set) == 0 {
		return body, false
	}
	out := withFields(m, set)
	if out == nil {
		return body, false
	}
	return out, injected
}

func (openaiDialect) Authorize(out, in *http.Request, apiKey string) {
	copyAllowedHeaders(out, in)
	if apiKey != "" {
		out.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (openaiDialect) NewMeter(op Operation, stream, injected bool) Meter {
	return &openaiMeter{injected: injected}
}

func (openaiDialect) WriteError(w http.ResponseWriter, status int, code, message string) {
	typ := "invalid_request_error"
	switch {
	case status == http.StatusUnauthorized:
		typ = "authentication_error"
	case status == http.StatusForbidden:
		typ = "permission_error"
	case status == http.StatusTooManyRequests:
		typ = "rate_limit_error"
	case status >= 500:
		typ = "api_error"
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": typ, "param": nil, "code": code}})
}

func (openaiDialect) ListModels(w http.ResponseWriter, models []ModelInfo) {
	data := make([]any, 0, len(models))
	for _, m := range models {
		item := map[string]any{
			"id":        m.Name,
			"object":    "model",
			"owned_by":  "station",
			"kind":      string(m.Kind),
			"reachable": m.Reachable,
			"targets":   m.Targets,
		}
		if m.DisplayName != "" {
			item["display_name"] = m.DisplayName
		}
		if len(m.Tags) > 0 {
			item["tags"] = m.Tags
		}
		if m.Capabilities != nil {
			item["capabilities"] = m.Capabilities
		}
		if m.Price != nil {
			item["price"] = m.Price
		}
		if m.Limits != nil {
			item["limits"] = m.Limits
		}
		if m.Modalities != nil {
			item["modalities"] = m.Modalities
		}
		if m.Metadata != nil {
			item["metadata"] = m.Metadata
		}
		data = append(data, item)
	}
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

type openaiMeter struct {
	injected  bool
	dropBlank bool
	usage     *Usage
	stop      string
}

type openaiUsageBlock struct {
	PromptTokens        int64 `json:"prompt_tokens"`
	CompletionTokens    int64 `json:"completion_tokens"`
	InputTokens         int64 `json:"input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
	InputTokensDetails *struct {
		CachedTokens int64 `json:"cached_tokens"`
	} `json:"input_tokens_details"`
}

type openaiPayload struct {
	Status  string `json:"status"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage    *openaiUsageBlock `json:"usage"`
	Response *struct {
		Status string            `json:"status"`
		Usage  *openaiUsageBlock `json:"usage"`
	} `json:"response"`
}

func (m *openaiMeter) absorb(p openaiPayload) {
	for _, c := range p.Choices {
		if c.FinishReason != "" {
			m.stop = c.FinishReason
		}
	}
	if p.Status == "completed" && m.stop == "" {
		m.stop = "stop"
	}
	if p.Response != nil && p.Response.Status == "completed" && m.stop == "" {
		m.stop = "stop"
	}
	ub := p.Usage
	if ub == nil && p.Response != nil {
		ub = p.Response.Usage
	}
	if ub == nil {
		return
	}
	inTok := ub.PromptTokens
	if inTok == 0 && ub.InputTokens > 0 {
		inTok = ub.InputTokens
	}
	outTok := ub.CompletionTokens
	if outTok == 0 && ub.OutputTokens > 0 {
		outTok = ub.OutputTokens
	}
	u := &Usage{Input: inTok, Output: outTok}
	if d := ub.PromptTokensDetails; d != nil {
		// prompt_tokens includes cached tokens; the ledger keeps them disjoint.
		u.Input = max(u.Input-d.CachedTokens, 0)
		u.CacheRead = ptr(d.CachedTokens)
	} else if d := ub.InputTokensDetails; d != nil {
		u.Input = max(u.Input-d.CachedTokens, 0)
		u.CacheRead = ptr(d.CachedTokens)
	}
	m.usage = u
}

func (m *openaiMeter) Line(line []byte) bool {
	if len(bytes.TrimSpace(line)) == 0 {
		if m.dropBlank {
			m.dropBlank = false
			return false
		}
		return true
	}
	data, ok := sseData(line)
	if !ok || string(data) == "[DONE]" {
		return true
	}
	var p openaiPayload
	if json.Unmarshal(data, &p) != nil {
		return true // not ours to judge; pass through untouched
	}
	m.absorb(p)
	if m.injected && p.Usage != nil && len(p.Choices) == 0 {
		m.dropBlank = true
		return false
	}
	return true
}

func (m *openaiMeter) Body(body []byte) {
	var p openaiPayload
	if json.Unmarshal(body, &p) == nil {
		m.absorb(p)
	}
}

func (m *openaiMeter) Usage() *Usage {
	if m.usage != nil {
		m.usage.StopReason = llmtypes.NormalizeStopReason(m.stop)
	}
	return m.usage
}

// ------------------------------------------------------------- Anthropic

// anthropicDialect speaks the Anthropic Messages API.
type anthropicDialect struct{}

func (anthropicDialect) Name() string { return "anthropic" }
func (anthropicDialect) UpstreamPath(op Operation) string {
	if op == OpChat {
		return "/messages"
	}
	return ""
}

func (anthropicDialect) Inspect(op Operation, body []byte) (Meta, error) {
	if op != OpChat {
		return Meta{}, errUnsupported
	}
	m, err := jsonObject(body)
	if err != nil {
		return Meta{}, err
	}
	return commonMeta(m)
}

func (anthropicDialect) Rewrite(_ Operation, body []byte, meta Meta, upstreamModel string) ([]byte, bool) {
	if upstreamModel == meta.Model {
		return body, false
	}
	m, err := jsonObject(body)
	if err != nil {
		return body, false
	}
	model, _ := json.Marshal(upstreamModel)
	if out := withFields(m, map[string]json.RawMessage{"model": model}); out != nil {
		return out, false
	}
	return body, false
}

func (anthropicDialect) Authorize(out, in *http.Request, apiKey string) {
	copyAllowedHeaders(out, in)
	if out.Header.Get("Anthropic-Version") == "" {
		out.Header.Set("Anthropic-Version", "2023-06-01")
	}
	if apiKey != "" {
		out.Header.Set("X-Api-Key", apiKey)
	}
}

func (anthropicDialect) NewMeter(_ Operation, stream, injected bool) Meter { return &anthropicMeter{} }

func (anthropicDialect) WriteError(w http.ResponseWriter, status int, code, message string) {
	typ := "invalid_request_error"
	switch code {
	case "model_not_found":
		typ = "not_found_error"
	case "policy_denied", "approval_required":
		typ = "permission_error"
	case "budget_exceeded":
		typ = "rate_limit_error"
	case "model_unavailable":
		typ = "overloaded_error"
	case "upstream_error":
		typ = "api_error"
	case "unauthorized":
		typ = "authentication_error"
	}
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": typ, "message": message}})
}

func (anthropicDialect) ListModels(w http.ResponseWriter, models []ModelInfo) {
	data := make([]any, 0, len(models))
	for _, m := range models {
		displayName := m.Name
		if m.DisplayName != "" {
			displayName = m.DisplayName
		}
		item := map[string]any{
			"type":         "model",
			"id":           m.Name,
			"display_name": displayName,
			"created_at":   "1970-01-01T00:00:00Z",
			"kind":         string(m.Kind),
			"reachable":    m.Reachable,
			"targets":      m.Targets,
		}
		if len(m.Tags) > 0 {
			item["tags"] = m.Tags
		}
		if m.Capabilities != nil {
			item["capabilities"] = m.Capabilities
		}
		if m.Price != nil {
			item["price"] = m.Price
		}
		if m.Limits != nil {
			item["limits"] = m.Limits
		}
		if m.Modalities != nil {
			item["modalities"] = m.Modalities
		}
		if m.Metadata != nil {
			item["metadata"] = m.Metadata
		}
		data = append(data, item)
	}
	out := map[string]any{"data": data, "has_more": false}
	if len(models) > 0 {
		out["first_id"], out["last_id"] = models[0].Name, models[len(models)-1].Name
	}
	writeJSON(w, http.StatusOK, out)
}

type anthropicMeter struct {
	usage *Usage
	stop  string
}

type anthropicUsage struct {
	InputTokens         int64  `json:"input_tokens"`
	OutputTokens        int64  `json:"output_tokens"`
	CacheCreationTokens *int64 `json:"cache_creation_input_tokens"`
	CacheReadTokens     *int64 `json:"cache_read_input_tokens"`
}

func (m *anthropicMeter) merge(u *anthropicUsage, startOfMessage bool) {
	if u == nil {
		return
	}
	if m.usage == nil {
		m.usage = &Usage{}
	}
	// message_start carries input and cache figures; message_delta carries the
	// cumulative output count. Anthropic's input_tokens already excludes cache.
	if startOfMessage || u.InputTokens > 0 {
		m.usage.Input = u.InputTokens
	}
	if u.OutputTokens > 0 || startOfMessage {
		m.usage.Output = u.OutputTokens
	}
	if u.CacheReadTokens != nil {
		m.usage.CacheRead = u.CacheReadTokens
	}
	if u.CacheCreationTokens != nil {
		m.usage.CacheWrite = u.CacheCreationTokens
	}
}

func (m *anthropicMeter) Line(line []byte) bool {
	data, ok := sseData(line)
	if !ok {
		return true
	}
	var ev struct {
		Type    string `json:"type"`
		Message struct {
			Usage *anthropicUsage `json:"usage"`
		} `json:"message"`
		Delta struct {
			StopReason string `json:"stop_reason"`
		} `json:"delta"`
		Usage *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(data, &ev) != nil {
		return true
	}
	switch ev.Type {
	case "message_start":
		m.merge(ev.Message.Usage, true)
	case "message_delta":
		m.merge(ev.Usage, false)
		if ev.Delta.StopReason != "" {
			m.stop = ev.Delta.StopReason
		}
	}
	return true
}

func (m *anthropicMeter) Body(body []byte) {
	var r struct {
		StopReason string          `json:"stop_reason"`
		Usage      *anthropicUsage `json:"usage"`
	}
	if json.Unmarshal(body, &r) == nil {
		m.merge(r.Usage, true)
		m.stop = r.StopReason
	}
}

func (m *anthropicMeter) Usage() *Usage {
	if m.usage != nil {
		m.usage.StopReason = llmtypes.NormalizeStopReason(strings.TrimSpace(m.stop))
	}
	return m.usage
}

// dialects is the registry of wire protocols the gateway can relay.
var dialects = map[string]Dialect{
	"openai":    openaiDialect{},
	"anthropic": anthropicDialect{},
	"jev":       jevDialect{},
}
