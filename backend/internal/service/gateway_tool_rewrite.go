package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"hash/fnv"
	"math/rand"
	"sort"
	"strings"

	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// toolNameRewriteKey 是 gin.Context 上存 ToolNameRewrite 映射的 key。
// 请求阶段写入，响应阶段读取，用于在结构化协议名称字段中还原假名 → 真名。
const toolNameRewriteKey = "claude_tool_name_rewrite"

// cacheInjectTraceKey 是 gin.Context 上存缓存注入 trace 的 key。
// rewriteMessageCacheControlIfEnabled 在请求阶段写入；handler 的 RecordUsage 闭包读取。
const cacheInjectTraceKey = "cache_inject_trace"

// CacheInjectTraceKey 返回 gin.Context 上缓存注入 trace 的 key（供 handler 层读取）。
func CacheInjectTraceKey() string { return cacheInjectTraceKey }

// staticToolNameRewrites 是"静态前缀映射"，与 Parrot src/transform/cc_mimicry.py
// TOOL_NAME_REWRITES 完全一致。只有以这些前缀开头的工具会被重写。
var staticToolNameRewrites = map[string]string{
	"sessions_": "cc_sess_",
	"session_":  "cc_ses_",
}

// fakeToolNamePrefixes 是"动态映射"的前缀池，与 Parrot _FAKE_PREFIXES 一致。
// 当 tools 数量 > dynamicToolMapThreshold 时随机选用其中前缀生成可读假名。
var fakeToolNamePrefixes = []string{
	"analyze_", "compute_", "fetch_", "generate_", "lookup_", "modify_",
	"process_", "query_", "render_", "resolve_", "sync_", "update_",
	"validate_", "convert_", "extract_", "manage_", "monitor_", "parse_",
	"review_", "search_", "transform_", "handle_", "invoke_", "notify_",
}

// dynamicToolMapThreshold 与 Parrot 一致：tools 数量超过 5 才启用动态映射。
// 少量工具不需要混淆（一般是 Claude Code 自己的核心工具 bash/edit/read 等）。
const dynamicToolMapThreshold = 5

// ToolNameRewrite 是单次请求内的工具名混淆映射。
//   - Forward: real → fake，请求阶段在 body 的协议工具字段上应用。
//   - Reverse: fake → real，响应阶段仅还原已知协议工具字段。
//
// ReverseOrdered 是按假名长度倒序的 (fake, real) 列表，保留既有映射顺序；
// 结构化响应还原现在按字段值精确查表，不依赖子串替换顺序。
type ToolNameRewrite struct {
	Forward        map[string]string
	Reverse        map[string]string
	ReverseOrdered [][2]string
}

// buildDynamicToolMap 构造 tools 的动态假名映射。
//
// 与 Parrot _build_dynamic_tool_map 语义等价：
//   - tools 数量 ≤ dynamicToolMapThreshold 时返回 nil（不做动态映射，走静态 fallback）
//   - 同一组 tool_names 在同进程内映射稳定（保证 cache 命中）
//
// Parrot 用 `random.Random(hash(tuple(tool_names)))` 作 seed + shuffle 前缀池；
// Go 无法字节级复刻 Python hash，但"稳定性"和"前缀池打散"两个不变量都保留：
// 用 fnv64a(strings.Join(names, "\x00")) 作 seed 喂 math/rand.New。
// 字节级不同不影响上游判定（Anthropic 不会验证我们的随机种子算法）。
func buildDynamicToolMap(toolNames []string) map[string]string {
	if len(toolNames) <= dynamicToolMapThreshold {
		return nil
	}
	h := fnv.New64a()
	for i, n := range toolNames {
		if i > 0 {
			_, _ = h.Write([]byte{0})
		}
		_, _ = h.Write([]byte(n))
	}
	rng := rand.New(rand.NewSource(int64(h.Sum64())))

	available := make([]string, len(fakeToolNamePrefixes))
	copy(available, fakeToolNamePrefixes)
	rng.Shuffle(len(available), func(i, j int) { available[i], available[j] = available[j], available[i] })

	mapping := make(map[string]string, len(toolNames))
	for i, name := range toolNames {
		prefix := available[i%len(available)]
		headLen := 3
		if len(name) < 3 {
			headLen = len(name)
		}
		fake := fmt.Sprintf("%s%s%02d", prefix, name[:headLen], i)
		mapping[name] = fake
	}
	return mapping
}

// sanitizeToolName 把真名转成假名。
// 与 Parrot _sanitize_tool_name 语义一致：动态映射优先，再走静态前缀映射。
func sanitizeToolName(name string, dynamic map[string]string) string {
	if dynamic != nil {
		if fake, ok := dynamic[name]; ok {
			return fake
		}
	}
	for prefix, replacement := range staticToolNameRewrites {
		if strings.HasPrefix(name, prefix) {
			return replacement + name[len(prefix):]
		}
	}
	return name
}

// shouldMimicToolName 指示某个 tool 是否需要重命名。
// server tool（type != "" 且不是 "function" / "custom"）是 Anthropic 协议语义的一部分，
// 比如 "web_search_20250305" / "computer_20250124"；误改会导致上游拒绝。
func shouldMimicToolName(toolType string) bool {
	if toolType == "" || toolType == "function" || toolType == "custom" {
		return true
	}
	return false
}

// buildToolNameRewriteFromBody 扫描 body 的 tools[*].name，构造 ToolNameRewrite
// 并返回它。若不需要混淆（tools 数量不足 + 没有匹配静态前缀的工具）返回 nil。
//
// 注意：只扫描，不改 body。真正的 body 改写在 applyToolNameRewriteToBody。
func buildToolNameRewriteFromBody(body []byte) *ToolNameRewrite {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return nil
	}

	mimicableNames := make([]string, 0)
	toolsArr := tools.Array()
	for _, t := range toolsArr {
		if !shouldMimicToolName(t.Get("type").String()) {
			continue
		}
		name := t.Get("name").String()
		if name == "" {
			continue
		}
		mimicableNames = append(mimicableNames, name)
	}

	dynamic := buildDynamicToolMap(mimicableNames)

	rw := &ToolNameRewrite{
		Forward: make(map[string]string),
		Reverse: make(map[string]string),
	}
	for _, name := range mimicableNames {
		fake := sanitizeToolName(name, dynamic)
		if fake == name {
			continue
		}
		rw.Forward[name] = fake
		rw.Reverse[fake] = name
	}
	if len(rw.Forward) == 0 {
		return nil
	}

	rw.ReverseOrdered = make([][2]string, 0, len(rw.Reverse))
	for fake, real := range rw.Reverse {
		rw.ReverseOrdered = append(rw.ReverseOrdered, [2]string{fake, real})
	}
	sort.SliceStable(rw.ReverseOrdered, func(i, j int) bool {
		return len(rw.ReverseOrdered[i][0]) > len(rw.ReverseOrdered[j][0])
	})

	return rw
}

type toolNameSpan struct {
	start, end int
	value      []byte
}

// applyToolNameRewriteToBody 把已构造的 ToolNameRewrite 应用到 body 上：
//
//   - 改写 $.tools[*].name（仅对 shouldMimicToolName 通过的 tool）
//   - 改写 $.tool_choice.name（仅当 $.tool_choice.type == "tool"）
//   - 改写 tool_use.name 和 tool_result 内 tool_reference.tool_name
//   - 在 $.tools[last].cache_control 上打 ephemeral 缓存断点
func applyToolNameRewriteToBody(body []byte, rw *ToolNameRewrite) []byte {
	if rw == nil || len(rw.Forward) == 0 {
		body = applyToolsLastCacheBreakpoint(body)
		return body
	}

	// gjson child indexes are absolute offsets into the original body. Gather all
	// replacements before changing its length, then copy the body only once.
	var edits []toolNameSpan
	addName := func(name gjson.Result) {
		if !name.Exists() {
			return
		}
		fake, ok := rw.Forward[name.String()]
		if !ok {
			return
		}
		encoded, err := json.Marshal(fake)
		if err != nil || name.Index < 0 || name.Index+len(name.Raw) > len(body) ||
			!bytes.Equal(body[name.Index:name.Index+len(name.Raw)], []byte(name.Raw)) {
			return
		}
		edits = append(edits, toolNameSpan{name.Index, name.Index + len(name.Raw), encoded})
	}

	tools := gjson.GetBytes(body, "tools")
	if tools.IsArray() {
		tools.ForEach(func(_, tool gjson.Result) bool {
			if shouldMimicToolName(tool.Get("type").String()) {
				addName(tool.Get("name"))
			}
			return true
		})
	}
	if choice := gjson.GetBytes(body, "tool_choice"); choice.Get("type").String() == "tool" {
		addName(choice.Get("name"))
	}
	if messages := gjson.GetBytes(body, "messages"); messages.IsArray() {
		messages.ForEach(func(_, msg gjson.Result) bool {
			content := msg.Get("content")
			if content.IsArray() {
				content.ForEach(func(_, block gjson.Result) bool {
					addAnthropicToolBlockName(block, addName)
					return true
				})
			}
			return true
		})
	}
	if len(edits) != 0 {
		sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
		var out []byte
		out = make([]byte, 0, len(body))
		pos := 0
		for _, edit := range edits {
			if edit.start < pos { // Ignore any overlapping result from malformed JSON.
				continue
			}
			out = append(out, body[pos:edit.start]...)
			out = append(out, edit.value...)
			pos = edit.end
		}
		body = append(out, body[pos:]...)
	}

	body = applyToolsLastCacheBreakpoint(body)
	return body
}

func addAnthropicToolBlockName(block gjson.Result, addName func(gjson.Result)) {
	switch block.Get("type").String() {
	case "tool_use":
		addName(block.Get("name"))
	case "tool_result":
		content := block.Get("content")
		if content.IsArray() {
			content.ForEach(func(_, nested gjson.Result) bool {
				addAnthropicToolReferenceName(nested, addName)
				return true
			})
		}
	case "tool_search_tool_result", "tool_search_tool_search_result":
		addAnthropicToolReferenceName(block, addName)
	}
}

func addAnthropicToolReferenceName(block gjson.Result, addName func(gjson.Result)) {
	switch block.Get("type").String() {
	case "tool_reference":
		addName(block.Get("tool_name"))
	case "tool_search_tool_result", "tool_search_tool_search_result":
		refs := block.Get("content.tool_references")
		if !refs.Exists() {
			refs = block.Get("tool_references")
		}
		if refs.IsArray() {
			refs.ForEach(func(_, ref gjson.Result) bool {
				if ref.Get("type").String() == "tool_reference" {
					addName(ref.Get("tool_name"))
				}
				return true
			})
		}
	}
}

// applyToolsLastCacheBreakpoint 在最后一个非延迟加载工具上注入 cache_control
// 断点。Anthropic 不允许 defer_loading=true 的工具携带 cache_control，
// 因此会先清理所有延迟加载工具上的客户端断点。兼容官方顶层字段和
// Claude Code 使用的 custom.defer_loading 字段。其余行为对齐 Parrot
// `tools[-1]["cache_control"] = {"type":"ephemeral","ttl":"1h"}`，
// 但 ttl 按本仓规则：
//   - 客户端已为该 tool 显式设置 cache_control.ttl → 完全透传不覆盖
//   - 否则注入 {"type":"ephemeral","ttl": claude.DefaultCacheControlTTL}
//
// 纯副作用函数，tools 不存在或为空数组时 no-op。
func applyToolsLastCacheBreakpoint(body []byte) []byte {
	body = stripDeferredToolCacheControl(body)
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	arr := tools.Array()
	if len(arr) == 0 {
		return body
	}
	lastIdx := -1
	for idx, tool := range arr {
		if isDeferredLoadingTool(tool) {
			continue
		}
		lastIdx = idx
	}
	if lastIdx == -1 {
		return body
	}

	existingCC := arr[lastIdx].Get("cache_control")

	if existingCC.Exists() && existingCC.Get("ttl").String() != "" {
		return body
	}

	if existingCC.Exists() {
		if next, err := sjson.SetBytes(body, fmt.Sprintf("tools.%d.cache_control.ttl", lastIdx), claude.DefaultCacheControlTTL); err == nil {
			body = next
		}
		return body
	}

	raw := fmt.Sprintf(`{"type":"ephemeral","ttl":%q}`, claude.DefaultCacheControlTTL)
	if next, err := sjson.SetRawBytes(body, fmt.Sprintf("tools.%d.cache_control", lastIdx), []byte(raw)); err == nil {
		body = next
	}
	return body
}

func isDeferredLoadingTool(tool gjson.Result) bool {
	return tool.Get("defer_loading").Type == gjson.True ||
		tool.Get("custom.defer_loading").Type == gjson.True
}

// stripDeferredToolCacheControl removes the cache marker Anthropic rejects on
// deferred tools. Only the literal JSON boolean true enables deferred loading.
func stripDeferredToolCacheControl(body []byte) []byte {
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	for idx, tool := range tools.Array() {
		if !isDeferredLoadingTool(tool) || !tool.Get("cache_control").Exists() {
			continue
		}
		if next, err := sjson.DeleteBytes(body, fmt.Sprintf("tools.%d.cache_control", idx)); err == nil {
			body = next
		}
	}
	return body
}

// restoreToolNamesInBytes restores names only in structured Anthropic tool
// reference fields. It accepts either a JSON response body or SSE data lines.
// rw may be nil; without a request mapping, names are left untouched.
func restoreToolNamesInBytes(data []byte, rw *ToolNameRewrite) []byte {
	if rw == nil || len(rw.Reverse) == 0 {
		return data
	}
	return rewriteAnthropicToolNameFields(data, func(name string) (string, bool) {
		real, ok := rw.Reverse[name]
		return real, ok && name != real
	})
}

func rewriteAnthropicToolNameFields(data []byte, replace func(string) (string, bool)) []byte {
	if len(data) == 0 {
		return data
	}
	var edits []toolNameSpan
	collect := func(payload []byte, base int) {
		if !gjson.ValidBytes(payload) {
			return
		}
		addField := func(object gjson.Result, key string) {
			field := object.Get(key)
			if field.Type != gjson.String {
				return
			}
			if replacement, ok := replace(field.String()); ok && field.Index >= 0 && field.Index+len(field.Raw) <= len(payload) {
				encoded, err := json.Marshal(replacement)
				if err == nil {
					edits = append(edits, toolNameSpan{base + field.Index, base + field.Index + len(field.Raw), encoded})
				}
			}
		}
		collectReference := func(block gjson.Result) {
			switch block.Get("type").String() {
			case "tool_reference":
				addField(block, "tool_name")
			case "tool_search_tool_result", "tool_search_tool_search_result":
				refs := block.Get("content.tool_references")
				if !refs.Exists() {
					refs = block.Get("tool_references")
				}
				if refs.IsArray() {
					refs.ForEach(func(_, ref gjson.Result) bool {
						if ref.Get("type").String() == "tool_reference" {
							addField(ref, "tool_name")
						}
						return true
					})
				}
			}
		}
		collectBlocks := func(blocks gjson.Result) {
			if !blocks.IsArray() {
				return
			}
			blocks.ForEach(func(_, block gjson.Result) bool {
				switch block.Get("type").String() {
				case "tool_use":
					addField(block, "name")
				case "tool_result":
					content := block.Get("content")
					if content.IsArray() {
						content.ForEach(func(_, nested gjson.Result) bool { collectReference(nested); return true })
					}
				default:
					collectReference(block)
				}
				return true
			})
		}
		root := gjson.ParseBytes(payload)
		collectBlocks(root.Get("content"))
		collectBlocks(root.Get("message.content"))
		if root.Get("content_block").Exists() {
			block := root.Get("content_block")
			if block.Get("type").String() == "tool_use" {
				addField(block, "name")
			} else {
				collectReference(block)
			}
		}
	}

	if gjson.ValidBytes(data) {
		collect(data, 0)
	} else {
		// Stream call sites pass complete SSE lines. Process only their data
		// payloads, preserving event names, spacing, and line endings verbatim.
		for start := 0; start < len(data); {
			end := bytes.IndexByte(data[start:], '\n')
			if end < 0 {
				end = len(data)
			} else {
				end += start
			}
			lineEnd := end
			if lineEnd > start && data[lineEnd-1] == '\r' {
				lineEnd--
			}
			line := data[start:lineEnd]
			if bytes.HasPrefix(line, []byte("data:")) {
				payloadStart := start + len("data:")
				if payloadStart < lineEnd && data[payloadStart] == ' ' {
					payloadStart++
				}
				collect(data[payloadStart:lineEnd], payloadStart)
			}
			if end == len(data) {
				break
			}
			start = end + 1
		}
	}
	if len(edits) == 0 {
		return data
	}
	sort.Slice(edits, func(i, j int) bool { return edits[i].start < edits[j].start })
	out := make([]byte, 0, len(data))
	pos := 0
	for _, edit := range edits {
		if edit.start < pos || edit.end > len(data) {
			continue
		}
		out = append(out, data[pos:edit.start]...)
		out = append(out, edit.value...)
		pos = edit.end
	}
	return append(out, data[pos:]...)
}

// toolNameRewriteFromContext 从 gin.Context 取出请求阶段保存的工具名映射。
// 找不到（c==nil 或 key 不存在或类型不对）时返回 nil；调用方必须能处理 nil。
func toolNameRewriteFromContext(c interface {
	Get(string) (any, bool)
}) *ToolNameRewrite {
	if c == nil {
		return nil
	}
	raw, ok := c.Get(toolNameRewriteKey)
	if !ok || raw == nil {
		return nil
	}
	rw, _ := raw.(*ToolNameRewrite)
	return rw
}

// reverseToolNamesIfPresent 是响应侧注入点的统一封装：从 c 取出 request mapping，
// 并仅还原已知协议工具名字段；没有 mapping 时保持原始响应不变。
func reverseToolNamesIfPresent(c interface {
	Get(string) (any, bool)
}, chunk []byte) []byte {
	rw := toolNameRewriteFromContext(c)
	if rw == nil || len(rw.Reverse) == 0 {
		return chunk
	}
	return restoreToolNamesInBytes(chunk, rw)
}
