// Package claude provides constants and helpers for Claude API integration.
package claude

import (
	"fmt"
	"regexp"
	"strings"
	"sync"
)

// init 在启动时校验 CLIDefaultVersion 与 DefaultHeaders["User-Agent"] 中携带的版本号
// 必须严格一致。Anthropic 上游会比对 UA 与 billing block 中的 cc_version；若错位
// 就会判定为第三方调用（"Third-party apps now draw from your extra usage"）。
//
// 用 panic 而不是日志 warn 是故意的：一个 cc_version 错位的二进制上线，不会被监控
// 立刻发现，但每个 OAuth 账号都会持续被打 third-party 标。让进程 fail-fast 比让
// 共享池静默劣化更安全。
//
// 注意：这只校验编译期"默认版本"对齐（init 时尚未注入运行期解析器）。运行期
// DefaultHeaders() 与 billing cc_version 均经 EffectiveCLIVersion() 取版本，天然一致。
func init() {
	ua, ok := DefaultHeaders()["User-Agent"]
	if !ok {
		panic("claude.DefaultHeaders missing User-Agent")
	}
	const prefix = "claude-cli/"
	idx := strings.Index(ua, prefix)
	if idx < 0 {
		panic(fmt.Sprintf("claude.DefaultHeaders()[\"User-Agent\"]=%q is not a claude-cli UA", ua))
	}
	rest := ua[idx+len(prefix):]
	end := strings.IndexAny(rest, " (")
	if end < 0 {
		end = len(rest)
	}
	uaVersion := strings.TrimSpace(rest[:end])
	if uaVersion != CLIDefaultVersion {
		panic(fmt.Sprintf(
			"claude version mismatch: CLIDefaultVersion=%q vs DefaultHeaders[\"User-Agent\"]=%q (extracted=%q). "+
				"Both must be bumped together; see CLIDefaultVersion docstring.",
			CLIDefaultVersion, ua, uaVersion))
	}
}

// Claude Code 客户端相关常量

// Beta header 常量
//
// 这里的常量对齐真实 Claude Code CLI 的最新流量（截至 2026-04）。
// 选型参考：与 Parrot (src/transform/cc_mimicry.py) 的 BETAS 保持一致，
// 原因：Anthropic 上游会基于 anthropic-beta 的完整集合判定请求来源；
// 缺少任何"官方 Claude Code 请求才会带"的 beta，都会被降级到第三方额度，
// 对应报错：`Third-party apps now draw from your extra usage, not your plan limits.`
const (
	BetaOAuth                    = "oauth-2025-04-20"
	BetaClaudeCode               = "claude-code-20250219"
	BetaInterleavedThinking      = "interleaved-thinking-2025-05-14"
	BetaFineGrainedToolStreaming = "fine-grained-tool-streaming-2025-05-14"
	BetaTokenCounting            = "token-counting-2024-11-01"
	BetaContext1M                = "context-1m-2025-08-07"
	BetaContextManagement        = "context-management-2025-06-27"
	BetaFastMode                 = "fast-mode-2026-02-01"

	// 新增（对齐官方 CLI 2.1.9x 以来的流量）
	BetaPromptCachingScope          = "prompt-caching-scope-2026-01-05"
	BetaEffort                      = "effort-2025-11-24"
	BetaRedactThinking              = "redact-thinking-2026-02-12"
	BetaThinkingBindingControls     = "thinking-binding-controls-2026-08-01"
	BetaMidConversationOutputConfig = "mid-conversation-output-config-2026-07-01"
	BetaExtendedCacheTTL            = "extended-cache-ttl-2025-04-11"

	// server-side refusal fallback beta 字段族（beta Messages API 专有）。
	// 客户端（Claude Code / SDK / OpenCode 等）会默认透传 body.fallbacks /
	// body.fallback_credit_token，上游仅在 anthropic-beta 携带对应 token 时接受；
	// 缺 token 时 Pydantic 拒收："fallbacks: Extra inputs are not permitted"。
	// 仅用于 sanitize 的条件判断（strip-or-keep），禁止加入
	// FullClaudeCodeMimicryBetas / DefaultBetaHeader / APIKeyBetaHeader /
	// Bedrock 白名单：server-side fallback 会换模型、改计费，不能默认打开。
	BetaServerSideFallback   = "server-side-fallback-2026-07-01"
	BetaFallbackCredit       = "fallback-credit-2026-07-01"
	BetaFallbackCreditLegacy = "fallback-credit-2026-06-01"
)

// DroppedBetas 是转发时需要从 anthropic-beta header 中移除的 beta token 列表。
// 这些 token 是客户端特有的，不应透传给上游 API。
var DroppedBetas = []string{}

// NonOfficialUpstreamSafeBetaPrefixes 是对非官方上游（聚合商/自定义 base URL 账号）
// 转发时允许保留的 beta token 前缀白名单。不在此列表中的 beta token 会被剥离，
// 防止 Bedrock 等后端返回 "invalid beta flag" 错误。
//
// 使用前缀匹配：新版本号的同族 beta（如 interleaved-thinking-2025-XX-XX）
// 无需手动更新此列表即可自动放行。
//
// 维护原则：只有被主流 Bedrock/Vertex 后端证实支持的 beta 才应加入。
var NonOfficialUpstreamSafeBetaPrefixes = []string{
	"claude-code-",
	"fine-grained-tool-streaming-",
	"interleaved-thinking-",
	"token-counting-",
	"prompt-caching-scope-",
	"effort-",
	"context-1m-", // anyrouter.top requires; injected by ForceContext1M
}

// DefaultBetaHeader Claude Code 客户端默认的 anthropic-beta header
const DefaultBetaHeader = BetaClaudeCode + "," + BetaOAuth + "," + BetaInterleavedThinking + "," + BetaFineGrainedToolStreaming

// MessageBetaHeaderNoTools /v1/messages 在无工具时的 beta header
//
// NOTE: Claude Code OAuth credentials are scoped to Claude Code. When we "mimic"
// Claude Code for non-Claude-Code clients, we must include the claude-code beta
// even if the request doesn't use tools, otherwise upstream may reject the
// request as a non-Claude-Code API request.
const MessageBetaHeaderNoTools = BetaClaudeCode + "," + BetaOAuth + "," + BetaInterleavedThinking

// MessageBetaHeaderWithTools /v1/messages 在有工具时的 beta header
const MessageBetaHeaderWithTools = BetaClaudeCode + "," + BetaOAuth + "," + BetaInterleavedThinking

// CountTokensBetaHeader count_tokens 请求使用的 anthropic-beta header
const CountTokensBetaHeader = BetaClaudeCode + "," + BetaOAuth + "," + BetaInterleavedThinking + "," + BetaTokenCounting

// HaikuBetaHeader Haiku 模型在 OAuth 真实客户端透传路径上的默认 anthropic-beta header。
// OAuth mimic 路径统一使用 FullClaudeCodeMimicryBetas。
const HaikuBetaHeader = BetaOAuth + "," + BetaInterleavedThinking

// APIKeyBetaHeader API-key 账号建议使用的 anthropic-beta header（不包含 oauth）
const APIKeyBetaHeader = BetaClaudeCode + "," + BetaInterleavedThinking + "," + BetaFineGrainedToolStreaming

// APIKeyHaikuBetaHeader Haiku 模型在 API-key 账号下使用的 anthropic-beta header（不包含 oauth / claude-code）
const APIKeyHaikuBetaHeader = BetaInterleavedThinking

// DefaultCacheControlTTL 是网关代理为自己生成的 cache_control 块默认使用的 ttl。
// 真实 Claude Code CLI 当前使用 "1h"，但本仓策略是"客户端透传 ttl 优先；
// 客户端缺省时统一使用 5m"，这样既不浪费 1h 缓存额度，也保留客户端自定义能力。
const DefaultCacheControlTTL = "5m"

// CLICurrentVersion 是源码层硬编码的"出厂默认"（内置基线）Claude Code CLI 版本号。
// 用于 billing attribution block 中的 cc_version=X.Y.Z.{fp} 前缀以及 fingerprint 计算的
// 下限校验。必须与 DefaultHeaders["User-Agent"] 中的版本号严格一致；不一致会被 Anthropic
// 判为第三方调用（"Third-party apps now draw from your extra usage"）。
//
// ⚠️ 不要直接引用本常量来"读当前生效版本"。生效版本优先级（高 → 低）：
//  1. 运行期解析器（SetCLIVersionResolver 注入）：面板手动值 claude_code_client_version →
//     ClaudeCodeVersionSyncService 同步值 → 下面 3/4 层；
//  2. fork 的 CLIVersionTrackerService 经 SetCLICurrentVersion 推入的值（仅在未注入解析器时生效）；
//  3. 环境变量 SUB2API_CLAUDE_CLI_VERSION（见 cli_version.go 的 CLIVersion()）；
//  4. 本常量。
//
// 读取生效版本请统一使用 EffectiveCLIVersion()（GetCLICurrentVersion() 为其别名）。
// 直接引用本常量只在"表达内置基线"时才正确（例如覆盖值的下限校验）。
const CLICurrentVersion = "2.1.258"

// CLIDefaultVersion 是"没有 DB 设置时"本进程使用的默认 CLI 版本号：
// 内置基线 CLICurrentVersion 叠加 SUB2API_CLAUDE_CLI_VERSION 环境覆盖后的结果。
//
// 在包初始化时解析一次并在进程生命周期内恒定：伪装身份必须自洽——User-Agent 头与请求体
// billing attribution 块里的 cc_version 由不同代码路径写入，两次读到不同的值会让同一个
// 请求自相矛盾，被上游判为非正版客户端。
var CLIDefaultVersion = CLIVersion()

var (
	// trackedCLIVersion 是 CLIVersionTrackerService 经 SetCLICurrentVersion 推入的版本号，
	// 受 cliVersionMu 保护。仅作为 EffectiveCLIVersion 在未注入解析器时的回退层。
	trackedCLIVersion string
	cliVersionMu      sync.RWMutex
)

// GetCLICurrentVersion 返回当前生效的 CLI 版本号（线程安全）。
// fork 历史入口，现为 EffectiveCLIVersion() 的别名，保证 UA 头、billing cc_version、
// identity 指纹始终读同一个解析链。
func GetCLICurrentVersion() string {
	return EffectiveCLIVersion()
}

// getTrackedCLIVersion 返回 SetCLICurrentVersion 推入的版本号；未设置或低于内置基线时返回空。
func getTrackedCLIVersion() string {
	cliVersionMu.RLock()
	defer cliVersionMu.RUnlock()
	if IsSupportedCLIVersion(trackedCLIVersion) {
		return trackedCLIVersion
	}
	return ""
}

// SetCLICurrentVersion 记录 CLIVersionTrackerService 回填 / 从 npm 拉取的版本号。
// 传入空字符串视为重置（回退到环境覆盖 / 内置常量）。
//
// 仅在严格 semver `X.Y.Z` 格式时接受，否则返回 false 并保持原值（防止 npm 偶发返回
// pre-release / 错误格式时污染 UA）。User-Agent 不再就地改写：DefaultHeaders() 每次
// 调用经 DefaultUserAgent() 现取版本。
func SetCLICurrentVersion(v string) bool {
	v = strings.TrimSpace(v)
	if v != "" && !semverRe.MatchString(v) {
		return false
	}
	cliVersionMu.Lock()
	defer cliVersionMu.Unlock()
	trackedCLIVersion = v
	return true
}

// semverRe 严格 X.Y.Z 三段 semver。
var semverRe = regexp.MustCompile(`^\d+\.\d+\.\d+$`)

// FullClaudeCodeMimicryBetas 返回最"像"真实 Claude Code CLI 的完整 beta 列表，
// 用于 OAuth 账号伪装成 Claude Code 时使用。
// 顺序与真实 CLI 抓包一致。
//
// 使用建议：
//   - OAuth mimic：所有模型（包括 Haiku）都使用这整份列表。
//   - OAuth 真实客户端透传：保留客户端 beta；未提供时使用模型对应默认值。
//   - API-key 账号：不要使用本函数，参见 APIKeyBetaHeader。
//   - 不默认加入 redact-thinking，避免上游抹除 thinking 内容；客户端显式传入时由合并逻辑保留。
func FullClaudeCodeMimicryBetas() []string {
	return []string{
		BetaClaudeCode,
		BetaOAuth,
		BetaInterleavedThinking,
		BetaPromptCachingScope,
		BetaEffort,
		BetaContextManagement,
		BetaThinkingBindingControls,
		BetaMidConversationOutputConfig,
		BetaExtendedCacheTTL,
	}
}

// DefaultHeaders 是 Claude Code 客户端默认请求头。
//
// Sync against real Claude CLI traffic so Anthropic does not reject OAuth
// requests as "non-CLI" third-party usage.
//
// Reference capture: Claude Code CLI 2.1.161 (external) — the x-stainless
// values below were verified against the installed Bun-compiled binary
// (package-version 0.94.0, runtime-version v24.3.0), and independently
// corroborated by public CLI captures (e.g. claude-cli/2.1.83 also reports
// X-Stainless-Runtime-Version: v24.3.0).
//
// Rules of thumb:
//   - Runtime-Version must be the real Node version the bundled CLI ships.
//     Recent CLI builds run Node v24 (LTS-track, even-numbered); only the
//     odd-numbered "current" releases (v23/v25) would mark the request as
//     "clearly not the bundled CLI".
//   - Package-Version should track @anthropic-ai/sdk's actual npm releases
//     for the impersonated CLI version (0.94.0 for 2.1.161).
//   - DO send anthropic-dangerous-direct-browser-access: the real CLI
//     constructs its SDK client with dangerouslyAllowBrowser: true, so the
//     SDK emits this header on every request. Omitting it is a third-party
//     tell. (Wire casing is all-lowercase; see service.resolveWireCasing.)
//
// 每次调用现构造：User-Agent 走 DefaultUserAgent()（运行期可变版本号），
// 不再在包 init 时固化。同一次请求内应只取一次 UA 字符串并在出站头与
// billing 两条路径间复用，避免版本缓存翻转瞬间头/体不一致。
func DefaultHeaders() map[string]string {
	return map[string]string{
		// Keep these in sync with recent Claude CLI traffic to reduce the chance
		// that Claude Code-scoped OAuth credentials are rejected as "non-CLI" usage.
		// 版本参考：对齐 Parrot (src/transform/cc_mimicry.py:49) 的 CLI_USER_AGENT。
		"User-Agent":                                DefaultUserAgent(),
		"X-Stainless-Lang":                          "js",
		"X-Stainless-Package-Version":               "0.94.0",
		"X-Stainless-OS":                            "Linux",
		"X-Stainless-Arch":                          "arm64",
		"X-Stainless-Runtime":                       "node",
		"X-Stainless-Runtime-Version":               "v24.3.0",
		"X-Stainless-Retry-Count":                   "0",
		"X-Stainless-Timeout":                       "600",
		"X-App":                                     "cli",
		"Anthropic-Dangerous-Direct-Browser-Access": "true",
	}
}

// Model 表示一个 Claude 模型
type Model struct {
	ID          string `json:"id"`
	Type        string `json:"type"`
	DisplayName string `json:"display_name"`
	CreatedAt   string `json:"created_at"`
}

// DefaultModels Claude Code 客户端支持的默认模型列表
var DefaultModels = []Model{
	{
		ID:          "claude-fable-5-1",
		Type:        "model",
		DisplayName: "Claude Fable 5.1",
		CreatedAt:   "2026-09-01T00:00:00Z",
	},
	{
		ID:          "claude-fable-5",
		Type:        "model",
		DisplayName: "Claude Fable 5",
		CreatedAt:   "2026-06-09T00:00:00Z",
	},
	{
		ID:          "claude-opus-4-5-20251101",
		Type:        "model",
		DisplayName: "Claude Opus 4.5",
		CreatedAt:   "2025-11-01T00:00:00Z",
	},
	{
		ID:          "claude-opus-4-6",
		Type:        "model",
		DisplayName: "Claude Opus 4.6",
		CreatedAt:   "2026-02-06T00:00:00Z",
	},
	{
		ID:          "claude-opus-4-7",
		Type:        "model",
		DisplayName: "Claude Opus 4.7",
		CreatedAt:   "2026-04-17T00:00:00Z",
	},
	{
		ID:          "claude-opus-4-8",
		Type:        "model",
		DisplayName: "Claude Opus 4.8",
		CreatedAt:   "2026-05-29T00:00:00Z",
	},
	{
		ID:          "claude-opus-5-5",
		Type:        "model",
		DisplayName: "Claude Opus 5.5",
		CreatedAt:   "2026-09-22T00:00:00Z",
	},
	{
		ID:          "claude-opus-5",
		Type:        "model",
		DisplayName: "Claude Opus 5",
		CreatedAt:   "2026-07-25T00:00:00Z",
	},
	{
		ID:          "claude-sonnet-5",
		Type:        "model",
		DisplayName: "Claude Sonnet 5",
		CreatedAt:   "2026-07-01T00:00:00Z",
	},
	{
		ID:          "claude-sonnet-4-6",
		Type:        "model",
		DisplayName: "Claude Sonnet 4.6",
		CreatedAt:   "2026-02-18T00:00:00Z",
	},
	{
		ID:          "claude-sonnet-4-5-20250929",
		Type:        "model",
		DisplayName: "Claude Sonnet 4.5",
		CreatedAt:   "2025-09-29T00:00:00Z",
	},
	{
		ID:          "claude-haiku-4-5-20251001",
		Type:        "model",
		DisplayName: "Claude Haiku 4.5",
		CreatedAt:   "2025-10-01T00:00:00Z",
	},
}

// DefaultModelIDs 返回默认模型的 ID 列表
func DefaultModelIDs() []string {
	ids := make([]string, len(DefaultModels))
	for i, m := range DefaultModels {
		ids[i] = m.ID
	}
	return ids
}

// DefaultTestModel 测试时使用的默认模型
const DefaultTestModel = "claude-sonnet-4-5-20250929"

// ModelIDOverrides Claude OAuth 请求需要的模型 ID 映射
var ModelIDOverrides = map[string]string{
	"claude-sonnet-4-5": "claude-sonnet-4-5-20250929",
	"claude-opus-4-5":   "claude-opus-4-5-20251101",
	"claude-haiku-4-5":  "claude-haiku-4-5-20251001",
}

// ModelIDReverseOverrides 用于将上游模型 ID 还原为短名
var ModelIDReverseOverrides = map[string]string{
	"claude-sonnet-4-5-20250929": "claude-sonnet-4-5",
	"claude-opus-4-5-20251101":   "claude-opus-4-5",
	"claude-haiku-4-5-20251001":  "claude-haiku-4-5",
}

// NormalizeModelID 根据 Claude OAuth 规则映射模型
func NormalizeModelID(id string) string {
	if id == "" {
		return id
	}
	if mapped, ok := ModelIDOverrides[id]; ok {
		return mapped
	}
	return id
}

// DenormalizeModelID 将上游模型 ID 转换为短名
func DenormalizeModelID(id string) string {
	if id == "" {
		return id
	}
	if mapped, ok := ModelIDReverseOverrides[id]; ok {
		return mapped
	}
	return id
}
