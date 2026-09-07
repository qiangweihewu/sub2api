package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unsafe"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/claude"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/xai"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/cespare/xxhash/v2"
	gocache "github.com/patrickmn/go-cache"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"

	"github.com/gin-gonic/gin"
)

const (
	claudeAPIURL            = "https://api.anthropic.com/v1/messages?beta=true"
	claudeAPICountTokensURL = "https://api.anthropic.com/v1/messages/count_tokens?beta=true"
	stickySessionTTL        = time.Hour // 粘性会话TTL
	defaultMaxLineSize      = 500 * 1024 * 1024
	// Canonical Claude Code banner. Keep it EXACT (no trailing whitespace/newlines)
	// to match real Claude CLI traffic as closely as possible. When we need a visual
	// separator between system blocks, we add "\n\n" at concatenation time.
	claudeCodeSystemPrompt = "You are Claude Code, Anthropic's official CLI for Claude."
	// claudeCodeSystemPromptExpansion 是真实 Claude Code 主系统提示词中"与具体工具无关"
	// 的通用段落（身份/用途总述 + 安全声明 + URL 告警 + Tone and style），逐字取自真实
	// CLI（2.1.x 一致）。伪装路径用它把 system 块数从 2 提升到 3、体量贴近真实 CC，同时
	// 刻意排除 # Doing tasks / # Using your tools / # Executing actions 等会污染被代理
	// 用户行为的工具专属指令。
	claudeCodeSystemPromptExpansion = `You are an interactive agent that helps users with software engineering tasks. Use the instructions below and the tools available to you to assist the user.

IMPORTANT: Assist with authorized security testing, defensive security, CTF challenges, and educational contexts. Refuse requests for destructive techniques, DoS attacks, mass targeting, supply chain compromise, or detection evasion for malicious purposes. Dual-use security tools (C2 frameworks, credential testing, exploit development) require clear authorization context: pentesting engagements, CTF competitions, security research, or defensive use cases.
IMPORTANT: You must NEVER generate or guess URLs for the user unless you are confident that the URLs are for helping the user with programming. You may use URLs provided by the user in their messages or local files.

# Tone and style
 - Only use emojis if the user explicitly requests it. Avoid using emojis in all communication unless asked.
 - Your responses should be short and concise.
 - When referencing specific functions or pieces of code include the pattern file_path:line_number to allow the user to easily navigate to the source code location.
 - When referencing GitHub issues or pull requests, use the owner/repo#123 format (e.g. anthropics/claude-code#100) so they render as clickable links.
 - Do not use a colon before tool calls. Your tool calls may not be shown directly in the output, so text like "Let me read the file:" followed by a read tool call should just be "Let me read the file." with a period.`
	maxCacheControlBlocks = 4 // Anthropic API 允许的最大 cache_control 块数量

	defaultUserGroupRateCacheTTL           = 30 * time.Second
	defaultModelsListCacheTTL              = 15 * time.Second
	postUsageBillingTimeout                = 15 * time.Second
	claudeCodeNoopDeltaKeepaliveMinVersion = "2.1.193"
	debugGatewayBodyEnv                    = "SUB2API_DEBUG_GATEWAY_BODY"
	// 上游错误体只需要提取错误 JSON/日志摘要，默认 512KiB 避免错误风暴叠加大请求体。
	gatewayUpstreamErrorBodyReadLimit int64 = 512 << 10
)

const (
	claudeMimicDebugInfoKey = "claude_mimic_debug_info"
)

const (
	cacheTTLTarget5m                   = "5m"
	cacheTTLTarget1h                   = "1h"
	compositeModelOwnershipCachePrefix = "composite-owner|"
)

// ForceCacheBillingContextKey 强制缓存计费上下文键
// 用于粘性会话切换时，将 input_tokens 转为 cache_read_input_tokens 计费
type forceCacheBillingKeyType struct{}

// accountWithLoad 账号与负载信息的组合，用于负载感知调度
type accountWithLoad struct {
	account  *Account
	loadInfo *AccountLoadInfo
	// routingRank 是模型路由的梯队序号（0 = 最高优先梯队，越小越优先）。
	// 仅模型路由选号路径会赋非零值；其余路径保持 0，排序行为与原先一致。
	routingRank int
}

var ForceCacheBillingContextKey = forceCacheBillingKeyType{}

var (
	windowCostPrefetchCacheHitTotal  atomic.Int64
	windowCostPrefetchCacheMissTotal atomic.Int64
	windowCostPrefetchBatchSQLTotal  atomic.Int64
	windowCostPrefetchFallbackTotal  atomic.Int64
	windowCostPrefetchErrorTotal     atomic.Int64

	userGroupRateCacheHitTotal      atomic.Int64
	userGroupRateCacheMissTotal     atomic.Int64
	userGroupRateCacheLoadTotal     atomic.Int64
	userGroupRateCacheSFSharedTotal atomic.Int64
	userGroupRateCacheFallbackTotal atomic.Int64

	modelsListCacheHitTotal   atomic.Int64
	modelsListCacheMissTotal  atomic.Int64
	modelsListCacheStoreTotal atomic.Int64

	// Deprecated: flusher_enabled=true 后不再增长(仅 flag=false 降级直写路径使用);新主路径见 FlusherMetrics。remove after 2026-09。
	// userPlatformQuotaDBIncrErrorTotal 统计 finalizePostUsageBilling 异步 goroutine
	// 中 IncrementUsageWithReset 失败次数。Redis 已成功累加 + DB 写失败意味着
	// Redis cache TTL 过期或被清后该笔 cost 会丢失（与实际消费偏差）。
	// oncall 通过 GatewayUserPlatformQuotaIncrStats() 暴露给 ops 面板做阈值告警。
	userPlatformQuotaDBIncrErrorTotal atomic.Int64
	// Deprecated: flusher_enabled=true 后不再增长(仅 flag=false 降级直写路径使用);新主路径见 FlusherMetrics。remove after 2026-09。
	// userPlatformQuotaDBIncrLegacyErrorTotal 统计 legacy postUsageBilling
	// （applyUsageBilling 在 repo==nil 时 fallback）路径下的失败次数；
	// 与 DB Incr 失败分开计数，便于区分"主路径暂时故障"vs"基础设施长期未配齐"。
	userPlatformQuotaDBIncrLegacyErrorTotal atomic.Int64
	// userPlatformQuotaSentinelSetCacheErrorTotal 统计 checkUserPlatformQuotaEligibility
	// 在 DB 无行时回填 sentinel cache entry 写 Redis 失败的次数（phase A）。
	userPlatformQuotaSentinelSetCacheErrorTotal atomic.Int64
)

func GatewayWindowCostPrefetchStats() (cacheHit, cacheMiss, batchSQL, fallback, errCount int64) {
	return windowCostPrefetchCacheHitTotal.Load(),
		windowCostPrefetchCacheMissTotal.Load(),
		windowCostPrefetchBatchSQLTotal.Load(),
		windowCostPrefetchFallbackTotal.Load(),
		windowCostPrefetchErrorTotal.Load()
}

func GatewayUserGroupRateCacheStats() (cacheHit, cacheMiss, load, singleflightShared, fallback int64) {
	return userGroupRateCacheHitTotal.Load(),
		userGroupRateCacheMissTotal.Load(),
		userGroupRateCacheLoadTotal.Load(),
		userGroupRateCacheSFSharedTotal.Load(),
		userGroupRateCacheFallbackTotal.Load()
}

func GatewayModelsListCacheStats() (cacheHit, cacheMiss, store int64) {
	return modelsListCacheHitTotal.Load(), modelsListCacheMissTotal.Load(), modelsListCacheStoreTotal.Load()
}

// GatewayUserPlatformQuotaIncrStats 返回 (mainPathErr, legacyPathErr, sentinelSetErr)。
// mainPathErr：finalizePostUsageBilling 异步 goroutine 写 DB 失败累计次数；
// legacyPathErr：postUsageBilling fallback 路径写 DB 失败累计次数；
// sentinelSetErr：DB 无行时回填 sentinel cache entry 写 Redis 失败累计次数。
// ops 监控面板可以按"持续上升斜率"做告警阈值。
func GatewayUserPlatformQuotaIncrStats() (mainPathErr, legacyPathErr, sentinelSetErr int64) {
	return userPlatformQuotaDBIncrErrorTotal.Load(),
		userPlatformQuotaDBIncrLegacyErrorTotal.Load(),
		userPlatformQuotaSentinelSetCacheErrorTotal.Load()
}

// GatewayUserPlatformQuotaFlusherStats 暴露 flusher 运行指标供 ops/health 面板查询。
func GatewayUserPlatformQuotaFlusherStats(f *UserPlatformQuotaUsageFlusher) map[string]int64 {
	if f == nil || f.metrics == nil {
		return nil
	}
	m := f.metrics
	return map[string]int64{
		"flush_success":        m.FlushSuccessTotal.Load(),
		"flush_error":          m.FlushErrorTotal.Load(),
		"flush_batch_size":     m.FlushBatchSizeTotal.Load(),
		"flush_latency_ms_max": m.FlushLatencyMsMax.Load(),
		"dirty_readd":          m.DirtyReaddTotal.Load(),
		"dirty_lost":           m.DirtyLostTotal.Load(),
		"flush_fk_violation":   m.FlushFKViolationTotal.Load(),
	}
}

func openAIStreamEventIsTerminal(data string) bool {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return false
	}
	if trimmed == "[DONE]" {
		return true
	}
	return openAIStreamEventTypeIsTerminal(gjson.Get(trimmed, "type").String())
}

// openAIStreamEventIsTerminalWithType 复用已提取的 type，避免 SSE 热路径重复扫描 JSON。
func openAIStreamEventIsTerminalWithType(data, eventType string) bool {
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return false
	}
	if trimmed == "[DONE]" {
		return true
	}
	return openAIStreamEventTypeIsTerminal(eventType)
}

func openAIStreamEventTypeIsTerminal(eventType string) bool {
	switch strings.TrimSpace(eventType) {
	case "response.completed", "response.done", "response.failed", "response.incomplete", "response.cancelled", "response.canceled", "error":
		return true
	default:
		return false
	}
}

func anthropicStreamEventIsTerminal(eventName, data string) bool {
	if strings.EqualFold(strings.TrimSpace(eventName), "message_stop") {
		return true
	}
	trimmed := strings.TrimSpace(data)
	if trimmed == "" {
		return false
	}
	if trimmed == "[DONE]" {
		return true
	}
	return gjson.Get(trimmed, "type").String() == "message_stop"
}

func cloneStringSlice(src []string) []string {
	if len(src) == 0 {
		return nil
	}
	dst := make([]string, len(src))
	copy(dst, src)
	return dst
}

// IsForceCacheBilling 检查是否启用强制缓存计费
func IsForceCacheBilling(ctx context.Context) bool {
	v, _ := ctx.Value(ForceCacheBillingContextKey).(bool)
	return v
}

// WithForceCacheBilling 返回带有强制缓存计费标记的上下文
func WithForceCacheBilling(ctx context.Context) context.Context {
	return context.WithValue(ctx, ForceCacheBillingContextKey, true)
}

func (s *GatewayService) debugModelRoutingEnabled() bool {
	if s == nil {
		return false
	}
	return s.debugModelRouting.Load()
}

func (s *GatewayService) debugClaudeMimicEnabled() bool {
	if s == nil {
		return false
	}
	return s.debugClaudeMimic.Load()
}

func parseDebugEnvBool(raw string) bool {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func shortSessionHash(sessionHash string) string {
	if sessionHash == "" {
		return ""
	}
	if len(sessionHash) <= 8 {
		return sessionHash
	}
	return sessionHash[:8]
}

func redactAuthHeaderValue(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	// Keep scheme for debugging, redact secret.
	if strings.HasPrefix(strings.ToLower(v), "bearer ") {
		return "Bearer [redacted]"
	}
	return "[redacted]"
}

func safeHeaderValueForLog(key string, v string) string {
	key = strings.ToLower(strings.TrimSpace(key))
	switch key {
	case "authorization", "x-api-key":
		return redactAuthHeaderValue(v)
	default:
		return strings.TrimSpace(v)
	}
}

func extractSystemPreviewFromBody(body []byte) string {
	if len(body) == 0 {
		return ""
	}
	sys := gjson.GetBytes(body, "system")
	if !sys.Exists() {
		return ""
	}

	switch {
	case sys.IsArray():
		for _, item := range sys.Array() {
			if !item.IsObject() {
				continue
			}
			if strings.EqualFold(item.Get("type").String(), "text") {
				if t := item.Get("text").String(); strings.TrimSpace(t) != "" {
					return t
				}
			}
		}
		return ""
	case sys.Type == gjson.String:
		return sys.String()
	default:
		return ""
	}
}

func buildClaudeMimicDebugLine(req *http.Request, body []byte, account *Account, tokenType string, mimicClaudeCode bool) string {
	if req == nil {
		return ""
	}

	// Only log a minimal fingerprint to avoid leaking user content.
	interesting := []string{
		"user-agent",
		"x-app",
		"anthropic-dangerous-direct-browser-access",
		"anthropic-version",
		"anthropic-beta",
		"x-stainless-lang",
		"x-stainless-package-version",
		"x-stainless-os",
		"x-stainless-arch",
		"x-stainless-runtime",
		"x-stainless-runtime-version",
		"x-stainless-retry-count",
		"x-stainless-timeout",
		"authorization",
		"x-api-key",
		"content-type",
		"accept",
		"x-stainless-helper-method",
	}

	h := make([]string, 0, len(interesting))
	for _, k := range interesting {
		if v := req.Header.Get(k); v != "" {
			h = append(h, fmt.Sprintf("%s=%q", k, safeHeaderValueForLog(k, v)))
		}
	}

	metaUserID := strings.TrimSpace(gjson.GetBytes(body, "metadata.user_id").String())
	sysPreview := strings.TrimSpace(extractSystemPreviewFromBody(body))

	// Truncate preview to keep logs sane.
	if len(sysPreview) > 300 {
		sysPreview = sysPreview[:300] + "..."
	}
	sysPreview = strings.ReplaceAll(sysPreview, "\n", "\\n")
	sysPreview = strings.ReplaceAll(sysPreview, "\r", "\\r")

	aid := int64(0)
	aname := ""
	if account != nil {
		aid = account.ID
		aname = account.Name
	}

	return fmt.Sprintf(
		"url=%s account=%d(%s) tokenType=%s mimic=%t meta.user_id=%q system.preview=%q headers={%s}",
		req.URL.String(),
		aid,
		aname,
		tokenType,
		mimicClaudeCode,
		metaUserID,
		sysPreview,
		strings.Join(h, " "),
	)
}

func logClaudeMimicDebug(req *http.Request, body []byte, account *Account, tokenType string, mimicClaudeCode bool) {
	line := buildClaudeMimicDebugLine(req, body, account, tokenType, mimicClaudeCode)
	if line == "" {
		return
	}
	logger.LegacyPrintf("service.gateway", "[ClaudeMimicDebug] %s", line)
}

func isClaudeCodeCredentialScopeError(msg string) bool {
	m := strings.ToLower(strings.TrimSpace(msg))
	if m == "" {
		return false
	}
	// Legacy credential-scope error (pre-2026).
	if strings.Contains(m, "only authorized for use with claude code") &&
		strings.Contains(m, "cannot be used for other api requests") {
		return true
	}
	// Current third-party detection error (2026+).
	if strings.Contains(m, "third-party apps") && strings.Contains(m, "extra usage") {
		return true
	}
	return false
}

// sseDataRe matches SSE data lines with optional whitespace after colon.
// Some upstream APIs return non-standard "data:" without space (should be "data: ").
var (
	sseDataRe            = regexp.MustCompile(`^data:\s*`)
	claudeCliUserAgentRe = regexp.MustCompile(`(?i)^claude-cli/\d+\.\d+\.\d+`)

	// claudeCodePromptPrefixes 用于检测 Claude Code 系统提示词的前缀列表
	// 支持多种变体：标准版、Agent SDK 版、Explore Agent 版、Compact 版等
	// 注意：前缀之间不应存在包含关系，否则会导致冗余匹配
	claudeCodePromptPrefixes = []string{
		"You are Claude Code, Anthropic's official CLI for Claude",             // 标准版 & Agent SDK 版（含 running within...）
		"You are a Claude agent, built on Anthropic's Claude Agent SDK",        // Agent SDK 变体
		"You are a file search specialist for Claude Code",                     // Explore Agent 版
		"You are a helpful AI assistant tasked with summarizing conversations", // Compact 版
	}
)

// ErrNoAvailableAccounts 表示没有可用的账号
var ErrNoAvailableAccounts = errors.New("no available accounts")

// ErrClaudeCodeOnly 表示分组仅允许 Claude Code 客户端访问
var ErrClaudeCodeOnly = errors.New("this group only allows Claude Code clients")

// ForwardResponseFinalizedKey 是 gin.Context 上的标志位。
// 当 gateway 内部已经把一条完整的错误/成功响应写给了客户端（例如
// handleErrorResponse 对上游 4xx 已经发了 c.JSON 或 c.Data），就把这个 key
// 置为 true，避免上层 handler 的 ensureForwardErrorResponse 又把
// `data: {"type":"error",...}` 的 SSE 错误帧拼到一个已经定稿的 HTTP 响应里。
// 该 bug 现象：客户端收到 application/json 响应体中混入 HTML 和 SSE 行。
const ForwardResponseFinalizedKey = "forward_response_finalized"

// MarkForwardResponseFinalized 标记 gin.Context 上的响应已经写完。
func MarkForwardResponseFinalized(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(ForwardResponseFinalizedKey, true)
}

// IsForwardResponseFinalized 返回 gin.Context 上的响应是否已经写完。
func IsForwardResponseFinalized(c *gin.Context) bool {
	if c == nil {
		return false
	}
	v, ok := c.Get(ForwardResponseFinalizedKey)
	if !ok {
		return false
	}
	b, ok := v.(bool)
	return ok && b
}

// allowedHeaders 白名单headers（参考CRS项目）
var allowedHeaders = map[string]bool{
	"accept":                      true,
	"x-stainless-retry-count":     true,
	"x-stainless-timeout":         true,
	"x-stainless-lang":            true,
	"x-stainless-package-version": true,
	"x-stainless-os":              true,
	"x-stainless-arch":            true,
	"x-stainless-runtime":         true,
	"x-stainless-runtime-version": true,
	"x-stainless-helper-method":   true,
	// Intentionally NOT whitelisted: anthropic-dangerous-direct-browser-access.
	// That header is for browser-origin SDK use; the real Claude Code CLI does
	// not emit it, so leaking it from a third-party client would mark the
	// request as non-CLI at Anthropic's detector.
	"anthropic-version":        true,
	"x-app":                    true,
	"anthropic-beta":           true,
	"accept-language":          true,
	"sec-fetch-mode":           true,
	"user-agent":               true,
	"content-type":             true,
	"accept-encoding":          true,
	"x-claude-code-session-id": true,
	"x-client-request-id":      true,
}

// ErrStickySessionNotFound is returned by GatewayCache.GetSessionAccountID
// when no binding exists for the session. It abstracts away the underlying
// cache implementation (e.g. redis.Nil), mirroring ErrRefreshTokenNotFound.
var ErrStickySessionNotFound = errors.New("sticky session not found")

// ErrReasoningContentNotFound is returned by GatewayCache.GetReasoningContent
// when no cached reasoning content exists for the reasoning item ID.
var ErrReasoningContentNotFound = errors.New("reasoning content not found")

// GatewayCache 定义网关服务的缓存操作接口。
// 提供粘性会话（Sticky Session）的存储、查询、刷新和删除功能。
//
// GatewayCache defines cache operations for gateway service.
// Provides sticky session storage, retrieval, refresh and deletion capabilities.
type GatewayCache interface {
	// GetSessionAccountID 获取粘性会话绑定的账号 ID；无绑定时返回
	// ErrStickySessionNotFound，使 service 层无需依赖具体缓存实现即可
	// 区分"未绑定"与真实读取失败。
	// Get the account ID bound to a sticky session. Returns
	// ErrStickySessionNotFound when no binding exists so service code can
	// distinguish a miss from a real read failure without importing the
	// cache driver.
	GetSessionAccountID(ctx context.Context, groupID int64, sessionHash string) (int64, error)
	// SetSessionAccountID 设置粘性会话与账号的绑定关系
	// Set the binding between sticky session and account
	SetSessionAccountID(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) error
	// RefreshSessionTTL 刷新粘性会话的过期时间
	// Refresh the expiration time of a sticky session
	RefreshSessionTTL(ctx context.Context, groupID int64, sessionHash string, ttl time.Duration) error
	// DeleteSessionAccountID 删除粘性会话绑定，用于账号不可用时主动清理
	// Delete sticky session binding, used to proactively clean up when account becomes unavailable
	DeleteSessionAccountID(ctx context.Context, groupID int64, sessionHash string) error

	// RecordSessionAccountFanout 记录会话触达的账号（P0-3 反扫荡）
	// Records an account that a session has touched for fanout limiting
	RecordSessionAccountFanout(ctx context.Context, groupID int64, sessionHash string, accountID int64, ttl time.Duration) error
	// GetSessionAccountFanoutCount 获取会话已触达的不同账号数
	// Gets the count of distinct accounts a session has touched
	GetSessionAccountFanoutCount(ctx context.Context, groupID int64, sessionHash string) (int, error)
	// DeleteSessionAccountFanout 清除会话的 fanout 记录（成功完成时可选调用）
	// Clears the fanout record for a session
	DeleteSessionAccountFanout(ctx context.Context, groupID int64, sessionHash string) error
	// Grok async video billing snapshot (create → status success).
	// SetGrokVideoPendingBilling stores create-time model/duration/resolution for status billing.
	SetGrokVideoPendingBilling(ctx context.Context, key string, payload []byte, ttl time.Duration) error
	// GetGrokVideoPendingBilling returns the create-time billing snapshot; miss → nil, nil.
	GetGrokVideoPendingBilling(ctx context.Context, key string) ([]byte, error)
	// ClaimGrokVideoBilled atomically marks a video request as billed (SetNX).
	// Returns true when this caller won the claim; false when already billed or claim unavailable.
	ClaimGrokVideoBilled(ctx context.Context, key string, ttl time.Duration) (bool, error)
	// ReleaseGrokVideoBilled clears a claim so a failed RecordUsage can retry billing.
	ReleaseGrokVideoBilled(ctx context.Context, key string) error

	// Reasoning content cache (Responses→Chat Completions 桥接）。
	// SetReasoningContent 按 reasoning item id 缓存 reasoning 全文，供后续请求
	// 在客户端不回传明文 summary 时回注 reasoning_content（DeepSeek thinking
	// mode 要求回传，否则 400）。
	SetReasoningContent(ctx context.Context, itemID string, content string, ttl time.Duration) error
	// GetReasoningContent 返回缓存的 reasoning 全文；未命中返回
	// ErrReasoningContentNotFound，使 service 层无需依赖具体缓存实现即可
	// 区分"未缓存"与真实读取失败。
	GetReasoningContent(ctx context.Context, itemID string) (string, error)
}

// derefGroupID safely dereferences *int64 to int64, returning 0 if nil
func derefGroupID(groupID *int64) int64 {
	if groupID == nil {
		return 0
	}
	return *groupID
}

func resolveUserGroupRateCacheTTL(cfg *config.Config) time.Duration {
	if cfg == nil || cfg.Gateway.UserGroupRateCacheTTLSeconds <= 0 {
		return defaultUserGroupRateCacheTTL
	}
	return time.Duration(cfg.Gateway.UserGroupRateCacheTTLSeconds) * time.Second
}

func resolveModelsListCacheTTL(cfg *config.Config) time.Duration {
	if cfg == nil || cfg.Gateway.ModelsListCacheTTLSeconds <= 0 {
		return defaultModelsListCacheTTL
	}
	return time.Duration(cfg.Gateway.ModelsListCacheTTLSeconds) * time.Second
}

func modelsListCacheKey(groupID *int64, platform string) string {
	return fmt.Sprintf("%d|%s", derefGroupID(groupID), strings.TrimSpace(platform))
}

func compositeModelOwnershipCacheKey(groupID int64, model string) string {
	return fmt.Sprintf("%s%d|%s", compositeModelOwnershipCachePrefix, groupID, strings.TrimSpace(model))
}

func prefetchedStickyGroupIDFromContext(ctx context.Context) (int64, bool) {
	return PrefetchedStickyGroupIDFromContext(ctx)
}

func prefetchedStickyAccountIDFromContext(ctx context.Context, groupID *int64) int64 {
	prefetchedGroupID, ok := prefetchedStickyGroupIDFromContext(ctx)
	if !ok || prefetchedGroupID != derefGroupID(groupID) {
		return 0
	}
	if accountID, ok := PrefetchedStickyAccountIDFromContext(ctx); ok && accountID > 0 {
		return accountID
	}
	return 0
}

// shouldClearStickySession 检查账号是否处于不可调度状态，需要清理粘性会话绑定。
// 委托 IsSchedulable() 判断账号级可调度性（状态、配额、过载、限流等），
// 额外检查模型级限流。
//
// shouldClearStickySession checks if an account is in an unschedulable state
// and the sticky session binding should be cleared.
// Delegates to IsSchedulable() for account-level checks, plus model-level rate limiting.
func shouldClearStickySession(account *Account, requestedModel string) bool {
	if account == nil {
		return false
	}
	if !account.IsSchedulable() {
		return true
	}
	if remaining := account.GetRateLimitRemainingTimeWithContext(context.Background(), requestedModel); remaining > 0 {
		return true
	}
	return false
}

type AccountWaitPlan struct {
	AccountID      int64
	MaxConcurrency int
	Timeout        time.Duration
	MaxWaiting     int
}

type AccountSelectionResult struct {
	Account     *Account
	Acquired    bool
	ReleaseFunc func()
	WaitPlan    *AccountWaitPlan // nil means no wait allowed
	// profitGate 携带本次选号真实生效的利润门（无门为 nil）。门安装在调度栈的
	// 局部 ctx 上，handler 必须经 ContextWithSelectionProfitGate 重放后才能在
	// 调度栈之外做抢槽后终检与准入后粘性绑定。
	profitGate *openAIProfitControlGate
}

// ProfitGateActive 报告本次选号是否处于利润门之下。
func (r *AccountSelectionResult) ProfitGateActive() bool {
	return r != nil && r.profitGate != nil
}

// ClaudeUsage 表示Claude API返回的usage信息
type ClaudeUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	CacheCreation5mTokens    int // 5分钟缓存创建token（来自嵌套 cache_creation 对象）
	CacheCreation1hTokens    int // 1小时缓存创建token（来自嵌套 cache_creation 对象）
	ImageOutputTokens        int `json:"image_output_tokens,omitempty"`
}

// ForwardResult 转发结果
type AudioUsage struct {
	Mode            string  // realtime | tts | stt
	DurationOrUnits float64 // minutes / million-chars / hours
}

type ForwardResult struct {
	RequestID string
	// UpstreamHeaders 是直接上游的响应头，用于按账户配置解析上游请求标识。
	UpstreamHeaders http.Header
	Usage           ClaudeUsage
	Model           string
	// UpstreamModel is the actual upstream model after mapping.
	// Prefer empty when it is identical to Model; persistence normalizes equal values away as no-op mappings.
	UpstreamModel string
	// UpstreamResponseModel is captured from the raw successful upstream
	// response before any client-facing rewrite or protocol conversion.
	UpstreamResponseModel         string
	UpstreamResponseModelConflict bool
	// UpstreamResponseServiceTier is the tier the upstream reports having used
	// (Anthropic usage.speed: "fast" / "standard"); "" when not declared.
	UpstreamResponseServiceTier string
	Stream                      bool
	Duration                    time.Duration
	FirstTokenMs                *int // 首字时间（流式请求）
	ClientDisconnect            bool // 客户端是否在流式传输过程中断开
	ReasoningEffort             *string
	// RequestedReasoningEffort is the client-requested effort before mapping.
	RequestedReasoningEffort *string
	// ServiceTier records the tier requested by the client. OpenAI uses
	// service_tier; Anthropic speed=fast is normalized to "fast". Usage recording
	// lowers it to UpstreamResponseServiceTier when the upstream reports a
	// cheaper tier (see ResolveBillingServiceTier).
	ServiceTier *string

	// 图片生成计费字段（图片生成模型使用）
	ImageCount         int    // 生成的图片数量
	ImageSize          string // 最终计费尺寸 "1K", "2K", "4K"
	ImageInputSize     string // 请求中的原始图片尺寸
	ImageOutputSize    string // 上游响应中的图片尺寸
	ImageOutputSizes   []string
	ImageSizeSource    string
	ImageSizeBreakdown map[string]int
	SearchCount        int
	AudioUsage         *AudioUsage
}

// GatewayFailureStage identifies which request stage failed. The zero value is
// intentionally treated as inference so existing UpstreamFailoverError callers
// retain their current behavior.
type GatewayFailureStage string

const (
	GatewayFailureStageInference   GatewayFailureStage = "inference"
	GatewayFailureStageAccountAuth GatewayFailureStage = "account_auth"
)

// GatewayFailureScope identifies whether selecting another account can help.
type GatewayFailureScope string

const (
	GatewayFailureScopeAccount  GatewayFailureScope = "account"
	GatewayFailureScopeProvider GatewayFailureScope = "provider"
	GatewayFailureScopeRequest  GatewayFailureScope = "request"
)

// NextAccountAction is tri-state for backwards compatibility. The zero value
// means legacy retry behavior; only NextAccountStop explicitly short-circuits.
type NextAccountAction uint8

const (
	NextAccountLegacyRetry NextAccountAction = iota
	NextAccountRetry
	NextAccountStop
)

type GatewayFailureReason string

// UpstreamFailoverError indicates an upstream or credential error that may
// trigger account failover. Additive metadata keeps existing composite literals
// source-compatible and preserves their legacy retry-next-account behavior.
type UpstreamFailoverError struct {
	StatusCode               int
	ResponseBody             []byte        // 上游响应体，用于错误透传规则匹配
	ResponseHeaders          http.Header   // 上游响应头，用于透传 cf-ray/cf-mitigated/content-type 等诊断信息
	ForceCacheBilling        bool          // Antigravity 粘性会话切换时设为 true
	RetryableOnSameAccount   bool          // 临时性错误（如 Google 间歇性 400、空响应），应在同一账号上重试 N 次再切换
	SameAccountRetryDelay    time.Duration // 同账号重试的最小间隔；零值使用 handler 默认值
	SameAccountRetryDeadline time.Time     // 同账号重试截止时间；零值表示仅受 retryLimit 限制
	SameAccountRetryMax      int           // 可选的错误级同账号重试上限，低于 handler 默认预算时优先采用
	QuotaExhausted           bool          // 上游账号配额/积分/余额耗尽，应清除粘性会话绑定
	SoftRateLimitOnExhaust   bool          // Bedrock 区域池：全部耗尽时把 429 呈现为可重试的 503 overloaded，避免客户端看到硬限流
	RequestScopedTransient   bool          // 故障因素与账号无关（如上游按客户端身份/模型容量降载）：可同账号重试，但不得据此对账号做临时封禁
	SafeToFailoverAfterWrite bool          // 仅写出 SSE 注释等非语义字节时，仍可在同一客户端流中切换账号
	Stage                    GatewayFailureStage
	Scope                    GatewayFailureScope
	Reason                   GatewayFailureReason
	NextAccountAction        NextAccountAction
	ClientStatusCode         int
	ClientMessage            string
}

func (e *UpstreamFailoverError) Error() string {
	if e != nil && e.Stage == GatewayFailureStageAccountAuth {
		return fmt.Sprintf("credential failure: %s (failover)", e.Reason)
	}
	return fmt.Sprintf("upstream error: %d (failover)", e.StatusCode)
}

func (e *UpstreamFailoverError) ShouldRetryNextAccount() bool {
	return e != nil && e.NextAccountAction != NextAccountStop
}

func (e *UpstreamFailoverError) IsCredentialFailure() bool {
	return e != nil && e.Stage == GatewayFailureStageAccountAuth
}

// ShouldReportAccountScheduleFailure prevents provider- and request-scoped
// credential failures from being misattributed to the selected account. Legacy
// and inference failures retain their existing scheduler-health behavior.
func (e *UpstreamFailoverError) ShouldReportAccountScheduleFailure() bool {
	if e == nil {
		return false
	}
	return !e.IsCredentialFailure() || e.Scope == GatewayFailureScopeAccount
}

// sseStreamErrorEventError 表示上游 SSE 流体内出现 event:error 帧。
// RawData 是该事件 data: 行的原始 JSON 字符串
// （Anthropic 标准结构 {"type":"error","error":{"type":"...","message":"..."}}）。
// Error() 保持原字符串以兼容现有日志/检索；调用方应通过 errors.As
// 提取 RawData 并构造 UpstreamFailoverError.ResponseBody。
type sseStreamErrorEventError struct {
	RawData string
}

func (e *sseStreamErrorEventError) Error() string { return "have error in stream" }

// TempUnscheduleRetryableError 对 RetryableOnSameAccount 类型的 failover 错误触发临时封禁。
// 由 handler 层在同账号重试全部用尽、切换账号时调用。
func (s *GatewayService) TempUnscheduleRetryableError(ctx context.Context, accountID int64, failoverErr *UpstreamFailoverError) {
	if failoverErr == nil || !failoverErr.RetryableOnSameAccount {
		return
	}
	// 请求级瞬时故障与账号健康无关：封禁只会把与故障无关的账号一并摘掉，
	// 而故障因素（客户端身份、模型容量）在下一个账号上完全相同。
	if failoverErr.RequestScopedTransient {
		return
	}
	// 根据状态码选择封禁策略
	switch failoverErr.StatusCode {
	case http.StatusBadRequest:
		tempUnscheduleGoogleConfigError(ctx, s.rateLimitService, s.accountRepo, accountID, "[handler]")
	case http.StatusBadGateway:
		tempUnscheduleEmptyResponse(ctx, s.rateLimitService, s.accountRepo, accountID, "[handler]")
	}
}

// UnbindStickySession 清除粘性会话绑定，用于账号配额耗尽时主动解绑。
// 避免后续请求继续粘到已耗尽的账号上。
func (s *GatewayService) UnbindStickySession(ctx context.Context, groupID *int64, sessionHash string) error {
	if s.cache == nil || sessionHash == "" {
		return nil
	}
	return s.cache.DeleteSessionAccountID(ctx, derefGroupID(groupID), sessionHash)
}

// GatewayService handles API gateway operations
type GatewayService struct {
	accountRepo           AccountRepository
	groupRepo             GroupRepository
	usageLogRepo          UsageLogRepository
	usageBillingRepo      UsageBillingRepository
	userRepo              UserRepository
	userSubRepo           UserSubscriptionRepository
	userGroupRateRepo     UserGroupRateRepository
	cache                 GatewayCache
	digestStore           *DigestSessionStore
	cfg                   *config.Config
	schedulerSnapshot     *SchedulerSnapshotService
	billingService        *BillingService
	rateLimitService      *RateLimitService
	billingCacheService   *BillingCacheService
	identityService       *IdentityService
	httpUpstream          HTTPUpstream
	deferredService       *DeferredService
	concurrencyService    *ConcurrencyService
	claudeTokenProvider   *ClaudeTokenProvider
	sessionLimitCache     SessionLimitCache // 会话数量限制缓存（仅 Anthropic OAuth/SetupToken）
	rpmCache              RPMCache          // RPM 计数缓存（仅 Anthropic OAuth/SetupToken）
	userGroupRateResolver *userGroupRateResolver
	userGroupRateCache    *gocache.Cache
	userGroupRateSF       singleflight.Group
	modelsListCache       *gocache.Cache
	modelsListCacheTTL    time.Duration
	settingService        *SettingService
	responseHeaderFilter  *responseheaders.CompiledHeaderFilter
	debugModelRouting     atomic.Bool
	debugClaudeMimic      atomic.Bool
	channelService        *ChannelService
	resolver              *ModelPricingResolver
	compositeResolver     *CompositeRouteResolver
	debugGatewayBodyFile  atomic.Pointer[os.File] // non-nil when SUB2API_DEBUG_GATEWAY_BODY is set
	tlsFPProfileService   *TLSFingerprintProfileService
	balanceNotifyService  *BalanceNotifyService

	// vertexTokenProvider 缓存每个 Vertex 账号的 OAuth2 TokenSource。
	// 通过 sync.Once 懒加载，避免修改 NewGatewayService 签名（与现网部署兼容）。
	vertexTokenProviderOnce sync.Once
	vertexTokenProvider     *VertexTokenProvider

	// P0-2: 长期绑定相关字段
	bindingRepo           UserAccountBindingRepository
	bindingThresholdCache *gocache.Cache // in-process counter for unstable FP write threshold

	// P0-3: (sub2api_user, platform) 维度的伪指纹画像。可空，当 cfg.Gateway.
	// IdentityProfileInjectEnabled=false 时即使非空也不会被 hot-path 消费。
	identityProfileService *IdentityProfileService

	userPlatformQuotaRepo UserPlatformQuotaRepository
}

// NewGatewayService creates a new GatewayService
func NewGatewayService(
	accountRepo AccountRepository,
	groupRepo GroupRepository,
	usageLogRepo UsageLogRepository,
	usageBillingRepo UsageBillingRepository,
	userRepo UserRepository,
	userSubRepo UserSubscriptionRepository,
	userGroupRateRepo UserGroupRateRepository,
	cache GatewayCache,
	cfg *config.Config,
	schedulerSnapshot *SchedulerSnapshotService,
	concurrencyService *ConcurrencyService,
	billingService *BillingService,
	rateLimitService *RateLimitService,
	billingCacheService *BillingCacheService,
	identityService *IdentityService,
	httpUpstream HTTPUpstream,
	deferredService *DeferredService,
	claudeTokenProvider *ClaudeTokenProvider,
	sessionLimitCache SessionLimitCache,
	rpmCache RPMCache,
	digestStore *DigestSessionStore,
	settingService *SettingService,
	tlsFPProfileService *TLSFingerprintProfileService,
	channelService *ChannelService,
	resolver *ModelPricingResolver,
	compositeResolver *CompositeRouteResolver,
	balanceNotifyService *BalanceNotifyService,
	bindingRepo UserAccountBindingRepository,
	identityProfileService *IdentityProfileService,
	userPlatformQuotaRepo UserPlatformQuotaRepository,
) *GatewayService {
	userGroupRateTTL := resolveUserGroupRateCacheTTL(cfg)
	modelsListTTL := resolveModelsListCacheTTL(cfg)

	svc := &GatewayService{
		accountRepo:            accountRepo,
		groupRepo:              groupRepo,
		usageLogRepo:           usageLogRepo,
		usageBillingRepo:       usageBillingRepo,
		userRepo:               userRepo,
		userSubRepo:            userSubRepo,
		userGroupRateRepo:      userGroupRateRepo,
		cache:                  cache,
		digestStore:            digestStore,
		cfg:                    cfg,
		schedulerSnapshot:      schedulerSnapshot,
		concurrencyService:     concurrencyService,
		billingService:         billingService,
		rateLimitService:       rateLimitService,
		billingCacheService:    billingCacheService,
		identityService:        identityService,
		httpUpstream:           httpUpstream,
		deferredService:        deferredService,
		claudeTokenProvider:    claudeTokenProvider,
		sessionLimitCache:      sessionLimitCache,
		rpmCache:               rpmCache,
		userGroupRateCache:     gocache.New(userGroupRateTTL, time.Minute),
		settingService:         settingService,
		modelsListCache:        gocache.New(modelsListTTL, time.Minute),
		modelsListCacheTTL:     modelsListTTL,
		responseHeaderFilter:   compileResponseHeaderFilter(cfg),
		tlsFPProfileService:    tlsFPProfileService,
		channelService:         channelService,
		resolver:               resolver,
		compositeResolver:      compositeResolver,
		balanceNotifyService:   balanceNotifyService,
		bindingRepo:            bindingRepo,
		bindingThresholdCache:  gocache.New(30*time.Minute, 5*time.Minute),
		identityProfileService: identityProfileService,
		userPlatformQuotaRepo:  userPlatformQuotaRepo,
	}
	if compositeResolver != nil {
		compositeResolver.SetModelOwnershipResolver(svc.resolveCompositeModelOwnership)
	}
	svc.userGroupRateResolver = newUserGroupRateResolver(
		userGroupRateRepo,
		svc.userGroupRateCache,
		userGroupRateTTL,
		&svc.userGroupRateSF,
		"service.gateway",
	)
	svc.debugModelRouting.Store(parseDebugEnvBool(os.Getenv("SUB2API_DEBUG_MODEL_ROUTING")))
	svc.debugClaudeMimic.Store(parseDebugEnvBool(os.Getenv("SUB2API_DEBUG_CLAUDE_MIMIC")))
	if path := strings.TrimSpace(os.Getenv(debugGatewayBodyEnv)); path != "" {
		svc.initDebugGatewayBodyFile(path)
	}

	// Startup audit: check each Anthropic OAuth account for a bound TLS profile.
	// Accounts using the built-in default profile still emit "Go-stdlib-like"
	// TLS signatures that Anthropic's third-party heuristic can match. Running
	// async so DB hiccups don't block service construction.
	go svc.auditAnthropicOAuthTLSProfiles(context.Background())

	return svc
}

// auditAnthropicOAuthTLSProfiles inspects every Anthropic OAuth/SetupToken
// account at startup and emits WARN logs for any that are likely to leak a
// non-CC TLS signature. Specifically:
//   - enable_tls_fingerprint explicitly set to false → full Go-stdlib handshake
//   - enabled but no profile bound (profile_id ≤ 0) → built-in default only
//   - bound profile_id missing in profile cache → silent fallback to default
//
// Each warning includes the account ID and remediation advice so operators can
// see at a glance which accounts need a real Node.js 24 TLS profile.
func (s *GatewayService) auditAnthropicOAuthTLSProfiles(ctx context.Context) {
	if s.accountRepo == nil {
		return
	}

	accounts, err := s.accountRepo.ListByPlatform(ctx, PlatformAnthropic)
	if err != nil {
		slog.Warn("tls_audit: list anthropic accounts failed", "error", err)
		return
	}

	var (
		totalOAuth    int
		disabled      int
		usingBuiltin  int
		boundExplicit int
		boundMissing  int
	)

	for i := range accounts {
		acc := &accounts[i]
		if !acc.IsAnthropicOAuthOrSetupToken() {
			continue
		}
		totalOAuth++

		if !acc.IsTLSFingerprintEnabled() {
			disabled++
			slog.Warn("tls_audit: TLS fingerprint disabled on OAuth account",
				"account_id", acc.ID,
				"account_name", acc.Name,
				"reason", "enable_tls_fingerprint=false",
				"impact", "upstream TLS handshake exposes Go stdlib signature")
			continue
		}

		profileID := acc.GetTLSFingerprintProfileID()
		if profileID <= 0 {
			usingBuiltin++
			slog.Warn("tls_audit: OAuth account uses built-in default TLS profile",
				"account_id", acc.ID,
				"account_name", acc.Name,
				"advice", "bind a curated Node.js 24 profile via Account Management → TLS Fingerprint Profiles")
			continue
		}

		if s.tlsFPProfileService != nil {
			if p := s.tlsFPProfileService.GetProfileByID(profileID); p == nil {
				boundMissing++
				slog.Warn("tls_audit: bound TLS profile not found, fallback to default",
					"account_id", acc.ID,
					"account_name", acc.Name,
					"profile_id", profileID,
					"advice", "profile was deleted; rebind a valid profile")
				continue
			}
		}
		boundExplicit++
	}

	if totalOAuth == 0 {
		return
	}

	slog.Info("tls_audit: Anthropic OAuth account summary",
		"total", totalOAuth,
		"bound_explicit_profile", boundExplicit,
		"using_builtin_default", usingBuiltin,
		"tls_disabled", disabled,
		"bound_profile_missing", boundMissing)
}

// GenerateSessionHash 从预解析请求计算粘性会话 hash
func (s *GatewayService) GenerateSessionHash(parsed *ParsedRequest) string {
	if parsed == nil {
		return ""
	}

	// 1. 最高优先级：从 metadata.user_id 提取 session_xxx
	if parsed.MetadataUserID != "" {
		uid := ParseMetadataUserID(parsed.MetadataUserID)
		if uid != nil && uid.SessionID != "" {
			slog.Info("sticky.hash_source",
				"source", "metadata_user_id",
				"session_id", uid.SessionID,
				"device_id", uid.DeviceID,
				"is_new_format", uid.IsNewFormat,
			)
			return uid.SessionID
		}
		slog.Info("sticky.hash_metadata_parse_failed",
			"metadata_user_id", parsed.MetadataUserID,
			"parsed_nil", uid == nil,
		)
	}

	// 2. 提取带 cache_control: {type: "ephemeral"} 的内容
	cacheableContent := s.extractCacheableContent(parsed)
	if cacheableContent != "" {
		hash := s.hashContent(cacheableContent)
		slog.Info("sticky.hash_source",
			"source", "cacheable_content",
			"hash", hash,
		)
		return hash
	}

	// 3. 最后 fallback: 使用 session上下文 + system + 所有消息的完整摘要串
	var combined strings.Builder
	// 混入请求上下文区分因子，避免不同用户相同消息产生相同 hash
	if parsed.SessionContext != nil {
		_, _ = combined.WriteString(parsed.SessionContext.ClientIP)
		_, _ = combined.WriteString(":")
		_, _ = combined.WriteString(NormalizeSessionUserAgent(parsed.SessionContext.UserAgent))
		_, _ = combined.WriteString(":")
		_, _ = combined.WriteString(strconv.FormatInt(parsed.SessionContext.APIKeyID, 10))
		_, _ = combined.WriteString("|")
	}
	if systemText := extractTextFromSystemRaw(parsed.SystemRaw()); systemText != "" {
		_, _ = combined.WriteString(systemText)
	}
	contentStart := combined.Len()
	appendMessageTextsFromRaw(&combined, parsed.MessagesRaw())
	if combined.Len() == contentStart {
		appendResponsesSessionAnchorFromRaw(&combined, parsed.InputRaw())
	}
	if combined.Len() > 0 {
		hash := s.hashContent(combined.String())
		slog.Info("sticky.hash_source",
			"source", "message_content_fallback",
			"hash", hash,
			"content_len", combined.Len(),
		)
		return hash
	}

	return ""
}

// BindStickySession sets session -> account binding with standard TTL.
func (s *GatewayService) BindStickySession(ctx context.Context, groupID *int64, sessionHash string, accountID int64) error {
	if sessionHash == "" || accountID <= 0 || s.cache == nil {
		return nil
	}
	return s.cache.SetSessionAccountID(ctx, derefGroupID(groupID), sessionHash, accountID, stickySessionTTL)
}

// bindGatewayStickySessionDuringSelection preserves the normal eager sticky
// behavior unless a profit gate is installed. Profit-controlled requests bind
// only after the terminal post-slot check, otherwise a rejected candidate could
// overwrite a healthy pre-existing sticky binding.
func (s *GatewayService) bindGatewayStickySessionDuringSelection(ctx context.Context, groupID *int64, sessionHash string, accountID int64) error {
	if gatewayProfitControlGateActive(ctx) {
		return nil
	}
	return s.BindStickySession(ctx, groupID, sessionHash, accountID)
}

// BindStickySessionAfterProfitAdmission records a terminally admitted
// account. Without a profit gate it preserves the pre-existing eager binding
// behavior at the handler bind points. With a gate it never replaces a
// different binding that already exists: a temporarily ineligible sticky
// account remains bound and automatically becomes eligible again if its
// account rate recovers.
func (s *GatewayService) BindStickySessionAfterProfitAdmission(ctx context.Context, groupID *int64, sessionHash string, accountID int64) error {
	if sessionHash == "" || accountID <= 0 || s.cache == nil {
		return nil
	}
	if !gatewayProfitControlGateActive(ctx) {
		return s.BindStickySession(ctx, groupID, sessionHash, accountID)
	}
	existingAccountID, err := s.cache.GetSessionAccountID(ctx, derefGroupID(groupID), sessionHash)
	if err != nil && !errors.Is(err, ErrStickySessionNotFound) {
		// 读失败时无法判断既有绑定，保守跳过而不是冒着覆盖健康绑定的风险写入。
		slog.Warn("profit_control_sticky_binding_read_failed", "group_id", derefGroupID(groupID), "account_id", accountID, "error", err)
		return nil
	}
	if existingAccountID > 0 && existingAccountID != accountID {
		return nil
	}
	return s.BindStickySession(ctx, groupID, sessionHash, accountID)
}

// GetCachedSessionAccountID retrieves the account ID bound to a sticky session.
// Returns 0 if no binding exists or on error.
func (s *GatewayService) GetCachedSessionAccountID(ctx context.Context, groupID *int64, sessionHash string) (int64, error) {
	if sessionHash == "" || s.cache == nil {
		return 0, nil
	}
	accountID, err := s.cache.GetSessionAccountID(ctx, derefGroupID(groupID), sessionHash)
	if err != nil {
		return 0, err
	}
	return accountID, nil
}

// ============ P0-3: Session account fanout limiting (anti-sweep) ============

// RecordSessionFanout 记录会话触达的账号（P0-3 反扫荡）。
// 同一账号被多次重试不会重复计数（Redis Set 去重）。
func (s *GatewayService) RecordSessionFanout(ctx context.Context, groupID *int64, sessionHash string, accountID int64) error {
	if sessionHash == "" || accountID <= 0 || s.cache == nil {
		return nil
	}
	fanoutWindowSec := 0
	if s.cfg != nil {
		fanoutWindowSec = s.cfg.Gateway.SessionAccountFanoutWindowSec
	}
	if fanoutWindowSec <= 0 {
		fanoutWindowSec = 60 // default 60s
	}
	ttl := time.Duration(fanoutWindowSec) * time.Second
	return s.cache.RecordSessionAccountFanout(ctx, derefGroupID(groupID), sessionHash, accountID, ttl)
}

// IsSessionFanoutExhausted 检查会话是否已达到 fanout 上限。
// 返回 (exhausted, currentCount)。limit=0 时始终返回 (false, 0)。
func (s *GatewayService) IsSessionFanoutExhausted(ctx context.Context, groupID *int64, sessionHash string) (bool, int) {
	limit := 0
	if s.cfg != nil {
		limit = s.cfg.Gateway.SessionAccountFanoutLimit
	}
	if limit <= 0 || sessionHash == "" || s.cache == nil {
		return false, 0
	}
	count, err := s.cache.GetSessionAccountFanoutCount(ctx, derefGroupID(groupID), sessionHash)
	if err != nil {
		return false, 0
	}
	return count >= limit, count
}

// ClearSessionFanout 清除会话的 fanout 记录（成功完成时可选调用）。
func (s *GatewayService) ClearSessionFanout(ctx context.Context, groupID *int64, sessionHash string) {
	if sessionHash == "" || s.cache == nil {
		return
	}
	_ = s.cache.DeleteSessionAccountFanout(ctx, derefGroupID(groupID), sessionHash)
}

// GetSessionFanoutConfig 返回 fanout 限制配置，供 handler 层传递给 FailoverState。
func (s *GatewayService) GetSessionFanoutConfig() (limit int, boundJitterMin, boundJitterMax time.Duration) {
	// 该方法通过 sessionFanoutConfigProvider 接口调用；调用方的 provider == nil 检查
	// 无法拦截"接口里装着 typed nil 指针"的情况，因此这里必须自己兜底。
	// limit=0 会让调用方按"fanout 未启用"处理。
	if s == nil {
		return 0, 0, 0
	}
	if s.cfg != nil {
		limit = s.cfg.Gateway.SessionAccountFanoutLimit
	}

	minMs := 0
	if s.cfg != nil {
		minMs = s.cfg.Gateway.BoundSessionSwitchJitterMinMs
	}
	if minMs <= 0 {
		minMs = 2000 // default 2s
	}
	maxMs := 0
	if s.cfg != nil {
		maxMs = s.cfg.Gateway.BoundSessionSwitchJitterMaxMs
	}
	if maxMs <= 0 {
		maxMs = 10000 // default 10s
	}
	if maxMs < minMs {
		maxMs = minMs
	}
	return limit, time.Duration(minMs) * time.Millisecond, time.Duration(maxMs) * time.Millisecond
}

// ============ P0-2: Long-term user→account bindings ============

// P0-2 long-term binding metrics. These counters are read by
// SnapshotLongTermBindingMetrics and exposed in /admin/metrics.
var (
	longTermBindingResolveHitTotal     atomic.Int64 // 解析时命中
	longTermBindingResolveMissTotal    atomic.Int64 // 解析时未命中（含没有 fp / 没有绑定）
	longTermBindingResolveExpiredTotal atomic.Int64 // 解析时命中但已过期（lazy 删除）
	longTermBindingWriteFirstTotal     atomic.Int64 // 第一次写入（accountID 视图首次出现）
	longTermBindingWriteRebindTotal    atomic.Int64 // 重绑（accountID 与上次写入不同）
	longTermBindingWriteRefreshTotal   atomic.Int64 // 续期（accountID 不变，仅刷新 TTL）
	longTermBindingWriteSkippedTotal   atomic.Int64 // 因不稳定 fp 阈值未达 / disabled 等原因被跳过
	longTermBindingDeleteTotal         atomic.Int64 // 主动删除（含 lazy delete）
)

// LongTermBindingMetricsSnapshot 是 P0-2 绑定层的运行时计数快照。
type LongTermBindingMetricsSnapshot struct {
	ResolveHitTotal     int64 `json:"resolve_hit_total"`
	ResolveMissTotal    int64 `json:"resolve_miss_total"`
	ResolveExpiredTotal int64 `json:"resolve_expired_total"`
	WriteFirstTotal     int64 `json:"write_first_total"`
	WriteRebindTotal    int64 `json:"write_rebind_total"`
	WriteRefreshTotal   int64 `json:"write_refresh_total"`
	WriteSkippedTotal   int64 `json:"write_skipped_total"`
	DeleteTotal         int64 `json:"delete_total"`
	// HitRate = hit / (hit + miss)，便于直接读出。
	HitRate float64 `json:"hit_rate"`
}

// SnapshotLongTermBindingMetrics returns a copy of the current P0-2 binding counters.
func SnapshotLongTermBindingMetrics() LongTermBindingMetricsSnapshot {
	hit := longTermBindingResolveHitTotal.Load()
	miss := longTermBindingResolveMissTotal.Load()
	rate := float64(0)
	if total := hit + miss; total > 0 {
		rate = float64(hit) / float64(total)
	}
	return LongTermBindingMetricsSnapshot{
		ResolveHitTotal:     hit,
		ResolveMissTotal:    miss,
		ResolveExpiredTotal: longTermBindingResolveExpiredTotal.Load(),
		WriteFirstTotal:     longTermBindingWriteFirstTotal.Load(),
		WriteRebindTotal:    longTermBindingWriteRebindTotal.Load(),
		WriteRefreshTotal:   longTermBindingWriteRefreshTotal.Load(),
		WriteSkippedTotal:   longTermBindingWriteSkippedTotal.Load(),
		DeleteTotal:         longTermBindingDeleteTotal.Load(),
		HitRate:             rate,
	}
}

// ExtractProjectFP computes a project fingerprint from a ParsedRequest.
// Returns (fingerprint, isStable) where isStable=true means device_id was available.
// The fingerprint is used as the key for long-term user→account bindings.
//
// 这是 Anthropic 路径的便捷入口；OpenAI / Gemini 等没有 ParsedRequest 的调用者
// 应使用 ExtractProjectFPRaw。
func (s *GatewayService) ExtractProjectFP(parsed *ParsedRequest) (string, bool) {
	if parsed == nil {
		return "", false
	}
	metadataUserID := parsed.MetadataUserID
	groupID := derefGroupID(parsed.GroupID)
	apiKeyID := int64(0)
	clientIP := ""
	if parsed.SessionContext != nil {
		apiKeyID = parsed.SessionContext.APIKeyID
		clientIP = parsed.SessionContext.ClientIP
	}
	return s.ExtractProjectFPRaw(metadataUserID, apiKeyID, clientIP, groupID)
}

// ExtractProjectFPRaw computes a project fingerprint from raw inputs.
// 用于 Gemini / OpenAI 等不构造 ParsedRequest 的路径。语义与 ExtractProjectFP
// 等价：优先用 metadata.user_id 里的 device_id（稳定路径），否则退化到
// api_key_id + IPv4 /24 子网（不稳定路径，需要阈值才会持久化）。
//
// metadataUserID 为空 / 不含 device_id 时自动走 fallback；非 Claude 客户端
// 直接传 "" 即可。
func (s *GatewayService) ExtractProjectFPRaw(metadataUserID string, apiKeyID int64, clientIP string, groupID int64) (string, bool) {
	// Stable path: device_id from metadata.user_id (Claude Code clients)
	if metadataUserID != "" {
		if uid := ParseMetadataUserID(metadataUserID); uid != nil && uid.DeviceID != "" {
			raw := uid.DeviceID + ":" + strconv.FormatInt(groupID, 10)
			return sha256hex(raw)[:32], true
		}
	}

	// Fallback path: api_key_id + /24 subnet
	if apiKeyID > 0 {
		ip24 := toIPNet24(clientIP)
		raw := fmt.Sprintf("ip:%d:%s:%d", apiKeyID, ip24, groupID)
		return sha256hex(raw)[:32], false
	}

	return "", false
}

// ResolveLongTermBinding looks up the persistent user→account binding.
// Returns the bound account ID, or 0 if no valid binding exists.
func (s *GatewayService) ResolveLongTermBinding(ctx context.Context, projectFP string, groupID int64) int64 {
	if s.bindingRepo == nil || projectFP == "" {
		longTermBindingResolveMissTotal.Add(1)
		return 0
	}
	binding, err := s.bindingRepo.GetBinding(ctx, projectFP, groupID)
	if err != nil || binding == nil {
		longTermBindingResolveMissTotal.Add(1)
		return 0
	}
	if time.Now().After(binding.ExpiresAt) {
		// Expired binding, clean it up
		_ = s.bindingRepo.DeleteBinding(ctx, projectFP, groupID)
		longTermBindingResolveExpiredTotal.Add(1)
		longTermBindingDeleteTotal.Add(1)
		longTermBindingResolveMissTotal.Add(1)
		return 0
	}
	longTermBindingResolveHitTotal.Add(1)
	return binding.AccountID
}

// MaybeWriteBinding writes or refreshes a long-term user→account binding.
// For stable (device_id) fingerprints: writes immediately on first request.
// For unstable (ip-based) fingerprints: writes only after 2+ requests in 30min.
func (s *GatewayService) MaybeWriteBinding(ctx context.Context, projectFP string, isStable bool, accountID int64, groupID int64) {
	if s.bindingRepo == nil || projectFP == "" || accountID <= 0 {
		longTermBindingWriteSkippedTotal.Add(1)
		return
	}
	ttlDays := s.cfg.Gateway.LongTermBindingTTLDays
	if ttlDays <= 0 {
		longTermBindingWriteSkippedTotal.Add(1)
		return // Disabled
	}

	if !isStable {
		// Check threshold: need 2+ requests for unstable fp
		cacheKey := "ltb_threshold:" + projectFP
		cnt := s.incrBindingThreshold(cacheKey)
		if cnt < 2 {
			longTermBindingWriteSkippedTotal.Add(1)
			return
		}
	}

	// 用一次 GetBinding 区分 first / rebind / refresh —— 用以观察上游对外用户数 P95。
	// 此调用对 hit/miss 计数没有影响（不走 ResolveLongTermBinding）。
	prev, _ := s.bindingRepo.GetBinding(ctx, projectFP, groupID)

	expiresAt := time.Now().Add(time.Duration(ttlDays) * 24 * time.Hour)
	if err := s.bindingRepo.UpsertBinding(ctx, projectFP, accountID, groupID, expiresAt); err != nil {
		longTermBindingWriteSkippedTotal.Add(1)
		return
	}

	switch {
	case prev == nil:
		longTermBindingWriteFirstTotal.Add(1)
	case prev.AccountID != accountID:
		longTermBindingWriteRebindTotal.Add(1)
	default:
		longTermBindingWriteRefreshTotal.Add(1)
	}
}

// DeleteLongTermBinding removes a specific user→account binding.
// Called when the bound account becomes unschedulable.
func (s *GatewayService) DeleteLongTermBinding(ctx context.Context, projectFP string, groupID int64) {
	if s.bindingRepo == nil || projectFP == "" {
		return
	}
	if err := s.bindingRepo.DeleteBinding(ctx, projectFP, groupID); err == nil {
		longTermBindingDeleteTotal.Add(1)
	}
}

// incrBindingThreshold atomically increments the threshold counter for a given key.
func (s *GatewayService) incrBindingThreshold(key string) int {
	if s.bindingThresholdCache == nil {
		return 1
	}
	val, found := s.bindingThresholdCache.Get(key)
	if !found {
		s.bindingThresholdCache.Set(key, 1, 30*time.Minute)
		return 1
	}
	cnt, ok := val.(int)
	if !ok {
		cnt = 0
	}
	cnt++
	s.bindingThresholdCache.Set(key, cnt, 30*time.Minute)
	return cnt
}

// sha256hex computes SHA256 and returns the hex string.
func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// toIPNet24 extracts the /24 subnet from an IP address.
// For IPv4: "1.2.3.4" -> "1.2.3.0/24"
// For IPv6: returns the full address (no /24 equivalent applied)
func toIPNet24(ip string) string {
	if ip == "" {
		return ""
	}
	parsed := net.ParseIP(ip)
	if parsed == nil {
		return ip
	}
	if v4 := parsed.To4(); v4 != nil {
		return fmt.Sprintf("%d.%d.%d.0/24", v4[0], v4[1], v4[2])
	}
	return ip // IPv6: use full address
}

// FindGeminiSession 查找 Gemini 会话（基于内容摘要链的 Fallback 匹配）
// 返回最长匹配的会话信息（uuid, accountID）
func (s *GatewayService) FindGeminiSession(_ context.Context, groupID int64, prefixHash, digestChain string) (uuid string, accountID int64, matchedChain string, found bool) {
	if digestChain == "" || s.digestStore == nil {
		return "", 0, "", false
	}
	return s.digestStore.Find(groupID, prefixHash, digestChain)
}

// SaveGeminiSession 保存 Gemini 会话。oldDigestChain 为 Find 返回的 matchedChain，用于删旧 key。
func (s *GatewayService) SaveGeminiSession(_ context.Context, groupID int64, prefixHash, digestChain, uuid string, accountID int64, oldDigestChain string) error {
	if digestChain == "" || s.digestStore == nil {
		return nil
	}
	s.digestStore.Save(groupID, prefixHash, digestChain, uuid, accountID, oldDigestChain)
	return nil
}

// FindAnthropicSession 查找 Anthropic 会话（基于内容摘要链的 Fallback 匹配）
func (s *GatewayService) FindAnthropicSession(_ context.Context, groupID int64, prefixHash, digestChain string) (uuid string, accountID int64, matchedChain string, found bool) {
	if digestChain == "" || s.digestStore == nil {
		return "", 0, "", false
	}
	return s.digestStore.Find(groupID, prefixHash, digestChain)
}

// SaveAnthropicSession 保存 Anthropic 会话
func (s *GatewayService) SaveAnthropicSession(_ context.Context, groupID int64, prefixHash, digestChain, uuid string, accountID int64, oldDigestChain string) error {
	if digestChain == "" || s.digestStore == nil {
		return nil
	}
	s.digestStore.Save(groupID, prefixHash, digestChain, uuid, accountID, oldDigestChain)
	return nil
}

func (s *GatewayService) extractCacheableContent(parsed *ParsedRequest) string {
	if parsed == nil {
		return ""
	}

	systemText := extractCacheableTextFromSystemRaw(parsed.SystemRaw())
	if messageText := extractCacheableTextFromMessagesRaw(parsed.MessagesRaw()); messageText != "" {
		return messageText
	}
	return systemText
}

func parseRawJSONView(raw []byte) gjson.Result {
	if len(raw) == 0 {
		return gjson.Result{}
	}
	// 这里只做同步只读解析，避免 gjson.ParseBytes 为大 messages/contents 复制整段 raw。
	return gjson.Parse(*(*string)(unsafe.Pointer(&raw)))
}

func extractTextFromSystemRaw(raw []byte) string {
	system := parseRawJSONView(raw)
	switch system.Type {
	case gjson.String:
		return system.String()
	case gjson.JSON:
		if !system.IsArray() {
			return ""
		}
		var builder strings.Builder
		system.ForEach(func(_, part gjson.Result) bool {
			if text := part.Get("text").String(); text != "" {
				_, _ = builder.WriteString(text)
			}
			return true
		})
		return builder.String()
	}
	return ""
}

func extractTextFromContentRaw(content gjson.Result) string {
	switch content.Type {
	case gjson.String:
		return content.String()
	case gjson.JSON:
		if !content.IsArray() {
			return ""
		}
		var builder strings.Builder
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("type").String() == "text" {
				if text := part.Get("text").String(); text != "" {
					_, _ = builder.WriteString(text)
				}
			}
			return true
		})
		return builder.String()
	}
	return ""
}

func appendMessageTextsFromRaw(builder *strings.Builder, raw []byte) {
	if builder == nil || len(raw) == 0 {
		return
	}
	messages := parseRawJSONView(raw)
	if !messages.IsArray() {
		return
	}
	messages.ForEach(func(_, msg gjson.Result) bool {
		if content := msg.Get("content"); content.Exists() {
			_, _ = builder.WriteString(extractTextFromContentRaw(content))
			return true
		}
		parts := msg.Get("parts")
		if parts.IsArray() {
			parts.ForEach(func(_, part gjson.Result) bool {
				if text := part.Get("text").String(); text != "" {
					_, _ = builder.WriteString(text)
				}
				return true
			})
		}
		return true
	})
}

func appendResponsesSessionAnchorFromRaw(builder *strings.Builder, raw []byte) {
	if builder == nil || len(raw) == 0 {
		return
	}
	input := parseRawJSONView(raw)
	if input.Type == gjson.String {
		_, _ = builder.WriteString(input.String())
		return
	}
	if !input.IsArray() {
		return
	}

	input.ForEach(func(_, item gjson.Result) bool {
		if item.Type == gjson.String {
			_, _ = builder.WriteString(item.String())
			return false
		}

		switch item.Get("role").String() {
		case "system", "developer":
			appendResponsesContentText(builder, item.Get("content"))
		case "user":
			appendResponsesContentText(builder, item.Get("content"))
			return false
		default:
			if item.Get("type").String() == "input_text" {
				if text := item.Get("text").String(); text != "" {
					_, _ = builder.WriteString(text)
				}
				return false
			}
		}
		return true
	})
}

func appendResponsesContentText(builder *strings.Builder, content gjson.Result) {
	if builder == nil || !content.Exists() {
		return
	}
	if content.Type == gjson.String {
		_, _ = builder.WriteString(content.String())
		return
	}
	if !content.IsArray() {
		return
	}
	content.ForEach(func(_, part gjson.Result) bool {
		switch part.Get("type").String() {
		case "input_text", "text":
			if text := part.Get("text").String(); text != "" {
				_, _ = builder.WriteString(text)
			}
		}
		return true
	})
}

func extractCacheableTextFromSystemRaw(raw []byte) string {
	system := parseRawJSONView(raw)
	if !system.IsArray() {
		return ""
	}
	var builder strings.Builder
	system.ForEach(func(_, part gjson.Result) bool {
		if part.Get("cache_control.type").String() == "ephemeral" {
			if text := part.Get("text").String(); text != "" {
				_, _ = builder.WriteString(text)
			}
		}
		return true
	})
	return builder.String()
}

func extractCacheableTextFromMessagesRaw(raw []byte) string {
	messages := parseRawJSONView(raw)
	if !messages.IsArray() {
		return ""
	}
	var text string
	messages.ForEach(func(_, msg gjson.Result) bool {
		content := msg.Get("content")
		if !content.IsArray() {
			return true
		}
		found := false
		content.ForEach(func(_, part gjson.Result) bool {
			if part.Get("cache_control.type").String() == "ephemeral" {
				found = true
				return false
			}
			return true
		})
		if found {
			text = extractTextFromContentRaw(content)
			return false
		}
		return true
	})
	return text
}

func (s *GatewayService) hashContent(content string) string {
	h := xxhash.Sum64String(content)
	return strconv.FormatUint(h, 36)
}

// replaceModelInBody 替换请求体中的model字段
// 优先使用定点修改，尽量保持客户端原始字段顺序。

// sanitizeSystemText rewrites only the fixed OpenCode identity sentence (if present).
// We intentionally avoid broad keyword replacement in system prompts to prevent
// accidentally changing user-provided instructions.

// pruneClaudeOAuthMetadataToUserIDOnly strips every metadata key except user_id.
// Real Claude Code sends metadata = {"user_id": "..."} exactly; extra keys
// (e.g. client-provided tracking ids) flag the request as non-CC on Anthropic's
// side. Only runs when metadata is an object with a non-empty user_id.
func pruneClaudeOAuthMetadataToUserIDOnly(body []byte) ([]byte, bool) {
	metadata := gjson.GetBytes(body, "metadata")
	if !metadata.Exists() || metadata.Type == gjson.Null {
		return body, false
	}
	if !strings.HasPrefix(strings.TrimSpace(metadata.Raw), "{") {
		return body, false
	}

	userID := metadata.Get("user_id")
	if !userID.Exists() || userID.Type != gjson.String || strings.TrimSpace(userID.String()) == "" {
		return body, false
	}

	hasExtra := false
	metadata.ForEach(func(key, _ gjson.Result) bool {
		if key.String() != "user_id" {
			hasExtra = true
			return false
		}
		return true
	})
	if !hasExtra {
		return body, false
	}

	raw, err := marshalAnthropicMetadata(userID.String())
	if err != nil {
		return body, false
	}
	return setJSONRawBytes(body, "metadata", raw)
}

// claudeOAuthStripFields enumerates top-level request fields that real Claude
// Code never sends on /v1/messages. They are removed when mimicking CC to
// avoid Anthropic's third-party heuristic ("Extra usage required") rejection.
var claudeOAuthStripFields = []string{
	"temperature",
	"tool_choice",
	"top_p",
	"top_k",
	"stop_sequences",
}

// ClaudeCodeShapeAudit captures structural divergences from real Claude Code
// request bodies. Non-empty Leaks / MetadataExtra means the payload carries
// fingerprints that Anthropic's third-party detector can match on.
type ClaudeCodeShapeAudit struct {
	Leaks           []string // top-level fields CC never sends
	MetadataExtra   []string // metadata keys other than user_id
	SystemShape     string   // "missing" | "string" | "array[N]" | "other"
	ToolsCount      int
	HasThinking     bool
	HasOutputConfig bool
}

// IsClean reports whether the body looks like a real Claude Code request.
func (a ClaudeCodeShapeAudit) IsClean() bool {
	return len(a.Leaks) == 0 && len(a.MetadataExtra) == 0
}

// String returns a compact, log-friendly representation.
func (a ClaudeCodeShapeAudit) String() string {
	return fmt.Sprintf(
		"leaks=%v metadata_extra=%v system=%s tools=%d thinking=%t output_config=%t",
		a.Leaks, a.MetadataExtra, a.SystemShape, a.ToolsCount, a.HasThinking, a.HasOutputConfig,
	)
}

// auditClaudeCodeShape inspects an Anthropic-format request body and reports
// structural features that deviate from real Claude Code output. Used by
// gateway forwarding paths to detect third-party fingerprints that survived
// request normalization.
func auditClaudeCodeShape(body []byte) ClaudeCodeShapeAudit {
	var a ClaudeCodeShapeAudit

	for _, field := range claudeOAuthStripFields {
		if gjson.GetBytes(body, field).Exists() {
			a.Leaks = append(a.Leaks, field)
		}
	}

	sys := gjson.GetBytes(body, "system")
	switch {
	case !sys.Exists():
		a.SystemShape = "missing"
	case sys.Type == gjson.String:
		a.SystemShape = "string"
	case sys.IsArray():
		count := 0
		sys.ForEach(func(_, _ gjson.Result) bool { count++; return true })
		a.SystemShape = fmt.Sprintf("array[%d]", count)
	default:
		a.SystemShape = "other"
	}

	if tools := gjson.GetBytes(body, "tools"); tools.IsArray() {
		tools.ForEach(func(_, _ gjson.Result) bool {
			a.ToolsCount++
			return true
		})
	}

	a.HasThinking = gjson.GetBytes(body, "thinking").Exists()
	a.HasOutputConfig = gjson.GetBytes(body, "output_config").Exists()

	if metadata := gjson.GetBytes(body, "metadata"); metadata.IsObject() {
		metadata.ForEach(func(key, _ gjson.Result) bool {
			if key.String() != "user_id" {
				a.MetadataExtra = append(a.MetadataExtra, key.String())
			}
			return true
		})
	}

	return a
}

// logClaudeCodeShapeAudit emits a log describing request body divergence from
// real Claude Code. Logs at WARN when shape leaks persist after normalization
// (indicating a gap in the strip logic), DEBUG when normalization cleaned a
// previously leaky body. Silent when the body was already clean.
func logClaudeCodeShapeAudit(pathLabel string, accountID int64, model string, pre, post ClaudeCodeShapeAudit) {
	if !post.IsClean() {
		logger.L().Warn("gateway: claude-code shape leak after normalize",
			zap.String("path", pathLabel),
			zap.Int64("account_id", accountID),
			zap.String("model", model),
			zap.String("pre_audit", pre.String()),
			zap.String("post_audit", post.String()),
		)
		return
	}
	if !pre.IsClean() {
		logger.L().Debug("gateway: claude-code shape normalized",
			zap.String("path", pathLabel),
			zap.Int64("account_id", accountID),
			zap.String("model", model),
			zap.String("pre_audit", pre.String()),
		)
	}
}

// applyClaudeCodeOAuthMimicryToBody 将"非 Claude Code 客户端 + Claude OAuth 账号"
// 路径上原本只在 /v1/messages 里做的完整伪装应用到任意 body 上。
//
// 这是 /v1/messages 主路径上 rewriteSystemForNonClaudeCode +
// normalizeClaudeOAuthRequestBody 流程的通用版，供 OpenAI 协议兼容层
// (ForwardAsChatCompletions / ForwardAsResponses) 复用。
//
// 未抽离之前，OpenAI 协议兼容层仅做 injectClaudeCodePrompt（前置追加），
// 而仓内 /v1/messages 路径自己的注释明确说过"仅前置追加无法通过 Anthropic
// 第三方检测"；那条注释就是本函数存在的根因。
//
// 参数：
//   - ctx / c：用于读取指纹和 gateway settings；c 可为 nil（如 count_tokens）。
//   - account：必须是 OAuth 账号，且调用方已判断不是 Claude Code 客户端。
//   - body：已经 marshal 成 Anthropic /v1/messages 格式的请求体。
//   - systemRaw：body 中原始 system 字段（用于判断是否需要 rewrite）。
//   - model：最终会发给上游的模型 ID（用于 haiku 旁路 + metadata 版本选择）。
//
// 返回：改写后的 body。即使中间任何一步失败，也会退化成原 body（不会 panic）。

// buildOAuthMetadataUserIDFromBody 是 buildOAuthMetadataUserID 的变体，
// 适用于调用方手上没有 ParsedRequest 的场景（如 OpenAI 协议兼容层）。
//
// 与 buildOAuthMetadataUserID 的唯一区别：
//   - session hash 从 body 本体按同样规则重算，而不是读取 ParsedRequest 缓存值。
//   - 如果 body 里已经存在 metadata.user_id，则返回空（由 ensureClaudeOAuthMetadataUserID
//     自行决定是否覆盖）。

// buildStableSessionSeed 为伪装路径合成的 metadata.user_id session_id 生成"会话级稳定"种子。
//
// 真实 Claude Code 的 session_id 是进程级随机 UUID，在一段会话内跨请求保持不变。无状态代理
// 无法恢复该值，这里用"会话内不变的锚点"近似：账号 ID + 客户端区分因子 + 首条 user 消息文本。
// 对话在尾部追加 messages 时这三者都不变，因此 generateSessionUUID(seed) 跨轮稳定。
//
// 注意：粘性路由键 GenerateSessionHash 按设计逐轮变化（见其测试），本函数与之独立、互不影响。
// accountID 恒存在，故 seed 永不为空 —— 输出始终是确定性 UUID，而非随机值。

// sessionContextDiscriminator 把请求上下文（客户端 IP / 归一化 UA / API Key ID）拼成
// 一个跨客户端的区分因子，避免不同用户的相同首条消息派生出相同 session_id。

// GenerateSessionUUID creates a deterministic UUID4 from a seed string.

func mimicCLIVersion() string {
	return ExtractCLIVersion(claude.DefaultHeaders["User-Agent"])
}

// SelectAccount 选择账号（粘性会话+优先级）

// SelectAccountForModel 选择支持指定模型的账号（粘性会话+优先级+模型映射）

// SelectAccountForModelWithExclusions selects an account supporting the requested model while excluding specified accounts.

// SelectAccountWithLoadAwareness selects account with load-awareness and wait plan.
// 调度流程文档见 docs/ACCOUNT_SCHEDULING_FLOW.md 。
// metadataUserID: 用于客户端亲和调度，从中提取客户端 ID
// sub2apiUserID: 系统用户 ID，用于二维亲和调度

// routingAccountTiersForRequest 解析请求模型的分层路由账号（含 "*" 兜底梯队）。
// 返回 (有序账号 ID, accountID->梯队序号)。仅 anthropic 分组生效。
func (s *GatewayService) routingAccountTiersForRequest(ctx context.Context, groupID *int64, requestedModel string, platform string) ([]int64, map[int64]int) {
	if groupID == nil || requestedModel == "" || platform != PlatformAnthropic {
		return nil, nil
	}
	group, err := s.resolveGroupByID(ctx, *groupID)
	if err != nil || group == nil {
		if s.debugModelRoutingEnabled() {
			logger.LegacyPrintf("service.gateway", "[ModelRoutingDebug] resolve group failed: group_id=%v model=%s platform=%s err=%v", derefGroupID(groupID), requestedModel, platform, err)
		}
		return nil, nil
	}
	// Model routing applies to anthropic groups, and to composite groups whose
	// request already resolved to Anthropic (upstream's composite-group support).
	if group.Platform != PlatformAnthropic && group.Platform != PlatformComposite {
		if s.debugModelRoutingEnabled() {
			logger.LegacyPrintf("service.gateway", "[ModelRoutingDebug] skip: non-anthropic group platform: group_id=%d group_platform=%s model=%s", group.ID, group.Platform, requestedModel)
		}
		return nil, nil
	}
	ids, tierRank := group.GetRoutingAccountIDsTiered(requestedModel)
	if s.debugModelRoutingEnabled() {
		logger.LegacyPrintf("service.gateway", "[ModelRoutingDebug] routing lookup: group_id=%d model=%s enabled=%v rules=%d matched_ids=%v",
			group.ID, requestedModel, group.ModelRoutingEnabled, len(group.ModelRouting), ids)
	}
	return ids, tierRank
}

// checkClaudeCodeRestriction 检查分组的 Claude Code 客户端限制
// 如果分组启用了 claude_code_only 且请求不是来自 Claude Code 客户端：
//   - 有降级分组：返回降级分组的 ID
//   - 无降级分组：返回 ErrClaudeCodeOnly 错误

// IsSingleAntigravityAccountGroup 检查指定分组是否只有一个 antigravity 平台的可调度账号。
// 用于 Handler 层在首次请求时提前设置 SingleAccountRetry context，
// 避免单账号分组收到 503 时错误地设置模型限流标记导致后续请求连续快速失败。

// isAccountInGroup checks if the account belongs to the specified group.
// When groupID is nil, returns true only for ungrouped accounts (no group assignments).

// effectiveAccountConcurrency 返回 acquire-slot 与 WaitPlan 应使用的并发上限。
// 当 account.Concurrency == 0（admin 未显式覆盖）时回退到全局默认
// gateway.account_default_concurrency；admin 显式设置时按设置值生效。
//
// 该方法是 P0-1 引入的"账号级硬限"安全垫的单一入口。所有计算
// WaitPlan.MaxConcurrency 或调用 tryAcquireAccountSlot 的位置都应使用它，
// 不要再直接读取 account.Concurrency。
func (s *GatewayService) effectiveAccountConcurrency(ctx context.Context, account *Account) int {
	return s.effectiveAccountConcurrencyWithGroup(account, GroupFromContext(ctx))
}

func (s *GatewayService) effectiveAccountConcurrencyWithGroup(account *Account, group *Group) int {
	fleetDefault := 0
	if s != nil && s.cfg != nil {
		fleetDefault = s.cfg.Gateway.AccountDefaultConcurrency
	}
	groupDefault := 0
	if group != nil {
		groupDefault = group.DefaultAccountConcurrency
	}
	return account.EffectiveConcurrencyWithGroup(groupDefault, fleetDefault)
}

// effectiveAccountBaseRPM 返回账号实际生效的 RPM 上限（per-account 优先，
// 全局默认 gateway.account_default_rpm 兜底）。
func (s *GatewayService) effectiveAccountBaseRPM(ctx context.Context, account *Account) int {
	return s.effectiveAccountBaseRPMWithGroup(account, GroupFromContext(ctx))
}

func (s *GatewayService) effectiveAccountBaseRPMWithGroup(account *Account, group *Group) int {
	fleetDefault := 0
	if s != nil && s.cfg != nil {
		fleetDefault = s.cfg.Gateway.AccountDefaultRPM
	}
	groupDefault := 0
	if group != nil {
		groupDefault = group.DefaultAccountRPM
	}
	return account.EffectiveBaseRPMWithGroup(groupDefault, fleetDefault)
}

// EffectiveAccountConcurrency 是 effectiveAccountConcurrency 的导出版本，
// 供 handler / 其它包查询账号实际生效的并发上限。
func (s *GatewayService) EffectiveAccountConcurrency(ctx context.Context, account *Account) int {
	return s.effectiveAccountConcurrency(ctx, account)
}

// EffectiveAccountConcurrencyFromCfg 提供给同 service 包内、没有持有 *GatewayService
// 的其它 service（Gemini/Antigravity/Ops 等）使用的辅助函数：从全局 *config.Config
// 读取 gateway.account_default_concurrency 兜底，并与账号自身设置组合得到实际
// 生效的并发上限。
func EffectiveAccountConcurrencyFromCfg(cfg *config.Config, account *Account) int {
	fleetDefault := 0
	if cfg != nil {
		fleetDefault = cfg.Gateway.AccountDefaultConcurrency
	}
	return account.EffectiveConcurrency(fleetDefault)
}

// EffectiveAccountConcurrencyFromCfgCtx 是 EffectiveAccountConcurrencyFromCfg 的
// ctx 感知版本：额外把请求 ctx 里的分组级默认（account > group > system）纳入解析。
// 请求路径（Gemini/Antigravity）应使用此版本；跨请求共享池（无每请求 group）继续用
// 非 ctx 版本。
func EffectiveAccountConcurrencyFromCfgCtx(ctx context.Context, cfg *config.Config, account *Account) int {
	fleetDefault := 0
	if cfg != nil {
		fleetDefault = cfg.Gateway.AccountDefaultConcurrency
	}
	groupDefault := 0
	if g := GroupFromContext(ctx); g != nil {
		groupDefault = g.DefaultAccountConcurrency
	}
	return account.EffectiveConcurrencyWithGroup(groupDefault, fleetDefault)
}

// EffectiveAccountBaseRPMFromCfg 是 RPM 版本的同名辅助函数。
func EffectiveAccountBaseRPMFromCfg(cfg *config.Config, account *Account) int {
	fleetDefault := 0
	if cfg != nil {
		fleetDefault = cfg.Gateway.AccountDefaultRPM
	}
	return account.EffectiveBaseRPM(fleetDefault)
}

// EffectiveAccountBaseRPM 是 effectiveAccountBaseRPM 的导出版本，
// 供 handler 在做 RPM gating / 计数判定时使用，避免直接读取
// account.GetBaseRPM() 而绕过全局兜底。
func (s *GatewayService) EffectiveAccountBaseRPM(ctx context.Context, account *Account) int {
	return s.effectiveAccountBaseRPM(ctx, account)
}

// isAccountSchedulableForQuota 检查账号是否在配额限制内
// 适用于配置了 quota_limit 的 apikey / bedrock / vertex 类型账号

// isAccountSchedulableForWindowCost 检查账号是否可根据窗口费用进行调度
// 仅适用于 Anthropic OAuth/SetupToken 账号
// 返回 true 表示可调度，false 表示不可调度

// rpmPrefetchContextKey is the context key for prefetched RPM counts.

// withRPMPrefetch 批量预取所有候选账号的 RPM 计数

// isAccountSchedulableForRPM 检查账号是否可根据 RPM 进行调度
// 仅适用于 Anthropic OAuth/SetupToken 账号
//
// P0-1（2026-04 加固）：使用 effectiveAccountBaseRPM 而非 account.GetBaseRPM()，
// 这样即便 admin 没有为某账号显式设置 base_rpm，也会按全局
// gateway.account_default_rpm 兜底进行三区判定，而不是直接放行。

// IncrementAccountRPM increments the RPM counter for the given account.
// 已知 TOCTOU 竞态：调度时读取 RPM 计数与此处递增之间存在时间窗口，
// 高并发下可能短暂超出 RPM 限制。这是与 WindowCost 一致的 soft-limit
// 设计权衡——可接受的少量超额优于加锁带来的延迟和复杂度。

// checkAndRegisterSession 检查并注册会话，用于会话数量限制
// 仅适用于 Anthropic OAuth/SetupToken 账号
// sessionID: 会话标识符（使用粘性会话的 hash）
// 返回 true 表示允许（在限制内或会话已存在），false 表示拒绝（超出限制且是新会话）

// filterByMinPriority 过滤出优先级最小的账号集合

// filterByMinLoadRate 过滤出负载率最低的账号集合

// filterBySoonestReset 过滤出「会话窗口最早重置」的账号集合（use-it-or-lose-it）。
// 仅保留拥有未来重置时间（SessionWindowEnd 在当前时间之后）且最早的账号；
// 窗口为空或已过期的账号视为无活跃窗口、优先级最低。
// 当所有账号都没有活跃窗口时，返回原集合（不改变后续 LRU 选择）。

// selectByLRU 从集合中选择最久未用的账号
// 如果有多个账号具有相同的最小 LastUsedAt，则随机选择一个

// shuffleWithinSortGroups 对排序后的 accountWithLoad 切片，按 (Priority, LoadRate, LastUsedAt) 分组后组内随机打乱。
// 防止并发请求读取同一快照时，确定性排序导致所有请求命中相同账号。

// sameAccountWithLoadGroup 判断两个 accountWithLoad 是否属于同一排序组

// shuffleWithinPriorityAndLastUsed 对排序后的 []*Account 切片，按 (Priority, LastUsedAt) 分组后组内随机打乱。
//
// 注意：当 preferOAuth=true 时，需要保证 OAuth 账号在同组内仍然优先，否则会把排序时的偏好打散掉。
// 因此这里采用"组内分区 + 分区内 shuffle"的方式：
// - 先把同组账号按 (OAuth / 非 OAuth) 拆成两段，保持 OAuth 段在前；
// - 再分别在各段内随机打散，避免热点。

// sameAccountGroup 判断两个 Account 是否属于同一排序组（Priority + LastUsedAt）

// sameLastUsedAt 判断两个 LastUsedAt 是否相同（精度到秒）

// sortCandidatesForFallback 根据配置选择排序策略
// mode: "last_used"(按最后使用时间) 或 "random"(随机)

// sortAccountsByPriorityOnly 仅按优先级排序

// shuffleWithinPriority 在同优先级内随机打乱顺序

// selectAccountForModelWithPlatform 选择单平台账户（完全隔离）

// selectAccountWithMixedScheduling 选择账户（支持混合调度）
// 查询原生平台账户 + 启用 mixed_scheduling 的 antigravity 账户

// isModelSupportedByAccountWithContext 根据账户平台检查模型支持（带 context）
// 对于 Antigravity 平台，会先获取映射后的最终模型名（包括 thinking 后缀）再检查支持

// isModelSupportedByAccount 根据账户平台检查模型支持（无 context，用于非 Antigravity 平台）

// GetAccessToken 获取账号凭证
func (s *GatewayService) GetAccessToken(ctx context.Context, account *Account) (string, string, error) {
	switch account.Type {
	case AccountTypeOAuth, AccountTypeSetupToken:
		// Both oauth and setup-token use OAuth token flow
		return s.getOAuthToken(ctx, account)
	case AccountTypeAPIKey:
		apiKey := account.GetCredential("api_key")
		if apiKey == "" {
			return "", "", errors.New("api_key not found in credentials")
		}
		return apiKey, "apikey", nil
	case AccountTypeBedrock:
		if account.IsClaudePlatformAWS() {
			apiKey := account.GetCredential("api_key")
			if apiKey == "" {
				return "", "", errors.New("api_key not found in credentials")
			}
			return apiKey, "apikey", nil
		}
		return "", "bedrock", nil // Bedrock 使用 SigV4 签名或 API Key，由 forwardBedrock 处理
	case AccountTypeServiceAccount, AccountTypeVertex:
		if account.Platform != PlatformAnthropic {
			return "", "", fmt.Errorf("unsupported service account platform: %s", account.Platform)
		}
		if s.claudeTokenProvider == nil {
			return "", "", errors.New("claude token provider not configured")
		}
		accessToken, err := s.claudeTokenProvider.GetAccessToken(ctx, account)
		if err != nil {
			return "", "", err
		}
		return accessToken, "service_account", nil
	default:
		return "", "", fmt.Errorf("unsupported account type: %s", account.Type)
	}
}

func (s *GatewayService) getOAuthToken(ctx context.Context, account *Account) (string, string, error) {
	// 对于 Anthropic OAuth 账号，使用 ClaudeTokenProvider 获取缓存的 token
	if account.Platform == PlatformAnthropic && account.Type == AccountTypeOAuth && s.claudeTokenProvider != nil {
		accessToken, err := s.claudeTokenProvider.GetAccessToken(ctx, account)
		if err != nil {
			return "", "", err
		}
		return accessToken, "oauth", nil
	}

	// Grok OAuth: prefer access_token from credentials (background refresher keeps it warm).
	if account.Platform == PlatformGrok && account.Type == AccountTypeOAuth {
		accessToken := account.GetGrokAccessToken()
		if accessToken == "" {
			return "", "", errors.New("grok access_token not found in credentials")
		}
		return accessToken, "oauth", nil
	}

	// 其他情况（Gemini 有自己的 TokenProvider，setup-token 类型等）直接从账号读取
	accessToken := account.GetCredential("access_token")
	if accessToken == "" {
		return "", "", errors.New("access_token not found in credentials")
	}
	// Token刷新由后台 TokenRefreshService 处理，此处只返回当前token
	return accessToken, "oauth", nil
}

// 重试相关常量
const (
// 最大尝试次数（包含首次请求）。过多重试会导致请求堆积与资源耗尽。

// 指数退避：第 N 次失败后的等待 = retryBaseDelay * 2^(N-1)，并且上限为 retryMaxDelay。

// 最大重试耗时（包含请求本身耗时 + 退避等待时间）。
// 用于防止极端情况下 goroutine 长时间堆积导致资源耗尽。
)

// shouldFailoverUpstreamError determines whether an upstream error should trigger account failover.
// Any 4xx/5xx triggers failover — if it's a genuine client error it will fail on all accounts
// and be returned after MaxSwitches is exhausted; if it's account-specific, failover helps.

// isClaudeCodeClient 判断请求是否来自真正的 Claude Code 客户端。
// 判定条件：
//  1. User-Agent 匹配 claude-cli/X.Y.Z（大小写不敏感）
//  2. metadata.user_id 符合 Claude Code 格式（legacy 或 JSON 格式）
//
// 只检查 metadata.user_id 非空不够严格：第三方工具（opencode 等）可能伪造 UA
// 并附带任意 metadata.user_id 字符串，从而绕过 mimicry。必须通过 ParseMetadataUserID
// 验证格式才能确认是真正的 Claude Code 客户端。

// normalizeSystemParam 将 json.RawMessage 类型的 system 参数转为标准 Go 类型（string / []any / nil），
// 避免 type switch 中 json.RawMessage（底层 []byte）无法匹配 case string / case []any / case nil 的问题。
// 这是 Go 的 typed nil 陷阱：(json.RawMessage, nil) ≠ (nil, nil)。

// systemIncludesClaudeCodePrompt 检查 system 中是否已包含 Claude Code 提示词
// 使用前缀匹配支持多种变体（标准版、Agent SDK 版等）

// hasClaudeCodePrefix 检查文本是否以 Claude Code 提示词的特征前缀开头

// injectClaudeCodePrompt 在 system 开头注入 Claude Code 提示词
// 处理 null、字符串、数组三种格式

// rewriteSystemForNonClaudeCode 把非 Claude Code 客户端的请求改造成真 Claude CLI
// 的 wire 结构：system 字段**仅**包含 Claude Code 身份声明块，客户端原始 system
// prompt 以 user/assistant 消息对的形式注入到 messages 数组开头。
//
// 为什么不直接追加到 system[]（v0.1.119 ~ v0.1.124 的做法）：
//
// Anthropic 的第三方检测基于 system 参数**内容**做语义匹配（见 2026-04 社区
// 研究报告）。如果客户端（如 OpenClaw / opencode / aider）的 system prompt
// 被追加为 system[1]，文本里充满了 "You are running inside OpenClaw"、
// "AGENTS.md"、"SOUL.md"、"/root/.openclaw/workspace/" 等强烈的自报家门
// 特征，即便 headers 完美 mimic、metadata 格式正确，请求依然会被判为第三方
// 应用，返回 400 "Third-party apps now draw from your extra usage…"。生产
// 抓包（2026-04-24）证实了这一点。
//
// 而 messages 字段是承载用户对话内容的地方，出现任何文本都是正常的（用户可以
// 谈论 OpenClaw、opencode、任何项目），不会被用作第三方身份识别信号。把客户端
// system prompt 搬到 messages 里注入，既保留了原始指令的功能性（模型还是能
// 读到），又彻底消除了 system 层面的语义指纹泄露。这和上游 sub2api 的策略
// 一致（见 upstream/main:backend/internal/service/gateway_service.go）。

// enforceCacheControlLimit 强制执行 cache_control 块数量限制（最多 4 个）
// 超限时优先移除工具断点，再移除 messages 断点，最后才移除 system 断点。

// injectAnthropicCacheControlTTL1h 将已有 ephemeral cache_control 块的 ttl 强制写为 1h。
// 仅修改已经存在的 cache_control，不新增缓存断点。

// shouldNormalizeClientDateline reports whether the request body's client
// dateline should be normalized before forwarding to Anthropic. The switch is
// scoped to Anthropic OAuth/SetupToken accounts only; API-Key accounts and
// non-Anthropic platforms bypass this step entirely.

// normalizeClientDatelineIfEnabled applies dateline normalization to body when
// the switch is on and the account qualifies. Returns (nextBody, true) only
// when the body actually changed; otherwise returns (nil, false) so callers
// can skip the writeback.

// Forward 转发请求到Claude API

// forwardBedrock 转发请求到 AWS Bedrock
// ApplyBedrockCCCompat 应用 Bedrock CC 兼容转换（渠道级模型映射后调用）
// 清理 body 中 Anthropic API 专有字段、修复 thinking/tool_use ID、过滤 beta token，
// 同时过滤 HTTP header 中的 anthropic-beta（防止 Passthrough 路径透传不支持的 token）。

// isBedrockCCCompatEnabled 检查渠道是否启用了 Bedrock CC 兼容模式

// executeBedrockUpstream 执行 Bedrock 上游请求（含重试逻辑）

// bedrockShouldFastFailover reports whether a Bedrock apikey account should skip in-service
// same-region retries and fail over to a different region immediately for this status.
// Retrying the same throttled/overloaded region in a multi-region pool is futile and slow.
func bedrockShouldFastFailover(account *Account, statusCode int, enabled bool) bool {
	if !enabled || account == nil || !account.IsBedrockAPIKey() {
		return false
	}
	switch statusCode {
	case http.StatusTooManyRequests, // 429
		http.StatusInternalServerError, // 500
		http.StatusBadGateway,          // 502
		http.StatusServiceUnavailable,  // 503
		http.StatusGatewayTimeout,      // 504
		529:                            // overloaded
		return true
	}
	return false
}

// handleBedrockUpstreamErrors 处理 Bedrock 上游 4xx/5xx 错误（failover + 错误响应）

// buildUpstreamRequestBedrock 构建 Bedrock 上游请求

// buildUpstreamRequestBedrockAPIKey 构建 Bedrock API Key (Bearer Token) 上游请求

// handleBedrockNonStreamingResponse 处理 Bedrock 非流式响应
// Bedrock InvokeModel 非流式响应的 body 格式与 Claude API 兼容

// ============================== Vertex AI ==============================

// getVertexTokenProvider 懒加载 Vertex token provider，与 GatewayService 生命周期一致。
// 不在 NewGatewayService 中初始化是为了避免修改构造函数签名（向后兼容已有部署）。
func (s *GatewayService) getVertexTokenProvider() *VertexTokenProvider {
	s.vertexTokenProviderOnce.Do(func() {
		s.vertexTokenProvider = NewVertexTokenProvider()
	})
	return s.vertexTokenProvider
}

// forwardVertex 转发请求到 Google Vertex AI（Anthropic publisher 模型）。
// 与 forwardBedrock 同构：解析模型 → 准备 body → 取 OAuth2 access token → 发送 → 处理响应。
// Vertex 与 Bedrock 的差别：
//   - 鉴权：GCP OAuth2 Bearer（VertexTokenProvider 自动刷新）
//   - 流式：标准 SSE（无需自定义解码器）
//   - body：注入 anthropic_version、剥 model/stream（PrepareVertexRequestBody 处理）
func (s *GatewayService) forwardVertex(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	parsed *ParsedRequest,
	startTime time.Time,
) (*ForwardResult, error) {
	reqModel := parsed.Model
	reqStream := parsed.Stream
	body := parsed.Body.Bytes()
	projectID := vertexProjectID(account)
	if projectID == "" {
		return nil, fmt.Errorf("vertex: gcp_project_id not configured for account %d", account.ID)
	}
	region := vertexRegion(account)
	if region == "" {
		return nil, fmt.Errorf("vertex: gcp_region not configured for account %d", account.ID)
	}

	mappedModel, ok := ResolveVertexModelID(account, reqModel)
	if !ok {
		return nil, fmt.Errorf("unsupported vertex model: %s", reqModel)
	}
	if mappedModel != reqModel {
		logger.LegacyPrintf("service.gateway", "[Vertex] Model mapping: %s -> %s (account: %s)", reqModel, mappedModel, account.Name)
	}

	vertexBody, err := PrepareVertexRequestBody(body)
	if err != nil {
		return nil, fmt.Errorf("prepare vertex request body: %w", err)
	}

	accessToken, err := s.getVertexTokenProvider().GetAccessToken(ctx, account)
	if err != nil {
		return nil, fmt.Errorf("vertex: obtain access token: %w", err)
	}

	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}

	logger.LegacyPrintf("service.gateway", "[Vertex] 命中 Vertex 分支: account=%d name=%s project=%s region=%s model=%s->%s stream=%v",
		account.ID, account.Name, projectID, region, reqModel, mappedModel, reqStream)

	resp, err := s.executeVertexUpstream(ctx, c, account, vertexBody, mappedModel, projectID, region, reqStream, accessToken, proxyURL)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 400 {
		return s.handleVertexUpstreamErrors(ctx, resp, c, account)
	}

	var usage *ClaudeUsage
	var firstTokenMs *int
	var clientDisconnect bool
	if reqStream {
		streamResult, err := s.handleVertexStreamingResponse(ctx, resp, c, account, startTime, reqModel)
		if err != nil {
			return nil, err
		}
		usage = streamResult.usage
		firstTokenMs = streamResult.firstTokenMs
		clientDisconnect = streamResult.clientDisconnect
	} else {
		usage, err = s.handleVertexNonStreamingResponse(ctx, resp, c, account)
		if err != nil {
			return nil, err
		}
	}
	if usage == nil {
		usage = &ClaudeUsage{}
	}

	return &ForwardResult{
		RequestID:        resp.Header.Get("x-request-id"),
		Usage:            *usage,
		Model:            reqModel,
		UpstreamModel:    mappedModel,
		Stream:           reqStream,
		Duration:         time.Since(startTime),
		FirstTokenMs:     firstTokenMs,
		ClientDisconnect: clientDisconnect,
	}, nil
}

// executeVertexUpstream 执行 Vertex 上游请求（含重试逻辑）。
// 重试策略与 Bedrock 一致：5xx / shouldRetry 命中时退避重试，最多 maxRetryAttempts 次。
func (s *GatewayService) executeVertexUpstream(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	body []byte,
	modelID string,
	projectID string,
	region string,
	stream bool,
	accessToken string,
	proxyURL string,
) (*http.Response, error) {
	var resp *http.Response
	var err error
	retryStart := time.Now()
	for attempt := 1; attempt <= maxRetryAttempts; attempt++ {
		upstreamReq, buildErr := s.buildUpstreamRequestVertex(ctx, body, projectID, region, modelID, stream, accessToken)
		if buildErr != nil {
			return nil, buildErr
		}

		resp, err = s.httpUpstream.DoWithTLS(upstreamReq, proxyURL, account.ID, s.effectiveAccountConcurrency(ctx, account), nil)
		if err != nil {
			if resp != nil && resp.Body != nil {
				_ = resp.Body.Close()
			}
			safeErr := sanitizeUpstreamErrorMessage(err.Error())
			setOpsUpstreamError(c, 0, safeErr, "")
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: 0,
				UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
				Kind:               "request_error",
				Message:            safeErr,
			})
			c.JSON(http.StatusBadGateway, gin.H{
				"type": "error",
				"error": gin.H{
					"type":    "upstream_error",
					"message": "Upstream request failed",
				},
			})
			MarkForwardResponseFinalized(c)
			return nil, fmt.Errorf("upstream request failed: %s", safeErr)
		}

		if resp.StatusCode >= 400 && resp.StatusCode != 400 && s.shouldRetryUpstreamError(account, resp.StatusCode) {
			if attempt < maxRetryAttempts {
				elapsed := time.Since(retryStart)
				if elapsed >= maxRetryElapsed {
					break
				}

				delay := retryBackoffDelay(attempt)
				remaining := maxRetryElapsed - elapsed
				if delay > remaining {
					delay = remaining
				}
				if delay <= 0 {
					break
				}

				respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
				_ = resp.Body.Close()
				appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
					Platform:           account.Platform,
					AccountID:          account.ID,
					AccountName:        account.Name,
					UpstreamStatusCode: resp.StatusCode,
					UpstreamURL:        safeUpstreamURL(upstreamReq.URL.String()),
					Kind:               "retry",
					Message:            extractUpstreamErrorMessage(respBody),
					Detail: func() string {
						if s.cfg != nil && s.cfg.Gateway.LogUpstreamErrorBody {
							return truncateString(string(respBody), s.cfg.Gateway.LogUpstreamErrorBodyMaxBytes)
						}
						return ""
					}(),
				})
				logger.LegacyPrintf("service.gateway", "[Vertex] account %d: upstream error %d, retry %d/%d after %v",
					account.ID, resp.StatusCode, attempt, maxRetryAttempts, delay)
				if err := sleepWithContext(ctx, delay); err != nil {
					return nil, err
				}
				continue
			}
			break
		}

		break
	}
	if resp == nil || resp.Body == nil {
		return nil, errors.New("upstream request failed: empty response")
	}
	return resp, nil
}

// buildUpstreamRequestVertex 构建 Vertex AI 上游请求：URL + Bearer + JSON body。
// anthropic-beta 头若客户端发了，会随上游请求一起转发（Vertex 透传给 Anthropic 模型）。
func (s *GatewayService) buildUpstreamRequestVertex(
	ctx context.Context,
	body []byte,
	projectID string,
	region string,
	modelID string,
	stream bool,
	accessToken string,
) (*http.Request, error) {
	targetURL := BuildVertexURL(projectID, region, modelID, stream)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	attachVertexAuth(req, accessToken)

	return req, nil
}

// handleVertexUpstreamErrors 处理 Vertex 上游 4xx/5xx 错误（failover + 错误响应）。
// 与 Bedrock 同构。
func (s *GatewayService) handleVertexUpstreamErrors(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
) (*ForwardResult, error) {
	// retry exhausted + failover
	if s.shouldRetryUpstreamError(account, resp.StatusCode) {
		if s.shouldFailoverUpstreamError(resp.StatusCode) {
			respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
			_ = resp.Body.Close()
			resp.Body = io.NopCloser(bytes.NewReader(respBody))

			s.handleFailoverSideEffects(ctx, resp, account)
			appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
				Platform:           account.Platform,
				AccountID:          account.ID,
				AccountName:        account.Name,
				UpstreamStatusCode: resp.StatusCode,
				Kind:               "retry_exhausted_failover",
				Message:            extractUpstreamErrorMessage(respBody),
			})
			return nil, &UpstreamFailoverError{
				StatusCode:             resp.StatusCode,
				ResponseBody:           respBody,
				RetryableOnSameAccount: account.IsPoolMode() && isPoolModeRetryableStatus(resp.StatusCode),
			}
		}
		return s.handleErrorResponse(ctx, resp, c, account)
	}

	// non-retryable failover
	if s.shouldFailoverUpstreamError(resp.StatusCode) {
		respBody, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
		_ = resp.Body.Close()
		resp.Body = io.NopCloser(bytes.NewReader(respBody))

		s.handleFailoverSideEffects(ctx, resp, account)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: resp.StatusCode,
			Kind:               "failover",
			Message:            extractUpstreamErrorMessage(respBody),
		})
		return nil, &UpstreamFailoverError{
			StatusCode:             resp.StatusCode,
			ResponseBody:           respBody,
			RetryableOnSameAccount: account.IsPoolMode() && isPoolModeRetryableStatus(resp.StatusCode),
		}
	}

	return s.handleErrorResponse(ctx, resp, c, account)
}

// handleVertexNonStreamingResponse 处理 Vertex :rawPredict 非流式响应。
// 响应体格式与 Anthropic Messages 完全一致，可直接透传给客户端。
func (s *GatewayService) handleVertexNonStreamingResponse(
	ctx context.Context,
	resp *http.Response,
	c *gin.Context,
	account *Account,
) (*ClaudeUsage, error) {
	body, err := ReadUpstreamResponseBody(resp.Body, s.cfg, c, anthropicTooLargeError)
	if err != nil {
		return nil, err
	}

	usage := parseClaudeUsageFromResponseBody(body)

	c.Header("Content-Type", "application/json")
	c.Data(resp.StatusCode, "application/json", body)
	return usage, nil
}

// vertexSupportedBetaTokens 是 Vertex AI 的 Anthropic 端点接受的 anthropic-beta
// 白名单。Vertex 对任何未知 token 直接 HTTP 400，故采用白名单（与 Bedrock 的
// bedrockSupportedBetaTokens 同思路）而非黑名单：未来 Claude Code 新增的、Vertex 尚未
// 支持的 token 天然被剥离。当 Vertex 新增支持某 beta 时在此补充。
//
// 明确排除（issue #3358 中 Vertex 报 400 的 token）：advisor-tool-2026-03-01、
// prompt-caching-scope-2026-01-05、redact-thinking-2026-02-12、
// thinking-token-count-2026-05-13；以及 claude-code-20250219 / oauth-2025-04-20 等
// 客户端身份 beta——Vertex service_account 走 Bearer 鉴权，不需要它们。

// filterVertexBetaTokens 解析 client 的 anthropic-beta header，先剔除 drop 集合中的
// token（BetaPolicy filter + 默认 drop），再只保留 Vertex 支持的 token，去重后逗号拼接。
// 返回最终 header（可能为空字符串）。

// getBetaHeader 处理anthropic-beta header
// 对于OAuth账号，需要确保包含oauth-2025-04-20

// bodyNeedsContextManagementBeta returns true when the Anthropic request body
// contains a context_management field, which requires the
// context-management-2025-06-27 beta header.
func bodyNeedsContextManagementBeta(body []byte) bool {
	return gjson.GetBytes(body, "context_management").Exists()
}

// computeFinalAnthropicBeta 计算发往上游的最终 anthropic-beta header 值。
//
// 设计动机：将原本在 buildUpstreamRequest 内联在一起、依赖 req.Header 的
// anthropic-beta 计算逻辑抽成纯函数。这样调用方可以在 NewRequest 之前
// 就提前拿到最终 beta header，进而能按它对 body 做能力维度 sanitize 后再做
// CCH 签名——一举修复了以下之前由顺序依赖导致的能力维度 sanitize
// 无法部署的问题（签名与最终 body 不一致可以被判 third-party）。
//
// 返回 (value, shouldSet)：
//   - shouldSet=false 意为“不主动设置 anthropic-beta header”，与原代码“
//     API-key 账号 + 客户端未传 anthropic-beta + InjectBetaForAPIKey 未开启或
//     requestNeedsBetaFeatures=false”的行为对齐。
//   - shouldSet=true 时 value 可能为空字符串（例如客户端透传的 beta 被 dropSet
//     全部过滤掉），这与原代码中 setHeaderRaw 的结果一致。
//
// clientHeaders 是客户端原始 HTTP header（通常为 c.Request.Header）；nil 时按“客户端
// 未传”处理。body 是已经 metadata 重写 / billing version sync 之后但未 sanitize 上游
// 不兼容字段之前的版本。

// computeFinalCountTokensAnthropicBeta 是 count_tokens 路径上 anthropic-beta header 的
// 计算纯函数。语义与 computeFinalAnthropicBeta 对齐，但备份了 count_tokens 独有的
// 两条特殊规则：
//
//   - OAuth mimic：requiredBetas 为 FullClaudeCodeMimicryBetas + BetaTokenCounting
//     （与 messages 不同的是：不按 haiku 排除；count_tokens 始终携带 token-counting beta）
//   - OAuth 透传 + 客户端未传 anthropic-beta：补齐 CountTokensBetaHeader
//   - OAuth 透传 + 客户端传了：补齐 BetaTokenCounting（如果未含）
//
// 返回语义同 computeFinalAnthropicBeta。

// stripBetaTokens removes the given beta tokens from a comma-separated header value.

// upstreamBetaQuerySuffix returns "?beta=true" for normal Anthropic-API
// compatible upstreams, matching the pre-v0.1.166 behavior. Bedrock-backed
// aggregators (e.g. anyrouter.top behind Aliyun ESA WAF, mixroute.ai routed to
// Bedrock) reject unknown query parameters, so they opt out by setting
// extra.upstream_passthrough.bedrock_backed_relay: true on the account.
//
// Callers must already have appended the path (e.g. /v1/messages); this helper
// only returns the query string suffix (with leading "?" or empty).
func upstreamBetaQuerySuffix(account *Account) string {
	if account == nil || account.UpstreamBedrockBackedRelay() {
		return ""
	}
	return "?beta=true"
}

// filterBetaToAllowlist 对 anthropic-beta header 进行白名单过滤：仅保留前缀匹配
// allowPrefixes 的 token。用于非官方上游（聚合商 / 自定义 base URL），这些后端
// 可能将请求路由到 Bedrock/Vertex，而 Bedrock 会拒绝不认识的 beta token。
func filterBetaToAllowlist(header string, allowPrefixes []string) string {
	if header == "" || len(allowPrefixes) == 0 {
		return ""
	}
	parts := strings.Split(header, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		for _, prefix := range allowPrefixes {
			if strings.HasPrefix(p, prefix) {
				out = append(out, p)
				break
			}
		}
	}
	if len(out) == 0 {
		return ""
	}
	return strings.Join(out, ",")
}

// BetaBlockedError indicates a request was blocked by a beta policy rule.

// betaPolicyResult holds the evaluated result of beta policy rules for a single request.

// evaluateBetaPolicy loads settings once and evaluates all rules against the given request.

// mergeDropSets merges the static defaultDroppedBetasSet with dynamic policy filter tokens.
// Returns defaultDroppedBetasSet directly when policySet is empty (zero allocation).

// betaPolicyFilterSetKey is the gin.Context key for caching the policy filter set within a request.

// getBetaPolicyFilterSet returns the beta policy filter set, using the gin context cache if available.
// In the /v1/messages path, Forward() evaluates the policy first and caches the result;
// buildUpstreamRequest reuses it (zero extra DB calls). In the count_tokens path, this
// evaluates on demand (one DB call).

// betaPolicyScopeMatches checks whether a rule's scope matches the current account type.
// isVertex was added alongside isBedrock so beta-policy rules can target Vertex
// accounts independently from API-Key / Bedrock; the apikey scope now also
// excludes vertex (matching its existing exclusion of bedrock).

// matchModelWhitelist checks if a model matches any pattern in the whitelist.
// Reuses matchModelPattern from group.go which supports exact and wildcard prefix matching.

// resolveRuleAction determines the effective action and error message for a rule given the request model.
// When ModelWhitelist is empty, the rule's primary Action/ErrorMessage applies unconditionally.
// When non-empty, Action applies to matching models; FallbackAction/FallbackErrorMessage applies to others.

// droppedBetaSet returns claude.DroppedBetas as a set, with optional extra tokens.

// containsBetaToken checks if a comma-separated header value contains the given token.

// checkBetaPolicyBlockForTokens 检查 token 列表中是否有被管理员 block 规则命中的 token。
// 用于补充 evaluateBetaPolicy 对 header 的检查，覆盖 body 自动注入的 token。

// applyClaudeCodeMimicHeaders fills missing Claude Code-style headers.
//
// 语义 (v0.1.127+): "fill-missing only" — 对每个 claude.DefaultHeaders 键,仅当
// 请求上还没有该头时才写入硬编码 fallback 值。
//
// 之所以不覆盖:调用点 (buildUpstreamRequest) 先走 ApplyFingerprint,把 per-account
// 缓存的真实 Claude Code 指纹 (UA=claude-cli/2.1.118 等当前值) 写到 req。即便
// claude.DefaultHeaders 已对齐到最新核实值,真实客户端缓存往往比我们的内置默认更新;
// 若用 DefaultHeaders 强覆盖会把更新的 cached 指纹打回旧值 → 上游看到过时指纹 →
// Anthropic 判第三方 → 400。
//
// 正确顺序是让 cached fingerprint 胜出;本函数只填 cache 没覆盖到的空位
// (或完全 cache-miss 时整体兜底)。

// shouldRectifySignatureError 统一判断是否应触发签名整流（strip thinking blocks 并重试）。
// 根据账号类型检查对应的开关和匹配模式。
//
// mappedModel 用于按 thinking 协议族分流：passback-required (DeepSeek/Kimi/GLM 等) 上游
// 的 400 不是签名缺失问题，retry 任何 thinking 变形都会破坏「原样回传」契约——直接透传
// 错误给客户端。详见 thinking_protocol.go。

// shouldPreemptivelyFilterThinkingBlocks returns true when the preemptive
// thinking-signature filter should run on the initial forward. Uses the
// same toggle as the retry-path rectifier.
func (s *GatewayService) shouldPreemptivelyFilterThinkingBlocks(ctx context.Context, account *Account) bool {
	if account == nil || s.settingService == nil {
		return false
	}
	return s.settingService.IsSignatureRectifierEnabled(ctx)
}

// isSignatureErrorPattern 仅做模式匹配，不检查开关。
// 用于已进入重试流程后的二阶段检测（此时开关已在首次调用时验证过）。

// matchSignaturePatterns 检查响应体是否匹配自定义关键词列表（不区分大小写）。

// isThinkingBlockSignatureError 检测是否是thinking block相关错误
// 这类错误可以通过过滤thinking blocks并重试来解决

// isInfraLevelUpstream4xxResponse 判断 4xx 响应是否属于"基础设施级"故障：
// 上游 nginx/CDN/代理返回了 HTML 错误页或空响应，而不是 Anthropic/OpenAI
// 风格的 JSON 错误体。典型场景：
//   - nginx 直接返回 "<html>...400 Bad Request..." HTML 页
//   - Cloudflare / 中间代理在账号链路异常时返回的 HTML 拦截页
//   - 空响应体
//
// 这类错误说明该账号的上游链路（不是 Anthropic 真正的 API）当前不可用，
// 继续打到同一账号无意义；应立即切到其他账号并临时封禁该账号。
//
// 故意保守：只匹配明确的 HTML/空响应特征，避免把合法的 JSON 错误（例如
// 真实的 invalid_request_error）误判为基础设施故障。
func isInfraLevelUpstream4xxResponse(headers http.Header, body []byte) bool {
	return isInfraLevelUnexpectedSuccessResponse(headers, body)
}

func isInfraLevelUnexpectedSuccessResponse(headers http.Header, body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	// 空响应体 → 上游链路异常（合法的 4xx 一定有 JSON 错误体）
	if len(trimmed) == 0 {
		return true
	}
	// Content-Type 指明是 HTML/纯文本（合法上游错误一律是 application/json）
	if ct := headers.Get("Content-Type"); ct != "" {
		lower := strings.ToLower(ct)
		if strings.HasPrefix(lower, "text/html") || strings.HasPrefix(lower, "text/plain") {
			return true
		}
	}
	// Body 以 HTML 标志开头（防御 Content-Type 缺失或被代理改写的情况）
	if len(trimmed) > 0 && trimmed[0] == '<' {
		head := bytes.ToLower(trimmed)
		if len(head) > 64 {
			head = head[:64]
		}
		if bytes.HasPrefix(head, []byte("<!doctype")) ||
			bytes.HasPrefix(head, []byte("<html")) ||
			bytes.Contains(head, []byte("<head")) ||
			bytes.Contains(head, []byte("<body")) {
			return true
		}
	}
	return false
}

// streamContentTypeLooksSuspicious 判断上游 streaming 响应的 Content-Type 是否
// 像「不是真的 SSE」——典型场景：上游网关把 HTML 错误页/JSON 错误吐成 200，
// Content-Type 不是 text/event-stream。返回 true 时调用方应抽样 body 决定是否失败。
//
// 故意保守：缺失 Content-Type 也视为可疑（合法 SSE 上游一定会显式带 Content-Type:
// text/event-stream），但仅在 isInfraLevelUnexpectedSuccessResponse 命中时才真正
// failover，避免误伤少数返回非标准 SSE 头的上游。
func streamContentTypeLooksSuspicious(h http.Header) bool {
	ct := strings.ToLower(strings.TrimSpace(h.Get("Content-Type")))
	if ct == "" {
		return true
	}
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	return ct != "text/event-stream"
}

// failoverOnInfraLevel2xx 在检测到上游 2xx + HTML/空 body 等"假成功"响应时调用：
//   - 写入 ops 事件，便于运维面板看到
//   - 触发 handleFailoverSideEffects（quota/标志位调整等）
//   - 临时下线该账号（指数退避），防止流量持续命中
//   - 返回 UpstreamFailoverError，让 handler 层切换到下一个账号
//
// 调用方必须确保此时还没有向客户端写过任何字节，否则 handler 层会因 streamStarted
// 而走 handleFailoverExhausted，无法切换账号。
func (s *GatewayService) failoverOnInfraLevel2xx(
	ctx context.Context,
	c *gin.Context,
	account *Account,
	resp *http.Response,
	body []byte,
	passthrough bool,
	logPrefix string,
) *UpstreamFailoverError {
	logger.LegacyPrintf("service.gateway",
		"%s Upstream invalid 2xx (HTML/empty body, failing over): Account=%d(%s) Status=%d RequestID=%s CT=%q BodyHead=%s",
		logPrefix, account.ID, account.Name, resp.StatusCode,
		resp.Header.Get("x-request-id"),
		resp.Header.Get("Content-Type"),
		truncateString(string(body), 200),
	)
	appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
		Platform:             account.Platform,
		AccountID:            account.ID,
		AccountName:          account.Name,
		Passthrough:          passthrough,
		UpstreamStatusCode:   resp.StatusCode,
		UpstreamRequestID:    resp.Header.Get("x-request-id"),
		UpstreamResponseBody: truncateString(string(body), 512),
		Kind:                 "failover_infra_2xx",
		Message:              "upstream returned HTML/empty body with 2xx",
		Detail:               truncateString(string(body), 512),
	})
	s.handleFailoverSideEffects(ctx, resp, account)
	if s.accountRepo != nil {
		tempUnscheduleGoogleConfigError(ctx, s.rateLimitService, s.accountRepo, account.ID, "[infra-2xx]")
	}
	// 空 body/真正的 HTML 标记：内容对错误透传规则匹配没有价值，合成一条通用提示。
	// 仅因 Content-Type 可疑（例如 text/plain 但内容其实是纯文本错误描述）触发时，
	// 保留原始上游响应体/响应头——与 invalidNonStreamingJSONFailoverError 的行为
	// 保持一致，ResponseBody 用于错误透传规则匹配（temp_unschedulable_rules 等），
	// 合成消息会让基于关键字/内容的匹配规则失效。
	if looksLikeHTMLOrEmptyBody(body) {
		return &UpstreamFailoverError{
			StatusCode:   http.StatusBadGateway,
			ResponseBody: []byte(`{"type":"error","error":{"type":"upstream_invalid_response","message":"upstream returned HTML/empty body with HTTP 200"}}`),
		}
	}
	return &UpstreamFailoverError{
		StatusCode:      http.StatusBadGateway,
		ResponseBody:    body,
		ResponseHeaders: resp.Header,
	}
}

// looksLikeHTMLOrEmptyBody 判断 body 本身是否为空或带有真实 HTML 标记
// （不依赖 Content-Type，因为 Content-Type 只是 isInfraLevelUnexpectedSuccessResponse
// 判定"疑似基础设施故障"的信号之一，body 本身不一定真的是 HTML）。
func looksLikeHTMLOrEmptyBody(body []byte) bool {
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return true
	}
	if trimmed[0] != '<' {
		return false
	}
	head := bytes.ToLower(trimmed)
	if len(head) > 64 {
		head = head[:64]
	}
	return bytes.HasPrefix(head, []byte("<!doctype")) ||
		bytes.HasPrefix(head, []byte("<html")) ||
		bytes.Contains(head, []byte("<head")) ||
		bytes.Contains(head, []byte("<body"))
}

// isQuotaExhaustedOn400 检查 400 响应是否为配额/积分耗尽错误。
// 用于在构造 UpstreamFailoverError 时设置 QuotaExhausted 标志。
func isQuotaExhaustedOn400(respBody []byte) bool {
	msg := strings.TrimSpace(extractUpstreamErrorMessage(respBody))
	return IsQuotaExhaustedMessage(msg)
}

// sanitizeStreamError 返回不含网络地址的客户端可见错误描述。
// 默认 (*net.OpError).Error() 会拼接 Source/Addr 字段，泄露内部 IP/端口与上游
// 服务器地址（例如 "read tcp 10.0.0.1:54321->52.1.2.3:443: read: connection
// reset by peer"）。该函数只保留可识别的错误类别，原始 err 仍在调用点写入日志。

// ExtractUpstreamErrorMessage 从上游响应体中提取错误消息
// 支持 Claude 风格的错误格式：{"type":"error","error":{"type":"...","message":"..."}}

// isLikelyJSONContent 判断上游 body 是否可以安全地以 application/json 回吐。
// 优先看 Content-Type，否则做一次轻量的 JSON 探嗅（首字符 + json.Valid）。
func isLikelyJSONContent(contentType string, body []byte) bool {
	ct := strings.ToLower(strings.TrimSpace(contentType))
	if i := strings.IndexByte(ct, ';'); i >= 0 {
		ct = strings.TrimSpace(ct[:i])
	}
	if ct == "application/json" || strings.HasSuffix(ct, "+json") {
		return true
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 {
		return false
	}
	switch trimmed[0] {
	case '{', '[':
	default:
		return false
	}
	return json.Valid(trimmed)
}

// handleRetryExhaustedError 处理重试耗尽后的错误
// OAuth 403：标记账号异常
// API Key 未配置错误码：仅返回错误，不标记账号

// streamingResult 流式响应结果

// applyCacheTTLOverride 将所有 cache creation tokens 归入指定的 TTL 类型。
// target 为 "5m" 或 "1h"。返回 true 表示发生了变更。

// rewriteCacheCreationJSON 在 JSON usage 对象中重写 cache_creation 嵌套对象的 TTL 分类。
// usageObj 是 usage JSON 对象（map[string]any）。

// applyCacheTTLOverrideToSSEBytes 直接在 SSE 事件 JSON 字节上应用 cache_creation 5m/1h
// 归并。用于 Bedrock 等保持字节流而不解构为 map 的处理路径——避免一次 unmarshal/marshal
// 往返。语义与 [rewriteCacheCreationJSON] 保持一致：将 5m+1h 全部归到目标桶，另一桶清零。
//
// 仅在 eventType ∈ {message_start, message_delta} 时处理；其它事件直接返回原 data。
func applyCacheTTLOverrideToSSEBytes(data []byte, eventType string, target string) []byte {
	var path string
	switch eventType {
	case "message_start":
		path = "message.usage"
	case "message_delta":
		path = "usage"
	default:
		return data
	}
	cc5m := gjson.GetBytes(data, path+".cache_creation.ephemeral_5m_input_tokens")
	cc1h := gjson.GetBytes(data, path+".cache_creation.ephemeral_1h_input_tokens")
	if !cc5m.Exists() && !cc1h.Exists() {
		return data
	}
	total := cc5m.Int() + cc1h.Int()
	if total == 0 {
		return data
	}
	switch target {
	case "1h":
		if cc1h.Int() == total {
			return data
		}
		if newData, err := sjson.SetBytes(data, path+".cache_creation.ephemeral_1h_input_tokens", total); err == nil {
			data = newData
		}
		if newData, err := sjson.SetBytes(data, path+".cache_creation.ephemeral_5m_input_tokens", 0); err == nil {
			data = newData
		}
	default: // "5m"
		if cc5m.Int() == total {
			return data
		}
		if newData, err := sjson.SetBytes(data, path+".cache_creation.ephemeral_5m_input_tokens", total); err == nil {
			data = newData
		}
		if newData, err := sjson.SetBytes(data, path+".cache_creation.ephemeral_1h_input_tokens", 0); err == nil {
			data = newData
		}
	}
	return data
}

// resolveCacheTTLUsageOverride 决定本次请求的 usage 归类目标 TTL（5m/1h）。
// 优先级链（高 → 低）：
//  1. 用户级偏好（APIKey.CacheStrategy 非 auto）
//  2. 账号级覆盖（Account.IsCacheTTLOverrideEnabled，D2 后已扩到所有支持平台）
//  3. 全局 5m 注入（仅 Anthropic OAuth/SetupToken；保留与 B 安全注入路径的耦合）
//  4. 否则：不归类
//
// 返回的 trace 字符串写入 usage_log.cache_policy_trace（A4 的可审计支柱）。
// 注意：该函数只控制"usage 归类"。请求体 cache_control TTL 改写见
// [resolveCacheTTLRequestRewriteTarget]——两者必须可独立演化以防"上游建 1h
// 缓存但按 5m 给客户计费"这类合规问题。
func (s *GatewayService) resolveCacheTTLUsageOverride(ctx context.Context, account *Account, apiKey *APIKey) (target string, ok bool, trace string) {
	if account == nil {
		return "", false, "skip:no_account"
	}
	if apiKey != nil {
		if t, applied := apiKey.CacheStrategyTTLTarget(); applied {
			// 用户偏好仅在账号支持缓存策略时才生效；否则保持 trace 显式记录"被忽略"
			// 以便运营排查"用户选了但没生效"的反馈。
			if account.SupportsCachePolicy() {
				return t, true, "user_pref:" + t
			}
			return "", false, "user_pref_ignored:account_unsupported"
		}
	}
	if account.IsCacheTTLOverrideEnabled() {
		t := account.GetCacheTTLOverrideTarget()
		return t, true, "acct_override:" + t
	}
	if account.IsAnthropicOAuthOrSetupToken() {
		if s != nil && s.settingService != nil && s.settingService.IsAnthropicCacheTTL1hInjectionEnabled(ctx) {
			return cacheTTLTarget5m, true, "global_inject:5m"
		}
		return "", false, "eligible:no_override"
	}
	return "", false, "skip:not_supported"
}

// resolveCacheTTLRequestRewriteTarget 决定请求体 cache_control TTL 的强制目标。
// 当前与 usage 归类共用同一优先级链；保留为独立函数以便未来在需要时分化
// （例如：允许"upstream 用 1h，billing 按 5m"被显式拒绝、或允许"upstream 用 5m
// 但 usage 计 1h"被显式允许）。任何分化都应当在 [resolveCacheTTLUsageOverride]
// 与本函数之间引入显式断言以保护合规。
//
// 调用者（如 B 的 addMessageCacheBreakpointsSafe）应据此决定注入的断点 TTL；
// 若返回 ok=false，应使用其历史默认（通常 5m）。
func (s *GatewayService) resolveCacheTTLRequestRewriteTarget(ctx context.Context, account *Account, apiKey *APIKey) (string, bool) {
	target, ok, _ := s.resolveCacheTTLUsageOverride(ctx, account, apiKey)
	return target, ok
}

// apiKeyFromGinContext 从 gin.Context 中取出 *APIKey。
// 使用字符串常量 "api_key"（与 middleware.ContextKeyAPIKey 同值）避免循环导入。
func apiKeyFromGinContext(c *gin.Context) *APIKey {
	if c == nil {
		return nil
	}
	v, ok := c.Get("api_key")
	if !ok {
		return nil
	}
	k, _ := v.(*APIKey)
	return k
}

// replaceModelInResponseBody 替换响应体中的model字段
// 使用 gjson/sjson 精确替换，避免全量 JSON 反序列化

// RecordUsageInput 记录使用量的输入参数。
// 异步 worker 只接收计费所需快照，不能持有 ParsedRequest/RequestBodyRef 这类大请求体引用。

// APIKeyQuotaUpdater defines the interface for updating API Key quota and rate limit usage

// postUsageBillingParams 统一扣费所需的参数

// PlatformFromAPIKey 从 APIKey 关联的 Group 推导 platform 名称。
// apiKey 为 nil 或 Group 信息缺失时返回空串（调用方据此 short-circuit quota 累加）。
// 导出供 handler 层调用。

// QuotaPlatform 返回 user×platform 配额计量使用的平台标识。
// 强制平台路由（如 /antigravity）优先按 ctx 中的 ForcePlatform 计量，否则回退到
// APIKey 关联 Group 的平台。
//
// 注意：必须用带 ForcePlatform 的请求 context 调用（如 handler 的 c.Request.Context()）。
// 后扣运行在 worker 池的 background ctx 上没有 ForcePlatform，因此后扣平台由 handler
// 预先算定、经 RecordUsageInput.QuotaPlatform 传入，不要在后扣链路用 worker ctx 调用本函数。

// postUsageBilling is the legacy fallback billing path used when the unified
// billing repo is unavailable (nil). Production uses applyUsageBilling → repo.Apply
// for atomic billing. This path only runs in tests or degraded mode.

// notifyBalanceLow sends balance low notification after deduction.
// When result.NewBalance is available (from DB transaction RETURNING), it is used directly
// to reconstruct oldBalance, avoiding stale Redis reads and concurrent-deduction races.

// resolveOldBalance returns the pre-deduction balance.
// Prefers the DB transaction result (newBalance + cost) over snapshot.

// notifyAccountQuota sends account quota threshold notification after increment.
// When result.QuotaState is available (from DB transaction RETURNING), it is passed directly
// to avoid a separate DB read that may see stale or concurrently-modified data.

// billingDeps 扣费逻辑依赖的服务（由各 gateway service 提供）

// recordUsageOpts 内部选项，参数化普通计费与长上下文计费的差异点。

// RecordUsage 记录使用量并扣费（或更新订阅用量）

// RecordUsageLongContextInput 记录使用量的输入参数（支持长上下文双倍计费）

// RecordUsageWithLongContext 记录使用量并扣费，支持长上下文双倍计费（用于 Gemini）

// recordUsageCoreInput 是 recordUsageCore 的公共输入字段，从两种输入结构体中提取。

// recordUsageCore 是 RecordUsage 和 RecordUsageWithLongContext 的统一实现。
// LongContextThreshold > 0 时 Token 计费回退走 CalculateCostWithLongContext。

// calculateRecordUsageCost 根据请求类型和选项计算费用。

// resolveChannelPricing 检查指定模型是否存在渠道级别定价。
// 返回非 nil 的 ResolvedPricing 表示有渠道定价，nil 表示走默认定价路径。

// calculateImageCost 计算图片生成费用：渠道级别定价优先，否则走按次计费。

// calculateTokenCost 计算 Token 计费：根据 opts 决定走普通/长上下文/渠道统一计费。

// buildRecordUsageLog 构建使用日志并设置计费模式。

// resolveBillingMode 根据计费结果和请求类型确定计费模式。

// ResolveChannelMapping 委托渠道服务解析模型映射

// ReplaceModelInBody 替换请求体中的模型名（导出供 handler 使用）

// IsModelRestricted 检查模型是否被渠道限制

// ResolveChannelMappingAndRestrict 解析渠道映射。
// 模型限制检查已移至调度阶段（checkChannelPricingRestriction），restricted 始终返回 false。

// checkChannelPricingRestriction 根据渠道计费基准检查模型是否受定价列表限制。
// 供调度阶段预检查（requested / channel_mapped）。
// upstream 需逐账号检查，此处返回 false。

// billingModelForRestriction 根据计费基准确定限制检查使用的模型。
// upstream 返回空（需逐账号检查）。

// isUpstreamModelRestrictedByChannel 检查账号映射后的上游模型是否受渠道定价限制。
// 仅在 BillingModelSource="upstream" 且 RestrictModels=true 时由调度循环调用。

// resolveAccountUpstreamModel 确定账号将请求模型映射为什么上游模型。

// needsUpstreamChannelRestrictionCheck 判断是否需要在调度循环中逐账号检查上游模型的渠道限制。

// isStickyAccountUpstreamRestricted 检查粘性会话命中的账号是否受 upstream 渠道限制。
// 合并 needsUpstreamChannelRestrictionCheck + isUpstreamModelRestrictedByChannel 两步调用，
// 供 sticky session 条件链使用，避免内联多个函数调用导致行过长。

// ForwardCountTokens 转发 count_tokens 请求到上游 API
// 特点：不记录使用量、仅支持非流式响应

// buildCountTokensRequest 构建 count_tokens 上游请求

// countTokensError 返回 count_tokens 错误响应

// buildCustomRelayURL 构建自定义中继转发 URL
// 在 path 后附加 ?beta=true（默认，pre-v0.1.166 行为）以及可选的 proxy 查询参数。
// 账号显式 opt-in extra.upstream_passthrough.bedrock_backed_relay 时跳过 beta=true。

// GetAvailableModels returns the list of models available for a group
// It aggregates model_mapping keys from all schedulable accounts in the group

// DoGrokNativeResponsesJSON POSTs a non-streaming Responses body to the account's
// Grok upstream and returns the raw JSON body. Used by /v1/web_search.
// Gin-free: UA is always the pinned Grok CLI identity (resolveGrokUpstreamUserAgent ignores inbound).
func (s *GatewayService) DoGrokNativeResponsesJSON(ctx context.Context, account *Account, body []byte) ([]byte, error) {
	if s == nil || s.httpUpstream == nil {
		return nil, errors.New("http upstream not configured")
	}
	if account == nil {
		return nil, errors.New("account is required")
	}
	if !account.IsGrok() {
		return nil, errors.New("grok account required")
	}
	token, _, err := s.GetAccessToken(ctx, account)
	if err != nil {
		// Credential/token failures should try the next Grok account in the pool.
		return nil, &UpstreamFailoverError{
			StatusCode: http.StatusUnauthorized,
			Reason:     GatewayFailureReason("grok_search_token"),
		}
	}
	targetURL, err := buildGrokResponsesURL(account, nil, s.settingService)
	if err != nil {
		return nil, err
	}
	if json.Valid(body) {
		if model := strings.TrimSpace(gjson.GetBytes(body, "model").String()); model == "" {
			if patched, patchErr := sjson.SetBytes(body, "model", xai.DefaultTextModel); patchErr == nil {
				body = patched
			}
		}
	}
	upstreamReq, err := http.NewRequestWithContext(ctx, http.MethodPost, targetURL, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("build grok responses request: %w", err)
	}
	upstreamReq.Header.Set("Authorization", "Bearer "+token)
	upstreamReq.Header.Set("Content-Type", "application/json")
	upstreamReq.Header.Set("Accept", "application/json")
	upstreamReq.Header.Set("User-Agent", defaultGrokUpstreamUserAgent())
	applyGrokCLIHeaders(upstreamReq.Header)
	account.ApplyHeaderOverrides(upstreamReq.Header)
	proxyURL := ""
	if account.ProxyID != nil && account.Proxy != nil {
		proxyURL = account.Proxy.URL()
	}
	resp, err := s.httpUpstream.Do(upstreamReq, proxyURL, account.ID, account.Concurrency)
	if err != nil {
		return nil, &UpstreamFailoverError{StatusCode: http.StatusBadGateway, Reason: GatewayFailureReason("grok_search_transport")}
	}
	defer func() { _ = resp.Body.Close() }()
	respBytes, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if readErr != nil {
		return nil, &UpstreamFailoverError{
			StatusCode: http.StatusBadGateway,
			Reason:     GatewayFailureReason("grok_search_read"),
		}
	}
	if resp.StatusCode >= 400 {
		msg := string(respBytes)
		if len(msg) > 200 {
			msg = msg[:200]
		}
		if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusPaymentRequired || resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 {
			return nil, &UpstreamFailoverError{StatusCode: resp.StatusCode, ResponseBody: respBytes}
		}
		return nil, fmt.Errorf("grok upstream %d: %s", resp.StatusCode, msg)
	}
	return respBytes, nil
}

func (s *GatewayService) GetAvailableModels(ctx context.Context, groupID *int64, platform string) []string {
	cacheKey := modelsListCacheKey(groupID, platform)
	if s.modelsListCache != nil {
		if cached, found := s.modelsListCache.Get(cacheKey); found {
			if models, ok := cached.([]string); ok {
				modelsListCacheHitTotal.Add(1)
				return cloneStringSlice(models)
			}
		}
	}
	modelsListCacheMissTotal.Add(1)

	var accounts []Account
	var err error

	if groupID != nil {
		accounts, err = s.accountRepo.ListSchedulableByGroupID(ctx, *groupID)
	} else {
		accounts, err = s.accountRepo.ListSchedulable(ctx)
	}

	if err != nil || len(accounts) == 0 {
		return nil
	}

	// Filter by platform if specified
	if platform != "" {
		filtered := make([]Account, 0)
		for _, acc := range accounts {
			if acc.Platform == platform {
				filtered = append(filtered, acc)
			}
		}
		accounts = filtered
	}

	// Collect unique models from all accounts
	modelSet := make(map[string]struct{})
	hasAnyMapping := false

	for _, acc := range accounts {
		// Passthrough routing accepts models independently of model_mapping. A stale
		// mapping on any eligible passthrough account therefore cannot define the
		// public whitelist; return nil so the handler uses its default model set.
		if platform == PlatformOpenAI && acc.IsOpenAIPassthroughEnabled() {
			if s.modelsListCache != nil {
				s.modelsListCache.Set(cacheKey, []string(nil), s.modelsListCacheTTL)
				modelsListCacheStoreTotal.Add(1)
			}
			return nil
		}

		mapping := acc.GetModelMapping()
		if len(mapping) > 0 {
			hasAnyMapping = true
			for model := range mapping {
				modelSet[model] = struct{}{}
			}
		}
	}

	// If no account has model_mapping, return nil (use default)
	if !hasAnyMapping {
		if s.modelsListCache != nil {
			s.modelsListCache.Set(cacheKey, []string(nil), s.modelsListCacheTTL)
			modelsListCacheStoreTotal.Add(1)
		}
		return nil
	}

	// Convert to slice
	models := make([]string, 0, len(modelSet))
	for model := range modelSet {
		models = append(models, model)
	}
	sort.Strings(models)

	if s.modelsListCache != nil {
		s.modelsListCache.Set(cacheKey, cloneStringSlice(models), s.modelsListCacheTTL)
		modelsListCacheStoreTotal.Add(1)
	}
	return cloneStringSlice(models)
}

func (s *GatewayService) resolveCompositeModelOwnership(ctx context.Context, groupID int64, model string) (CompositeModelOwnership, error) {
	model = strings.TrimSpace(model)
	if s == nil || s.accountRepo == nil || groupID <= 0 || model == "" {
		return CompositeModelOwnership{}, nil
	}

	cacheKey := compositeModelOwnershipCacheKey(groupID, model)
	if s.modelsListCache != nil {
		if cached, found := s.modelsListCache.Get(cacheKey); found {
			if ownership, ok := cached.(CompositeModelOwnership); ok {
				return ownership, nil
			}
		}
	}

	accounts, err := s.accountRepo.ListSchedulableByGroupID(ctx, groupID)
	if err != nil {
		return CompositeModelOwnership{}, err
	}

	platforms := make(map[string]struct{})
	for _, account := range accounts {
		platform := strings.TrimSpace(account.Platform)
		if !isConcreteRequestPlatform(platform) || !explicitModelMappingClaims(account, model) {
			continue
		}
		platforms[platform] = struct{}{}
	}

	ownership := CompositeModelOwnership{}
	if len(platforms) == 1 {
		for platform := range platforms {
			ownership.TargetPlatform = platform
		}
		ownership.Matched = true
	} else if len(platforms) > 1 {
		ownership.Ambiguous = true
	}

	if s.modelsListCache != nil {
		s.modelsListCache.Set(cacheKey, ownership, s.modelsListCacheTTL)
	}
	return ownership, nil
}

func explicitModelMappingClaims(account Account, model string) bool {
	if account.Credentials == nil || model == "" {
		return false
	}
	mapped, ok := stringMappingFromRaw(account.Credentials["model_mapping"])[model]
	return ok && strings.TrimSpace(mapped) != ""
}

// GetSchedulablePlatforms returns the concrete platforms that currently have
// schedulable accounts in the target group.
func (s *GatewayService) GetSchedulablePlatforms(ctx context.Context, groupID *int64) map[string]struct{} {
	platforms := make(map[string]struct{})
	if s == nil || s.accountRepo == nil {
		return platforms
	}

	var accounts []Account
	var err error
	if groupID != nil {
		accounts, err = s.accountRepo.ListSchedulableByGroupID(ctx, *groupID)
	} else {
		accounts, err = s.accountRepo.ListSchedulable(ctx)
	}
	if err != nil {
		return platforms
	}

	for _, acc := range accounts {
		platform := strings.TrimSpace(acc.Platform)
		if platform != "" {
			platforms[platform] = struct{}{}
		}
	}
	return platforms
}

func (s *GatewayService) InvalidateAvailableModelsCache(groupID *int64, platform string) {
	if s == nil || s.modelsListCache == nil {
		return
	}
	s.invalidateCompositeModelOwnershipCache(groupID)

	normalizedPlatform := strings.TrimSpace(platform)
	// 完整匹配时精准失效；否则按维度批量失效。
	if groupID != nil && normalizedPlatform != "" {
		s.modelsListCache.Delete(modelsListCacheKey(groupID, normalizedPlatform))
		return
	}

	targetGroup := derefGroupID(groupID)
	for key := range s.modelsListCache.Items() {
		parts := strings.SplitN(key, "|", 2)
		if len(parts) != 2 {
			continue
		}
		groupPart, parseErr := strconv.ParseInt(parts[0], 10, 64)
		if parseErr != nil {
			continue
		}
		if groupID != nil && groupPart != targetGroup {
			continue
		}
		if normalizedPlatform != "" && parts[1] != normalizedPlatform {
			continue
		}
		s.modelsListCache.Delete(key)
	}
}

// 注：reconcileCachedTokens 的实现与文档注释都在 gateway_upstream_response.go；
// 本文件此处历史遗留的孤立注释已随本次合并删除。

func (s *GatewayService) invalidateCompositeModelOwnershipCache(groupID *int64) {
	for key := range s.modelsListCache.Items() {
		if !strings.HasPrefix(key, compositeModelOwnershipCachePrefix) {
			continue
		}
		if groupID == nil {
			s.modelsListCache.Delete(key)
			continue
		}
		parts := strings.SplitN(strings.TrimPrefix(key, compositeModelOwnershipCachePrefix), "|", 2)
		if len(parts) != 2 {
			continue
		}
		cachedGroupID, err := strconv.ParseInt(parts[0], 10, 64)
		if err == nil && cachedGroupID == *groupID {
			s.modelsListCache.Delete(key)
		}
	}
}

const debugGatewayBodyDefaultFilename = "gateway_debug.log"

// initDebugGatewayBodyFile 初始化网关调试日志文件。
//
//   - "1"/"true" 等布尔值 → 当前目录下 gateway_debug.log
//   - 已有目录路径        → 该目录下 gateway_debug.log
//   - 其他               → 视为完整文件路径
func (s *GatewayService) initDebugGatewayBodyFile(path string) {
	if parseDebugEnvBool(path) {
		path = debugGatewayBodyDefaultFilename
	}

	// 如果 path 指向一个已存在的目录，自动追加默认文件名
	if info, err := os.Stat(path); err == nil && info.IsDir() { //nolint:gosec // G703: path 仅来自启动环境变量 SUB2API_DEBUG_GATEWAY_BODY（运维配置），非请求输入
		path = filepath.Join(path, debugGatewayBodyDefaultFilename)
	}

	// 确保父目录存在
	if dir := filepath.Dir(path); dir != "." {
		if err := os.MkdirAll(dir, 0755); err != nil { //nolint:gosec // G703: 同上
			slog.Error("failed to create gateway debug log directory", "dir", dir, "error", err)
			return
		}
	}

	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0644) //nolint:gosec // G703: 同上
	if err != nil {
		slog.Error("failed to open gateway debug log file", "path", path, "error", err)
		return
	}
	s.debugGatewayBodyFile.Store(f)
	slog.Info("gateway debug logging enabled", "path", path)
}

// debugLogGatewaySnapshot 将网关请求的完整快照（headers + body）写入独立的调试日志文件，
// 用于对比客户端原始请求和上游转发请求。
//
// 启用方式（环境变量）：
//
//	SUB2API_DEBUG_GATEWAY_BODY=1                          # 写入 gateway_debug.log
//	SUB2API_DEBUG_GATEWAY_BODY=/tmp/gateway_debug.log     # 写入指定路径
//
// tag: "CLIENT_ORIGINAL" 或 "UPSTREAM_FORWARD"
func (s *GatewayService) debugLogGatewaySnapshot(tag string, headers http.Header, body []byte, extra map[string]string) {
	f := s.debugGatewayBodyFile.Load()
	if f == nil {
		return
	}

	var buf strings.Builder
	ts := time.Now().Format("2006-01-02 15:04:05.000")
	fmt.Fprintf(&buf, "\n========== [%s] %s ==========\n", ts, tag)

	// 1. context
	if len(extra) > 0 {
		fmt.Fprint(&buf, "--- context ---\n")
		extraKeys := make([]string, 0, len(extra))
		for k := range extra {
			extraKeys = append(extraKeys, k)
		}
		sort.Strings(extraKeys)
		for _, k := range extraKeys {
			fmt.Fprintf(&buf, "  %s: %s\n", k, extra[k])
		}
	}

	// 2. headers（按真实 Claude CLI wire 顺序排列，便于与抓包对比；auth 脱敏）
	fmt.Fprint(&buf, "--- headers ---\n")
	for _, k := range sortHeadersByWireOrder(headers) {
		for _, v := range headers[k] {
			fmt.Fprintf(&buf, "  %s: %s\n", k, safeHeaderValueForLog(k, v))
		}
	}

	// 3. body（完整输出，格式化 JSON 便于 diff）
	fmt.Fprint(&buf, "--- body ---\n")
	if len(body) == 0 {
		fmt.Fprint(&buf, "  (empty)\n")
	} else {
		var pretty bytes.Buffer
		if json.Indent(&pretty, body, "  ", "  ") == nil {
			fmt.Fprintf(&buf, "  %s\n", pretty.Bytes())
		} else {
			// JSON 格式化失败时原样输出
			fmt.Fprintf(&buf, "  %s\n", body)
		}
	}

	// 写入文件（调试用，并发写入可能交错但不影响可读性）
	_, _ = f.WriteString(buf.String())
}
