package llmgateway

import (
	"bytes"
	"encoding/json"
	"net/http"
)

// jevDialect speaks JEV's System-1 decision API: POST {state, model,
// questions} and receive {model, answers, usage}. It is an LLM like the others,
// with a kind of "decision" instead of "chat": the same provider, target,
// capability, policy, guard and ledger machinery applies, and only the wire
// protocol differs, so it is just another Dialect.
//
// Because the relay does not re-encode nested values, the order of questions,
// options and levels (which JEV reads: it leans toward the first option) is
// preserved byte for byte. Tether's old JEV adapter hand-marshalled every
// ordered structure to protect that; passing the bytes through makes the
// problem not arise.
type jevDialect struct{}

func (jevDialect) Name() string { return "jev" }

func (jevDialect) UpstreamPath(op Operation) string {
	if op == OpDecide {
		return "/systemone"
	}
	return ""
}

func (jevDialect) Inspect(op Operation, body []byte) (Meta, error) {
	if op != OpDecide {
		return Meta{}, errUnsupported
	}
	m, err := jsonObject(body)
	if err != nil {
		return Meta{}, err
	}
	var meta Meta
	raw, ok := m["model"]
	if !ok {
		return meta, errNoModel
	}
	if json.Unmarshal(raw, &meta.Model) != nil || meta.Model == "" {
		return meta, errBadModel
	}
	if s := bytes.TrimSpace(m["state"]); len(s) == 0 || string(s) == "null" || string(s) == `""` {
		return meta, errNoState
	}
	var qs map[string]json.RawMessage
	if json.Unmarshal(m["questions"], &qs) != nil || len(qs) == 0 {
		return meta, errNoQuestions
	}
	return meta, nil
}

func (jevDialect) Rewrite(_ Operation, body []byte, meta Meta, upstreamModel string) ([]byte, bool) {
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

func (jevDialect) Authorize(out, in *http.Request, apiKey string) {
	copyAllowedHeaders(out, in)
	if apiKey != "" {
		out.Header.Set("Authorization", "Bearer "+apiKey)
	}
}

func (jevDialect) NewMeter(Operation, bool, bool) Meter { return &jevMeter{} }

func (jevDialect) WriteError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"code": code, "message": message}})
}

// ListModels is not used: decision models are not part of the OpenAI or
// Anthropic model lists, which each client library parses strictly.
func (jevDialect) ListModels(w http.ResponseWriter, models []ModelInfo) {
	openaiDialect{}.ListModels(w, models)
}

const (
	errNoState     fixedError = "state is required"
	errNoQuestions fixedError = "questions must be a non-empty object"
)

// maxDetail bounds the answers kept in the ledger.
const maxDetail = 64 << 10

type jevMeter struct {
	usage   *Usage
	answers json.RawMessage
}

func (m *jevMeter) Line([]byte) bool { return true } // JEV does not stream

func (m *jevMeter) Body(body []byte) {
	var r struct {
		Answers json.RawMessage `json:"answers"`
		Usage   *struct {
			InputTokens  int64 `json:"input_tokens"`
			OutputTokens int64 `json:"output_tokens"`
		} `json:"usage"`
	}
	if json.Unmarshal(body, &r) != nil {
		return
	}
	if r.Usage != nil {
		m.usage = &Usage{Input: r.Usage.InputTokens, Output: r.Usage.OutputTokens}
	}
	if len(r.Answers) > 0 && len(r.Answers) <= maxDetail {
		m.answers = append(json.RawMessage(nil), r.Answers...)
	}
}

func (m *jevMeter) Usage() *Usage { return m.usage }

// Detail is the decision's answers: labels, scores and probabilities, the
// output a decision ledger exists to keep.
func (m *jevMeter) Detail() json.RawMessage { return m.answers }
