package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math/rand"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/pkg/ip"
	"github.com/Wei-Shaw/sub2api/internal/pkg/logger"
	"github.com/Wei-Shaw/sub2api/internal/pkg/openai"
	"github.com/Wei-Shaw/sub2api/internal/platform/liveattestation"
	"github.com/Wei-Shaw/sub2api/internal/util/responseheaders"
	"github.com/cespare/xxhash/v2"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const (
	// ChatGPT internal API for OAuth accounts
	chatgptCodexURL = "https://chatgpt.com/backend-api/codex/responses"
	// OpenAI Platform API for API Key accounts (fallback)
	openaiPlatformAPIURL            = "https://api.openai.com/v1/responses"
	openaiPlatformAPIInputTokensURL = "https://api.openai.com/v1/responses/input_tokens"
	openaiStickySessionTTL          = time.Hour // 粘性会话TTL
	// 与真实 Codex TUI 的 User-Agent 结构对齐：
	// {originator}/{version} ({OS} {OS_version}; {arch}) {terminal}
	// 缺少 OS/架构/终端后缀的形态易被上游指纹识别为非官方客户端。
	// 该后缀是 UA 形态的唯一定义处，buildCodexCLIUserAgent 按运行时版本号复用它。
	codexCLIUserAgentSuffix = " (Ubuntu 22.4.0; x86_64) xterm-256color"
	// codexCLIUserAgent 是编译期兜底 UA；运行时优先使用由后台版本号拼出的规范 UA。
	// 版本段必须来自 codexCLIVersion：UA 与 version 头是同一个版本声明的两个出口，
	// 各自硬编码会漂移成互相矛盾的身份。
	codexCLIUserAgent = openai.CodexDefaultOriginator + "/" + codexCLIVersion + codexCLIUserAgentSuffix
	// codex_cli_only 拒绝时单个请求头日志长度上限（字符）
	codexCLIOnlyHeaderValueMaxBytes = 256

	// OpenAI WS Mode 失败后的重连次数上限（不含首次尝试）。
	// 与 Codex 客户端保持一致：失败后最多重连 5 次。
	openAIWSReconnectRetryLimit = 5
	// 上游错误体只需要提取错误 JSON/日志摘要，默认 512KiB 避免错误风暴叠加大请求体。
	openAIUpstreamErrorBodyReadLimit int64 = 512 << 10
	// OpenAI WS Mode 重连退避默认值（可由配置覆盖）。
	openAIWSRetryBackoffInitialDefault = 120 * time.Millisecond
	openAIWSRetryBackoffMaxDefault     = 2 * time.Second
	openAIWSRetryJitterRatioDefault    = 0.2
	openAICompactSessionSeedKey        = "openai_compact_session_seed"
	openAIUpstreamEndpointContextKey   = "openai_actual_upstream_endpoint"
	// codexCLIVersion 是网关对上游声明的 Codex 客户端版本，同时供 codexCLIUserAgent
	// 与 version 头使用。上游 /backend-api/codex 在容量紧张时按客户端身份分优先级降载，
	// 陈旧版本会被优先丢弃（HTTP 200 + 流内 server_is_overloaded）；非官方客户端配不出
	// 官方身份时整体回退到本常量，因此它必须跟随官方 CLI 的当前发布版本，
	// 落后多个版本会让这些请求稳定落在被优先丢弃的一侧。
	codexCLIVersion = "0.146.0"
	// Codex 限额快照仅用于后台展示/诊断，不需要每个成功请求都立即落库。
	openAICodexSnapshotPersistMinInterval = 30 * time.Second
	// 配额自动暂停时，超过该时长仍未刷新的 used% 快照视为陈旧，不再据此暂停账号。
	// 被暂停的账号收不到流量，其快照永远不会从上游响应头刷新；该兜底让账号在快照
	// 陈旧时放行一次请求，从而通过正常响应头自愈，而无需等待整个窗口（5h/7d）重置。
	openAICodexAutoPauseStaleAfter = 2 * time.Hour
)

// OpenAI allowed headers whitelist (for non-passthrough).
var openaiAllowedHeaders = map[string]bool{
	"accept-language":         true,
	"content-type":            true,
	"conversation_id":         true,
	"user-agent":              true,
	"originator":              true,
	"session_id":              true,
	"x-codex-beta-features":   true,
	"x-codex-installation-id": true,
	"x-codex-turn-state":      true,
	"x-codex-turn-metadata":   true,
	"x-codex-window-id":       true,
	responsesLiteHeaderKey:    true,
}

// OpenAI passthrough allowed headers whitelist.
// 透传模式下仅放行这些低风险请求头，避免将非标准/环境噪声头传给上游触发风控。
var openaiPassthroughAllowedHeaders = map[string]bool{
	"accept":                  true,
	"accept-language":         true,
	"content-type":            true,
	"conversation_id":         true,
	"openai-beta":             true,
	"user-agent":              true,
	"originator":              true,
	"session_id":              true,
	"x-codex-beta-features":   true,
	"x-codex-installation-id": true,
	"x-codex-turn-state":      true,
	"x-codex-turn-metadata":   true,
	"x-codex-window-id":       true,
	responsesLiteHeaderKey:    true,
}

// codex_cli_only 拒绝时记录的请求头白名单（仅用于诊断日志，不参与上游透传）
var codexCLIOnlyDebugHeaderWhitelist = []string{
	"User-Agent",
	"Content-Type",
	"Accept",
	"Accept-Language",
	"OpenAI-Beta",
	"Originator",
	"Session_ID",
	"Conversation_ID",
	"X-Request-ID",
	"X-Client-Request-ID",
	"X-Forwarded-For",
	"X-Real-IP",
}

// OpenAICodexUsageSnapshot represents Codex API usage limits from response headers
type OpenAICodexUsageSnapshot struct {
	PrimaryUsedPercent          *float64 `json:"primary_used_percent,omitempty"`
	PrimaryResetAfterSeconds    *int     `json:"primary_reset_after_seconds,omitempty"`
	PrimaryWindowMinutes        *int     `json:"primary_window_minutes,omitempty"`
	SecondaryUsedPercent        *float64 `json:"secondary_used_percent,omitempty"`
	SecondaryResetAfterSeconds  *int     `json:"secondary_reset_after_seconds,omitempty"`
	SecondaryWindowMinutes      *int     `json:"secondary_window_minutes,omitempty"`
	PrimaryOverSecondaryPercent *float64 `json:"primary_over_secondary_percent,omitempty"`
	UpdatedAt                   string   `json:"updated_at,omitempty"`
}

// NormalizedCodexLimits contains normalized 5h/7d rate limit data
type NormalizedCodexLimits struct {
	Used5hPercent   *float64
	Reset5hSeconds  *int
	Window5hMinutes *int
	Used7dPercent   *float64
	Reset7dSeconds  *int
	Window7dMinutes *int
}

// Normalize converts primary/secondary fields to canonical 5h/7d fields.
// Strategy: Compare window_minutes to determine which is 5h vs 7d.
// Returns nil if snapshot is nil or has no useful data.
func (s *OpenAICodexUsageSnapshot) Normalize() *NormalizedCodexLimits {
	if s == nil {
		return nil
	}

	result := &NormalizedCodexLimits{}

	primaryMins := 0
	secondaryMins := 0
	hasPrimaryWindow := false
	hasSecondaryWindow := false

	if s.PrimaryWindowMinutes != nil {
		primaryMins = *s.PrimaryWindowMinutes
		hasPrimaryWindow = true
	}
	if s.SecondaryWindowMinutes != nil {
		secondaryMins = *s.SecondaryWindowMinutes
		hasSecondaryWindow = true
	}

	// Determine mapping based on window_minutes
	use5hFromPrimary := false
	use7dFromPrimary := false

	if hasPrimaryWindow && hasSecondaryWindow {
		// Both known: smaller window is 5h, larger is 7d
		if primaryMins < secondaryMins {
			use5hFromPrimary = true
		} else {
			use7dFromPrimary = true
		}
	} else if hasPrimaryWindow {
		// Only primary known: classify by threshold (<=360 min = 6h -> 5h window)
		if primaryMins <= 360 {
			use5hFromPrimary = true
		} else {
			use7dFromPrimary = true
		}
	} else if hasSecondaryWindow {
		// Only secondary known: classify by threshold
		if secondaryMins <= 360 {
			// 5h from secondary, so primary (if any data) is 7d
			use7dFromPrimary = true
		} else {
			// 7d from secondary, so primary (if any data) is 5h
			use5hFromPrimary = true
		}
	} else {
		// No window_minutes: fall back to legacy assumption (primary=7d, secondary=5h)
		use7dFromPrimary = true
	}

	// Assign values
	if use5hFromPrimary {
		result.Used5hPercent = s.PrimaryUsedPercent
		result.Reset5hSeconds = s.PrimaryResetAfterSeconds
		result.Window5hMinutes = s.PrimaryWindowMinutes
		result.Used7dPercent = s.SecondaryUsedPercent
		result.Reset7dSeconds = s.SecondaryResetAfterSeconds
		result.Window7dMinutes = s.SecondaryWindowMinutes
	} else if use7dFromPrimary {
		result.Used7dPercent = s.PrimaryUsedPercent
		result.Reset7dSeconds = s.PrimaryResetAfterSeconds
		result.Window7dMinutes = s.PrimaryWindowMinutes
		result.Used5hPercent = s.SecondaryUsedPercent
		result.Reset5hSeconds = s.SecondaryResetAfterSeconds
		result.Window5hMinutes = s.SecondaryWindowMinutes
	}

	return result
}

// OpenAIUsage represents OpenAI API response usage
type OpenAIUsage struct {
	InputTokens              int `json:"input_tokens"`
	ImageInputTokens         int `json:"image_input_tokens,omitempty"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens,omitempty"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens,omitempty"`
	ImageOutputTokens        int `json:"image_output_tokens,omitempty"`
}

// OpenAIForwardResult represents the result of forwarding
type OpenAIForwardResult struct {
	RequestID  string
	ResponseID string
	// UpstreamHeaders 是直接上游的响应头，用于按账户配置解析上游请求标识。
	UpstreamHeaders http.Header
	Usage           OpenAIUsage
	Model           string // 原始模型（用于响应和日志显示）
	// BillingModel is the model used for cost calculation.
	// When non-empty, CalculateCost uses this instead of Model.
	// This is set by the Anthropic Messages conversion path where
	// the mapped upstream model differs from the client-facing model.
	BillingModel string
	// UpstreamModel is the actual model sent to the upstream provider after mapping.
	// Empty when no mapping was applied (requested model was used as-is).
	UpstreamModel string
	// UpstreamResponseModel is captured from the raw successful upstream
	// response before any client-facing rewrite or protocol conversion.
	UpstreamResponseModel         string
	UpstreamResponseModelConflict bool
	// UpstreamResponseServiceTier is the tier the upstream reports having used
	// (response service_tier: "priority" / "default" / "flex" / ...); "" when not declared.
	UpstreamResponseServiceTier string
	// UpstreamEndpoint is the actual upstream API path used for this request.
	// It avoids guessing when one downstream protocol can use multiple upstream endpoints.
	UpstreamEndpoint string
	// ServiceTier is the final tier sent upstream after policy rewriting.
	// The upstream response declaration remains separate above and is reconciled
	// at usage-recording time, where the credential protocol is available.
	ServiceTier *string
	// ReasoningEffort is extracted from request body (reasoning.effort) or derived from model suffix
	// after group policy rewriting and model-family remapping.
	// Stored for usage records display; nil means not provided / not applicable.
	ReasoningEffort *string
	// RequestedReasoningEffort is the client-requested effort before mapping.
	// Empty/nil means it should fall back to ReasoningEffort at persistence.
	RequestedReasoningEffort *string
	Stream                   bool
	OpenAIWSMode             bool
	// UpstreamTerminalEvent is the normalized terminal event observed on an
	// upstream Responses WebSocket turn. Empty preserves legacy/non-WS success.
	UpstreamTerminalEvent string
	ResponseHeaders       http.Header
	Duration              time.Duration
	FirstTokenMs          *int
	ClientDisconnect      bool
	ImageCount            int
	ImageSize             string
	ImageInputSize        string
	ImageOutputSize       string
	ImageOutputSizes      []string
	ImageSizeSource       string
	ImageSizeBreakdown    map[string]int
	VideoCount            int
	VideoResolution       string
	// VideoDurationSeconds 是提交时请求的生成时长（xAI 按输出秒数计费），已归一化到 1-15 秒。
	VideoDurationSeconds int
	// WebSearchCalls 是 Codex alpha/search 网页搜索调用次数（每次成功请求为 1）。
	// 上游不返回 usage 字段，>0 时走按次计费（分组单价 × 次数 × 倍率）。
	WebSearchCalls int
	// SearchCount is Grok-native web_search / tool search call count (per 1k pricing).
	SearchCount int
	// AudioUsage carries Voice billing units when present.
	AudioUsage *AudioUsage

	wsReplayInput                []json.RawMessage
	wsReplayInputExists          bool
	wsAccountFailoverReplayInput []json.RawMessage
}

// SucceededForScheduling reports whether this result is an upstream success
// that may clear model-scoped transient state. The zero value remains a success
// for existing non-WS callers.
func (r *OpenAIForwardResult) SucceededForScheduling() bool {
	if r == nil || !r.OpenAIWSMode || r.UpstreamTerminalEvent == "" {
		return true
	}
	switch r.UpstreamTerminalEvent {
	case "response.completed", "response.done":
		return true
	default:
		return false
	}
}

// SetActualOpenAIUpstreamEndpoint records the endpoint selected by the current
// forwarding attempt. It covers error paths where no OpenAIForwardResult is
// available for usage and operations logging.
func SetActualOpenAIUpstreamEndpoint(c *gin.Context, endpoint string) {
	if c == nil {
		return
	}
	if endpoint = strings.TrimSpace(endpoint); endpoint != "" {
		c.Set(openAIUpstreamEndpointContextKey, endpoint)
	}
}

// ClearActualOpenAIUpstreamEndpoint 清理当前转发尝试记录的端点。
// Handler 会在账号 failover 尝试间复用同一个 Gin context，因此每次尝试
// 都必须从无残留状态开始。
func ClearActualOpenAIUpstreamEndpoint(c *gin.Context) {
	if c == nil {
		return
	}
	c.Set(openAIUpstreamEndpointContextKey, "")
}

// GetActualOpenAIUpstreamEndpoint returns the endpoint recorded by the latest
// forwarding attempt in this request.
func GetActualOpenAIUpstreamEndpoint(c *gin.Context) string {
	if c == nil {
		return ""
	}
	value, exists := c.Get(openAIUpstreamEndpointContextKey)
	if !exists {
		return ""
	}
	endpoint, _ := value.(string)
	return strings.TrimSpace(endpoint)
}

type OpenAIWSRetryMetricsSnapshot struct {
	RetryAttemptsTotal            int64 `json:"retry_attempts_total"`
	RetryBackoffMsTotal           int64 `json:"retry_backoff_ms_total"`
	RetryExhaustedTotal           int64 `json:"retry_exhausted_total"`
	NonRetryableFastFallbackTotal int64 `json:"non_retryable_fast_fallback_total"`
}

type OpenAICompatibilityFallbackMetricsSnapshot struct {
	SessionHashLegacyReadFallbackTotal int64   `json:"session_hash_legacy_read_fallback_total"`
	SessionHashLegacyReadFallbackHit   int64   `json:"session_hash_legacy_read_fallback_hit"`
	SessionHashLegacyDualWriteTotal    int64   `json:"session_hash_legacy_dual_write_total"`
	SessionHashLegacyReadHitRate       float64 `json:"session_hash_legacy_read_hit_rate"`

	MetadataLegacyFallbackIsMaxTokensOneHaikuTotal int64 `json:"metadata_legacy_fallback_is_max_tokens_one_haiku_total"`
	MetadataLegacyFallbackThinkingEnabledTotal     int64 `json:"metadata_legacy_fallback_thinking_enabled_total"`
	MetadataLegacyFallbackPrefetchedStickyAccount  int64 `json:"metadata_legacy_fallback_prefetched_sticky_account_total"`
	MetadataLegacyFallbackPrefetchedStickyGroup    int64 `json:"metadata_legacy_fallback_prefetched_sticky_group_total"`
	MetadataLegacyFallbackSingleAccountRetryTotal  int64 `json:"metadata_legacy_fallback_single_account_retry_total"`
	MetadataLegacyFallbackAccountSwitchCountTotal  int64 `json:"metadata_legacy_fallback_account_switch_count_total"`
	MetadataLegacyFallbackTotal                    int64 `json:"metadata_legacy_fallback_total"`
}

type openAIWSRetryMetrics struct {
	retryAttempts            atomic.Int64
	retryBackoffMs           atomic.Int64
	retryExhausted           atomic.Int64
	nonRetryableFastFallback atomic.Int64
}

type accountWriteThrottle struct {
	minInterval time.Duration
	mu          sync.Mutex
	lastByID    map[int64]time.Time
}

func newAccountWriteThrottle(minInterval time.Duration) *accountWriteThrottle {
	return &accountWriteThrottle{
		minInterval: minInterval,
		lastByID:    make(map[int64]time.Time),
	}
}

func (t *accountWriteThrottle) Allow(id int64, now time.Time) bool {
	if t == nil || id <= 0 || t.minInterval <= 0 {
		return true
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if last, ok := t.lastByID[id]; ok && now.Sub(last) < t.minInterval {
		return false
	}
	t.lastByID[id] = now

	if len(t.lastByID) > 4096 {
		cutoff := now.Add(-4 * t.minInterval)
		for accountID, writtenAt := range t.lastByID {
			if writtenAt.Before(cutoff) {
				delete(t.lastByID, accountID)
			}
		}
	}

	return true
}

var defaultOpenAICodexSnapshotPersistThrottle = newAccountWriteThrottle(openAICodexSnapshotPersistMinInterval)

// ErrNoAvailableCompactAccounts indicates a legacy /responses/compact request
// needs compact support but no compatible account is available.
var ErrNoAvailableCompactAccounts = errors.New("no available accounts support /responses/compact")

// OpenAIGatewayService handles OpenAI API gateway operations
type OpenAIGatewayService struct {
	accountRepo           AccountRepository
	usageLogRepo          UsageLogRepository
	usageBillingRepo      UsageBillingRepository
	userRepo              UserRepository
	userSubRepo           UserSubscriptionRepository
	cache                 GatewayCache
	cfg                   *config.Config
	codexDetector         CodexClientRestrictionDetector
	schedulerSnapshot     *SchedulerSnapshotService
	concurrencyService    *ConcurrencyService
	billingService        *BillingService
	rateLimitService      *RateLimitService
	billingCacheService   *BillingCacheService
	userGroupRateResolver *userGroupRateResolver
	httpUpstream          HTTPUpstream
	pluginManager         *PluginManager
	deferredService       *DeferredService
	openAITokenProvider   *OpenAITokenProvider
	grokTokenProvider     *GrokTokenProvider
	toolCorrector         *CodexToolCorrector
	openaiWSResolver      OpenAIWSProtocolResolver
	resolver              *ModelPricingResolver
	channelService        *ChannelService
	balanceNotifyService  *BalanceNotifyService
	settingService        *SettingService
	userPlatformQuotaRepo UserPlatformQuotaRepository
	liveAttestation       liveattestation.Provider
	liveAttestationCipher SecretEncryptor

	openaiWSPoolOnce               sync.Once
	openaiWSStateStoreOnce         sync.Once
	openaiSchedulerOnce            sync.Once
	openaiProxyStreamCircuitOnce   sync.Once
	openaiWSPassthroughDialerOnce  sync.Once
	openaiModelTransientOnce       sync.Once
	agentIdentityTaskMu            sync.Mutex
	openaiWSPool                   *openAIWSConnPool
	openaiWSStateStore             OpenAIWSStateStore
	openaiScheduler                OpenAIAccountScheduler
	openaiWSPassthroughDialer      openAIWSClientDialer
	openaiWSSessionPreemptions     openAIWSSessionPreemptRegistry
	openaiAccountStats             *openAIAccountRuntimeStats
	openaiModelTransient           *openAIAccountModelTransientState
	openaiProxyStreamCircuit       *openAIProxyStreamCircuit
	openaiProxyStreamFailOpenLogAt atomic.Int64

	openaiWSFallbackUntil               sync.Map // key: int64(accountID), value: time.Time
	openaiAccountRuntimeBlockUntil      sync.Map // key: int64(accountID), value: time.Time
	openaiAccountRuntimeBlockLocks      sync.Map // key: int64(accountID), value: *sync.Mutex
	openaiAccountRuntimeBlockGeneration sync.Map // key: int64(accountID), value: uint64
	openaiAccountRuntimeBlockSequence   atomic.Uint64
	openaiOAuth429RetryStartedAt        sync.Map // key: int64(accountID), value: time.Time
	grokCredentialMutationLocks         sync.Map // key: int64(accountID), value: *sync.Mutex
	openaiOAuth429WindowStartUnixNano   atomic.Int64
	openaiOAuth429WindowCount           atomic.Int64
	openaiWSRetryMetrics                openAIWSRetryMetrics
	responseHeaderFilter                *responseheaders.CompiledHeaderFilter
	codexSnapshotThrottle               *accountWriteThrottle
	codexModelsManifestCache            codexModelsManifestCache
	openaiCompatSessionResponses        sync.Map
	openaiCompatAnthropicDigestSessions sync.Map
	// openaiCodexTurnStateOrigins: 下游会话 seed → openAICodexTurnStateOrigin，
	// 记录最近一次向该会话下发 x-codex-turn-state 的铸造账号，供出站守卫
	// 剥离跨账号回带（openai_codex_turn_state.go）。
	openaiCodexTurnStateOrigins sync.Map
	openaiCodexTurnStateWrites  atomic.Uint64

	// P0-3: (sub2api_user, platform) 维度伪指纹画像。可空。
	identityProfileService *IdentityProfileService
}

// NewOpenAIGatewayService creates a new OpenAIGatewayService
func NewOpenAIGatewayService(
	accountRepo AccountRepository,
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
	httpUpstream HTTPUpstream,
	deferredService *DeferredService,
	openAITokenProvider *OpenAITokenProvider,
	grokTokenProvider *GrokTokenProvider,
	resolver *ModelPricingResolver,
	channelService *ChannelService,
	balanceNotifyService *BalanceNotifyService,
	settingService *SettingService,
	identityProfileService *IdentityProfileService,
	userPlatformQuotaRepo UserPlatformQuotaRepository,
) *OpenAIGatewayService {
	// enforceCodexIdentityHeaders 是 HTTP / 透传 / WS / 探针 等出站路径共用的纯函数收口点，
	// 拿不到配置，故在此发布进程级开关快照。配置取反义，零值即「强制统一出口开启」。
	if cfg != nil {
		SetCodexIdentityEnforcementEnabled(!cfg.Gateway.DisableCodexIdentityEnforcement)
	}
	svc := &OpenAIGatewayService{
		accountRepo:         accountRepo,
		usageLogRepo:        usageLogRepo,
		usageBillingRepo:    usageBillingRepo,
		userRepo:            userRepo,
		userSubRepo:         userSubRepo,
		cache:               cache,
		cfg:                 cfg,
		codexDetector:       NewOpenAICodexClientRestrictionDetector(cfg),
		schedulerSnapshot:   schedulerSnapshot,
		concurrencyService:  concurrencyService,
		billingService:      billingService,
		rateLimitService:    rateLimitService,
		billingCacheService: billingCacheService,
		userGroupRateResolver: newUserGroupRateResolver(
			userGroupRateRepo,
			nil,
			resolveUserGroupRateCacheTTL(cfg),
			nil,
			"service.openai_gateway",
		),
		httpUpstream:           httpUpstream,
		deferredService:        deferredService,
		openAITokenProvider:    openAITokenProvider,
		grokTokenProvider:      grokTokenProvider,
		toolCorrector:          NewCodexToolCorrector(),
		openaiWSResolver:       NewOpenAIWSProtocolResolver(cfg),
		resolver:               resolver,
		channelService:         channelService,
		balanceNotifyService:   balanceNotifyService,
		settingService:         settingService,
		userPlatformQuotaRepo:  userPlatformQuotaRepo,
		liveAttestation:        liveattestation.NewProvider(),
		liveAttestationCipher:  newLiveAttestationCipher(cfg),
		responseHeaderFilter:   compileResponseHeaderFilter(cfg),
		codexSnapshotThrottle:  newAccountWriteThrottle(openAICodexSnapshotPersistMinInterval),
		identityProfileService: identityProfileService,
		openaiModelTransient:   newOpenAIAccountModelTransientState(openAIModelTransientDefaultMax),
	}
	if rateLimitService != nil {
		rateLimitService.SetAccountRuntimeBlocker(svc)
	}
	if openAITokenProvider != nil {
		openAITokenProvider.SetAccountRuntimeBlocker(svc)
	}
	svc.logOpenAIWSModeBootstrap()
	return svc
}

// ResolveChannelMapping 解析渠道级模型映射（代理到 ChannelService）
func (s *OpenAIGatewayService) ResolveChannelMapping(ctx context.Context, groupID int64, model string) ChannelMappingResult {
	if s.channelService == nil {
		return ChannelMappingResult{MappedModel: model}
	}
	return s.channelService.ResolveChannelMapping(ctx, groupID, model)
}

// IsModelRestricted 检查模型是否被渠道限制（代理到 ChannelService）
func (s *OpenAIGatewayService) IsModelRestricted(ctx context.Context, groupID int64, model string) bool {
	if s.channelService == nil {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, groupID, model)
}

// ResolveChannelMappingAndRestrict 解析渠道映射。
// 模型限制检查已移至调度阶段，restricted 始终返回 false。
func (s *OpenAIGatewayService) ResolveChannelMappingAndRestrict(ctx context.Context, groupID *int64, model string) (ChannelMappingResult, bool) {
	if s.channelService == nil {
		return ChannelMappingResult{MappedModel: model}, false
	}
	return s.channelService.ResolveChannelMappingAndRestrict(ctx, groupID, model)
}

func (s *OpenAIGatewayService) isCodexImageGenerationBridgeEnabled(ctx context.Context, account *Account, apiKey *APIKey) bool {
	if override := account.CodexImageGenerationBridgeOverride(); override != nil {
		return *override
	}
	if s != nil && s.channelService != nil && apiKey != nil && apiKey.GroupID != nil {
		ch, err := s.channelService.GetChannelForGroup(ctx, *apiKey.GroupID)
		if err != nil {
			slog.Warn("failed to resolve codex image generation bridge channel override", "group_id", *apiKey.GroupID, "error", err)
		} else if override := ch.CodexImageGenerationBridgeOverride(PlatformOpenAI); override != nil {
			return *override
		}
	}
	return s != nil && s.cfg != nil && s.cfg.Gateway.CodexImageGenerationBridgeEnabled
}

func (s *OpenAIGatewayService) checkChannelPricingRestriction(ctx context.Context, groupID *int64, requestedModel string) bool {
	if groupID == nil || s.channelService == nil || requestedModel == "" {
		return false
	}
	mapping := s.channelService.ResolveChannelMapping(ctx, *groupID, requestedModel)
	billingModel := billingModelForRestriction(mapping.BillingModelSource, requestedModel, mapping.MappedModel)
	if billingModel == "" {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, *groupID, billingModel)
}

func (s *OpenAIGatewayService) isUpstreamModelRestrictedByChannel(ctx context.Context, groupID int64, account *Account, requestedModel string, requireCompact bool) bool {
	if s.channelService == nil {
		return false
	}
	if compactForwardModel, ok := openAIForwardModelFromContext(ctx); ok {
		requestedModel = compactForwardModel.model
		requireCompact = compactForwardModel.useCompactModelMapping
	}
	upstreamModel := resolveOpenAIAccountUpstreamModelForRequest(account, requestedModel, requireCompact)
	if upstreamModel == "" {
		return false
	}
	return s.channelService.IsModelRestricted(ctx, groupID, upstreamModel)
}

func (s *OpenAIGatewayService) needsUpstreamChannelRestrictionCheck(ctx context.Context, groupID *int64) bool {
	if groupID == nil || s.channelService == nil {
		return false
	}
	ch, err := s.channelService.GetChannelForGroup(ctx, *groupID)
	if err != nil {
		slog.Warn("failed to check openai channel upstream restriction", "group_id", *groupID, "error", err)
		return false
	}
	if ch == nil || !ch.RestrictModels {
		return false
	}
	return ch.BillingModelSource == BillingModelSourceUpstream
}

// ReplaceModelInBody 替换请求体中的 JSON model 字段（通用 gjson/sjson 实现）。
func (s *OpenAIGatewayService) ReplaceModelInBody(body []byte, newModel string) []byte {
	return ReplaceModelInBody(body, newModel)
}

func (s *OpenAIGatewayService) getCodexSnapshotThrottle() *accountWriteThrottle {
	if s != nil && s.codexSnapshotThrottle != nil {
		return s.codexSnapshotThrottle
	}
	return defaultOpenAICodexSnapshotPersistThrottle
}

func (s *OpenAIGatewayService) billingDeps() *billingDeps {
	return &billingDeps{
		accountRepo:           s.accountRepo,
		userRepo:              s.userRepo,
		userSubRepo:           s.userSubRepo,
		billingCacheService:   s.billingCacheService,
		deferredService:       s.deferredService,
		balanceNotifyService:  s.balanceNotifyService,
		userPlatformQuotaRepo: s.userPlatformQuotaRepo,
	}
}

// CloseOpenAIWSPool 关闭 OpenAI WebSocket 连接池的后台 worker 和空闲连接。
// 应在应用优雅关闭时调用。
func (s *OpenAIGatewayService) CloseOpenAIWSPool() {
	if s != nil && s.openaiWSPool != nil {
		s.openaiWSPool.Close()
	}
}

func (s *OpenAIGatewayService) InvalidateAgentIdentityWSConnections(accountID int64) {
	if pool := s.getOpenAIWSConnPool(); pool != nil {
		pool.ClearAccount(accountID)
	}
}

func (s *OpenAIGatewayService) logOpenAIWSModeBootstrap() {
	if s == nil || s.cfg == nil {
		return
	}
	wsCfg := s.cfg.Gateway.OpenAIWS
	logOpenAIWSModeInfo(
		"bootstrap enabled=%v oauth_enabled=%v apikey_enabled=%v force_http=%v responses_websockets_v2=%v responses_websockets=%v payload_log_sample_rate=%.3f event_flush_batch_size=%d event_flush_interval_ms=%d prewarm_cooldown_ms=%d retry_backoff_initial_ms=%d retry_backoff_max_ms=%d retry_jitter_ratio=%.3f retry_total_budget_ms=%d ws_read_limit_bytes=%d",
		wsCfg.Enabled,
		wsCfg.OAuthEnabled,
		wsCfg.APIKeyEnabled,
		wsCfg.ForceHTTP,
		wsCfg.ResponsesWebsocketsV2,
		wsCfg.ResponsesWebsockets,
		wsCfg.PayloadLogSampleRate,
		wsCfg.EventFlushBatchSize,
		wsCfg.EventFlushIntervalMS,
		wsCfg.PrewarmCooldownMS,
		wsCfg.RetryBackoffInitialMS,
		wsCfg.RetryBackoffMaxMS,
		wsCfg.RetryJitterRatio,
		wsCfg.RetryTotalBudgetMS,
		openAIWSMessageReadLimitBytes,
	)
}

func (s *OpenAIGatewayService) getCodexClientRestrictionDetector() CodexClientRestrictionDetector {
	if s != nil && s.codexDetector != nil {
		return s.codexDetector
	}
	var cfg *config.Config
	if s != nil {
		cfg = s.cfg
	}
	return NewOpenAICodexClientRestrictionDetector(cfg)
}

func (s *OpenAIGatewayService) getOpenAIWSProtocolResolver() OpenAIWSProtocolResolver {
	if s != nil && s.openaiWSResolver != nil {
		return s.openaiWSResolver
	}
	var cfg *config.Config
	if s != nil {
		cfg = s.cfg
	}
	return NewOpenAIWSProtocolResolver(cfg)
}

func classifyOpenAIWSReconnectReason(err error) (string, bool) {
	if err == nil {
		return "", false
	}
	var fallbackErr *openAIWSFallbackError
	if !errors.As(err, &fallbackErr) || fallbackErr == nil {
		return "", false
	}
	reason := strings.TrimSpace(fallbackErr.Reason)
	if reason == "" {
		return "", false
	}

	baseReason := strings.TrimPrefix(reason, "prewarm_")

	switch baseReason {
	case "policy_violation",
		"message_too_big",
		"upgrade_required",
		"ws_unsupported",
		"auth_failed",
		"invalid_encrypted_content",
		"previous_response_not_found":
		return reason, false
	}

	switch baseReason {
	case "read_event",
		"write_request",
		"write",
		"acquire_timeout",
		"acquire_conn",
		"conn_queue_full",
		"dial_failed",
		"upstream_5xx",
		"event_error",
		"error_event",
		"upstream_error_event",
		"ws_connection_limit_reached",
		"missing_final_response":
		return reason, true
	default:
		return reason, false
	}
}

func resolveOpenAIWSFallbackErrorResponse(err error) (statusCode int, errType string, clientMessage string, upstreamMessage string, ok bool) {
	if err == nil {
		return 0, "", "", "", false
	}
	var fallbackErr *openAIWSFallbackError
	if !errors.As(err, &fallbackErr) || fallbackErr == nil {
		return 0, "", "", "", false
	}

	reason := strings.TrimSpace(fallbackErr.Reason)
	reason = strings.TrimPrefix(reason, "prewarm_")
	if reason == "" {
		return 0, "", "", "", false
	}

	var dialErr *openAIWSDialError
	if fallbackErr.Err != nil && errors.As(fallbackErr.Err, &dialErr) && dialErr != nil {
		if dialErr.StatusCode > 0 {
			statusCode = dialErr.StatusCode
		}
		if dialErr.Err != nil {
			upstreamMessage = sanitizeUpstreamErrorMessage(strings.TrimSpace(dialErr.Err.Error()))
		}
	}

	switch reason {
	case "invalid_encrypted_content":
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
		errType = "invalid_request_error"
		if upstreamMessage == "" {
			upstreamMessage = "encrypted content could not be verified"
		}
	case "previous_response_not_found":
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
		errType = "invalid_request_error"
		if upstreamMessage == "" {
			upstreamMessage = "previous response not found"
		}
	case "upgrade_required":
		if statusCode == 0 {
			statusCode = http.StatusUpgradeRequired
		}
	case "ws_unsupported":
		if statusCode == 0 {
			statusCode = http.StatusBadRequest
		}
	case "auth_failed":
		if statusCode == 0 {
			statusCode = http.StatusUnauthorized
		}
	case "upstream_rate_limited":
		if statusCode == 0 {
			statusCode = http.StatusTooManyRequests
		}
	default:
		if statusCode == 0 {
			return 0, "", "", "", false
		}
	}

	if upstreamMessage == "" && fallbackErr.Err != nil {
		upstreamMessage = sanitizeUpstreamErrorMessage(strings.TrimSpace(fallbackErr.Err.Error()))
	}
	if upstreamMessage == "" {
		switch reason {
		case "upgrade_required":
			upstreamMessage = "upstream websocket upgrade required"
		case "ws_unsupported":
			upstreamMessage = "upstream websocket not supported"
		case "auth_failed":
			upstreamMessage = "upstream authentication failed"
		case "upstream_rate_limited":
			upstreamMessage = "upstream rate limit exceeded, please retry later"
		default:
			upstreamMessage = "Upstream request failed"
		}
	}

	if errType == "" {
		if statusCode == http.StatusTooManyRequests {
			errType = "rate_limit_error"
		} else {
			errType = "upstream_error"
		}
	}
	clientMessage = upstreamMessage
	return statusCode, errType, clientMessage, upstreamMessage, true
}

func (s *OpenAIGatewayService) writeOpenAIWSFallbackErrorResponse(c *gin.Context, account *Account, wsErr error) bool {
	if c == nil || c.Writer == nil || c.Writer.Written() {
		return false
	}
	statusCode, errType, clientMessage, upstreamMessage, ok := resolveOpenAIWSFallbackErrorResponse(wsErr)
	if !ok {
		return false
	}
	if strings.TrimSpace(clientMessage) == "" {
		clientMessage = "Upstream request failed"
	}
	if strings.TrimSpace(upstreamMessage) == "" {
		upstreamMessage = clientMessage
	}

	setOpsUpstreamError(c, statusCode, upstreamMessage, "")
	if account != nil {
		proxyID, proxyName := opsUpstreamWSProxyAttribution(account)
		appendOpsUpstreamError(c, OpsUpstreamErrorEvent{
			ProxyID:            proxyID,
			ProxyName:          proxyName,
			Platform:           account.Platform,
			AccountID:          account.ID,
			AccountName:        account.Name,
			UpstreamStatusCode: statusCode,
			Kind:               "ws_error",
			Message:            upstreamMessage,
		})
	}
	c.JSON(statusCode, gin.H{
		"error": gin.H{
			"type":    errType,
			"message": clientMessage,
		},
	})
	return true
}

func (s *OpenAIGatewayService) openAIWSRetryBackoff(attempt int) time.Duration {
	if attempt <= 0 {
		return 0
	}

	initial := openAIWSRetryBackoffInitialDefault
	maxBackoff := openAIWSRetryBackoffMaxDefault
	jitterRatio := openAIWSRetryJitterRatioDefault
	if s != nil && s.cfg != nil {
		wsCfg := s.cfg.Gateway.OpenAIWS
		if wsCfg.RetryBackoffInitialMS > 0 {
			initial = time.Duration(wsCfg.RetryBackoffInitialMS) * time.Millisecond
		}
		if wsCfg.RetryBackoffMaxMS > 0 {
			maxBackoff = time.Duration(wsCfg.RetryBackoffMaxMS) * time.Millisecond
		}
		if wsCfg.RetryJitterRatio >= 0 {
			jitterRatio = wsCfg.RetryJitterRatio
		}
	}
	if initial <= 0 {
		return 0
	}
	if maxBackoff <= 0 {
		maxBackoff = initial
	}
	if maxBackoff < initial {
		maxBackoff = initial
	}
	if jitterRatio < 0 {
		jitterRatio = 0
	}
	if jitterRatio > 1 {
		jitterRatio = 1
	}

	shift := attempt - 1
	if shift < 0 {
		shift = 0
	}
	backoff := initial
	if shift > 0 {
		backoff = initial * time.Duration(1<<shift)
	}
	if backoff > maxBackoff {
		backoff = maxBackoff
	}
	if jitterRatio <= 0 {
		return backoff
	}
	jitter := time.Duration(float64(backoff) * jitterRatio)
	if jitter <= 0 {
		return backoff
	}
	delta := time.Duration(rand.Int63n(int64(jitter)*2+1)) - jitter
	withJitter := backoff + delta
	if withJitter < 0 {
		return 0
	}
	return withJitter
}

func (s *OpenAIGatewayService) openAIWSRetryTotalBudget() time.Duration {
	if s != nil && s.cfg != nil {
		ms := s.cfg.Gateway.OpenAIWS.RetryTotalBudgetMS
		if ms <= 0 {
			return 0
		}
		return time.Duration(ms) * time.Millisecond
	}
	return 0
}

func (s *OpenAIGatewayService) recordOpenAIWSRetryAttempt(backoff time.Duration) {
	if s == nil {
		return
	}
	s.openaiWSRetryMetrics.retryAttempts.Add(1)
	if backoff > 0 {
		s.openaiWSRetryMetrics.retryBackoffMs.Add(backoff.Milliseconds())
	}
}

func (s *OpenAIGatewayService) recordOpenAIWSRetryExhausted() {
	if s == nil {
		return
	}
	s.openaiWSRetryMetrics.retryExhausted.Add(1)
}

func (s *OpenAIGatewayService) recordOpenAIWSNonRetryableFastFallback() {
	if s == nil {
		return
	}
	s.openaiWSRetryMetrics.nonRetryableFastFallback.Add(1)
}

func (s *OpenAIGatewayService) SnapshotOpenAIWSRetryMetrics() OpenAIWSRetryMetricsSnapshot {
	if s == nil {
		return OpenAIWSRetryMetricsSnapshot{}
	}
	return OpenAIWSRetryMetricsSnapshot{
		RetryAttemptsTotal:            s.openaiWSRetryMetrics.retryAttempts.Load(),
		RetryBackoffMsTotal:           s.openaiWSRetryMetrics.retryBackoffMs.Load(),
		RetryExhaustedTotal:           s.openaiWSRetryMetrics.retryExhausted.Load(),
		NonRetryableFastFallbackTotal: s.openaiWSRetryMetrics.nonRetryableFastFallback.Load(),
	}
}

func SnapshotOpenAICompatibilityFallbackMetrics() OpenAICompatibilityFallbackMetricsSnapshot {
	legacyReadFallbackTotal, legacyReadFallbackHit, legacyDualWriteTotal := openAIStickyCompatStats()
	isMaxTokensOneHaiku, thinkingEnabled, prefetchedStickyAccount, prefetchedStickyGroup, singleAccountRetry, accountSwitchCount := RequestMetadataFallbackStats()

	readHitRate := float64(0)
	if legacyReadFallbackTotal > 0 {
		readHitRate = float64(legacyReadFallbackHit) / float64(legacyReadFallbackTotal)
	}
	metadataFallbackTotal := isMaxTokensOneHaiku + thinkingEnabled + prefetchedStickyAccount + prefetchedStickyGroup + singleAccountRetry + accountSwitchCount

	return OpenAICompatibilityFallbackMetricsSnapshot{
		SessionHashLegacyReadFallbackTotal: legacyReadFallbackTotal,
		SessionHashLegacyReadFallbackHit:   legacyReadFallbackHit,
		SessionHashLegacyDualWriteTotal:    legacyDualWriteTotal,
		SessionHashLegacyReadHitRate:       readHitRate,

		MetadataLegacyFallbackIsMaxTokensOneHaikuTotal: isMaxTokensOneHaiku,
		MetadataLegacyFallbackThinkingEnabledTotal:     thinkingEnabled,
		MetadataLegacyFallbackPrefetchedStickyAccount:  prefetchedStickyAccount,
		MetadataLegacyFallbackPrefetchedStickyGroup:    prefetchedStickyGroup,
		MetadataLegacyFallbackSingleAccountRetryTotal:  singleAccountRetry,
		MetadataLegacyFallbackAccountSwitchCountTotal:  accountSwitchCount,
		MetadataLegacyFallbackTotal:                    metadataFallbackTotal,
	}
}

func (s *OpenAIGatewayService) detectCodexClientRestriction(c *gin.Context, account *Account, body []byte) CodexClientRestrictionDetectionResult {
	// 安全默认：即便缺 settingService（仅测试/误配可达）也保持指纹门为默认种子，
	// 避免零值 policy（nil 信号）让指纹门失败开放。有 settingService 时整体覆盖为全局策略。
	policy := CodexRestrictionPolicy{EngineFingerprintSignals: openai.DefaultEngineFingerprintSignals}
	if account != nil && account.IsCodexCLIOnlyEnabled() && s != nil && s.settingService != nil {
		ctx := context.Background()
		if c != nil && c.Request != nil {
			ctx = c.Request.Context()
		}
		policy = s.settingService.GetCodexRestrictionPolicy(ctx)
	}
	return s.getCodexClientRestrictionDetector().Detect(c, account, policy, body)
}

func getAPIKeyIDFromContext(c *gin.Context) int64 {
	if c == nil {
		return 0
	}
	v, exists := c.Get("api_key")
	if !exists {
		return 0
	}
	apiKey, ok := v.(*APIKey)
	if !ok || apiKey == nil {
		return 0
	}
	return apiKey.ID
}

// isolateOpenAISessionID 将 apiKeyID 混入 session 标识符，
// 确保不同 API Key 的用户即使使用相同的原始 session_id/conversation_id，
// 到达上游的标识符也不同，防止跨用户会话碰撞。
func isolateOpenAISessionID(apiKeyID int64, raw string) string {
	return isolateOpenAISessionIDWithUserSeed(apiKeyID, "", raw)
}

// isolateOpenAISessionIDWithUserSeed 在 isolateOpenAISessionID 基础上额外混入
// 用户级种子（P0-3 §4.4 task 2）。userSeed=="" 时与 isolateOpenAISessionID 完全
// 等价，向后兼容。
//
// 注意：种子只影响哈希结果，不会泄漏到上游；上游看到的仍然是 16 hex。
func isolateOpenAISessionIDWithUserSeed(apiKeyID int64, userSeed, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	h := xxhash.New()
	_, _ = fmt.Fprintf(h, "k%d:", apiKeyID)
	if userSeed != "" {
		_, _ = h.WriteString("u")
		_, _ = h.WriteString(userSeed)
		_, _ = h.WriteString(":")
	}
	_, _ = h.WriteString(raw)
	return fmt.Sprintf("%016x", h.Sum64())
}

// isolateOpenAIUpstreamSessionIDWithUserSeed 把两条隔离维度合并：
//   - 上游（0.2.x）的账号身份命名空间隔离 isolateOpenAIUpstreamSessionID，
//     保证 failover 换号后同一会话不会带着同一标识跨账号出站；
//   - fork 的用户级种子（P0-3 §4.4 task 2），保证同一用户跨 apiKey 的
//     session_id 稳定。
//
// userSeed == ""（P0-3 注入未开启，默认）时逐字节等价于
// isolateOpenAIUpstreamSessionID，因此上游既有断言不受影响。
func isolateOpenAIUpstreamSessionIDWithUserSeed(apiKeyID int64, account *Account, userSeed, raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	if strings.TrimSpace(userSeed) == "" {
		return isolateOpenAIUpstreamSessionID(apiKeyID, account, raw)
	}
	// 先混入用户种子，再交给账号命名空间隔离。
	return isolateOpenAIUpstreamSessionID(apiKeyID, account, isolateOpenAISessionIDWithUserSeed(apiKeyID, userSeed, raw))
}

func logCodexCLIOnlyDetection(ctx context.Context, c *gin.Context, account *Account, apiKeyID int64, result CodexClientRestrictionDetectionResult, body []byte) {
	if !result.Enabled {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	accountID := int64(0)
	if account != nil {
		accountID = account.ID
	}
	fields := []zap.Field{
		zap.String("component", "service.openai_gateway"),
		zap.Int64("account_id", accountID),
		zap.Bool("codex_cli_only_enabled", result.Enabled),
		zap.Bool("codex_official_client_match", result.Matched),
		zap.String("reject_reason", result.Reason),
	}
	if apiKeyID > 0 {
		fields = append(fields, zap.Int64("api_key_id", apiKeyID))
	}
	if !result.Matched {
		fields = appendCodexCLIOnlyRejectedRequestFields(fields, c, body)
	}
	log := logger.FromContext(ctx).With(fields...)
	if result.Matched {
		log.Info("OpenAI codex_cli_only 放行请求")
		return
	}
	log.Warn("OpenAI codex_cli_only 拒绝非官方客户端请求")
}

func appendCodexCLIOnlyRejectedRequestFields(fields []zap.Field, c *gin.Context, body []byte) []zap.Field {
	if c == nil || c.Request == nil {
		return fields
	}

	req := c.Request
	requestModel, requestStream, promptCacheKey := extractOpenAIRequestMetaFromBody(body)
	fields = append(fields,
		zap.String("request_method", strings.TrimSpace(req.Method)),
		zap.String("request_path", strings.TrimSpace(req.URL.Path)),
		zap.String("request_query", strings.TrimSpace(req.URL.RawQuery)),
		zap.String("request_host", strings.TrimSpace(req.Host)),
		zap.String("request_client_ip", strings.TrimSpace(ip.GetClientIP(c))),
		zap.String("request_remote_addr", strings.TrimSpace(req.RemoteAddr)),
		zap.String("request_user_agent", strings.TrimSpace(req.Header.Get("User-Agent"))),
		zap.String("request_content_type", strings.TrimSpace(req.Header.Get("Content-Type"))),
		zap.Int64("request_content_length", req.ContentLength),
		zap.Bool("request_stream", requestStream),
	)
	if requestModel != "" {
		fields = append(fields, zap.String("request_model", requestModel))
	}
	if promptCacheKey != "" {
		fields = append(fields, zap.String("request_prompt_cache_key_sha256", hashSensitiveValueForLog(promptCacheKey)))
	}

	if headers := snapshotCodexCLIOnlyHeaders(req.Header); len(headers) > 0 {
		fields = append(fields, zap.Any("request_headers", headers))
	}
	fields = append(fields, zap.Int("request_body_size", len(body)))
	return fields
}

func snapshotCodexCLIOnlyHeaders(header http.Header) map[string]string {
	if len(header) == 0 {
		return nil
	}
	result := make(map[string]string, len(codexCLIOnlyDebugHeaderWhitelist))
	for _, key := range codexCLIOnlyDebugHeaderWhitelist {
		value := strings.TrimSpace(header.Get(key))
		if value == "" {
			continue
		}
		result[strings.ToLower(key)] = truncateString(value, codexCLIOnlyHeaderValueMaxBytes)
	}
	return result
}

func hashSensitiveValueForLog(raw string) string {
	value := strings.TrimSpace(raw)
	if value == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:8])
}

// ExtractSessionID extracts the raw session ID from headers or body without hashing.
// Used by ForwardAsAnthropic to pass as prompt_cache_key for upstream cache.

// GenerateExplicitSessionHash generates a sticky-session hash only from explicit
// client session signals. It intentionally skips content-derived fallback and is
// used by stateless endpoints such as /v1/images.

// GenerateSessionHash generates a sticky-session hash for OpenAI requests.
//
// Priority:
//  1. Header: session_id
//  2. Header: conversation_id
//  3. Body:   prompt_cache_key (opencode)
//  4. Body:   content-based fallback (model + system + tools + first user message)

// GenerateSessionHashWithFallback 先按常规信号生成会话哈希；
// 当未携带 session_id/conversation_id/prompt_cache_key 时，使用 fallbackSeed 生成稳定哈希。
// 该方法用于 WS ingress，避免会话信号缺失时发生跨账号漂移。

// BindStickySession sets session -> account binding with standard TTL.

// SelectAccount selects an OpenAI account with sticky session support

// SelectAccountForModel selects an account supporting the requested model

// SelectAccountForModelWithExclusions selects an account supporting the requested model while excluding specified accounts.
// SelectAccountForModelWithExclusions 选择支持指定模型的账号，同时排除指定的账号。

// noAvailableOpenAISelectionError builds the standard "no account available" error
// while preserving the compact-specific error when applicable.

// openAICompactSupportTier classifies an OpenAI account by compact capability.
// 0 = explicitly unsupported, 1 = unknown / not yet probed, 2 = explicitly supported.

// isOpenAICompatibleAccountEligibleForRequest 判断 OpenAI 兼容账号是否满足本次请求的调度条件。
// 检查内容包括：平台匹配、账号可用性、quota 自动暂停、spark 路由限制、模型支持及端点能力。
//
// 注意：对 spark 影子账号，调用方还须额外调用 parentHealthyForShadow(account, lookup)
// 检查母账号凭据可用性；该检查未内置于本函数，以避免注入 DB 依赖。

// resolveAccountExtraBool reads a bool-like value from account extra, tolerating
// the few shapes JSON unmarshalling may produce (real bool, "true"/"false"
// strings, 0/1 numbers).

// resolveOpenAIQuotaUtilization returns the current utilization ratio (0..1) for the
// given Codex usage window. ok=false means there is no usable signal to pause on:
// either no snapshot exists, or the window has already rolled over so the cached
// percentage is stale. The stale guard matters because a paused account stops
// receiving requests, so its snapshot is never refreshed from upstream headers —
// without this check an old used_percent would keep the account paused forever even
// after the real window reset.

// openAICodexSnapshotStaleForPause reports whether the Codex usage snapshot is stale
// enough that it should no longer keep an account auto-paused. It anchors on
// codex_usage_updated_at (always written by buildCodexUsageExtraUpdates). A missing or
// unparseable timestamp returns false (treated as fresh, so the account stays paused) —
// this is deliberate: it prevents any snapshot without a write time from silently escaping
// auto-pause, and a genuinely-exhausted account that is actively served refreshes the
// timestamp on every response so it never crosses the staleness bound.

// openAIQuotaWindowReset reports whether the Codex usage window's reset time has
// already passed relative to now. It prefers the absolute codex_<window>_reset_at
// timestamp and falls back to codex_<window>_reset_after_seconds anchored at
// codex_usage_updated_at, mirroring AccountUsageService's window-progress logic.

// prioritizeOpenAICompactAccounts re-orders a slice so that accounts with known
// compact support are tried first, followed by unknown, then explicitly unsupported.
// The relative order within each tier is preserved.

// resolveOpenAIAccountUpstreamModelForRequest resolves the upstream model that
// would be sent for a given request, honouring compact-only mappings when the
// caller is on the /responses/compact path.

// tryStickySessionHit 尝试从粘性会话获取账号。
// 如果命中且账号可用则返回账号；如果账号不可用则清理会话并返回 nil。
//
// tryStickySessionHit attempts to get account from sticky session.
// Returns account if hit and usable; clears session and returns nil if account is unavailable.

// selectBestAccount 从候选账号中选择最佳账号（优先级 + LRU）。
// 返回 nil 表示无可用账号。
//
// selectBestAccount selects the best account from candidates (priority + LRU).
// Returns nil if no available account. The second return reports whether at
// least one candidate was filtered out solely because it lacks compact support
// (only meaningful when requireCompact=true).

// isBetterAccount 判断 candidate 是否比 current 更优。
// 规则：优先级更高（数值更小）优先；同优先级时，未使用过的优先，其次是最久未使用的。
//
// isBetterAccount checks if candidate is better than current.
// Rules: higher priority (lower value) wins; same priority: never used > least recently used.

// SelectAccountWithLoadAwareness selects an account with load-awareness and wait plan.

// effectiveAccountConcurrency 返回账号在 OpenAI 网关侧 acquire-slot / WaitPlan 应使用的并发上限。
// 当 account.Concurrency == 0 时回退到全局默认 gateway.account_default_concurrency。
// 与 GatewayService.effectiveAccountConcurrency 保持语义一致（P0-1 安全垫）。
// doOpenAIUpstreamWithEffectiveConcurrency 是 doOpenAIUpstream（插件路由）与 fork 的
// effectiveAccountConcurrency（account.Concurrency == 0 时回退到组/全局默认并发）
// 的合流版本：OpenAI 网关的真实转发路径统一走这里，既保留上游 0.2.x 的
// OAuth 插件 RoundTrip，也不丢 fork 的并发安全垫。
//
// 注意：openai_plugin_transport.go 里的 doOpenAIUpstream 仍供 alpha search 等
// 无 group ctx 的旁路使用。
func (s *OpenAIGatewayService) doOpenAIUpstreamWithEffectiveConcurrency(
	ctx context.Context,
	request *http.Request,
	proxyURL string,
	account *Account,
) (*http.Response, error) {
	if s.pluginManager != nil {
		response, handled, err := s.pluginManager.RoundTripOpenAIOAuth(request.Context(), request, proxyURL, account)
		if handled {
			return response, err
		}
	}
	return s.httpUpstream.Do(request, proxyURL, account.ID, s.effectiveAccountConcurrency(ctx, account))
}

func (s *OpenAIGatewayService) effectiveAccountConcurrency(ctx context.Context, account *Account) int {
	fleetDefault := 0
	if s != nil && s.cfg != nil {
		fleetDefault = s.cfg.Gateway.AccountDefaultConcurrency
	}
	groupDefault := 0
	if g := GroupFromContext(ctx); g != nil {
		groupDefault = g.DefaultAccountConcurrency
	}
	return account.EffectiveConcurrencyWithGroup(groupDefault, fleetDefault)
}

// effectiveAccountBaseRPM 返回 OpenAI 网关侧账号实际生效的 RPM 上限。
func (s *OpenAIGatewayService) effectiveAccountBaseRPM(ctx context.Context, account *Account) int {
	fleetDefault := 0
	if s != nil && s.cfg != nil {
		fleetDefault = s.cfg.Gateway.AccountDefaultRPM
	}
	groupDefault := 0
	if g := GroupFromContext(ctx); g != nil {
		groupDefault = g.DefaultAccountRPM
	}
	return account.EffectiveBaseRPMWithGroup(groupDefault, fleetDefault)
}

// EffectiveAccountConcurrency 是 effectiveAccountConcurrency 的导出版本，
// 供 handler 在 OpenAI 网关路径上查询账号实际生效的并发上限。
func (s *OpenAIGatewayService) EffectiveAccountConcurrency(ctx context.Context, account *Account) int {
	return s.effectiveAccountConcurrency(ctx, account)
}

// EffectiveAccountBaseRPM 是 effectiveAccountBaseRPM 的导出版本。
func (s *OpenAIGatewayService) EffectiveAccountBaseRPM(ctx context.Context, account *Account) int {
	return s.effectiveAccountBaseRPM(ctx, account)
}

// parentAccountLookup 返回供 parentHealthyForShadow 使用的母账号解析闭包:经 accountRepo
// 按 ID 取当前 Account(repo 为空时 fail-closed 返回 nil)。统一调度/粘连各路径的母账号解析,
// 取代各调用点重复内联的同一闭包(历史上 recheck 等路径还漏写过 accountRepo==nil 守卫)。
// L2 候选循环改用带 per-pass 缓存的 parentLookupL2,不走此方法。

// GetAccessToken gets the access token for an OpenAI account
func (s *OpenAIGatewayService) GetAccessToken(ctx context.Context, account *Account) (string, string, error) {
	if account.IsShadow() {
		credAccount, err := resolveCredentialAccount(ctx, s.accountRepo, account)
		if err != nil {
			return "", "", err
		}
		account = credAccount
	}
	switch account.Type {
	case AccountTypeOAuth:
		if account.IsOpenAIAgentIdentity() {
			return "", OpenAIAuthModeAgentIdentity, nil
		}
		if account.Platform == PlatformGrok {
			if s.grokTokenProvider != nil {
				accessToken, err := s.grokTokenProvider.GetAccessToken(ctx, account)
				if err != nil {
					return "", "", err
				}
				return accessToken, "oauth", nil
			}
			accessToken := account.GetGrokAccessToken()
			if accessToken == "" {
				return "", "", errors.New("access_token not found in credentials")
			}
			return accessToken, "oauth", nil
		}
		// 使用 TokenProvider 获取缓存的 token
		if s.openAITokenProvider != nil {
			accessToken, err := s.openAITokenProvider.GetAccessToken(ctx, account)
			if err != nil {
				return "", "", err
			}
			return accessToken, "oauth", nil
		}
		// 降级：TokenProvider 未配置时直接从账号读取
		accessToken := account.GetOpenAIAccessToken()
		if accessToken == "" {
			return "", "", errors.New("access_token not found in credentials")
		}
		return accessToken, "oauth", nil
	case AccountTypeSetupToken:
		if !account.IsOpenAIOAuthLike() {
			return "", "", fmt.Errorf("unsupported account type: %s", account.Type)
		}
		// OpenAI setup tokens are inference-only bearer credentials. They use the
		// Codex OAuth forwarding protocol but have no refresh-token lifecycle.
		accessToken := account.GetOpenAIAccessToken()
		if accessToken == "" {
			return "", "", errors.New("access_token not found in credentials")
		}
		return accessToken, "oauth", nil
	case AccountTypeAPIKey:
		if account.Platform == PlatformGrok {
			apiKey := strings.TrimSpace(account.GetCredential("api_key"))
			if apiKey == "" {
				return "", "", errors.New("api_key not found in credentials")
			}
			return apiKey, "apikey", nil
		}
		apiKey := strings.TrimSpace(account.GetOpenAIProtocolAPIKey())
		if apiKey == "" {
			return "", "", errors.New("api_key not found in credentials")
		}
		return apiKey, "apikey", nil
	default:
		return "", "", fmt.Errorf("unsupported account type: %s", account.Type)
	}
}

// Forward forwards request to OpenAI API

// handlePassthroughSSEToJSON converts an SSE response body into a JSON
// response for the passthrough path. It mirrors handleSSEToJSON while
// preserving passthrough payloads, except compact-only model remapping may
// rewrite model fields back to the original requested model.

// overrideBrowserUserAgent 检查请求的最终 user-agent，若为浏览器 UA 则替换为后台配置的 Codex UA。
// 用于规避 Cloudflare 对浏览器型 UA 在 ChatGPT 内部接口上的访问质询。
// 影响范围严格限定：仅 OAuth（Codex/ChatGPT 内部接口）账号生效；API Key 等其他账号原样透传。
// 仅在识别为浏览器（Mozilla/...）时改写，其他 CLI/工具 UA 不动。

// compatErrorWriter is the signature for format-specific error writers used by
// the compat paths (Chat Completions and Anthropic Messages).

// handleCompatErrorResponse is the shared non-failover error handler for the
// Chat Completions and Anthropic Messages compat paths. It mirrors the logic of
// handleErrorResponse (passthrough rules, ShouldHandleErrorCode, rate-limit
// tracking, secondary failover) but delegates the final error write to the
// format-specific writer function.

// openaiStreamingResult streaming response result

// extractOpenAISSEDataLine 低开销提取 SSE `data:` 行内容。
// 兼容 `data: xxx` 与 `data:xxx` 两种格式。

// correctToolCallsInResponseBody 修正响应体中的工具调用

// reconstructResponseOutputFromSSE scans raw SSE body text for delta events and
// returns a JSON-encoded output array reconstructed from accumulated deltas.
// Returns (nil, false) if no content was found in deltas.

// buildOpenAIResponsesURL 组装 OpenAI Responses 端点。
// - base 以 /v1 结尾：追加 /responses
// - base 以其他版本段结尾（如 /v4）：追加 /responses
// - base 已是 /responses：原样返回
// - 其他情况：追加 /v1/responses

// OpenAIRecordUsageInput input for recording usage

// CyberPolicyUsageInput 是 cyber 拒绝、未走正常 RecordUsage 的请求记录用量的入参。
// 用量按上游真实 token 计费，与 WS cyber 及正常请求口径一致（InputTokens/OutputTokens
// 取自上游 response.failed 报告的 usage，即 mark.UpstreamInTok/OutTok）。

// RecordCyberPolicyUsageLog 为被上游 cyber_policy 拒绝、未走正常 RecordUsage 的请求
// （HTTP forward 返回错误路径）记录用量并按上游真实 token 计费，使其与 WS cyber 路径、
// 与正常请求的计费口径统一（不再是 tokens=0 免费行）。token 取自上游 response.failed
// 报告的 usage（非流式直接拒通常为 0，cost 随之为 0）。复用 RecordUsage 完成成本计算、
// 扣费与用量行写入（request_type=cyber 由 CyberBlocked 置位）。仅 forward 返回错误的
// 路径由 handler 调用，避免与成功路径的正常 RecordUsage 重复。

// RecordUsage records usage and deducts balance

// ParseCodexRateLimitHeaders extracts Codex usage limits from response headers.
// Exported for use in ratelimit_service when handling OpenAI 429 responses.

// updateCodexUsageSnapshot saves the Codex usage snapshot to account's Extra field
// updateCodexUsageSnapshot 把 /responses 的 x-codex-* 全局头快照写入账号 codex_* Extra。
// ⚠️ 调用方必须排除 spark 影子账号(account.IsShadow()):影子的 codex_* 仅由 QueryUsage
// (/wham/usage bengalfox 道)更新,不能被全局头口径污染(外审第7轮 P1)。本函数仅持 accountID,
// 无法在此自检影子,故守卫前置到各调用点。

// Decode 保留阶段一既有 full-map 行为；后续阶段会把调用点下沉到复杂分支。

// normalizeOpenAIPassthroughOAuthBody 将透传 OAuth 请求体收敛为旧链路关键行为：
// 1) 删除 ChatGPT internal API 不支持的顶层 Responses 参数
// 2) store=false 3) 非 compact 保持 stream=true；compact 强制 stream=false

// OpenAIFastBlockedError indicates a request was rejected by the OpenAI fast
// policy (action=block). Mirrors BetaBlockedError on the Claude side.

// evaluateOpenAIFastPolicy returns the action and error message that should be
// applied for a request with the given account/model/service_tier. When the
// policy service is unavailable or no rule matches, it returns
// (BetaPolicyActionPass, "") so callers can short-circuit safely.
//
// Matching rules:
//   - Scope filters by account type (all / oauth / apikey / bedrock)
//   - ServiceTier must be empty (= any), "all", or equal the normalized tier
//   - ModelWhitelist narrows the rule to specific models; FallbackAction
//     handles the non-matching case (default: pass)
//
// 与 Claude BetaPolicy 的差异（保留首条匹配 short-circuit）：
//   - BetaPolicy 处理的是 anthropic-beta header 中的 token 集合，不同
//     规则可能针对不同 token，filter 需要累加成 set；block 则 first-match。
//   - OpenAI fast policy 操作的是单个字段 service_tier：filter 即删字段，
//     没有可累加的对象。一次请求只携带一个 service_tier，规则的 tier
//     维度天然互斥；同一 (scope, tier) 下若多条规则的 model whitelist
//     发生重叠，admin 可通过规则顺序明确意图。因此采用 first-match 而
//     非 BetaPolicy 那样的"block 覆盖 filter 覆盖 pass"语义。

// evaluateOpenAIFastPolicyWithSettings is the pure-function core extracted so
// long-lived sessions (e.g. WS) can prefetch settings once and avoid hitting
// the settingService on every frame. See WSSession entry and
// openAIFastPolicySettingsFromContext for the caching glue.

// openAIFastPolicyCtxKey 是 context 中预取的 OpenAIFastPolicySettings 缓存
// 键，仅用于 WebSocket 长会话内多帧复用同一份策略快照，避免每帧 DB 命中。
//
// Trade-off：策略变更不会影响当前 WS session（只影响新 session）。这是
// 有意为之 —— 对长会话来说，"策略一致性"比"立刻生效"更重要，且 Claude
// BetaPolicy 的 gin.Context 缓存也是同样取舍。需要 hot-reload 时管理员
// 可以通过踢断 session 强制刷新。

// withOpenAIFastPolicyContext 将一份 settings 快照绑定到 context，供该 ctx
// 衍生 goroutine 中的 evaluateOpenAIFastPolicy 复用。

// applyOpenAIFastPolicyToBody applies the OpenAI fast policy to a raw request
// body. When action=filter it removes the service_tier field; when
// action=block it returns (body, *OpenAIFastBlockedError). On pass it
// normalizes the service_tier value (e.g. client alias "fast" → "priority"),
// rewriting the body so the upstream receives a slug it recognizes.
//
// Rationale for normalize-on-pass: chat-completions / messages 入口在调用本
// 函数之前已经通过 normalizeResponsesBodyServiceTier 把 service_tier 归一化
// 到了上游可识别值；passthrough（OpenAI 自动透传） / native /responses 等
// 入口没有这一前置步骤，pass 路径下若不在此处归一化，"fast" 就会被原样
// 透传到 OpenAI 上游导致 400/拒绝。把归一化收敛到本函数，所有入口行为一致。

// writeOpenAIFastPolicyBlockedResponse writes a 403 JSON response for a
// request blocked by the OpenAI fast policy.

// applyOpenAIFastPolicyToWSResponseCreate evaluates the OpenAI fast policy
// against a single client→upstream WebSocket frame whose top-level
// "type"=="response.create". It mirrors the HTTP-side
// applyOpenAIFastPolicyToBody contract but operates on a Realtime/Responses
// WS payload:
//
//   - pass: keeps service_tier, normalizing aliases such as "fast" to "priority"
//   - filter: returns a copy with top-level service_tier removed
//   - block: returns (frame, *OpenAIFastBlockedError)
//
// Only frames whose "type" field strictly equals "response.create" are
// inspected/mutated. Any other frame type — including the empty string —
// passes through untouched. The OpenAI Realtime client-event spec requires
// "type" to be set, so an empty type is treated as a malformed frame we do
// not police; the upstream is the source of truth for rejecting it.
//
// service_tier lives at the top level of response.create — same as the
// Responses HTTP body shape (see openai_gateway_chat_completions.go:304 +
// extractOpenAIServiceTierFromBody at line 5593, and the test fixture at
// openai_ws_forwarder_ingress_session_test.go:402). We therefore only need
// to inspect / strip the top-level field; there is no nested form in the
// schema today.
//
// The caller is responsible for choosing the upstream model passed in —
// this helper does not re-derive it.

// newOpenAIFastPolicyWSEventID returns a Realtime-style event_id for a
// server-emitted error event. Matches the loose "evt_<rand>" convention used
// by upstream Realtime servers; the exact value is not load-bearing and is
// only required for client-side log correlation. We reuse the existing
// google/uuid dependency rather than pulling a new one.

// buildOpenAIFastPolicyBlockedWSEvent renders an OpenAI Realtime/Responses
// style "error" event payload for a request blocked by the OpenAI fast
// policy. The shape mirrors Realtime error events as observed in upstream
// traces and per the spec's server "error" event:
//
//	{
//	  "event_id": "evt_<random>",
//	  "type": "error",
//	  "error": {
//	    "type": "invalid_request_error",
//	    "code": "policy_violation",
//	    "message": "..."
//	  }
//	}
//
// event_id lets clients correlate the rejection in their logs; "code" gives
// programmatic clients a stable identifier (HTTP-side equivalent is the
// 403 permission_error JSON body).
