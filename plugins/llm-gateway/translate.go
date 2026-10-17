package llmgateway

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/hollis-labs/libs/llm/core/llmtypes"
)

// Translator adapts a request and response when a caller on clientDialect
// routes to a target speaking upstreamDialect.
//
// Per CW-20261013-0030, translation is strictly for cross-protocol targets and
// is never invoked on the same-dialect relay path.
type Translator interface {
	TranslateRequest(op Operation, body []byte, upstreamModel string) ([]byte, error)
	TranslateResponse(op Operation, body []byte, requestedModel string) ([]byte, *Usage, error)
	TranslateSSE(w io.Writer, rc *http.ResponseController, body io.Reader, requestedModel string, maxCapture int) (*Usage, []json.RawMessage, error)
}

type translatorKey struct {
	client   string
	upstream string
	op       Operation
}

var translators = map[translatorKey]Translator{
	{client: "openai", upstream: "anthropic", op: OpChat}: openaiToAnthropic{},
	{client: "anthropic", upstream: "openai", op: OpChat}: anthropicToOpenAI{},
}

func lookupTranslator(clientDialect, upstreamDialect string, op Operation) Translator {
	return translators[translatorKey{client: clientDialect, upstream: upstreamDialect, op: op}]
}

func hasTranslatorTo(upstreamDialect string, op Operation) bool {
	for k := range translators {
		if k.upstream == upstreamDialect && k.op == op {
			return true
		}
	}
	return false
}

// ------------------------------------------------ OpenAI client -> Anthropic upstream

type openaiToAnthropic struct{}

func (openaiToAnthropic) TranslateRequest(_ Operation, body []byte, upstreamModel string) ([]byte, error) {
	var in struct {
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		Stream              bool     `json:"stream,omitempty"`
		MaxTokens           int      `json:"max_tokens,omitempty"`
		MaxCompletionTokens int      `json:"max_completion_tokens,omitempty"`
		Temperature         *float64 `json:"temperature,omitempty"`
		TopP                *float64 `json:"top_p,omitempty"`
		Tools               []struct {
			Type     string `json:"type"`
			Function struct {
				Name        string          `json:"name"`
				Description string          `json:"description,omitempty"`
				Parameters  json.RawMessage `json:"parameters,omitempty"`
			} `json:"function"`
		} `json:"tools,omitempty"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, errBadJSON
	}
	maxTok := in.MaxCompletionTokens
	if maxTok <= 0 {
		maxTok = in.MaxTokens
	}
	if maxTok <= 0 {
		maxTok = 4096
	}
	var (
		sysParts []string
		msgs     []map[string]any
	)
	for _, m := range in.Messages {
		role := strings.ToLower(m.Role)
		if role == "system" || role == "developer" {
			var s string
			if json.Unmarshal(m.Content, &s) == nil && s != "" {
				sysParts = append(sysParts, s)
			}
			continue
		}
		if role != "assistant" {
			role = "user"
		}
		msgs = append(msgs, map[string]any{"role": role, "content": m.Content})
	}
	if len(msgs) == 0 {
		return nil, errBadMessages
	}
	out := map[string]any{
		"model":      upstreamModel,
		"max_tokens": maxTok,
		"messages":   msgs,
	}
	if len(sysParts) > 0 {
		out["system"] = strings.Join(sysParts, "\n\n")
	}
	if in.Stream {
		out["stream"] = true
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if len(in.Tools) > 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for _, t := range in.Tools {
			if t.Function.Name == "" {
				continue
			}
			schema := t.Function.Parameters
			if len(schema) == 0 {
				schema = json.RawMessage(`{"type":"object","properties":{}}`)
			}
			tools = append(tools, map[string]any{
				"name":         t.Function.Name,
				"description":  t.Function.Description,
				"input_schema": schema,
			})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	return json.Marshal(out)
}

func (openaiToAnthropic) TranslateResponse(_ Operation, body []byte, requestedModel string) ([]byte, *Usage, error) {
	var in struct {
		ID      string `json:"id"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text,omitempty"`
			ID    string          `json:"id,omitempty"`
			Name  string          `json:"name,omitempty"`
			Input json.RawMessage `json:"input,omitempty"`
		} `json:"content"`
		StopReason string          `json:"stop_reason"`
		Usage      *anthropicUsage `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, nil, err
	}
	var (
		textParts []string
		toolCalls []map[string]any
	)
	for _, c := range in.Content {
		switch c.Type {
		case "text":
			textParts = append(textParts, c.Text)
		case "tool_use":
			args := "{}"
			if len(c.Input) > 0 {
				args = string(c.Input)
			}
			toolCalls = append(toolCalls, map[string]any{
				"id":   c.ID,
				"type": "function",
				"function": map[string]any{
					"name":      c.Name,
					"arguments": args,
				},
			})
		}
	}
	msg := map[string]any{
		"role":    "assistant",
		"content": strings.Join(textParts, ""),
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
	}
	finish := "stop"
	switch in.StopReason {
	case "max_tokens":
		finish = "length"
	case "tool_use":
		finish = "tool_calls"
	}
	var (
		meter anthropicMeter
		uMap  map[string]any
	)
	meter.merge(in.Usage, true)
	meter.stop = in.StopReason
	usage := meter.Usage()
	if usage != nil {
		prompt := usage.Input
		if usage.CacheRead != nil {
			prompt += *usage.CacheRead
		}
		uMap = map[string]any{
			"prompt_tokens":     prompt,
			"completion_tokens": usage.Output,
			"total_tokens":      prompt + usage.Output,
		}
	}
	id := in.ID
	if id == "" {
		id = "chatcmpl-translated"
	}
	resp := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"model":   requestedModel,
		"choices": []map[string]any{{"index": 0, "message": msg, "finish_reason": finish}},
	}
	if uMap != nil {
		resp["usage"] = uMap
	}
	out, err := json.Marshal(resp)
	return out, usage, err
}

func (openaiToAnthropic) TranslateSSE(w io.Writer, rc *http.ResponseController, body io.Reader, requestedModel string, maxCapture int) (*Usage, []json.RawMessage, error) {
	br := bufio.NewReaderSize(body, 64<<10)
	var (
		meter    anthropicMeter
		msgID    = "chatcmpl-stream"
		events   []json.RawMessage
		capBytes int
		sentRole bool
	)
	emitChunk := func(delta map[string]any, finish any) error {
		chunk := map[string]any{
			"id":      msgID,
			"object":  "chat.completion.chunk",
			"model":   requestedModel,
			"choices": []map[string]any{{"index": 0, "delta": delta, "finish_reason": finish}},
		}
		b, err := json.Marshal(chunk)
		if err != nil {
			return err
		}
		if maxCapture > 0 && capBytes+len(b) <= maxCapture {
			events = append(events, json.RawMessage(b))
			capBytes += len(b)
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return err
		}
		if ferr := rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
			return ferr
		}
		return nil
	}

	var line []byte
	for {
		part, err := br.ReadSlice('\n')
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(line) > maxSSELine {
				return nil, nil, errors.New("sse line too long")
			}
			continue
		}
		if len(line) > 0 {
			if data, ok := sseData(line); ok {
				var ev struct {
					Type    string `json:"type"`
					Message struct {
						ID    string          `json:"id"`
						Usage *anthropicUsage `json:"usage"`
					} `json:"message"`
					Delta struct {
						Type       string `json:"type"`
						Text       string `json:"text"`
						StopReason string `json:"stop_reason"`
					} `json:"delta"`
					Usage *anthropicUsage `json:"usage"`
				}
				if json.Unmarshal(data, &ev) == nil {
					switch ev.Type {
					case "message_start":
						if ev.Message.ID != "" {
							msgID = ev.Message.ID
						}
						meter.merge(ev.Message.Usage, true)
						if !sentRole {
							sentRole = true
							if werr := emitChunk(map[string]any{"role": "assistant"}, nil); werr != nil {
								return nil, nil, werr
							}
						}
					case "content_block_delta":
						if ev.Delta.Text != "" {
							if !sentRole {
								sentRole = true
								_ = emitChunk(map[string]any{"role": "assistant"}, nil)
							}
							if werr := emitChunk(map[string]any{"content": ev.Delta.Text}, nil); werr != nil {
								return nil, nil, werr
							}
						}
					case "message_delta":
						meter.merge(ev.Usage, false)
						if ev.Delta.StopReason != "" {
							meter.stop = ev.Delta.StopReason
						}
						finish := "stop"
						if ev.Delta.StopReason == "max_tokens" {
							finish = "length"
						}
						if werr := emitChunk(map[string]any{}, finish); werr != nil {
							return nil, nil, werr
						}
					}
				}
			}
			line = line[:0]
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, nil, err
		}
	}
	if _, err := io.WriteString(w, "data: [DONE]\n\n"); err != nil {
		return nil, nil, err
	}
	_ = rc.Flush()
	return meter.Usage(), events, nil
}

// ------------------------------------------------ Anthropic client -> OpenAI upstream

type anthropicToOpenAI struct{}

func (anthropicToOpenAI) TranslateRequest(_ Operation, body []byte, upstreamModel string) ([]byte, error) {
	var in struct {
		System   json.RawMessage `json:"system,omitempty"`
		Messages []struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
		MaxTokens   int      `json:"max_tokens,omitempty"`
		Stream      bool     `json:"stream,omitempty"`
		Temperature *float64 `json:"temperature,omitempty"`
		TopP        *float64 `json:"top_p,omitempty"`
		Tools       []struct {
			Name        string          `json:"name"`
			Description string          `json:"description,omitempty"`
			InputSchema json.RawMessage `json:"input_schema,omitempty"`
		} `json:"tools,omitempty"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, errBadJSON
	}
	var msgs []map[string]any
	if len(in.System) > 0 && string(in.System) != "null" {
		var sysText string
		if json.Unmarshal(in.System, &sysText) == nil && sysText != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": sysText})
		}
	}
	for _, m := range in.Messages {
		msgs = append(msgs, map[string]any{"role": m.Role, "content": m.Content})
	}
	if len(msgs) == 0 {
		return nil, errBadMessages
	}
	out := map[string]any{
		"model":    upstreamModel,
		"messages": msgs,
	}
	if in.MaxTokens > 0 {
		out["max_tokens"] = in.MaxTokens
	}
	if in.Stream {
		out["stream"] = true
		out["stream_options"] = map[string]any{"include_usage": true}
	}
	if in.Temperature != nil {
		out["temperature"] = *in.Temperature
	}
	if in.TopP != nil {
		out["top_p"] = *in.TopP
	}
	if len(in.Tools) > 0 {
		tools := make([]map[string]any, 0, len(in.Tools))
		for _, t := range in.Tools {
			if t.Name == "" {
				continue
			}
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        t.Name,
					"description": t.Description,
					"parameters":  t.InputSchema,
				},
			})
		}
		if len(tools) > 0 {
			out["tools"] = tools
		}
	}
	return json.Marshal(out)
}

func (anthropicToOpenAI) TranslateResponse(_ Operation, body []byte, requestedModel string) ([]byte, *Usage, error) {
	var in struct {
		ID      string `json:"id"`
		Choices []struct {
			Message struct {
				Content   string `json:"content"`
				ToolCalls []struct {
					ID       string `json:"id"`
					Function struct {
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"function"`
				} `json:"tool_calls"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
		Usage *struct {
			PromptTokens        int64 `json:"prompt_tokens"`
			CompletionTokens    int64 `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int64 `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(body, &in); err != nil {
		return nil, nil, err
	}
	var contentBlocks []map[string]any
	stop := "end_turn"
	if len(in.Choices) > 0 {
		ch := in.Choices[0]
		if ch.Message.Content != "" || len(ch.Message.ToolCalls) == 0 {
			contentBlocks = append(contentBlocks, map[string]any{"type": "text", "text": ch.Message.Content})
		}
		for _, tc := range ch.Message.ToolCalls {
			var input any = map[string]any{}
			if tc.Function.Arguments != "" {
				var parsed any
				if json.Unmarshal([]byte(tc.Function.Arguments), &parsed) == nil {
					input = parsed
				}
			}
			contentBlocks = append(contentBlocks, map[string]any{
				"type":  "tool_use",
				"id":    tc.ID,
				"name":  tc.Function.Name,
				"input": input,
			})
		}
		switch {
		case ch.FinishReason == "tool_calls" || len(ch.Message.ToolCalls) > 0:
			stop = "tool_use"
		case ch.FinishReason == "length":
			stop = "max_tokens"
		}
	}
	if len(contentBlocks) == 0 {
		contentBlocks = []map[string]any{{"type": "text", "text": ""}}
	}
	var (
		u    *Usage
		uMap = map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)}
	)
	if in.Usage != nil {
		u = &Usage{
			Input:      in.Usage.PromptTokens,
			Output:     in.Usage.CompletionTokens,
			StopReason: llmtypes.NormalizeStopReason(stop),
		}
		if d := in.Usage.PromptTokensDetails; d != nil {
			u.Input = max(u.Input-d.CachedTokens, 0)
			u.CacheRead = ptr(d.CachedTokens)
		}
		uMap["input_tokens"] = u.Input
		uMap["output_tokens"] = u.Output
	}
	id := in.ID
	if id == "" {
		id = "msg_translated"
	}
	resp := map[string]any{
		"id":          id,
		"type":        "message",
		"role":        "assistant",
		"model":       requestedModel,
		"content":     contentBlocks,
		"stop_reason": stop,
		"usage":       uMap,
	}
	out, err := json.Marshal(resp)
	return out, u, err
}

func (anthropicToOpenAI) TranslateSSE(w io.Writer, rc *http.ResponseController, body io.Reader, requestedModel string, maxCapture int) (*Usage, []json.RawMessage, error) {
	br := bufio.NewReaderSize(body, 64<<10)
	var (
		meter    openaiMeter
		events   []json.RawMessage
		capBytes int
		started  bool
	)
	writeEvent := func(evType string, payload map[string]any) error {
		b, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		if maxCapture > 0 && capBytes+len(b) <= maxCapture {
			events = append(events, json.RawMessage(b))
			capBytes += len(b)
		}
		if _, err := fmt.Fprintf(w, "event: %s\ndata: %s\n\n", evType, b); err != nil {
			return err
		}
		if ferr := rc.Flush(); ferr != nil && !errors.Is(ferr, http.ErrNotSupported) {
			return ferr
		}
		return nil
	}

	var line []byte
	for {
		part, err := br.ReadSlice('\n')
		line = append(line, part...)
		if errors.Is(err, bufio.ErrBufferFull) {
			if len(line) > maxSSELine {
				return nil, nil, errors.New("sse line too long")
			}
			continue
		}
		if len(line) > 0 {
			if data, ok := sseData(line); ok && string(data) != "[DONE]" {
				var p struct {
					ID      string `json:"id"`
					Choices []struct {
						Delta struct {
							Content string `json:"content"`
						} `json:"delta"`
						FinishReason string `json:"finish_reason"`
					} `json:"choices"`
					Usage *struct {
						PromptTokens        int64 `json:"prompt_tokens"`
						CompletionTokens    int64 `json:"completion_tokens"`
						PromptTokensDetails *struct {
							CachedTokens int64 `json:"cached_tokens"`
						} `json:"prompt_tokens_details"`
					} `json:"usage"`
				}
				if json.Unmarshal(data, &p) == nil {
					meter.Body(data)
					if !started {
						started = true
						if werr := writeEvent("message_start", map[string]any{
							"type": "message_start",
							"message": map[string]any{
								"id": p.ID, "type": "message", "role": "assistant", "model": requestedModel,
								"usage": map[string]any{"input_tokens": int64(0), "output_tokens": int64(0)},
							},
						}); werr != nil {
							return nil, nil, werr
						}
					}
					for _, c := range p.Choices {
						if c.Delta.Content != "" {
							if werr := writeEvent("content_block_delta", map[string]any{
								"type":  "content_block_delta",
								"index": 0,
								"delta": map[string]any{"type": "text_delta", "text": c.Delta.Content},
							}); werr != nil {
								return nil, nil, werr
							}
						}
					}
				}
			}
			line = line[:0]
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return nil, nil, err
		}
	}
	usage := meter.Usage()
	inTok, outTok := int64(0), int64(0)
	if usage != nil {
		inTok, outTok = usage.Input, usage.Output
	}
	if err := writeEvent("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": "end_turn"},
		"usage": map[string]any{"input_tokens": inTok, "output_tokens": outTok},
	}); err != nil {
		return nil, nil, err
	}
	if err := writeEvent("message_stop", map[string]any{"type": "message_stop"}); err != nil {
		return nil, nil, err
	}
	return usage, events, nil
}
