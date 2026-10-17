package llmgateway

import (
	"bytes"
	"encoding/json"
	"strings"

	"github.com/hollis-labs/libs/security/redact"
)

const defaultCaptureMaxBytes = 256 << 10 // 256 KiB per payload

// Capturer sanitizes request, response, leader (system/instructions) and tool
// payloads through security/redact v0.2.0's Sanitizer (credential rules +
// known-secret Scope + mandatory PII rules + fail-closed size/depth bounds)
// before they are stored in Entry.Detail.
type Capturer struct {
	mode      string
	maxBytes  int
	sanitizer *redact.Sanitizer
}

// NewCapturer builds a Capturer from cfg and the shared redaction scope.
func NewCapturer(cfg CaptureConfig, scope *redact.Scope) (*Capturer, error) {
	maxBytes := cfg.MaxBytes
	if maxBytes <= 0 {
		maxBytes = defaultCaptureMaxBytes
	}
	var opts []redact.Option
	if scope != nil {
		opts = append(opts, redact.WithScope(scope))
	}
	san, err := redact.NewSanitizer(redact.Policy{
		Mode:        redact.Denylist,
		Format:      redact.JSON,
		MaxBytes:    maxBytes,
		Sensitivity: redact.Private,
	}, opts...)
	if err != nil {
		return nil, err
	}
	mode := cfg.Mode
	if mode == "" {
		mode = "off"
	}
	return &Capturer{
		mode:      mode,
		maxBytes:  maxBytes,
		sanitizer: san,
	}, nil
}

// MaxBytes returns the byte ceiling for capturing response bodies or SSE lines.
func (c *Capturer) MaxBytes() int {
	if c == nil || c.maxBytes <= 0 {
		return defaultCaptureMaxBytes
	}
	return c.maxBytes
}

// Enabled reports whether full request/response payload capture is active.
func (c *Capturer) Enabled() bool {
	return c != nil && c.mode == "full"
}

// CaptureReport summarizes what the sanitizer found and redacted.
type CaptureReport struct {
	RequestRedactions  uint32             `json:"request_redactions,omitempty"`
	ResponseRedactions uint32             `json:"response_redactions,omitempty"`
	PII                bool               `json:"pii,omitempty"`
	Sensitivity        redact.Sensitivity `json:"sensitivity"`
	RequestErrorClass  redact.ErrorClass  `json:"request_error_class,omitempty"`
	ResponseErrorClass redact.ErrorClass  `json:"response_error_class,omitempty"`
}

// BuildDetail constructs the sanitized JSON detail record for one call.
//
// When mode is "off", only meterDetail (such as JEV decision answers) is kept,
// passed through Sanitizer so secrets or PII in decision outputs are still
// scrubbed.
//
// When mode is "full", Detail holds:
//   - leader: sanitized system prompt / instructions extracted from the request
//   - tools: sanitized tool/function declarations from the request
//   - request: sanitized request JSON
//   - response: sanitized response JSON (or {"sse_events":[...]} for streams)
//   - answers: sanitized decision answers (when present)
//   - redaction: counts, PII flag, sensitivity and any fail-closed error class
func (c *Capturer) BuildDetail(reqBody, respBody []byte, sseEvents []json.RawMessage, meterDetail json.RawMessage) json.RawMessage {
	if c == nil || c.sanitizer == nil {
		return meterDetail
	}
	if !c.Enabled() {
		if len(meterDetail) == 0 {
			return nil
		}
		san, _ := c.sanitizer.Sanitize(meterDetail)
		return json.RawMessage(san)
	}

	out := map[string]any{}
	rep := CaptureReport{Sensitivity: redact.Public}
	mergeRep := func(r redact.Report, isReq bool) {
		if isReq {
			rep.RequestRedactions += r.Redactions
			if r.ErrorClass != "" {
				rep.RequestErrorClass = r.ErrorClass
			}
		} else {
			rep.ResponseRedactions += r.Redactions
			if r.ErrorClass != "" {
				rep.ResponseErrorClass = r.ErrorClass
			}
		}
		if r.PII {
			rep.PII = true
		}
		if r.Sensitivity == redact.Secret || (r.Sensitivity == redact.Private && rep.Sensitivity == redact.Public) {
			rep.Sensitivity = r.Sensitivity
		}
	}

	if len(reqBody) > 0 {
		if leaderRaw := extractLeader(reqBody); len(leaderRaw) > 0 {
			sanLeader, _ := c.sanitizer.Sanitize(leaderRaw)
			out["leader"] = json.RawMessage(sanLeader)
		}
		if toolsRaw := extractTools(reqBody); len(toolsRaw) > 0 {
			sanTools, _ := c.sanitizer.Sanitize(toolsRaw)
			out["tools"] = json.RawMessage(sanTools)
		}
		sanReq, r := c.sanitizer.Sanitize(reqBody)
		mergeRep(r, true)
		out["request"] = json.RawMessage(sanReq)
	}

	if len(respBody) > 0 {
		sanResp, r := c.sanitizer.Sanitize(respBody)
		mergeRep(r, false)
		out["response"] = json.RawMessage(sanResp)
	} else if len(sseEvents) > 0 {
		wrapped, err := json.Marshal(map[string]any{"sse_events": sseEvents})
		if err == nil {
			sanResp, r := c.sanitizer.Sanitize(wrapped)
			mergeRep(r, false)
			out["response"] = json.RawMessage(sanResp)
		}
	}

	if len(meterDetail) > 0 {
		sanAns, r := c.sanitizer.Sanitize(meterDetail)
		mergeRep(r, false)
		out["answers"] = json.RawMessage(sanAns)
	}

	out["redaction"] = rep
	b, err := json.Marshal(out)
	if err != nil {
		return nil
	}
	return json.RawMessage(b)
}

// extractLeader pulls out the system prompt / instructions ("leader") from an
// OpenAI Chat, OpenAI Responses, or Anthropic Messages request body as valid
// JSON so it can be inspected independently in the ledger.
func extractLeader(reqBody []byte) []byte {
	m, err := jsonObject(reqBody)
	if err != nil {
		return nil
	}
	if sys, ok := m["system"]; ok && len(bytes.TrimSpace(sys)) > 0 && string(bytes.TrimSpace(sys)) != "null" {
		return sys
	}
	if inst, ok := m["instructions"]; ok && len(bytes.TrimSpace(inst)) > 0 && string(bytes.TrimSpace(inst)) != "null" {
		return inst
	}
	rawMsgs, ok := m["messages"]
	if !ok {
		rawMsgs = m["input"]
	}
	var msgs []struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(rawMsgs, &msgs) != nil {
		return nil
	}
	var leaders []json.RawMessage
	for _, msg := range msgs {
		role := strings.ToLower(msg.Role)
		if (role == "system" || role == "developer") && len(msg.Content) > 0 {
			leaders = append(leaders, msg.Content)
		}
	}
	if len(leaders) == 0 {
		return nil
	}
	if len(leaders) == 1 {
		return leaders[0]
	}
	b, _ := json.Marshal(leaders)
	return b
}

// extractTools pulls out the tools or functions array from a request body.
func extractTools(reqBody []byte) []byte {
	m, err := jsonObject(reqBody)
	if err != nil {
		return nil
	}
	for _, k := range []string{"tools", "functions"} {
		if raw, ok := m[k]; ok {
			trimmed := bytes.TrimSpace(raw)
			if len(trimmed) > 2 && trimmed[0] == '[' {
				return trimmed
			}
		}
	}
	return nil
}
