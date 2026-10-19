package loomy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"

	"workbuddy-api/internal/loopguard"
	"workbuddy-api/internal/provider"
)

// 上游已是标准 OpenAI SSE（`data: {...}` + 结尾 `data: [DONE]`），
// 事件形状见阶段 C 实测：`object:"chat.completion.chunk"`，
// `choices[0].delta` 里 `reasoning_content` 与 `content` 分别下发（思考先于正文）。

// Stream 实现 provider.Upstream。
func (c *Client) Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	return Stream(w, r, model)
}

// Aggregate 实现 provider.Upstream。
func (c *Client) Aggregate(r io.Reader, model string) (map[string]any, error) {
	return aggregate(r, model)
}

// streamSniffLimit 非 SSE 原文的留存上限：足够覆盖多行业务错误信封，
// 又不会把整段异常响应驻留内存。
const streamSniffLimit = 8 << 10

// Stream 边读边把上游 SSE 转写为标准 OpenAI SSE 给客户端（每个 chunk 重写 model）。
// 返回末帧捕获的 usage（OpenAI 形状，供 handler 记 token 流水）；上游未返回时为 nil。
func Stream(w http.ResponseWriter, r io.Reader, model string) (map[string]any, error) {
	var usage map[string]any
	h := w.Header()
	h.Set("Content-Type", "text/event-stream")
	h.Set("Cache-Control", "no-cache")
	h.Set("Connection", "keep-alive")
	h.Set("X-Accel-Buffering", "no")
	fl, _ := w.(http.Flusher)
	flush := func() {
		if fl != nil {
			fl.Flush()
		}
	}
	// 思考链死循环守卫：仅在全局启用时创建。命中即中断、不罚账号。
	var guard *loopguard.Guard
	if loopguard.Enabled() {
		guard = loopguard.NewDefault()
	}
	// 上游可能整段不是 SSE（HTTP 200 + 纯 JSON 业务错误体）：记录「是否透传过有效帧」
	// 与「被跳过的原文」，供流结束时兜底判定（覆盖多行信封 / 非 `{` 开头行）。
	var (
		sawFrame bool
		skipped  strings.Builder
	)

	sc := newSSEScanner(r)
	for sc.Scan() {
		line := sc.Text()
		payload, ok := ssePayload(line)
		if !ok {
			// 非 `data:` 行：可能是「HTTP 200 + 业务码」信封（上游鉴权失效就走这条形态）。
			// 只对以 `{` 开头的行做判定——注释行/空行里偶然出现的数字不该触发误判。
			if trimmed := strings.TrimSpace(line); strings.HasPrefix(trimmed, "{") {
				if berr := errFromBusinessEnvelope(trimmed); berr != nil {
					var pe *provider.Error
					if errors.As(berr, &pe) {
						writeErrorFrame(w, pe.Kind, pe.Msg)
					}
					return usage, berr
				}
			}
			// 同时留存非 SSE 原文：若整段都不是 SSE（多行信封、或错误体不满足单行判定），
			// 由流末的统一判定兜底。上限见 streamSniffLimit。
			if !sawFrame && skipped.Len() < streamSniffLimit {
				skipped.WriteString(line)
				skipped.WriteByte('\n')
			}
			continue
		}
		sawFrame = true
		if payload == "[DONE]" {
			_, _ = io.WriteString(w, "data: [DONE]\n\n")
			flush()
			return usage, nil
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			if _, werr := fmt.Fprintf(w, "data: %s\n\n", payload); werr != nil {
				return usage, werr
			}
			flush()
			continue
		}
		// 思考链死循环守卫：只在 reasoning_content 非空时喂入。命中即中断。
		if guard != nil {
			if rc := loopguard.ReasoningDeltaOf(chunk); rc != "" {
				if hit := guard.Push(rc); hit != nil {
					gerr := &loopguard.Error{Hit: *hit}
					if _, werr := io.WriteString(w, "data: "+loopguard.ErrorFrameJSON(hit)+"\n\n"); werr != nil {
						log.Printf("loomy reasoning loop error frame write failed: %v", werr)
					} else {
						flush()
					}
					return usage, gerr
				}
			}
		}
		// usage 捕获：末帧覆盖前面（OpenAI 语义末帧才是全量），并照常透传
		if u, ok := chunk["usage"].(map[string]any); ok && len(u) > 0 {
			usage = u
		}
		chunk["model"] = model
		raw, err := json.Marshal(chunk)
		if err != nil {
			continue
		}
		if _, err := fmt.Fprintf(w, "data: %s\n\n", raw); err != nil {
			return usage, err
		}
		flush()
	}
	if err := sc.Err(); err != nil {
		// 流中断（空闲超时 / 连接被切断）：响应头早已按 200 发出，只能用流内帧表达故障。
		// 不补帧 = 客户端收到无收尾的截断流 =「突然无响应」（2026-09-24 实测形态）。
		provider.WriteTruncationFrames(w, err)
		return usage, err
	}
	// 整段没有任何有效 SSE 帧，且残留文本是上游业务错误体（HTTP 200 + 业务码）
	// ⇒ 这不是「空回答」，是上游以 200 返回的鉴权/业务失败。补一帧 error（**不补 [DONE]**）
	// 并把错误上抛，避免「静默成功 + 空流」把故障藏起来。
	if !sawFrame {
		if berr := errFromBusinessEnvelope(skipped.String()); berr != nil {
			var pe *provider.Error
			if errors.As(berr, &pe) {
				writeErrorFrame(w, pe.Kind, pe.Msg)
			}
			return usage, berr
		}
	}
	_, _ = io.WriteString(w, "data: [DONE]\n\n")
	flush()
	return usage, nil
}

// aggregate 把上游响应转成单个 chat.completion（非流式请求走这里）。
//
// 上游对非流式请求**可能直接返回 JSON 而不是 SSE**（实测 Loomy 的 stream=false 路径就是普通
// chat.completion JSON）。若只按 SSE 解析，会得到「content 空 + created 用 time.Now() 兜底」的
// 假响应 —— 静默丢失全部内容。所以先探测 JSON，探测不到再回落 SSE 聚合。
func aggregate(r io.Reader, model string) (map[string]any, error) {
	raw, err := io.ReadAll(io.LimitReader(r, 16<<20))
	if err != nil {
		return nil, err
	}
	trimmed := bytes.TrimSpace(raw)
	if len(trimmed) > 0 && trimmed[0] == '{' {
		// ⚠️ 先判业务信封：`{"code":"100002"}` 同样以 `{` 开头，直接当 chat.completion 返回
		// 会让客户端收到一个**没有 choices 的假响应**（2026-10-17 修复 D1）。
		if berr := errFromBusinessEnvelope(string(trimmed)); berr != nil {
			return nil, berr
		}
		var out map[string]any
		if err := json.Unmarshal(trimmed, &out); err == nil {
			// 回填客户端模型名（上游常返回裸名或别名）。
			out["model"] = model
			return out, nil
		}
	}
	return aggregateSSE(bytes.NewReader(trimmed), model)
}

// aggregateSSE 按 SSE 逐块聚合（上游返回流式时的路径）。
func aggregateSSE(r io.Reader, model string) (map[string]any, error) {
	var (
		id        string
		created   float64
		content   strings.Builder
		reasoning strings.Builder
		finish    string
		usage     map[string]any
		toolIdx   []int
		toolCalls = map[int]map[string]any{}
	)

	sc := newSSEScanner(r)
	for sc.Scan() {
		payload, ok := ssePayload(sc.Text())
		if !ok || payload == "[DONE]" {
			continue
		}
		var chunk map[string]any
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}
		if id == "" {
			if s, ok := chunk["id"].(string); ok {
				id = s
			}
		}
		if c, ok := chunk["created"].(float64); ok && created == 0 {
			created = c
		}
		if u, ok := chunk["usage"].(map[string]any); ok {
			usage = u
		}
		choices, _ := chunk["choices"].([]any)
		if len(choices) == 0 {
			continue
		}
		ch, _ := choices[0].(map[string]any)
		if ch == nil {
			continue
		}
		if fr, ok := ch["finish_reason"].(string); ok && fr != "" {
			finish = fr
		}
		delta, _ := ch["delta"].(map[string]any)
		if delta == nil {
			if msg, ok := ch["message"].(map[string]any); ok {
				delta = msg
			}
		}
		if delta == nil {
			continue
		}
		if s, ok := delta["content"].(string); ok {
			content.WriteString(s)
		}
		if s, ok := delta["reasoning_content"].(string); ok {
			reasoning.WriteString(s)
		}
		if tcs, ok := delta["tool_calls"].([]any); ok {
			for _, item := range tcs {
				tc, _ := item.(map[string]any)
				if tc == nil {
					continue
				}
				idx := 0
				if f, ok := tc["index"].(float64); ok {
					idx = int(f)
				}
				merged, seen := toolCalls[idx]
				if !seen {
					merged = map[string]any{"index": idx}
					toolCalls[idx] = merged
					toolIdx = append(toolIdx, idx)
				}
				mergeToolCallDelta(merged, tc)
			}
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}

	msg := map[string]any{"role": "assistant", "content": content.String()}
	if reasoning.Len() > 0 {
		msg["reasoning_content"] = reasoning.String()
	}
	if len(toolIdx) > 0 {
		sort.Ints(toolIdx)
		list := make([]any, 0, len(toolIdx))
		for _, i := range toolIdx {
			list = append(list, toolCalls[i])
		}
		msg["tool_calls"] = list
	}
	if finish == "" {
		finish = "stop"
	}
	if created == 0 {
		created = float64(time.Now().Unix())
	}
	out := map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": int64(created),
		"model":   model,
		"choices": []any{map[string]any{
			"index":         0,
			"message":       msg,
			"finish_reason": finish,
		}},
	}
	if usage != nil {
		out["usage"] = usage
	}
	return out, nil
}

// mergeToolCallDelta 合并流式 tool_calls 分片（function.arguments 为字符串拼接）。
func mergeToolCallDelta(merged, delta map[string]any) {
	if s, ok := delta["id"].(string); ok && s != "" {
		merged["id"] = s
	}
	if s, ok := delta["type"].(string); ok && s != "" {
		merged["type"] = s
	}
	dfn, _ := delta["function"].(map[string]any)
	if dfn == nil {
		return
	}
	fn, _ := merged["function"].(map[string]any)
	if fn == nil {
		fn = map[string]any{}
		merged["function"] = fn
	}
	if s, ok := dfn["name"].(string); ok && s != "" {
		fn["name"] = s
	}
	if s, ok := dfn["arguments"].(string); ok && s != "" {
		prev, _ := fn["arguments"].(string)
		fn["arguments"] = prev + s
	}
}

// newSSEScanner SSE 行扫描器（单行上限 4 MiB）。
func newSSEScanner(r io.Reader) *bufio.Scanner {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	return sc
}

// ssePayload 取一行的 `data:` 载荷；非 data 行返回 ok=false。
func ssePayload(line string) (string, bool) {
	line = strings.TrimRight(line, "\r")
	if !strings.HasPrefix(line, "data:") {
		return "", false
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" {
		return "", false
	}
	return payload, true
}

// ---------------------------------------------------------------------------
// 「HTTP 200 + 业务码」信封识别（2026-10-17 修复 D1）
// ---------------------------------------------------------------------------

// errFromBusinessEnvelope 识别 Loomy 用 **HTTP 200** 表达的业务失败（如 `{"code":"100002"}`）。
//
// 为什么必须在这里再判一次：`ChatStream` 对 200 是**直接透传响应体**的，而下游解析只认
// SSE 的 `data:` 行 —— 于是这个信封会被整行跳过，表现为：
//   - 流式路径：一行有效帧都没有 → 写 `data: [DONE]` 后 `return nil, nil`
//     ⇒ 客户端看到「空回答但成功」（用户以为模型不说话，实际是凭据失效）；
//   - 非流式路径（aggregate）：见到 `{` 就当 chat.completion 返回
//     ⇒ 一个**没有 choices 的假响应**（比空流更难查）。
//
// 命中时返回带语义的 *provider.Error（100002/020002 → ErrSessionDead），由调用方决定
// 是写错误帧（流式）还是直接上抛（非流式）。未命中返回 nil。
func errFromBusinessEnvelope(body string) error {
	kind := businessErrKind(body)
	if kind == provider.ErrNone {
		return nil
	}
	return &provider.Error{Kind: kind, Status: http.StatusOK, Msg: truncateForFrame(body)}
}

// truncateForFrame 截断上游原文到 200 字符：错误帧会进客户端日志，塞入整个响应体会撑大它。
func truncateForFrame(s string) string {
	s = strings.TrimSpace(s)
	rs := []rune(s)
	if len(rs) <= 200 {
		return s
	}
	return string(rs[:200])
}

// writeErrorFrame 在**已开始输出**的流里补写一帧 error（**不补 `[DONE]`**）并 flush。
//
// 形态与 `provider.WriteTruncationFrames` 一致（`data: {"error":{message,type,code}}`），
// 便于客户端用同一套逻辑处理「流内错误」。R36 的纪律：失败不得伪装成正常收尾。
func writeErrorFrame(w io.Writer, kind provider.ErrKind, msg string) {
	frame, err := json.Marshal(map[string]any{"error": map[string]any{
		"message": truncateForFrame(msg),
		"type":    "upstream_error",
		"code":    kind.String(),
	}})
	if err != nil {
		// 纯字符串 map 不该序列化失败；退化成固定帧，仍保证收尾。
		frame = []byte(`{"error":{"message":"upstream error","type":"upstream_error","code":"none"}}`)
	}
	_, _ = w.Write(append(append([]byte("data: "), frame...), []byte("\n\n")...))
	if fl, ok := w.(interface{ Flush() }); ok {
		fl.Flush()
	}
}
