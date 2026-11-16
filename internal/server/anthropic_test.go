package server

import (
	"encoding/json"
	"testing"
)

// decodeBody 把 toChatBody 的产物解回 map，便于断言。
func decodeBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("unmarshal chat body: %v", err)
	}
	return m
}

func TestToChatBodyText(t *testing.T) {
	req := &anthropicRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: 100,
		Messages: []anthropicTurn{
			{Role: "user", Content: json.RawMessage(`"你好"`)},
		},
	}
	raw, err := toChatBody(req, "cn:glm-5.2")
	if err != nil {
		t.Fatalf("toChatBody: %v", err)
	}
	m := decodeBody(t, raw)
	if m["model"] != "cn:glm-5.2" {
		t.Errorf("model=%v want cn:glm-5.2", m["model"])
	}
	if m["stream"] != true {
		t.Errorf("stream=%v want true（上游只收流式）", m["stream"])
	}
	msgs := m["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("messages len=%d want 1", len(msgs))
	}
	first := msgs[0].(map[string]any)
	if first["role"] != "user" || first["content"] != "你好" {
		t.Errorf("first=%v", first)
	}
}

func TestToChatBodySystemBlocks(t *testing.T) {
	req := &anthropicRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: 50,
		System:    json.RawMessage(`[{"type":"text","text":"你是助手"},{"type":"text","text":"请简洁"}]`),
		Messages: []anthropicTurn{
			{Role: "user", Content: json.RawMessage(`"hi"`)},
		},
	}
	raw, err := toChatBody(req, "cn:glm-5.2")
	if err != nil {
		t.Fatalf("toChatBody: %v", err)
	}
	msgs := decodeBody(t, raw)["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("messages len=%d want 2", len(msgs))
	}
	sys := msgs[0].(map[string]any)
	if sys["role"] != "system" {
		t.Errorf("sys role=%v", sys["role"])
	}
	if sys["content"] != "你是助手\n请简洁" {
		t.Errorf("sys content=%q", sys["content"])
	}
}

func TestToChatBodyToolUseAndResult(t *testing.T) {
	req := &anthropicRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: 200,
		Messages: []anthropicTurn{
			{Role: "user", Content: json.RawMessage(`"查天气"`)},
			{Role: "assistant", Content: json.RawMessage(`[{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"北京"}}]`)},
			{Role: "user", Content: json.RawMessage(`[{"type":"tool_result","tool_use_id":"toolu_1","content":"晴 7℃"}]`)},
		},
	}
	raw, err := toChatBody(req, "cn:glm-5.2")
	if err != nil {
		t.Fatalf("toChatBody: %v", err)
	}
	msgs := decodeBody(t, raw)["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("messages len=%d want 3（user/assistant/tool）", len(msgs))
	}
	asst := msgs[1].(map[string]any)
	if asst["role"] != "assistant" {
		t.Errorf("asst role=%v", asst["role"])
	}
	calls := asst["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["id"] != "toolu_1" {
		t.Errorf("tool_call id=%v want toolu_1（透传）", call["id"])
	}
	fn := call["function"].(map[string]any)
	if fn["name"] != "get_weather" {
		t.Errorf("fn name=%v", fn["name"])
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "toolu_1" {
		t.Errorf("tool msg=%v", toolMsg)
	}
	if toolMsg["content"] != "晴 7℃" {
		t.Errorf("tool content=%v", toolMsg["content"])
	}
}

func TestToChatBodyToolsAndChoice(t *testing.T) {
	req := &anthropicRequest{
		Model:     "claude-sonnet-4",
		MaxTokens: 100,
		Messages:  []anthropicTurn{{Role: "user", Content: json.RawMessage(`"hi"`)}},
		Tools: []anthropicTool{
			{Name: "get_weather", Description: "查天气", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)},
		},
		ToolChoice: json.RawMessage(`{"type":"tool","name":"get_weather"}`),
	}
	raw, err := toChatBody(req, "cn:glm-5.2")
	if err != nil {
		t.Fatalf("toChatBody: %v", err)
	}
	m := decodeBody(t, raw)
	tools := m["tools"].([]any)
	f := tools[0].(map[string]any)["function"].(map[string]any)
	if f["name"] != "get_weather" {
		t.Errorf("tool name=%v", f["name"])
	}
	tc := m["tool_choice"].(map[string]any)
	if tc["type"] != "function" {
		t.Errorf("tool_choice=%v", tc)
	}
}

func TestBuildAnthropicMessageText(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "stop",
			"message":       map[string]any{"content": "1+1=2", "role": "assistant"},
		}},
		"usage": map[string]any{"prompt_tokens": float64(10), "completion_tokens": float64(5)},
	}
	msg := buildAnthropicMessage(resp, "cn:glm-5.2")
	if msg["type"] != "message" || msg["role"] != "assistant" {
		t.Errorf("msg=%v", msg)
	}
	if msg["stop_reason"] != "end_turn" {
		t.Errorf("stop_reason=%v", msg["stop_reason"])
	}
	content := msg["content"].([]any)
	if content[0].(map[string]any)["text"] != "1+1=2" {
		t.Errorf("content=%v", content)
	}
	usage := msg["usage"].(map[string]any)
	if usage["input_tokens"] != 10 || usage["output_tokens"] != 5 {
		t.Errorf("usage=%v", usage)
	}
}

func TestBuildAnthropicMessageToolUse(t *testing.T) {
	resp := map[string]any{
		"choices": []any{map[string]any{
			"finish_reason": "tool_calls",
			"message": map[string]any{
				"content": "",
				"tool_calls": []any{map[string]any{
					"id":   "toolu_1",
					"type": "function",
					"function": map[string]any{
						"name":      "get_weather",
						"arguments": `{"city":"北京"}`,
					},
				}},
			},
		}},
	}
	msg := buildAnthropicMessage(resp, "cn:glm-5.2")
	if msg["stop_reason"] != "tool_use" {
		t.Errorf("stop_reason=%v want tool_use", msg["stop_reason"])
	}
	block := msg["content"].([]any)[0].(map[string]any)
	if block["type"] != "tool_use" || block["id"] != "toolu_1" {
		t.Errorf("block=%v", block)
	}
	if block["input"].(map[string]any)["city"] != "北京" {
		t.Errorf("input=%v", block["input"])
	}
}

func TestMapAnthropicModel(t *testing.T) {
	if got := mapAnthropicModel("cn:glm-5.2"); got != "cn:glm-5.2" {
		t.Errorf("cn: prefix 应原样，got %q", got)
	}
	if got := mapAnthropicModel("global:gpt-5.6-sol"); got != "global:gpt-5.6-sol" {
		t.Errorf("global: prefix 应原样，got %q", got)
	}
	if got := mapAnthropicModel("claude-sonnet-4"); got != defaultAnthropicModel {
		t.Errorf("claude-* 应映射默认，got %q", got)
	}
	if got := mapAnthropicModel("Deepseek-V4.1-Flash[1M]"); got != "cn:deepseek-v4.1-flash" {
		t.Errorf("deepseek 显示名应映射别名，got %q", got)
	}
	if got := mapAnthropicModel("deepseek-v4-pro"); got != "cn:deepseek-v4-pro" {
		t.Errorf("deepseek-v4-pro 应映射别名，got %q", got)
	}
	t.Setenv("WB2A_ANTHROPIC_MODEL", "cn:deepseek-v4-flash")
	if got := mapAnthropicModel("claude-opus-4"); got != "cn:deepseek-v4-flash" {
		t.Errorf("环境变量覆盖失效，got %q", got)
	}
}

func TestFinishToStopReason(t *testing.T) {
	cases := map[string]string{
		"stop":       "end_turn",
		"tool_calls": "tool_use",
		"length":     "max_tokens",
		"":           "end_turn",
	}
	for in, want := range cases {
		if got := finishToStopReason(in); got != want {
			t.Errorf("finishToStopReason(%q)=%q want %q", in, got, want)
		}
	}
}
