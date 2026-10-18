package llmgateway

import (
	"bytes"
	"encoding/json"
	"io"
	"math"
	"math/big"
	"strconv"
)

const tokenizedEmbeddingBound = "tokenized_embeddings_v1"

// CostBound records an admission ceiling at the captured rate, not a provider bill.
// The operator must assert the target's flat input-only token billing contract.
// Version the method when its accepted request shapes or billing assumptions change.
type CostBound struct {
	Method        string `json:"method"`
	InputTokens   int64  `json:"input_tokens"`
	MaxCostMicros int64  `json:"max_cost_micros"`
}

func (b *Budgets) requiresCostBound(caller Caller, model string) bool {
	if b == nil {
		return false
	}
	principal := caller.ID
	if !caller.Known || principal == "" {
		principal = "anonymous"
	}
	for _, item := range b.items {
		if item.cfg.RequireCostBound && item.matches(principal, model) {
			return true
		}
	}
	return false
}

// boundAttempt reads the actual rewritten provider request. It never guesses a
// tokenizer, prices arbitrary feature fields, or trusts a caller's cost estimate.
func boundAttempt(op Operation, body []byte, t *target) *CostBound {
	if op != OpEmbed || t.costBoundMethod != tokenizedEmbeddingBound || t.provider.dialect.Name() != "openai" || !usableBasePrice(op, t.price) {
		return nil
	}
	m, ok := boundObject(body)
	if !ok {
		return nil
	}
	for name, raw := range m {
		switch name {
		case "model", "user":
			var value string
			if json.Unmarshal(raw, &value) != nil || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) {
				return nil
			}
			if name == "model" && value != t.model {
				return nil
			}
		case "input":
		case "dimensions":
			var value int64
			if json.Unmarshal(raw, &value) != nil || value <= 0 {
				return nil
			}
		case "encoding_format":
			var value string
			if json.Unmarshal(raw, &value) != nil || (value != "float" && value != "base64") {
				return nil
			}
		default:
			return nil
		}
	}
	if _, ok := m["model"]; !ok {
		return nil
	}
	tokens, ok := embeddingTokenCount(m["input"])
	if !ok {
		return nil
	}
	// USD/token = rate/1e6, so microdollars = rate * token count.
	micros, ok := scaledMicros(t.price.InputPerMillion, tokens, true)
	if !ok {
		return nil
	}
	return &CostBound{Method: tokenizedEmbeddingBound, InputTokens: tokens, MaxCostMicros: micros}
}

// boundObject rejects duplicate names: parsers at the gateway and provider must
// not disagree about which input or usage object is authoritative.
func boundObject(raw []byte) (map[string]json.RawMessage, bool) {
	d := json.NewDecoder(bytes.NewReader(raw))
	tok, err := d.Token()
	if err != nil || tok != json.Delim('{') {
		return nil, false
	}
	m := map[string]json.RawMessage{}
	for d.More() {
		tok, err := d.Token()
		if err != nil {
			return nil, false
		}
		key, ok := tok.(string)
		if !ok {
			return nil, false
		}
		if _, exists := m[key]; exists {
			return nil, false
		}
		var value json.RawMessage
		if d.Decode(&value) != nil {
			return nil, false
		}
		m[key] = value
	}
	if tok, err = d.Token(); err != nil || tok != json.Delim('}') {
		return nil, false
	}
	if _, err := d.Token(); err != io.EOF {
		return nil, false
	}
	return m, true
}

func embeddingTokenCount(raw []byte) (int64, bool) {
	var inputs []json.RawMessage
	if json.Unmarshal(raw, &inputs) != nil || len(inputs) == 0 {
		return 0, false
	}
	array := bytes.HasPrefix(bytes.TrimSpace(inputs[0]), []byte("["))
	var count int64
	for _, input := range inputs {
		var tokens []json.RawMessage
		if array {
			if json.Unmarshal(input, &tokens) != nil || len(tokens) == 0 {
				return 0, false
			}
		} else {
			tokens = []json.RawMessage{input}
		}
		for _, token := range tokens {
			var id int64
			if json.Unmarshal(token, &id) != nil || bytes.Equal(bytes.TrimSpace(token), []byte("null")) || id < 0 || id > math.MaxInt32 {
				return 0, false
			}
			if count == math.MaxInt64 {
				return 0, false
			}
			count++
		}
	}
	return count, true
}

// scaledMicros uses the configured decimal value, avoiding binary-float boundary
// rounding. Limits round down; admission and historical charges round up.
func scaledMicros(value float64, scale int64, roundUp bool) (int64, bool) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || scale < 0 {
		return 0, false
	}
	r, ok := new(big.Rat).SetString(strconv.FormatFloat(value, 'g', -1, 64))
	if !ok {
		return 0, false
	}
	r.Mul(r, new(big.Rat).SetInt64(scale))
	q, rem := new(big.Int), new(big.Int)
	q.QuoRem(r.Num(), r.Denom(), rem)
	if roundUp && rem.Sign() > 0 {
		q.Add(q, big.NewInt(1))
	}
	return q.Int64(), q.IsInt64()
}

func boundUsageAgrees(bound *CostBound, u *Usage, e Entry) bool {
	if bound.Method != tokenizedEmbeddingBound || u == nil || u.Input != bound.InputTokens || u.Output != 0 || e.Row.Price == nil || e.CostUSD == nil || e.CostPartial || !validBudgetUSD(*e.CostUSD) {
		return false
	}
	if (u.CacheRead != nil && *u.CacheRead != 0) || (u.CacheWrite != nil && *u.CacheWrite != 0) {
		return false
	}
	micros, ok := scaledMicros(e.Row.Price.InputPerMillion, u.Input, true)
	return ok && micros == bound.MaxCostMicros
}

// boundedEmbeddingMeter accepts only the usage shape covered by the declared
// contract. Extra dimensions or inconsistent/missing counts stay unknown.
type boundedEmbeddingMeter struct{ usage *Usage }

func (m *boundedEmbeddingMeter) Line([]byte) bool { return true }
func (m *boundedEmbeddingMeter) Usage() *Usage    { return m.usage }
func (m *boundedEmbeddingMeter) Body(body []byte) {
	m.usage = nil
	envelope, ok := boundObject(body)
	if !ok {
		return
	}
	u, ok := boundObject(envelope["usage"])
	if !ok || len(u) != 2 {
		return
	}
	var prompt, total int64
	if raw := u["prompt_tokens"]; len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &prompt) != nil {
		return
	}
	if raw := u["total_tokens"]; len(raw) == 0 || bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &total) != nil {
		return
	}
	if prompt < 0 || prompt != total {
		return
	}
	m.usage = &Usage{Input: prompt}
}
