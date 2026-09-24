package domain

// Status constants
const (
	StatusActive   = "active"
	StatusDisabled = "disabled"
	StatusError    = "error"
	StatusUnused   = "unused"
	StatusUsed     = "used"
	StatusExpired  = "expired"
)

// Role constants
const (
	RoleAdmin = "admin"
	RoleUser  = "user"
	// RoleReadonlyAdmin 只读管理员：可进入管理后台的受限模块，不可执行任何写操作。
	// 授权由 middleware.ReadonlyAdminGuard 的白名单强制，默认拒绝。
	RoleReadonlyAdmin = "readonly_admin"
)

// Platform constants
const (
	PlatformAnthropic   = "anthropic"
	PlatformOpenAI      = "openai"
	PlatformGemini      = "gemini"
	PlatformAntigravity = "antigravity"
	PlatformKiro        = "kiro"
	PlatformGrok        = "grok"
	// 国产 OpenAI 兼容供应商（经 OpenAI 网关转发，按 Chat Completions 协议）。
	PlatformKimi     = "kimi"     // Kimi (月之暗面 / Moonshot)
	PlatformZhipu    = "zhipu"    // 智谱 GLM (bigmodel)
	PlatformDeepseek = "deepseek" // DeepSeek
	PlatformMiniMax  = "minimax"  // MiniMax (M 系列)
	// PlatformOpenCodeGo 是 OpenCode 平台（账号类型 Zen 按量 / Go 订阅）。
	// 值保持 opencode_go 以兼容已落库的分组、配额与 Composite 路由 CHECK。
	PlatformOpenCodeGo = "opencode_go"
	PlatformComposite  = "composite"
)

// Account mode constants 区分国产供应商的「按量付费（余额）」与「Coding Plan」两种接入方式。
// 存储于 credentials["account_mode"]，决定 base_url 预设与额度监控方式。
const (
	AccountModePayG   = "payg"   // 按量付费：消耗余额，做余额检测冷却
	AccountModeCoding = "coding" // Coding Plan：滚动用量窗口冷却（5h / weekly）
	AccountModeZen    = "zen"    // OpenCode Zen：按量付费，https://opencode.ai/zen/v1
	AccountModeGo     = "go"     // OpenCode Go：订阅额度窗口，https://opencode.ai/zen/go/v1
)

// API protocol constants 国产供应商的上游 API 协议维度。存储于
// credentials["api_protocol"]，与 account_mode 正交：协议决定转发端点与格式，
// 模式决定额度监控方式。同协议请求零转换直通；跨协议组合才走转换链。
const (
	APIProtocolChatCompletions = "chat_completions" // OpenAI Chat Completions（默认）
	APIProtocolAnthropic       = "anthropic"        // 原生 Anthropic /v1/messages（适配 Claude Code）
	APIProtocolResponses       = "responses"        // OpenAI Responses（deepseek / kimi / minimax 原生端点，适配 Codex）
	APIProtocolAdaptive        = "adaptive"         // 按入站协议优先选择供应商原生端点
)

// Account type constants
const (
	AccountTypeOAuth          = "oauth"           // OAuth类型账号（full scope: profile + inference）
	AccountTypeSetupToken     = "setup-token"     // Setup Token类型账号（inference only scope）
	AccountTypeAPIKey         = "apikey"          // API Key类型账号
	AccountTypeUpstream       = "upstream"        // 上游透传类型账号（通过 Base URL + API Key 连接上游）
	AccountTypeBedrock        = "bedrock"         // AWS Bedrock 类型账号（通过 SigV4 签名或 API Key 连接 Bedrock，由 credentials.auth_mode 区分）
	AccountTypeServiceAccount = "service_account" // Google Service Account 类型账号（用于 Vertex AI）
	// AccountTypeVertex 为历史存储/旧客户端使用的类型值，与 Vertex AI Service Account 等价。
	AccountTypeVertex = "vertex"
)

// SupportsAccountQuotaType 返回给定账号类型是否支持账号级配额与池模式。
// 用于 repository 层只持有 type 字符串、无法调用 (*Account) 方法时的对应检查。
// 与 (*Account).SupportsAccountQuota() 保持一致。
func SupportsAccountQuotaType(accountType string) bool {
	switch accountType {
	case AccountTypeAPIKey, AccountTypeBedrock, AccountTypeServiceAccount, AccountTypeVertex:
		return true
	default:
		return false
	}
}

// Redeem type constants
const (
	RedeemTypeBalance      = "balance"
	RedeemTypeConcurrency  = "concurrency"
	RedeemTypeSubscription = "subscription"
	RedeemTypeInvitation   = "invitation"
)

// PromoCode status constants
const (
	PromoCodeStatusActive   = "active"
	PromoCodeStatusDisabled = "disabled"
)

// Admin adjustment type constants
const (
	AdjustmentTypeAdminBalance     = "admin_balance"     // 管理员调整余额
	AdjustmentTypeAdminConcurrency = "admin_concurrency" // 管理员调整并发数
)

// Group subscription type constants
const (
	SubscriptionTypeStandard     = "standard"     // 标准计费模式（按余额扣费）
	SubscriptionTypeSubscription = "subscription" // 订阅模式（按限额控制）
)

// Subscription status constants
const (
	SubscriptionStatusActive    = "active"
	SubscriptionStatusExpired   = "expired"
	SubscriptionStatusSuspended = "suspended"
)

// AntigravityGemini31ProAgentModel is the upstream route for Gemini 3.1 Pro High.
const AntigravityGemini31ProAgentModel = "gemini-pro-agent"

// DefaultAntigravityModelMapping 是 Antigravity 平台的默认模型映射
// 当账号未配置 model_mapping 时使用此默认值
// 与前端 useModelWhitelist.ts 中的 antigravityDefaultMappings 保持一致
var DefaultAntigravityModelMapping = map[string]string{
	// Claude 白名单
	"claude-fable-5-1":           "claude-fable-5-1",         // 官方模型
	"claude-fable-5":             "claude-fable-5",           // 官方模型
	"claude-opus-4-8":            "claude-opus-4-8",          // 官方模型
	"claude-opus-4-7":            "claude-opus-4-7",          // 官方模型
	"claude-opus-4-6-thinking":   "claude-opus-4-6-thinking", // 官方模型
	"claude-opus-4-6":            "claude-opus-4-6-thinking", // 简称映射
	"claude-opus-4-5-thinking":   "claude-opus-4-6-thinking", // 迁移旧模型
	"claude-sonnet-4-6":          "claude-sonnet-4-6",
	"claude-sonnet-4-5":          "claude-sonnet-4-5", // 显式 canonical 选择透传
	"claude-sonnet-4-5-thinking": "claude-sonnet-4-6", // 迁移旧兼容别名
	// Claude 详细版本 ID 映射
	"claude-opus-4-5-20251101":   "claude-opus-4-6-thinking", // 迁移旧模型
	"claude-sonnet-4-5-20250929": "claude-sonnet-4-6",        // 迁移旧模型
	// Claude Haiku → Sonnet（无 Haiku 支持）
	"claude-haiku-4-5":          "claude-sonnet-4-6",
	"claude-haiku-4-5-20251001": "claude-sonnet-4-6",
	// Gemini 2.5 白名单
	"gemini-2.5-flash":               "gemini-2.5-flash",
	"gemini-2.5-flash-image":         "gemini-2.5-flash-image",
	"gemini-2.5-flash-image-preview": "gemini-2.5-flash-image",
	"gemini-2.5-flash-lite":          "gemini-2.5-flash-lite",
	"gemini-2.5-flash-thinking":      "gemini-2.5-flash-thinking",
	"gemini-2.5-pro":                 "gemini-2.5-pro",
	// Gemini 3 白名单
	"gemini-3-flash":    "gemini-3-flash",
	"gemini-3-pro-high": "gemini-3-pro-high",
	"gemini-3-pro-low":  "gemini-3-pro-low",
	// Gemini 3 preview 映射
	"gemini-3-flash-preview": "gemini-3-flash",
	"gemini-3-pro-preview":   "gemini-3-pro-high",
	// Gemini 3.1 白名单
	AntigravityGemini31ProAgentModel: AntigravityGemini31ProAgentModel,
	"gemini-3.1-pro":                 AntigravityGemini31ProAgentModel,
	"gemini-3.1-pro-high":            AntigravityGemini31ProAgentModel,
	"gemini-3.1-pro-low":             "gemini-3.1-pro-low",
	// Gemini 3.1 preview 映射
	"gemini-3.1-pro-preview": AntigravityGemini31ProAgentModel,
	// Gemini 3.1 image 白名单
	"gemini-3.1-flash-image": "gemini-3.1-flash-image",
	// Gemini 3.1 image preview 映射
	"gemini-3.1-flash-image-preview": "gemini-3.1-flash-image",
	// Gemini 3.6 Flash tiered models
	"gemini-3.6-flash":        "gemini-3.6-flash",
	"gemini-3.6-flash-high":   "gemini-3.6-flash-high",
	"gemini-3.6-flash-low":    "gemini-3.6-flash-low",
	"gemini-3.6-flash-medium": "gemini-3.6-flash-medium",
	"gemini-3.6-flash-tiered": "gemini-3.6-flash-tiered",
	// Gemini 3.7 Flash tiered models
	"gemini-3.7-flash":        "gemini-3.7-flash",
	"gemini-3.7-flash-high":   "gemini-3.7-flash-high",
	"gemini-3.7-flash-low":    "gemini-3.7-flash-low",
	"gemini-3.7-flash-medium": "gemini-3.7-flash-medium",
	"gemini-3.7-flash-tiered": "gemini-3.7-flash-tiered",
	// Gemini 3.8 Flash tiered models
	"gemini-3.8-flash":        "gemini-3.8-flash",
	"gemini-3.8-flash-high":   "gemini-3.8-flash-high",
	"gemini-3.8-flash-low":    "gemini-3.8-flash-low",
	"gemini-3.8-flash-medium": "gemini-3.8-flash-medium",
	"gemini-3.8-flash-tiered": "gemini-3.8-flash-tiered",
	// Gemini 3 image 兼容映射（向 3.1 image 迁移）
	"gemini-3-pro-image":         "gemini-3.1-flash-image",
	"gemini-3-pro-image-preview": "gemini-3.1-flash-image",
	// 其他官方模型
	"gpt-oss-120b-medium":    "gpt-oss-120b-medium",
	"tab_flash_lite_preview": "tab_flash_lite_preview",
}

// DefaultVertexModelMapping 是 Google Vertex AI 平台的默认模型映射
// 将 Anthropic 标准模型名映射到 Vertex 上发布的模型 ID（@日期 形式）。
// 仅包含已在 Vertex AI Console 公开发布的模型 ID；用户如需新模型可在
// 账号 model_mapping 中自定义覆盖（覆盖优先级高于此默认表）。
//
// 模型可用性按 region 不同；本表不区分 region，由账号 gcp_region 决定实际命中。
// 若 region 不支持映射后的模型 ID，Vertex 会返回 404，调用方应在错误信息中提示。
var DefaultVertexModelMapping = map[string]string{
	// Claude Opus 4.x — 新模型使用 @default 版本，直接用模型名即可
	"claude-opus-4-7":          "claude-opus-4-7",
	"claude-opus-4-6":          "claude-opus-4-6",
	"claude-opus-4-6-thinking": "claude-opus-4-6",
	"claude-opus-4-1":          "claude-opus-4-1@20250805",
	"claude-opus-4-1-20250805": "claude-opus-4-1@20250805",
	"claude-opus-4":            "claude-opus-4@20250514",
	"claude-opus-4-20250514":   "claude-opus-4@20250514",
	// Claude Sonnet 4.x — 新模型使用 @default 版本，直接用模型名即可
	"claude-sonnet-4-6":          "claude-sonnet-4-6",
	"claude-sonnet-4-6-thinking": "claude-sonnet-4-6",
	"claude-sonnet-4-5":          "claude-sonnet-4-5@20250929",
	"claude-sonnet-4-5-thinking": "claude-sonnet-4-5@20250929",
	"claude-sonnet-4-5-20250929": "claude-sonnet-4-5@20250929",
	"claude-sonnet-4":            "claude-sonnet-4@20250514",
	"claude-sonnet-4-20250514":   "claude-sonnet-4@20250514",
	// Claude Haiku 4.x
	"claude-haiku-4-5":          "claude-haiku-4-5@20251001",
	"claude-haiku-4-5-20251001": "claude-haiku-4-5@20251001",
	// Claude 3.7 Sonnet
	"claude-3-7-sonnet":          "claude-3-7-sonnet@20250219",
	"claude-3-7-sonnet-20250219": "claude-3-7-sonnet@20250219",
}

// DefaultBedrockModelMapping 是 AWS Bedrock 平台的默认模型映射
// 将 Anthropic 标准模型名映射到 Bedrock 模型 ID
// 注意：此处的 "us." 前缀仅为默认值，ResolveBedrockModelID 会根据账号配置的
// aws_region 自动调整为匹配的区域前缀（如 eu.、apac.、jp. 等）
var DefaultBedrockModelMapping = map[string]string{
	// Claude Fable
	"claude-fable-5-1": "anthropic.claude-fable-5-1",
	"claude-fable-5":   "anthropic.claude-fable-5",
	// Claude Opus
	"claude-opus-5":            "us.anthropic.claude-opus-5-v1",
	"claude-opus-4-8":          "us.anthropic.claude-opus-4-8-v1",
	"claude-opus-4-7":          "us.anthropic.claude-opus-4-7-v1",
	"claude-opus-4-6-thinking": "us.anthropic.claude-opus-4-6-v1",
	"claude-opus-4-6":          "us.anthropic.claude-opus-4-6-v1",
	"claude-opus-4-5-thinking": "us.anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-opus-4-5-20251101": "us.anthropic.claude-opus-4-5-20251101-v1:0",
	"claude-opus-4-1":          "us.anthropic.claude-opus-4-1-20250805-v1:0",
	"claude-opus-4-20250514":   "us.anthropic.claude-opus-4-20250514-v1:0",
	// Claude Sonnet
	"claude-sonnet-5":            "us.anthropic.claude-sonnet-5-v1",
	"claude-sonnet-4-6-thinking": "us.anthropic.claude-sonnet-4-6",
	"claude-sonnet-4-6":          "us.anthropic.claude-sonnet-4-6",
	"claude-sonnet-4-5":          "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-sonnet-4-5-thinking": "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-sonnet-4-5-20250929": "us.anthropic.claude-sonnet-4-5-20250929-v1:0",
	"claude-sonnet-4-20250514":   "us.anthropic.claude-sonnet-4-20250514-v1:0",
	// Claude Haiku
	"claude-haiku-4-5":          "us.anthropic.claude-haiku-4-5-20251001-v1:0",
	"claude-haiku-4-5-20251001": "us.anthropic.claude-haiku-4-5-20251001-v1:0",
}

// DefaultKiroModelMapping 是 Kiro 平台的默认模型映射。
// Kiro 上游接受形如 "claude-sonnet-4.6" 的简短模型 ID（点号分隔），
// 这里将我们对外暴露的标准模型名映射到 Kiro 实际支持的 ID。
// 请求转换层负责剥离 "-thinking" 后缀并将其表达为 inference flag，
// 此映射表只关心纯模型名。
var DefaultKiroModelMapping = map[string]string{
	"claude-sonnet-4-6":          "claude-sonnet-4.6",
	"claude-sonnet-4-6-thinking": "claude-sonnet-4.6",
	"claude-sonnet-4-5":          "claude-sonnet-4.5",
	"claude-sonnet-4-5-thinking": "claude-sonnet-4.5",
	"claude-sonnet-4":            "claude-sonnet-4",
	"claude-opus-4-7":            "claude-opus-4.7",
	"claude-opus-4-6":            "claude-opus-4.6",
	"claude-opus-4-6-thinking":   "claude-opus-4.6",
	"claude-haiku-4-5":           "claude-haiku-4.5",
}
