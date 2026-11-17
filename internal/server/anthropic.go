// anthropic.go Anthropic Messages 协议转换层（POST /v1/messages）。
//
// 存在理由：Claude Code 等 Anthropic 系客户端只说 Messages 协议（POST /v1/messages），
// 而网关的模型端点是 OpenAI Chat 形态（/v1/chat/completions）。原站上游也不提供
// /v1/messages（实测 global 404、CN 500，且 /v2/chat/completions 只收流式），因此
// 必须在网关侧做协议双向转换：
//
//	入站 Anthropic Messages ──► 内部 OpenAI Chat body ──► 上游 /v2/chat/completions(流式)
//	上游 SSE chunk ──► Anthropic SSE 事件 / 聚合 Message
//
// 复用既有 chat 路径的轮转/租约/刷新/错误策略/usage 记账，不重造内核；本文件只做
// 协议适配与响应形态转换。
package server

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"

	"github.com/linguo2625469/workbuddy2api-panel/internal/auth"
	"github.com/linguo2625469/workbuddy2api-panel/internal/logfmt"
	"github.com/linguo2625469/workbuddy2api-panel/internal/session"
	"github.com/linguo2625469/workbuddy2api-panel/internal/upstream"
)

// defaultAnthropicModel 请求 model 无法识别时的回落目标（claude-* 等别名映射到此）。
// 可用环境变量 WB2A_ANTHROPIC_MODEL 覆盖（Claude Code 的 ANTHROPIC_MODEL 可指向
// 任意 claude-* 名，由本层统一映射到网关实际可用模型）。
const defaultAnthropicModel = "cn:glm-5.2"

// anthropicRequest Anthropic Messages 请求中与转换相关的字段。
type anthropicRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Messages      []anthropicTurn `json:"messages"`
	System        json.RawMessage `json:"system"`
	Tools         []anthropicTool `json:"tools"`
	ToolChoice    json.RawMessage `json:"tool_choice"`
	Stream        bool            `json:"stream"`
	Temperature   *float64        `json:"temperature"`
	TopP          *float64        `json:"top_p"`
	StopSequences []string        `json:"stop_sequences"`
}

// anthropicTurn 单条消息。content 既可能是字符串，也可能是 content block 数组。
type anthropicTurn struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

// anthropicTool Anthropic 工具声明（input_schema 即 OpenAI 的 parameters）。
type anthropicTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// anthropicBlock Anthropic content block。只解析本层关心的类型：
// text / tool_use / tool_result（thinking、图片等降级忽略）。
type anthropicBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
	IsError   bool            `json:"is_error"`
}

// openAIChunk 上游流式帧（OpenAI chat.completion.chunk 形态）。
type openAIChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage map[string]any `json:"usage"`
}

// anthropicMessages 处理 POST /v1/messages（Anthropic Messages 协议）。
func (h *Handler) anthropicMessages(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "failed to read request body")
		return
	}
	var req anthropicRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", "request body must be a JSON object")
		return
	}
	if strings.TrimSpace(req.Model) == "" || req.MaxTokens <= 0 {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error",
			"model and max_tokens are required")
		return
	}

	chatModel := mapAnthropicModel(req.Model)
	chatBody, err := toChatBody(&req, chatModel)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}

	realm, bareModel, _ := resolveModelDetail(chatModel)
	// realm 前缀是网关侧路由协议，上游只认裸模型名（与 chat 路径 D6 同口径）。
	chatBody = rewriteModel(chatBody, bareModel)
	clientIP := upstream.ExtractClientIP(r)
	meta := searchMetaFromRequest(r)

	// 会话粘性：复用 chat 路径同一套会话键提取（body 携带 conversation_id /
	// prompt_cache_key 等时生效；Claude Code 一般靠轮转，无显式键）。
	sessKey := session.ExtractKey(chatBody)
	stickyUID := ""
	if h.cfg.Session != nil && sessKey != "" {
		if uid, ok := h.cfg.Session.ResolveForModel(sessKey, chatModel); ok {
			stickyUID = uid
		}
	}

	tried := map[string]bool{}
	var lastErr error
	var lastStatus int
	var lastKind upstream.ErrKind

	for i := 0; i < h.cfg.MaxRotate; i++ {
		var acct *auth.Auth
		if stickyUID != "" && !tried[stickyUID] {
			acct = h.cfg.Pool.PickByUIDForModel(stickyUID, bareModel)
		}
		if acct == nil {
			acct = h.cfg.Pool.PickExcludingForRealm(tried, bareModel, realm)
		}
		if acct == nil {
			break
		}
		tried[acct.UID] = true
		if !h.cfg.Pool.Acquire(acct.UID) {
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}
		// token 临近过期先刷新（与 chat 同口径，避免过期 token 被当账号故障处置）。
		if acct.NeedsRefresh(h.cfg.RefreshSkew) {
			if rerr := h.cfg.Upstream.RefreshToken(acct); rerr != nil {
				h.cfg.Pool.Release(acct.UID)
				var ue *upstream.Error
				if errors.As(rerr, &ue) {
					lastStatus, lastKind, lastErr = ue.Status, ue.Kind, ue
					h.applyErrorPolicy(acct.UID, ue.Kind, ue.Msg, bareModel, ue)
				} else {
					h.cfg.Pool.NoteFailures(acct.UID)
					lastStatus, lastErr = http.StatusServiceUnavailable, rerr
				}
				if !rotateBackoff(i, r.Context()) {
					break
				}
				continue
			}
			if serr := acct.SaveAtomic(); serr != nil {
				log.Printf("ERR: [server] anthropic refresh acct=%s: save auth failed: %v",
					logfmt.Label(acct.UID, acct.Nickname), serr)
			}
		}

		rc, status, respBody, cerr := h.cfg.Upstream.ChatStreamContext(r.Context(), acct, chatBody, clientIP, meta)
		h.cfg.Pool.Release(acct.UID)
		if cerr != nil {
			var ue *upstream.Error
			if errors.As(cerr, &ue) {
				lastStatus, lastKind, lastErr = ue.Status, ue.Kind, ue
				h.applyErrorPolicy(acct.UID, ue.Kind, ue.Msg, bareModel, ue)
			} else {
				h.cfg.Pool.NoteFailures(acct.UID)
				lastStatus, lastErr = http.StatusServiceUnavailable, cerr
			}
			_ = status
			_ = respBody
			if !rotateBackoff(i, r.Context()) {
				break
			}
			continue
		}

		if tr := requestTraceFrom(r); tr != nil {
			tr.account = logfmt.Label(acct.UID, acct.Nickname)
			if tr.model == "" {
				tr.model = bareModel
			}
		}

		if req.Stream {
			// 上游只在流末帧回传真实 prompt_tokens；为让客户端（claude-hud 等）
			// 从第一帧起就有可用的 input_tokens，先用请求内容估算，末帧再用上游值覆盖。
			streamAnthropic(w, rc, chatModel, estimateInputTokens(&req))
		} else {
			resp, aerr := upstream.Aggregate(rc)
			rc.Close()
			if aerr != nil {
				writeAnthropicError(w, http.StatusBadGateway, "api_error", "upstream parse failed: "+aerr.Error())
				return
			}
			writeJSON(w, http.StatusOK, buildAnthropicMessage(resp, chatModel))
		}

		h.cfg.Pool.NoteSuccess(acct.UID)
		h.cfg.Pool.BlockModelClear(acct.UID, bareModel)
		if h.cfg.Session != nil && sessKey != "" {
			h.cfg.Session.Bind(sessKey, acct.UID)
		}
		return
	}

	writeAnthropicFailure(w, lastErr, lastStatus, lastKind)
}

// anthropicModelAliases 客户端习惯的模型显示名 → 网关实际模型。
// 客户端（如 Claude Code）常把供应商侧的展示名（含 [1M] 之类的上下文标注）
// 直接当 model 发来；这里按归一化后的名字映射到账号池内真实可用模型。
var anthropicModelAliases = map[string]string{
	"deepseek-v4.1-flash": "cn:deepseek-v4.1-flash",
	"deepseek-v4.1-pro":   "cn:deepseek-v4.1-pro",
	"deepseek-v4-flash":   "cn:deepseek-v4-flash",
	"deepseek-v4-pro":     "cn:deepseek-v4-pro",
	"glm-5.2":             "cn:glm-5.2",
	"glm-5.3":             "cn:glm-5.3",
	"glm-5.3-flash":       "cn:glm-5.3-flash",
	"kimi-k3":             "cn:kimi-k3",
	"kimi-k3-1":           "cn:kimi-k3-1",
	"minimax-m3.1-flash":  "cn:minimax-m3.1-flash",
}

// normalizeModelAlias 归一化模型显示名：去掉 [1M]/[1m] 等后缀，转小写去空白，
// 使 "Deepseek-V4.1-Flash[1M]" 与 "deepseek-v4.1-flash" 同键。
func normalizeModelAlias(s string) string {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, '['); i >= 0 {
		s = s[:i]
	}
	return strings.ToLower(strings.TrimSpace(s))
}

// mapAnthropicModel 把入站 model 映射为网关实际模型：
//   - 已带 cn:/global: 前缀 → 原样使用（用户显式指定域，直接依赖上游名称）
//   - 命中别名表（deepseek/glm/kimi 等显示名）→ 对应实际模型
//   - 其余（claude-* 等）→ WB2A_ANTHROPIC_MODEL 或默认 cn:glm-5.2
//
// 设计原则：尽量依赖上游名称，少写胶水。[1m]/[1M] 这类上下文标注是 Claude Code
// 的 UI 显示约定，客户端发请求时已自行剥离，网关收不到带后缀的 model，故无需处理。
func mapAnthropicModel(model string) string {
	if strings.HasPrefix(model, "cn:") || strings.HasPrefix(model, "global:") {
		return model
	}
	if m, ok := anthropicModelAliases[normalizeModelAlias(model)]; ok {
		return m
	}
	if def := strings.TrimSpace(os.Getenv("WB2A_ANTHROPIC_MODEL")); def != "" {
		return def
	}
	return defaultAnthropicModel
}

// estimateInputTokens 粗略估算请求输入 token 数。
//
// 用途：流式首帧 message_start.usage.input_tokens。上游只在**流末帧**回传真实
// prompt_tokens，而客户端状态栏（claude-hud 等）需要从第一帧起就有可用的
// input_tokens 才能算出上下文占用；否则恒为 0，上下文条一直显示 0%。
// 口径：CJK 约 0.75 token/字，其余约 4 字符/token；覆盖 system / tools / messages 原文。
func estimateInputTokens(req *anthropicRequest) int {
	var b strings.Builder
	if len(req.System) > 0 {
		b.Write(req.System)
	}
	if len(req.Tools) > 0 {
		if raw, err := json.Marshal(req.Tools); err == nil {
			b.Write(raw)
		}
	}
	for _, m := range req.Messages {
		b.Write(m.Content)
	}
	cjk, other := 0, 0
	for _, r := range b.String() {
		if (r >= 0x4E00 && r <= 0x9FFF) || (r >= 0x3000 && r <= 0x303F) || (r >= 0xFF00 && r <= 0xFFEF) {
			cjk++
		} else {
			other++
		}
	}
	est := int(float64(cjk)*0.75 + float64(other)/4.0)
	if est < 1 {
		est = 1
	}
	return est
}

// toChatBody 把 Anthropic Messages 请求转成内部 OpenAI Chat body（流式）。
// 上游只支持流式，故无论入站是否 stream，body 恒 stream:true。
func toChatBody(req *anthropicRequest, chatModel string) ([]byte, error) {
	msgs := make([]any, 0, len(req.Messages)+1)
	if len(req.System) > 0 && string(req.System) != "null" {
		if text, ok := systemText(req.System); ok && strings.TrimSpace(text) != "" {
			msgs = append(msgs, map[string]any{"role": "system", "content": text})
		}
	}
	for _, m := range req.Messages {
		blocks, isBlocks := parseBlocks(m.Content)
		if !isBlocks {
			var s string
			if err := json.Unmarshal(m.Content, &s); err != nil {
				return nil, fmt.Errorf("message content must be a string or an array of content blocks")
			}
			msgs = append(msgs, map[string]any{"role": anthropicRoleToChat(m.Role), "content": s})
			continue
		}

		var textParts []string
		var toolUses, toolResults []anthropicBlock
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					textParts = append(textParts, b.Text)
				}
			case "tool_use":
				toolUses = append(toolUses, b)
			case "tool_result":
				toolResults = append(toolResults, b)
			}
		}
		text := strings.Join(textParts, "\n")

		// tool_result：拆成 OpenAI role=tool 消息（tool_call_id 即 Anthropic tool_use_id，
		// 原样透传，省掉一层 id 映射）。
		//
		// 顺序关键：OpenAI 要求 role=tool 消息**紧跟**声明它的 assistant(tool_calls)。
		// 同一条 user 消息里若混着正文，必须把正文放到 tool 消息**之后**，否则
		// assistant(tool_calls) → user(正文) → tool(result) 会被上游判 11148
		// （tool calls and tool results do not match / tool_call_sequence_broken）。
		if len(toolResults) > 0 {
			for _, tr := range toolResults {
				msgs = append(msgs, map[string]any{
					"role": "tool", "tool_call_id": tr.ToolUseID, "content": toolResultContent(tr),
				})
			}
			if strings.TrimSpace(text) != "" {
				msgs = append(msgs, map[string]any{"role": "user", "content": text})
			}
			continue
		}
		// tool_use：assistant 消息携带 tool_calls（arguments 用原始 JSON 串）。
		if len(toolUses) > 0 {
			calls := make([]any, 0, len(toolUses))
			for i, tu := range toolUses {
				args := "{}"
				if len(tu.Input) > 0 {
					args = string(tu.Input)
				}
				calls = append(calls, map[string]any{
					"id": tu.ID, "type": "function", "index": i,
					"function": map[string]any{"name": tu.Name, "arguments": args},
				})
			}
			msgs = append(msgs, map[string]any{"role": "assistant", "content": text, "tool_calls": calls})
			continue
		}
		msgs = append(msgs, map[string]any{"role": anthropicRoleToChat(m.Role), "content": text})
	}

	body := map[string]any{
		"model":      chatModel,
		"messages":   msgs,
		"max_tokens": req.MaxTokens,
		"stream":     true,
	}
	if req.Temperature != nil {
		body["temperature"] = *req.Temperature
	}
	if req.TopP != nil {
		body["top_p"] = *req.TopP
	}
	if len(req.StopSequences) > 0 {
		body["stop"] = req.StopSequences
	}
	if len(req.Tools) > 0 {
		tools := make([]any, 0, len(req.Tools))
		for _, t := range req.Tools {
			tools = append(tools, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name": t.Name, "description": t.Description, "parameters": t.InputSchema,
				},
			})
		}
		body["tools"] = tools
	}
	if len(req.ToolChoice) > 0 {
		if tc := parseToolChoice(req.ToolChoice); tc != nil {
			body["tool_choice"] = tc
		}
	}
	return json.Marshal(body)
}

// parseBlocks 解析 content：是 block 数组时返回 (blocks, true)，否则 (nil, false)。
func parseBlocks(raw json.RawMessage) ([]anthropicBlock, bool) {
	if len(raw) == 0 {
		return nil, false
	}
	trimmed := strings.TrimSpace(string(raw))
	if !strings.HasPrefix(trimmed, "[") {
		return nil, false
	}
	var blocks []anthropicBlock
	if json.Unmarshal(raw, &blocks) != nil {
		return nil, false
	}
	return blocks, true
}

// systemText 提取 system 文本：支持字符串与 block 数组（取 text 块拼接，忽略 cache_control）。
func systemText(raw json.RawMessage) (string, bool) {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s, true
	}
	blocks, ok := parseBlocks(raw)
	if !ok {
		return "", false
	}
	var parts []string
	for _, b := range blocks {
		if b.Type == "text" && strings.TrimSpace(b.Text) != "" {
			parts = append(parts, b.Text)
		}
	}
	return strings.Join(parts, "\n"), true
}

// toolResultContent 提取 tool_result 的 content：字符串原样；block 数组取 text 拼接。
func toolResultContent(b anthropicBlock) string {
	if len(b.Content) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(b.Content, &s) == nil {
		return s
	}
	blocks, ok := parseBlocks(b.Content)
	if !ok {
		return ""
	}
	var parts []string
	for _, pb := range blocks {
		if pb.Type == "text" && strings.TrimSpace(pb.Text) != "" {
			parts = append(parts, pb.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// anthropicRoleToChat 角色映射：assistant 原样；其余（user 等）归 user。
func anthropicRoleToChat(role string) string {
	if role == "assistant" {
		return "assistant"
	}
	return "user"
}

// parseToolChoice 把 Anthropic tool_choice 转 OpenAI 形态。
func parseToolChoice(raw json.RawMessage) any {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		switch s {
		case "any":
			return "required"
		case "auto", "none":
			return s
		}
		return nil
	}
	var obj struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if json.Unmarshal(raw, &obj) == nil && obj.Type == "tool" && obj.Name != "" {
		return map[string]any{"type": "function", "function": map[string]any{"name": obj.Name}}
	}
	return nil
}

// streamAnthropic 把上游 OpenAI chunk 流转成 Anthropic SSE 事件流。
//
// 事件序列：message_start → (content_block_start → content_block_delta* →
// content_block_stop)* → message_delta → message_stop。
// 文本块 index 动态分配；每个工具调用一个 tool_use 块，arguments 以 input_json_delta
// 分片（客户端拼接后为完整 JSON）。
func streamAnthropic(w http.ResponseWriter, rc io.ReadCloser, model string, estInput int) {
	defer rc.Close()
	fl, _ := w.(http.Flusher)
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")

	msgID := "msg_" + session.NewMessageID()
	send := func(event string, payload any) {
		data, err := json.Marshal(payload)
		if err != nil {
			return
		}
		if _, werr := io.WriteString(w, "event: "+event+"\ndata: "+string(data)+"\n\n"); werr != nil {
			return
		}
		if fl != nil {
			fl.Flush()
		}
	}

	send("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": msgID, "type": "message", "role": "assistant", "content": []any{},
			"model": model, "stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{"input_tokens": estInput, "output_tokens": 0},
		},
	})

	br := bufio.NewReaderSize(rc, 64*1024)
	textIdx := -1
	toolIdx := map[int]int{} // 上游 tool_call index → Anthropic block index
	nextIdx := 0
	finishReason := ""
	outTokens := 0
	inTokens := estInput

	for {
		line, rerr := br.ReadString('\n')
		if rerr != nil && line == "" {
			break
		}
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			break
		}
		var chunk openAIChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil || len(chunk.Choices) == 0 {
			continue
		}
		ch := chunk.Choices[0]
		if ch.Delta.Content != "" && textIdx == -1 {
			textIdx = nextIdx
			nextIdx++
			send("content_block_start", map[string]any{
				"type": "content_block_start", "index": textIdx,
				"content_block": map[string]any{"type": "text", "text": ""},
			})
		}
		if ch.Delta.Content != "" {
			send("content_block_delta", map[string]any{
				"type": "content_block_delta", "index": textIdx,
				"delta": map[string]any{"type": "text_delta", "text": ch.Delta.Content},
			})
		}
		for _, tc := range ch.Delta.ToolCalls {
			bi, seen := toolIdx[tc.Index]
			if !seen {
				bi = nextIdx
				nextIdx++
				toolIdx[tc.Index] = bi
				send("content_block_start", map[string]any{
					"type": "content_block_start", "index": bi,
					"content_block": map[string]any{
						"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": map[string]any{},
					},
				})
			}
			if tc.Function.Arguments != "" {
				send("content_block_delta", map[string]any{
					"type": "content_block_delta", "index": bi,
					"delta": map[string]any{"type": "input_json_delta", "partial_json": tc.Function.Arguments},
				})
			}
		}
		if ch.FinishReason != "" {
			finishReason = ch.FinishReason
		}
		if chunk.Usage != nil {
			if v, ok := chunk.Usage["prompt_tokens"].(float64); ok && v > 0 {
				inTokens = int(v)
			}
			if v, ok := chunk.Usage["completion_tokens"].(float64); ok {
				outTokens = int(v)
			}
		}
		if rerr == io.EOF {
			break
		}
	}

	if textIdx >= 0 {
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": textIdx})
	}
	for _, bi := range toolIdx {
		send("content_block_stop", map[string]any{"type": "content_block_stop", "index": bi})
	}
	send("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{"stop_reason": finishToStopReason(finishReason), "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": inTokens, "output_tokens": outTokens},
	})
	send("message_stop", map[string]any{"type": "message_stop"})
}

// buildAnthropicMessage 把聚合后的 OpenAI 响应转成 Anthropic Message。
func buildAnthropicMessage(resp map[string]any, model string) map[string]any {
	content := []any{}
	stopReason := "end_turn"
	if choices, ok := resp["choices"].([]any); ok && len(choices) > 0 {
		if c, ok := choices[0].(map[string]any); ok {
			if m, ok := c["message"].(map[string]any); ok {
				if txt, _ := m["content"].(string); txt != "" {
					content = append(content, map[string]any{"type": "text", "text": txt})
				}
				for _, tc := range toolCallMaps(m["tool_calls"]) {
					fn, _ := tc["function"].(map[string]any)
					name, _ := fn["name"].(string)
					args, _ := fn["arguments"].(string)
					var input any = map[string]any{}
					if json.Unmarshal([]byte(args), &input) != nil {
						input = map[string]any{}
					}
					id, _ := tc["id"].(string)
					content = append(content, map[string]any{
						"type": "tool_use", "id": id, "name": name, "input": input,
					})
				}
			}
			if fr, ok := c["finish_reason"].(string); ok && fr != "" {
				stopReason = finishToStopReason(fr)
			}
		}
	}
	return map[string]any{
		"id":            "msg_" + session.NewMessageID(),
		"type":          "message",
		"role":          "assistant",
		"model":         model,
		"content":       content,
		"stop_reason":   stopReason,
		"stop_sequence": nil,
		"usage":         anthropicUsage(resp),
	}
}

// toolCallMaps 自适应提取 tool_calls：Aggregate 输出 []map[string]any，
// JSON 反序列化路径则是 []any，两者都要能解析。
func toolCallMaps(v any) []map[string]any {
	switch t := v.(type) {
	case []map[string]any:
		return t
	case []any:
		out := make([]map[string]any, 0, len(t))
		for _, e := range t {
			if m, ok := e.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	return nil
}

// anthropicUsage 从 OpenAI usage 映射 input/output tokens。
func anthropicUsage(resp map[string]any) map[string]any {
	out := map[string]any{"input_tokens": 0, "output_tokens": 0}
	if u, ok := resp["usage"].(map[string]any); ok {
		if v, ok := u["prompt_tokens"].(float64); ok {
			out["input_tokens"] = int(v)
		}
		if v, ok := u["completion_tokens"].(float64); ok {
			out["output_tokens"] = int(v)
		}
	}
	return out
}

// finishToStopReason OpenAI finish_reason → Anthropic stop_reason。
func finishToStopReason(fr string) string {
	switch fr {
	case "tool_calls":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop", "":
		return "end_turn"
	default:
		return "end_turn"
	}
}

// writeAnthropicError 按 Anthropic 错误形态写出。
func writeAnthropicError(w http.ResponseWriter, status int, errType, msg string) {
	writeJSON(w, status, map[string]any{
		"type":  "error",
		"error": map[string]any{"type": errType, "message": msg},
	})
}

// writeAnthropicFailure 轮转全部失败后的出口：映射状态码与错误类型。
func writeAnthropicFailure(w http.ResponseWriter, err error, status int, kind upstream.ErrKind) {
	if err == nil {
		writeAnthropicError(w, http.StatusServiceUnavailable, "api_error", "no healthy account")
		return
	}
	st := http.StatusBadGateway
	if status >= 400 && status < 600 {
		st = status
	}
	if kind == upstream.ErrSoftRate {
		st = http.StatusTooManyRequests
	}
	writeAnthropicError(w, st, anthropicErrorType(st), err.Error())
}

// anthropicErrorType HTTP 状态码 → Anthropic 错误类型。
func anthropicErrorType(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusUnauthorized, http.StatusForbidden:
		return "authentication_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case 529:
		return "overloaded_error"
	default:
		return "api_error"
	}
}
