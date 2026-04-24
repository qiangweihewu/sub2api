package service

import (
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// gateway_body_scrub.go
//
// 第三方客户端（OpenClaw、opencode、ACP 等）请求体里会携带强特征字符串（"OpenClaw"、
// 小写工具名、`[System Instructions]` 标记、`<env>...</env>` 这类编排 XML）。
// Anthropic 的第三方应用检测器会扫描 system/messages 的**内容**做语义匹配，
// 即便 header mimic 完美也会被判为第三方并返回 400 "Third-party apps now draw
// from your extra usage..."。
//
// 本文件提供三个独立的 scrub 技术，分别借鉴自：
//   - ObfuscateSensitiveWords: CLIProxyAPI 的零宽空格插入（cloak_obfuscate.go）
//   - StripThirdPartyMarkers:  Meridian 的 XML 编排标签剥离（sanitize.ts）
//   - RemapToolNames:          CLIProxyAPI 的 oauthToolRenameMap（claude_executor.go）
//
// 调用顺序由 ScrubThirdPartyBody 统筹：RemapToolNames → StripThirdPartyMarkers →
// ObfuscateSensitiveWords。先重命名/丢弃工具可以让后续的标记剥离不用再碰 tool schema，
// 而零宽空格混淆放在最后，避免自己插入的 ​ 被标签正则误删。

// zeroWidthSpace 是 U+200B 零宽空格；对人眼不可见，但打断了字符串连续匹配。
const zeroWidthSpace = "​"

// DefaultSensitiveWords 是默认的敏感词列表，会被 ObfuscateSensitiveWords 插入零宽空格。
// 扩展这份列表需要谨慎——误伤常规用户文本会降低模型理解质量。
var DefaultSensitiveWords = []string{
	"OpenClaw", "openclaw",
	"opencode", "OpenCode",
	"ACP", "ai-sdk",
	"[System Instructions]",
}

// oauthToolRenameMap 借鉴 CLIProxyAPI 的同名表（claude_executor.go:47），
// 补充了 exec/web_fetch/web_search 等 sub2api 观察到的第三方命名。
// key 必须全部小写——匹配时会把 tools[].name 先转小写再查表。
var oauthToolRenameMap = map[string]string{
	"bash":         "Bash",
	"read":         "Read",
	"write":        "Write",
	"edit":         "Edit",
	"glob":         "Glob",
	"grep":         "Grep",
	"task":         "Task",
	"webfetch":     "WebFetch",
	"web_fetch":    "WebFetch",
	"web_search":   "WebSearch",
	"todowrite":    "TodoWrite",
	"todoread":     "TodoRead",
	"ls":           "LS",
	"notebookedit": "NotebookEdit",
	"exec":         "Bash",
}

// oauthToolsDropList 列出必须被整体丢弃的工具名（没有 Claude Code 等价实现）。
// 对应的 tool_use / tool_result 也会被替换为占位文本，避免 assistant content 空数组。
var oauthToolsDropList = map[string]bool{
	"music_generate":   true,
	"video_generate":   true,
	"image_generate":   true,
	"sessions_spawn":   true,
	"sessions_list":    true,
	"sessions_history": true,
	"sessions_send":    true,
	"sessions_yield":   true,
	"subagents":        true,
	"session_status":   true,
	"memory_get":       true,
	"memory_search":    true,
}

// --------------------------------------------------------------------------
// ObfuscateSensitiveWords
// --------------------------------------------------------------------------

// ObfuscateSensitiveWords 遍历 body 里所有字符串类型的值（system 块 + messages
// content 块），对每一个命中的敏感词，在首字节和次字节之间插入一个零宽空格。
//
// 行为约定：
//   - 大小写不敏感匹配，但保留命中处的原始大小写。
//   - 如果词本身已经包含零宽空格，跳过（幂等）。
//   - 只修改字符串值，不碰 JSON key。
//   - 词长小于 2 rune 时跳过（没法插入）。
func ObfuscateSensitiveWords(body []byte, words []string) []byte {
	if len(body) == 0 || len(words) == 0 {
		return body
	}
	matcher := buildSensitiveWordRegex(words)
	if matcher == nil {
		return body
	}
	return walkStringValues(body, func(s string) string {
		return matcher.ReplaceAllStringFunc(s, insertZeroWidthSpace)
	})
}

// buildSensitiveWordRegex 构建大小写不敏感的 alternation 正则。长词优先匹配，
// 避免 "OpenClaw" 被 "open" 前缀切开。空输入返回 nil，调用方必须检查。
func buildSensitiveWordRegex(words []string) *regexp.Regexp {
	valid := make([]string, 0, len(words))
	for _, w := range words {
		w = strings.TrimSpace(w)
		if utf8.RuneCountInString(w) < 2 {
			continue
		}
		if strings.Contains(w, zeroWidthSpace) {
			continue
		}
		valid = append(valid, w)
	}
	if len(valid) == 0 {
		return nil
	}
	sort.Slice(valid, func(i, j int) bool {
		return len(valid[i]) > len(valid[j])
	})
	escaped := make([]string, len(valid))
	for i, w := range valid {
		escaped[i] = regexp.QuoteMeta(w)
	}
	re, err := regexp.Compile("(?i)" + strings.Join(escaped, "|"))
	if err != nil {
		return nil
	}
	return re
}

// insertZeroWidthSpace 在输入 word 的第 1 rune 和第 2 rune 之间插入零宽空格。
// 如果 word 已含零宽空格或长度不足，返回原值（幂等保证）。
func insertZeroWidthSpace(word string) string {
	if strings.Contains(word, zeroWidthSpace) {
		return word
	}
	r, size := utf8.DecodeRuneInString(word)
	if r == utf8.RuneError || size >= len(word) {
		return word
	}
	return string(r) + zeroWidthSpace + word[size:]
}

// --------------------------------------------------------------------------
// StripThirdPartyMarkers
// --------------------------------------------------------------------------

// orchestrationTags 来自 Meridian sanitize.ts 的 ORCHESTRATION_TAGS 列表，
// 精简到 sub2api 当前观察到的子集，避免误伤（例如 `thinking` 在 Anthropic 的
// structured content block 里是合法的，我们不在这里动它——只剥离作为 **文本内容**
// 出现的成对 XML 标签）。
var orchestrationTags = []string{
	"env",
	"system_information",
	"current_working_directory",
	"task_metadata",
	"tool_exec",
	"tool_output",
	"skill_content",
}

// pairedTagRegex 编译成形如 <env\b[^>]*>[\s\S]*?</env>，使用非贪婪匹配并关闭大小写敏感。
var pairedTagRegex = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(orchestrationTags))
	for _, tag := range orchestrationTags {
		out = append(out, regexp.MustCompile(`(?is)<`+regexp.QuoteMeta(tag)+`\b[^>]*>.*?</`+regexp.QuoteMeta(tag)+`>`))
	}
	return out
}()

// toolingSectionRegex 匹配 `\n## Tooling\n` 起始的段落，直到下一个空行或下一个
// `## ` 二级标题。这是 OpenClaw 往 user 消息里塞的工具列表，正是 Anthropic 拿来
// 做第三方匹配的强指纹。
var toolingSectionRegex = regexp.MustCompile(`(?s)\n## Tooling\n.*?(\n\n|\n## |$)`)

// systemInstructionsMarker 是被 rewriteSystemForNonClaudeCode 插入的包裹字符串。
// 虽然它是我们自己加的，但也是 Anthropic 第三方检测器的一个已知特征，要剥掉。
const systemInstructionsMarker = "[System Instructions]"

// StripThirdPartyMarkers 扫描 body 里所有字符串值并剥离第三方编排标记。
// 不会碰 JSON key 或非字符串字段（避免破坏 tools schema）。
func StripThirdPartyMarkers(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	return walkStringValues(body, stripMarkersFromString)
}

func stripMarkersFromString(s string) string {
	if s == "" {
		return s
	}
	// 1. 字面标记
	s = strings.ReplaceAll(s, systemInstructionsMarker, "")
	// 2. `\n## Tooling\n ...` 段落
	s = toolingSectionRegex.ReplaceAllStringFunc(s, func(match string) string {
		// 保留引出的下一标题/空行边界，避免把后续段落吃掉。
		if strings.HasSuffix(match, "\n## ") {
			return "\n## "
		}
		if strings.HasSuffix(match, "\n\n") {
			return "\n\n"
		}
		return ""
	})
	// 3. 成对 XML 标签
	for _, re := range pairedTagRegex {
		s = re.ReplaceAllString(s, "")
	}
	return s
}

// --------------------------------------------------------------------------
// RemapToolNames
// --------------------------------------------------------------------------

// RemapToolNames 同时处理 `tools[].name` 和 `messages[].content[].tool_use.name`，
// 把第三方惯用的小写/snake_case 名字改写成 Claude Code 的 TitleCase 名字。
// 同时丢弃 oauthToolsDropList 里的工具，连带清理引用它们的 tool_use / tool_result。
//
// 返回的 map 是 "原始名 -> 新名" 的实际使用情况（幂等：对已经是 TitleCase 的工具，
// 不会往 map 里塞冗余条目）。调用方可以拿这张表在响应流里做反向映射，把 TitleCase
// 名字改回客户端原始期望的名字（本次不实现反向映射，但保留扩展位）。
func RemapToolNames(body []byte) ([]byte, map[string]string) {
	used := make(map[string]string)
	if len(body) == 0 {
		return body, used
	}

	// 第一趟：扫 tools[]，构建 "drop + rename" 决策表。
	droppedToolNames := make(map[string]bool)   // 原始名 -> 需要丢弃
	renameDecisions := make(map[string]string)  // 原始名 -> 新名（不变则 key 不入表）

	toolsResult := gjson.GetBytes(body, "tools")
	if toolsResult.IsArray() {
		toolsResult.ForEach(func(_, tool gjson.Result) bool {
			origName := tool.Get("name").String()
			if origName == "" {
				return true
			}
			lower := strings.ToLower(origName)
			if oauthToolsDropList[lower] {
				droppedToolNames[origName] = true
				return true
			}
			if mapped, ok := oauthToolRenameMap[lower]; ok {
				if mapped != origName {
					renameDecisions[origName] = mapped
				}
			}
			return true
		})
	}

	// 第二趟：重建 tools 数组（过滤掉被丢弃的，改名保留的）。
	if toolsResult.IsArray() && (len(droppedToolNames) > 0 || len(renameDecisions) > 0) {
		newTools := make([]string, 0)
		toolsResult.ForEach(func(_, tool gjson.Result) bool {
			name := tool.Get("name").String()
			if droppedToolNames[name] {
				return true // 丢弃
			}
			raw := tool.Raw
			if mapped, ok := renameDecisions[name]; ok {
				updated, err := sjson.SetBytes([]byte(raw), "name", mapped)
				if err == nil {
					raw = string(updated)
					used[name] = mapped
				}
			}
			newTools = append(newTools, raw)
			return true
		})
		rebuilt := "[" + strings.Join(newTools, ",") + "]"
		if next, err := sjson.SetRawBytes(body, "tools", []byte(rebuilt)); err == nil {
			body = next
		}
	}

	// 第三趟：遍历 messages[].content[]，对 tool_use 做同样的改名/丢弃，
	// 同时收集被丢弃的 tool_use_id，方便下一轮清理对应的 tool_result。
	droppedToolUseIDs := make(map[string]bool)
	messagesResult := gjson.GetBytes(body, "messages")
	if messagesResult.IsArray() {
		messagesResult.ForEach(func(msgKey, msg gjson.Result) bool {
			content := msg.Get("content")
			if !content.IsArray() {
				return true
			}
			msgPath := "messages." + msgKey.String()
			// 先做改名（安全：不动数组长度）
			content.ForEach(func(blockKey, block gjson.Result) bool {
				if block.Get("type").String() != "tool_use" {
					return true
				}
				name := block.Get("name").String()
				if droppedToolNames[name] {
					if id := block.Get("id").String(); id != "" {
						droppedToolUseIDs[id] = true
					}
					return true
				}
				lower := strings.ToLower(name)
				if mapped, ok := oauthToolRenameMap[lower]; ok && mapped != name {
					path := msgPath + ".content." + blockKey.String() + ".name"
					if next, err := sjson.SetBytes(body, path, mapped); err == nil {
						body = next
						used[name] = mapped
					}
				}
				return true
			})
			return true
		})
	}

	// 第四趟：重建含有被丢弃 tool_use / tool_result 的 content 数组。
	// 这一步要慎重：删掉全部 block 会让 content 变空数组，Anthropic 会报 400。
	// 所以统一用一个占位 text block 代替。
	if len(droppedToolNames) > 0 || len(droppedToolUseIDs) > 0 {
		messagesResult = gjson.GetBytes(body, "messages")
		if messagesResult.IsArray() {
			messagesResult.ForEach(func(msgKey, msg gjson.Result) bool {
				content := msg.Get("content")
				if !content.IsArray() {
					return true
				}
				msgPath := "messages." + msgKey.String()
				newContent := make([]string, 0)
				removed := false
				content.ForEach(func(_, block gjson.Result) bool {
					btype := block.Get("type").String()
					if btype == "tool_use" {
						name := block.Get("name").String()
						if droppedToolNames[name] {
							removed = true
							return true
						}
					}
					if btype == "tool_result" {
						id := block.Get("tool_use_id").String()
						if droppedToolUseIDs[id] {
							removed = true
							return true
						}
					}
					newContent = append(newContent, block.Raw)
					return true
				})
				if !removed {
					return true
				}
				if len(newContent) == 0 {
					placeholder := `{"type":"text","text":"[omitted tool call]"}`
					newContent = append(newContent, placeholder)
				}
				rebuilt := "[" + strings.Join(newContent, ",") + "]"
				if next, err := sjson.SetRawBytes(body, msgPath+".content", []byte(rebuilt)); err == nil {
					body = next
				}
				return true
			})
		}
	}

	return body, used
}

// --------------------------------------------------------------------------
// ScrubThirdPartyBody - 顶层编排
// --------------------------------------------------------------------------

// ScrubThirdPartyBody 是 mimic 路径的顶层入口：按 rename → strip markers →
// obfuscate 的顺序清洗 body。返回改后的 body 和实际生效的 tool rename 映射
// （可用于响应反向映射，当前调用方丢弃该返回值）。
func ScrubThirdPartyBody(body []byte) ([]byte, map[string]string) {
	if len(body) == 0 {
		return body, map[string]string{}
	}
	body, remap := RemapToolNames(body)
	body = StripThirdPartyMarkers(body)
	body = ObfuscateSensitiveWords(body, DefaultSensitiveWords)
	return body, remap
}

// --------------------------------------------------------------------------
// 辅助函数：遍历 body 里所有字符串值
// --------------------------------------------------------------------------

// walkStringValues 对 system 块、messages content 块、以及 messages content
// 直接是 string 的情况下的所有字符串值应用 transform。
// 这是 CLIProxyAPI cloak_obfuscate.go 的 obfuscateSystemBlocks /
// obfuscateMessages 合并精简版——scrub 只关心这两个字段，不做真正的深度 JSON walk。
func walkStringValues(body []byte, transform func(string) string) []byte {
	if len(body) == 0 {
		return body
	}

	// system: string | [{type:"text",text:"..."}]
	sys := gjson.GetBytes(body, "system")
	if sys.Exists() {
		if sys.Type == gjson.String {
			orig := sys.String()
			if n := transform(orig); n != orig {
				if next, err := sjson.SetBytes(body, "system", n); err == nil {
					body = next
				}
			}
		} else if sys.IsArray() {
			sys.ForEach(func(key, val gjson.Result) bool {
				if val.Get("type").String() == "text" {
					orig := val.Get("text").String()
					if n := transform(orig); n != orig {
						path := "system." + key.String() + ".text"
						if next, err := sjson.SetBytes(body, path, n); err == nil {
							body = next
						}
					}
				}
				return true
			})
		}
	}

	// messages[].content: string | [{type:"text",text:"..."} | {type:"tool_result", content: "..." | [{type:"text",text:"..."}]}]
	msgs := gjson.GetBytes(body, "messages")
	if !msgs.IsArray() {
		return body
	}
	msgs.ForEach(func(msgKey, msg gjson.Result) bool {
		msgPath := "messages." + msgKey.String()
		content := msg.Get("content")
		if !content.Exists() {
			return true
		}
		if content.Type == gjson.String {
			orig := content.String()
			if n := transform(orig); n != orig {
				if next, err := sjson.SetBytes(body, msgPath+".content", n); err == nil {
					body = next
				}
			}
			return true
		}
		if !content.IsArray() {
			return true
		}
		content.ForEach(func(blockKey, block gjson.Result) bool {
			btype := block.Get("type").String()
			blockPath := msgPath + ".content." + blockKey.String()
			switch btype {
			case "text":
				orig := block.Get("text").String()
				if n := transform(orig); n != orig {
					if next, err := sjson.SetBytes(body, blockPath+".text", n); err == nil {
						body = next
					}
				}
			case "tool_result":
				// tool_result.content 可能是 string 或 array of {type:"text",text:"..."}
				tc := block.Get("content")
				if tc.Type == gjson.String {
					orig := tc.String()
					if n := transform(orig); n != orig {
						if next, err := sjson.SetBytes(body, blockPath+".content", n); err == nil {
							body = next
						}
					}
				} else if tc.IsArray() {
					tc.ForEach(func(tcKey, tcBlock gjson.Result) bool {
						if tcBlock.Get("type").String() == "text" {
							orig := tcBlock.Get("text").String()
							if n := transform(orig); n != orig {
								path := blockPath + ".content." + tcKey.String() + ".text"
								if next, err := sjson.SetBytes(body, path, n); err == nil {
									body = next
								}
							}
						}
						return true
					})
				}
			}
			return true
		})
		return true
	})
	return body
}
