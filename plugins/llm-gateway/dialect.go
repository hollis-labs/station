package llmgateway

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/hollis-labs/libs/llm/core/llmtypes"
	"github.com/hollis-labs/libs/llm/core/modelsdev"
	"github.com/hollis-labs/libs/llm/core/usageledger"
)

// Dialect is the adapter for one provider-native wire protocol. The gateway is
// a relay first: a request that arrives in dialect D is sent to an upstream
// that speaks D, and the bytes pass through. A Dialect therefore never
// converts a request into a common envelope. It only does the small, bounded
// jobs a relay needs:
//
//   - read what routing and policy need from the request (Inspect),
//   - authenticate to the upstream with the hub-held credential (Authorize),
//   - tap the response for usage without changing it (NewMeter),
//   - answer in its own error shape when the gateway itself must refuse
//     (WriteError).
//
// Cross-protocol translation, when it exists, is a separate optional adapter
// keyed by (client dialect, upstream dialect). None is registered today, so a
// target whose dialect differs from the endpoint is simply not eligible.
// Operation is what a request asks the model to do.
type Operation string

const (
	OpChat      Operation = "chat"
	OpResponses Operation = "responses"
	OpEmbed     Operation = "embedding"
	// OpDecide is a System-1 judgment: structured questions about a state,
	// answered with labels and probabilities rather than prose.
	OpDecide Operation = "decision"
)

type Dialect interface {
	// Name is the dialect's configuration name ("openai", "anthropic").
	Name() string
	// UpstreamPath is appended to a provider's base_url for op. Empty means the
	// dialect does not offer the operation.
	UpstreamPath(op Operation) string
	// Inspect reads routing facts from a request body. An error is a
	// caller-safe message for a 400.
	Inspect(op Operation, body []byte) (Meta, error)
	// Rewrite returns the body to send upstream: the model replaced by the
	// target's model, and, for streaming requests that did not ask for usage,
	// whatever the dialect needs to make the upstream report it. It reports
	// whether metering injected something the client did not request, so the
	// meter can hide it again. A body needing no change is returned as is.
	Rewrite(op Operation, body []byte, meta Meta, upstreamModel string) (out []byte, injected bool)
	// Authorize sets credential and protocol headers on the upstream request.
	// in is the client's request, used only for allow-listed protocol headers.
	Authorize(out *http.Request, in *http.Request, apiKey string)
	// NewMeter returns a meter for one response.
	NewMeter(op Operation, stream, injected bool) Meter
	// WriteError writes a gateway-originated error in the dialect's own shape.
	// message is always a fixed string chosen by the gateway.
	WriteError(w http.ResponseWriter, status int, code, message string)
	// ListModels writes the model list in the dialect's shape.
	ListModels(w http.ResponseWriter, models []ModelInfo)
}

// ModelInfo describes one configured model alias, its live reachability, and
// its resolved capabilities, pricing, limits, modalities, and metadata.
type ModelInfo struct {
	Name         string                     `json:"name"`
	DisplayName  string                     `json:"display_name,omitempty"`
	Kind         Operation                  `json:"kind"`
	Reachable    bool                       `json:"reachable"`
	Tags         []string                   `json:"tags,omitempty"`
	Capabilities *TargetCapabilities        `json:"capabilities,omitempty"`
	Price        *usageledger.PriceSnapshot `json:"price,omitempty"`
	Limits       *modelsdev.Limits          `json:"limits,omitempty"`
	Modalities   *modelsdev.Modality        `json:"modalities,omitempty"`
	Metadata     *ModelMetadata             `json:"metadata,omitempty"`
	Targets      []TargetInfo               `json:"targets,omitempty"`
}

// TargetInfo describes one target behind a model alias, its breaker state, and
// its resolved capabilities, pricing, limits, modalities, and metadata.
type TargetInfo struct {
	Provider     string                     `json:"provider"`
	Model        string                     `json:"model"`
	Dialect      string                     `json:"dialect"`
	Tags         []string                   `json:"tags,omitempty"`
	BestEffort   bool                       `json:"best_effort,omitempty"`
	Reachable    bool                       `json:"reachable"`
	State        string                     `json:"state"`
	Capabilities *TargetCapabilities        `json:"capabilities,omitempty"`
	Price        *usageledger.PriceSnapshot `json:"price,omitempty"`
	Limits       *modelsdev.Limits          `json:"limits,omitempty"`
	Modalities   *modelsdev.Modality        `json:"modalities,omitempty"`
	Metadata     *ModelMetadata             `json:"metadata,omitempty"`
}

// Meta is what the gateway reads from a request, never more.
type Meta struct {
	Model  string
	Stream bool
	// Needs are the capabilities the request requires of a target.
	Needs Needs
}

// Needs mirrors the capability flags a target can lack.
type Needs struct {
	Tools  bool
	Vision bool
}

// satisfiedBy reports whether caps cover the needs and the streaming mode.
func (m Meta) satisfiedBy(caps llmtypes.ProviderCapabilities) bool {
	if m.Needs.Tools && !caps.SupportsToolCalling {
		return false
	}
	if m.Needs.Vision && !caps.SupportsImageInput {
		return false
	}
	if m.Stream && !caps.SupportsStreamJSON {
		return false
	}
	return true
}

// Meter taps a response. Feed is called with response bytes in order.
// For a streaming response it is called once per SSE line and may ask for a
// line to be withheld from the client (only ever the usage line metering
// itself injected). Usage returns what the upstream reported, or nil if it
// reported none; a response with no usage is unknown, never zero.
type Meter interface {
	// Line handles one SSE line (including its newline). forward=false
	// withholds it from the client.
	Line(line []byte) (forward bool)
	// Body handles a complete non-streaming response body.
	Body(body []byte)
	Usage() *Usage
}

// allowedRequestHeaders are the only client headers that cross to the upstream.
// Credentials are deliberately absent: the caller's Station key is never
// forwarded, and the upstream key is never taken from the caller.
var allowedRequestHeaders = []string{"Content-Type", "Accept", "Anthropic-Version", "Anthropic-Beta", "OpenAI-Beta"}

func copyAllowedHeaders(out, in *http.Request) {
	for _, h := range allowedRequestHeaders {
		if v := in.Header.Values(h); len(v) > 0 {
			out.Header[http.CanonicalHeaderKey(h)] = append([]string(nil), v...)
		}
	}
}

// hopByHop and sensitive response headers never reach the client.
var droppedResponseHeaders = map[string]bool{
	"Connection": true, "Keep-Alive": true, "Proxy-Authenticate": true, "Proxy-Authorization": true,
	"Te": true, "Trailer": true, "Transfer-Encoding": true, "Upgrade": true,
	"Set-Cookie": true, "Content-Length": true, "Content-Encoding": true,
}

// jsonObject decodes a request body into top-level raw fields.
func jsonObject(body []byte) (map[string]json.RawMessage, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil || m == nil {
		return nil, errBadJSON
	}
	return m, nil
}

type fixedError string

func (e fixedError) Error() string { return string(e) }

const (
	errBadJSON     fixedError = "request body is not a valid JSON object"
	errNoModel     fixedError = "model is required"
	errBadModel    fixedError = "model must be a string"
	errBadStream   fixedError = "stream must be a boolean"
	errBadMessages fixedError = "messages must be a non-empty array"
	errNoInput     fixedError = "input is required"
	errUnsupported fixedError = "this endpoint does not support that operation"
)

// commonMeta parses the fields every supported dialect shares.
func commonMeta(m map[string]json.RawMessage) (Meta, error) {
	var meta Meta
	raw, ok := m["model"]
	if !ok {
		return meta, errNoModel
	}
	if err := json.Unmarshal(raw, &meta.Model); err != nil || meta.Model == "" {
		return meta, errBadModel
	}
	if raw, ok := m["stream"]; ok {
		if err := json.Unmarshal(raw, &meta.Stream); err != nil {
			return meta, errBadStream
		}
	}
	var msgs []struct {
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(m["messages"], &msgs); err != nil || len(msgs) == 0 {
		return meta, errBadMessages
	}
	for _, msg := range msgs {
		meta.Needs.Vision = meta.Needs.Vision || hasImagePart(msg.Content)
	}
	if raw, ok := m["tools"]; ok {
		var tools []json.RawMessage
		if json.Unmarshal(raw, &tools) == nil && len(tools) > 0 {
			meta.Needs.Tools = true
		}
	}
	return meta, nil
}

func hasImagePart(content json.RawMessage) bool {
	trimmed := bytes.TrimSpace(content)
	if len(trimmed) == 0 || trimmed[0] != '[' {
		return false
	}
	var parts []struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(trimmed, &parts) != nil {
		return false
	}
	for _, p := range parts {
		switch strings.ToLower(p.Type) {
		case "image", "image_url", "input_image":
			return true
		}
	}
	return false
}

// withFields re-encodes body with the given top-level fields replaced. Every
// other field is carried as raw bytes, so numbers and nested values are not
// reinterpreted. Only key order changes.
func withFields(m map[string]json.RawMessage, set map[string]json.RawMessage) []byte {
	for k, v := range set {
		m[k] = v
	}
	out, err := json.Marshal(m)
	if err != nil { // cannot happen for RawMessage values that already parsed
		return nil
	}
	return out
}

// sseLine splits "data: payload" lines. It returns the payload and true for data lines.
func sseData(line []byte) ([]byte, bool) {
	rest, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return nil, false
	}
	return bytes.TrimSpace(rest), true
}
