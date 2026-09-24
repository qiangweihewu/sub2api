package routes

import (
	"net/http"
	"net/http/httptest"
	"sort"
	"strings"
	"testing"

	"github.com/Wei-Shaw/sub2api/internal/handler"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// reviewedDenyPrefixes 列出【已经过人工审阅、确认对 readonly_admin 拒绝】的管理端模块。
//
// 本测试要求每一条已注册的 admin 路由要么在 middleware 的白名单中、要么匹配本表的
// 某个前缀。新增端点时如果两边都没登记，测试直接失败，强制做出一次显式的安全决策。
//
// 拒绝侧允许用前缀（整块模块被拒时逐条列举无价值）；白名单侧禁止前缀匹配。
// 不对称是有意的：拒绝侧宽松只会让端点保持 403，放行侧宽松会泄露数据。
var reviewedDenyPrefixes = []string{
	"/api/v1/admin/settings",
	// 支付管理端（含订单、订阅计划、渠道实例）。管理端订单的真实路径是
	// /api/v1/admin/payment/orders，本前缀即已覆盖；计划书里那条独立的
	// "/api/v1/admin/orders" 匹配不到任何路由，已按 DenyPrefixesAreAllLive 移除。
	"/api/v1/admin/payment",
	"/api/v1/admin/redeem",
	"/api/v1/admin/promo-codes",
	"/api/v1/admin/backup",
	"/api/v1/admin/data-management",
	"/api/v1/admin/audit-logs",
	"/api/v1/admin/proxies",
	"/api/v1/admin/announcements",
	"/api/v1/admin/dashboard",
	"/api/v1/admin/subscriptions",
	"/api/v1/admin/affiliates",
	"/api/v1/admin/channel-monitors",
	"/api/v1/admin/channel-monitor-templates",
	"/api/v1/admin/channel-monitor-v2",
	"/api/v1/admin/openai",
	"/api/v1/admin/gemini",
	"/api/v1/admin/antigravity",
	"/api/v1/admin/kiro",
	"/api/v1/admin/grok",
	"/api/v1/admin/metrics",
	"/api/v1/admin/usage",

	// ---- 其余整体拒绝的模块 ----
	"/api/v1/admin/api-keys",                 // 客户 API Key 管理
	"/api/v1/admin/cn-providers",             // CN 供应商余额/配额探测：会对上游发真实请求
	"/api/v1/admin/compliance",               // 合规确认（含 accept 写操作）
	"/api/v1/admin/error-passthrough-rules",  // 网关错误透传规则（配置面）
	"/api/v1/admin/prompt-audit",             // 提示词审计：客户请求原文
	"/api/v1/admin/risk-control",             // 风控：封禁/解封/命中日志
	"/api/v1/admin/scheduled-test-plans",     // 定时探测计划（会真发上游请求）
	"/api/v1/admin/system",                   // 版本/更新/回滚/重启
	"/api/v1/admin/plugins",                  // 插件管理：上传/启停/配置/UI 会话，等价于代码执行面
	"/api/v1/admin/tls-fingerprint-profiles", // TLS 指纹配置
	"/api/v1/admin/user-attributes",          // 用户属性定义（配置面）
}

// partiallyAllowedModuleRoots 是【存在放行端点】的五个模块根。
//
// 这些模块里既有白名单端点、也有被拒端点，因此它们的拒绝项【必须】逐条登记到
// reviewedDenyExact，绝不能用前缀。理由见 TestReadonlyAdminDenyPrefixesDoNotCoverPartialModules。
var partiallyAllowedModuleRoots = []string{
	"/api/v1/admin/accounts",
	"/api/v1/admin/groups",
	"/api/v1/admin/channels",
	"/api/v1/admin/users",
	"/api/v1/admin/ops",
}

// reviewedDenyExact 列出【位于部分放行模块内部、已人工审阅并确认拒绝】的端点。
//
// 与 reviewedDenyPrefixes 的区别：那张表用于整体拒绝的模块，模块内新增端点默认拒绝
// 即是正确答案，无需人工介入；而本表覆盖的五个模块正是 readonly_admin 唯一能触达的
// 界面，模块内新增端点究竟该放行还是拒绝【必须】有人拍板。因此这里用精确匹配：
// 在这五个模块中新增任何路由，若未登记到白名单或本表，完备性测试立即失败。
//
// 本表长是特性不是缺陷。
var reviewedDenyExact = map[string]struct{}{
	// ---- accounts（51 条）----
	// 上游账号：写操作 / 凭据导出导入 / 触发上游调用
	"POST /api/v1/admin/accounts": {},
	// 批量删除账号：破坏性写操作。
	"POST /api/v1/admin/accounts/batch-delete": {},
	// force=true 会绕过缓存对上游发起真实用量查询（可放大外呼），只读管理员
	// 已有 GET /accounts/:id/usage 与 today-stats/batch 覆盖读取需求。
	"POST /api/v1/admin/accounts/usage/batch":                        {},
	"PUT /api/v1/admin/accounts/:id":                                 {},
	"DELETE /api/v1/admin/accounts/:id":                              {},
	"POST /api/v1/admin/accounts/:id/apply-oauth-credentials":        {},
	"POST /api/v1/admin/accounts/:id/clear-error":                    {},
	"POST /api/v1/admin/accounts/:id/clear-rate-limit":               {},
	"POST /api/v1/admin/accounts/:id/duplicate":                      {},
	"POST /api/v1/admin/accounts/:id/models/sync-upstream":           {},
	"GET /api/v1/admin/accounts/:id/ollama-cloud-usage":              {},
	"PUT /api/v1/admin/accounts/:id/ollama-cloud-usage/auto-refresh": {},
	"POST /api/v1/admin/accounts/:id/ollama-cloud-usage/refresh":     {},
	"PUT /api/v1/admin/accounts/:id/ollama-cloud-usage/session":      {},
	"DELETE /api/v1/admin/accounts/:id/ollama-cloud-usage/session":   {},
	// opencode-go 用量与 grok 媒体资格：与 ollama-cloud-usage 同类（会触发上游探测/改设置），整体拒绝。
	"GET /api/v1/admin/accounts/:id/opencode-go-usage":              {},
	"POST /api/v1/admin/accounts/:id/opencode-go-usage/refresh":     {},
	"PUT /api/v1/admin/accounts/:id/opencode-go-usage/auto-refresh": {},
	"GET /api/v1/admin/accounts/opencode-go-usage/settings":         {},
	"PUT /api/v1/admin/accounts/opencode-go-usage/settings":         {},
	"GET /api/v1/admin/accounts/:id/grok-media-eligibility":         {},
	"PUT /api/v1/admin/accounts/:id/grok-media-eligibility":         {},
	"POST /api/v1/admin/accounts/:id/recover-state":                 {},
	"POST /api/v1/admin/accounts/:id/refresh":                       {},
	"POST /api/v1/admin/accounts/:id/refresh-tier":                  {},
	"POST /api/v1/admin/accounts/:id/reset-quota":                   {},
	"POST /api/v1/admin/accounts/:id/revert-proxy-fallback":         {},
	"POST /api/v1/admin/accounts/:id/schedulable":                   {},
	"GET /api/v1/admin/accounts/:id/scheduled-test-plans":           {},
	"POST /api/v1/admin/accounts/:id/set-privacy":                   {},
	"POST /api/v1/admin/accounts/:id/shadow":                        {},
	"DELETE /api/v1/admin/accounts/:id/temp-unschedulable":          {},
	"POST /api/v1/admin/accounts/:id/test":                          {},
	"POST /api/v1/admin/accounts/:id/upstream-billing-probe":        {},
	"PUT /api/v1/admin/accounts/:id/upstream-billing-probe":         {},
	"GET /api/v1/admin/accounts/antigravity/default-model-mapping":  {},
	"POST /api/v1/admin/accounts/batch":                             {},
	"POST /api/v1/admin/accounts/batch-clear-error":                 {},
	"POST /api/v1/admin/accounts/batch-refresh":                     {},
	"POST /api/v1/admin/accounts/batch-refresh-tier":                {},
	"POST /api/v1/admin/accounts/batch-update-credentials":          {},
	"POST /api/v1/admin/accounts/bulk-update":                       {},
	"POST /api/v1/admin/accounts/check-mixed-channel":               {},
	"POST /api/v1/admin/accounts/cookie-auth":                       {},
	"GET /api/v1/admin/accounts/data":                               {},
	"POST /api/v1/admin/accounts/data":                              {},
	"POST /api/v1/admin/accounts/exchange-code":                     {},
	"POST /api/v1/admin/accounts/exchange-setup-token-code":         {},
	"POST /api/v1/admin/accounts/generate-auth-url":                 {},
	"POST /api/v1/admin/accounts/generate-setup-token-url":          {},
	"POST /api/v1/admin/accounts/import/codex-session":              {},
	"POST /api/v1/admin/accounts/models/sync-upstream-preview":      {},
	"GET /api/v1/admin/accounts/ollama-cloud-usage/settings":        {},
	"PUT /api/v1/admin/accounts/ollama-cloud-usage/settings":        {},
	"POST /api/v1/admin/accounts/setup-token-cookie-auth":           {},
	"POST /api/v1/admin/accounts/sync/crs":                          {},
	"POST /api/v1/admin/accounts/sync/crs/preview":                  {},
	"POST /api/v1/admin/accounts/upstream-billing-probe/batch":      {},
	"GET /api/v1/admin/accounts/upstream-billing-probe/settings":    {},
	// 上游计费倍率总览：按账号列出上游成本口径（含账号名/平台/分组）。
	// 与同模块的 upstream-billing-probe 一致按拒绝登记——放行侧宽松会泄露数据，
	// 拒绝侧只是保持 403。如需给只读管理员开放，改登记到 middleware 白名单即可。
	"GET /api/v1/admin/accounts/upstream-billing-rates":          {},
	"PUT /api/v1/admin/accounts/upstream-billing-probe/settings": {},

	// ---- groups（15 条）----
	// 分组：写操作 + 客户 API Key、订阅等客户数据
	"POST /api/v1/admin/groups":                                  {},
	"PUT /api/v1/admin/groups/:id":                               {},
	"DELETE /api/v1/admin/groups/:id":                            {},
	"GET /api/v1/admin/groups/:id/api-keys":                      {},
	"POST /api/v1/admin/groups/:id/composite-routes":             {},
	"PUT /api/v1/admin/groups/:id/composite-routes/:route_id":    {},
	"DELETE /api/v1/admin/groups/:id/composite-routes/:route_id": {},
	"POST /api/v1/admin/groups/:id/composite-routes/preview":     {},
	"POST /api/v1/admin/groups/:id/duplicate":                    {},
	"PUT /api/v1/admin/groups/:id/rate-multipliers":              {},
	"DELETE /api/v1/admin/groups/:id/rate-multipliers":           {},
	"PUT /api/v1/admin/groups/:id/rpm-overrides":                 {},
	"DELETE /api/v1/admin/groups/:id/rpm-overrides":              {},
	"GET /api/v1/admin/groups/:id/subscriptions":                 {},
	"PUT /api/v1/admin/groups/sort-order":                        {},

	// ---- channels（4 条）----
	// 渠道与定价：写操作 + 伪装成 GET 的上游同步
	"POST /api/v1/admin/channels":                    {},
	"PUT /api/v1/admin/channels/:id":                 {},
	"DELETE /api/v1/admin/channels/:id":              {},
	"GET /api/v1/admin/channels/pricing/sync-models": {},

	// ---- users（18 条）----
	// 用户：写操作 + 其他客户的密钥/财务/用量/配额
	"POST /api/v1/admin/users":                           {},
	"PUT /api/v1/admin/users/:id":                        {},
	"DELETE /api/v1/admin/users/:id":                     {},
	"GET /api/v1/admin/users/:id/api-keys":               {},
	"GET /api/v1/admin/users/:id/attributes":             {},
	"PUT /api/v1/admin/users/:id/attributes":             {},
	"POST /api/v1/admin/users/:id/auth-identities":       {},
	"POST /api/v1/admin/users/:id/balance":               {},
	"GET /api/v1/admin/users/:id/balance-history":        {},
	"GET /api/v1/admin/users/:id/platform-quotas":        {},
	"PUT /api/v1/admin/users/:id/platform-quotas":        {},
	"POST /api/v1/admin/users/:id/platform-quotas/reset": {},
	"POST /api/v1/admin/users/:id/replace-group":         {},
	"GET /api/v1/admin/users/:id/rpm-status":             {},
	"GET /api/v1/admin/users/:id/subscriptions":          {},
	"GET /api/v1/admin/users/:id/usage":                  {},
	"POST /api/v1/admin/users/batch-concurrency":         {},
	"POST /api/v1/admin/users/batch-limits":              {},

	// ---- ops（16 条）----
	// 运维：全部写端点（读端点见白名单）
	"PUT /api/v1/admin/ops/advanced-settings":           {},
	"PUT /api/v1/admin/ops/alert-events/:id/status":     {},
	"POST /api/v1/admin/ops/alert-rules":                {},
	"PUT /api/v1/admin/ops/alert-rules/:id":             {},
	"DELETE /api/v1/admin/ops/alert-rules/:id":          {},
	"POST /api/v1/admin/ops/alert-silences":             {},
	"PUT /api/v1/admin/ops/email-notification/config":   {},
	"PUT /api/v1/admin/ops/errors/:id/resolve":          {},
	"PUT /api/v1/admin/ops/performance-settings":        {},
	"PUT /api/v1/admin/ops/request-errors/:id/resolve":  {},
	"PUT /api/v1/admin/ops/runtime/alert":               {},
	"PUT /api/v1/admin/ops/runtime/logging":             {},
	"POST /api/v1/admin/ops/runtime/logging/reset":      {},
	"PUT /api/v1/admin/ops/settings/metric-thresholds":  {},
	"POST /api/v1/admin/ops/system-logs/cleanup":        {},
	"PUT /api/v1/admin/ops/upstream-errors/:id/resolve": {},
}

// newFullAdminRouter 注册【全部】管理端路由。
//
// 注意：并非所有 /api/v1/admin/** 路由都来自 RegisterAdminRoutes —— 支付管理端在
// routes/payment.go 里另起了一个 v1.Group("/admin/payment")，有自己的中间件链。
// 只注册 RegisterAdminRoutes 会让那批路由从完备性测试中彻底消失（既不被枚举、也就
// 无从分类），reviewedDenyPrefixes 里的 "/api/v1/admin/payment" 会沦为一条谁也保护
// 不了的死条目。任何新的 admin 路由组都必须在这里补注册。
func newFullAdminRouter() *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{}}
	noop := func(c *gin.Context) { c.Next() }
	v1 := router.Group("/api/v1")
	RegisterAdminRoutes(
		v1,
		handlers,
		servermiddleware.AdminAuthMiddleware(noop),
		servermiddleware.AuditLogMiddleware(noop),
		servermiddleware.StepUpAuthMiddleware(noop),
		nil,
		nil,
	)
	RegisterPaymentRoutes(
		v1,
		nil,
		nil,
		nil,
		servermiddleware.JWTAuthMiddleware(noop),
		servermiddleware.AdminAuthMiddleware(noop),
		servermiddleware.AuditLogMiddleware(noop),
		nil,
		nil,
	)
	return router
}

func allowlistSet() map[string]struct{} {
	allow := make(map[string]struct{})
	for _, k := range servermiddleware.ReadonlyAdminAllowlistKeys() {
		allow[k] = struct{}{}
	}
	return allow
}

// TestReadonlyAdminAllowlistIsExhaustive 是"默认拒绝"性质的执行者。
func TestReadonlyAdminAllowlistIsExhaustive(t *testing.T) {
	allow := allowlistSet()
	var unclassified []string
	for _, r := range newFullAdminRouter().Routes() {
		if !strings.HasPrefix(r.Path, "/api/v1/admin") {
			continue
		}
		key := r.Method + " " + r.Path
		if _, ok := allow[key]; ok {
			continue
		}
		if _, ok := reviewedDenyExact[key]; ok {
			continue
		}
		denied := false
		for _, p := range reviewedDenyPrefixes {
			if strings.HasPrefix(r.Path, p) {
				denied = true
				break
			}
		}
		if !denied {
			unclassified = append(unclassified, key)
		}
	}
	sort.Strings(unclassified)
	require.Emptyf(t, unclassified,
		"以下 admin 端点既不在 readonlyAdminAllowlist 中，也不在 reviewedDenyPrefixes 中。\n"+
			"请逐条判断该端点对只读管理员是否安全，然后登记到其中一处：\n  %s",
		strings.Join(unclassified, "\n  "))
}

// TestReadonlyAdminAllowlistHasNoStaleEntries 防止白名单残留已删除/改名的路由。
func TestReadonlyAdminAllowlistHasNoStaleEntries(t *testing.T) {
	registered := make(map[string]struct{})
	for _, r := range newFullAdminRouter().Routes() {
		registered[r.Method+" "+r.Path] = struct{}{}
	}
	var stale []string
	for _, k := range servermiddleware.ReadonlyAdminAllowlistKeys() {
		if _, ok := registered[k]; !ok {
			stale = append(stale, k)
		}
	}
	sort.Strings(stale)
	require.Emptyf(t, stale,
		"白名单中存在未注册的路由（路由被删除或改名后未同步）：\n  %s",
		strings.Join(stale, "\n  "))
}

// TestReadonlyAdminSensitiveEndpointsStayDenied 是敏感端点哨兵。
// 如果哪天有人把这些误加进白名单，本测试立刻红。
func TestReadonlyAdminSensitiveEndpointsStayDenied(t *testing.T) {
	allow := allowlistSet()
	for _, key := range []string{
		"GET /api/v1/admin/accounts/data",                // 导出上游凭据原文
		"POST /api/v1/admin/accounts/data",               // 导入
		"GET /api/v1/admin/proxies/data",                 // 导出代理凭据原文
		"GET /api/v1/admin/groups/:id/api-keys",          // 其他客户的 API Key
		"GET /api/v1/admin/users/:id/api-keys",           // 其他客户的 API Key
		"GET /api/v1/admin/users/:id/balance-history",    // 其他客户的财务流水
		"GET /api/v1/admin/channels/pricing/sync-models", // 伪装成 GET 的上游同步写操作
		"POST /api/v1/admin/accounts",
		"PUT /api/v1/admin/accounts/:id",
		"DELETE /api/v1/admin/accounts/:id",
		"POST /api/v1/admin/accounts/:id/test",
		"POST /api/v1/admin/ops/alert-rules",
		"PUT /api/v1/admin/ops/advanced-settings",
		"PUT /api/v1/admin/ops/performance-settings",
		"POST /api/v1/admin/users/:id/balance",
		"DELETE /api/v1/admin/users/:id",
	} {
		_, found := allow[key]
		require.Falsef(t, found,
			"敏感端点 %q 被加入了 readonly_admin 白名单。这会向只读账号泄露凭据或允许写操作。", key)
	}
}

// TestReadonlyAdminDenyPrefixesDoNotCoverPartialModules 守住"精确 / 前缀"两套机制的边界。
//
// 背景：拒绝侧按【路径前缀】匹配，且不区分方法。一旦有人把 "/api/v1/admin/accounts"
// 这类模块根塞进 reviewedDenyPrefixes（很有诱惑力 —— 集合级写端点 POST
// /api/v1/admin/accounts 的完整路径就是模块根，加一条前缀就能"清账"），该模块内
// 所有未登记路由都会被自动判为"已审阅拒绝"，完备性测试从此不再报告它们。
//
// 运行时依然安全（未进白名单的路由一律 403），但强制决策的机制在【最需要它的五个
// 模块】上失效了 —— 这五个模块正是 readonly_admin 唯一能触达的界面，其中新增端点
// 到底该放行还是拒绝，必须有人明确拍板，而不是悄悄默认成 403。
//
// 因此：这五个模块内部的拒绝项一律登记到 reviewedDenyExact（精确匹配、逐条枚举）。
func TestReadonlyAdminDenyPrefixesDoNotCoverPartialModules(t *testing.T) {
	for _, prefix := range reviewedDenyPrefixes {
		for _, root := range partiallyAllowedModuleRoots {
			overlaps := strings.HasPrefix(root, prefix) || strings.HasPrefix(prefix, root)
			require.Falsef(t, overlaps,
				"reviewedDenyPrefixes 中的 %q 覆盖了部分放行模块 %q。\n"+
					"这五个模块既有放行端点也有拒绝端点，用前缀登记会让模块内新增的路由\n"+
					"自动被判为『已审阅拒绝』，完备性测试从此不再要求任何人做决策 ——\n"+
					"而这正是本套测试存在的唯一理由。\n"+
					"请把该模块内需要拒绝的路由逐条加入 reviewedDenyExact（精确 METHOD+路径），\n"+
					"而不是删掉这条断言。", prefix, root)
		}
	}
}

// TestReadonlyAdminDenyExactStaysInsidePartialModules 反向约束：reviewedDenyExact
// 只服务于那五个部分放行的模块。整体拒绝的模块请用 reviewedDenyPrefixes，逐条枚举
// 它们没有意义，只会让本表淹没在噪音里，掩盖真正需要人工盯防的五个模块。
func TestReadonlyAdminDenyExactStaysInsidePartialModules(t *testing.T) {
	for key := range reviewedDenyExact {
		parts := strings.SplitN(key, " ", 2)
		require.Lenf(t, parts, 2, "reviewedDenyExact 条目格式应为 \"METHOD /path\"，实际为 %q", key)
		path := parts[1]
		inside := false
		for _, root := range partiallyAllowedModuleRoots {
			if path == root || strings.HasPrefix(path, root+"/") {
				inside = true
				break
			}
		}
		require.Truef(t, inside,
			"reviewedDenyExact 中的 %q 不属于任何部分放行模块 %v。\n"+
				"整体拒绝的模块请登记到 reviewedDenyPrefixes。", key, partiallyAllowedModuleRoots)
	}
}

// newAdminRouterWithRole 构造与生产环境同构的完整管理端路由，并让 adminAuth 桩件把
// 指定角色写入 context —— 即"已通过管理端认证的某角色用户"。
//
// 处理器桩件是零值的 handler.Handlers，真正被放行的请求会在处理器内部因空指针 panic。
// 这里用引擎级中间件把 panic 收敛成 599，从而把"是否被守卫拦下"与"是否走到了处理器"
// 区分开：403 = 被守卫拒绝；其余任何状态 = 已越过守卫。
func newAdminRouterWithRole(role string) *gin.Engine {
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(func(c *gin.Context) {
		defer func() {
			if r := recover(); r != nil {
				c.AbortWithStatus(599) // 走到了处理器（桩件为 nil，必然 panic）
			}
		}()
		c.Next()
	})
	handlers := &handler.Handlers{Admin: &handler.AdminHandlers{}}
	setRole := func(c *gin.Context) {
		c.Set(string(servermiddleware.ContextKeyUserRole), role)
		c.Next()
	}
	noop := func(c *gin.Context) { c.Next() }
	v1 := router.Group("/api/v1")
	RegisterAdminRoutes(
		v1,
		handlers,
		servermiddleware.AdminAuthMiddleware(setRole),
		servermiddleware.AuditLogMiddleware(noop),
		servermiddleware.StepUpAuthMiddleware(noop),
		nil,
		nil,
	)
	RegisterPaymentRoutes(
		v1, nil, nil, nil,
		servermiddleware.JWTAuthMiddleware(noop),
		servermiddleware.AdminAuthMiddleware(setRole),
		servermiddleware.AuditLogMiddleware(noop),
		nil, nil,
	)
	return router
}

func statusFor(t *testing.T, router *gin.Engine, method, path string) int {
	t.Helper()
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(method, path, nil))
	return recorder.Code
}

// TestReadonlyAdminGuardIsMounted 断言守卫【确实挂在了路由链上】。
//
// 其余测试全部只读白名单这张数据表：哪怕有人把 admin.go 里的
// admin.Use(middleware.ReadonlyAdminGuard()) 整行删掉，它们依然全绿。而 admin.go /
// payment.go 都是上游合并的高冲突文件，这一行被合并冲突吃掉是很现实的失效模式。
// 本测试跑的是真实中间件链，删掉挂载点它立刻红。
func TestReadonlyAdminGuardIsMounted(t *testing.T) {
	readonly := newAdminRouterWithRole(service.RoleReadonlyAdmin)

	// 白名单外的写端点：必须被守卫拦下。
	require.Equal(t, http.StatusForbidden,
		statusFor(t, readonly, http.MethodDelete, "/api/v1/admin/accounts/1"),
		"DELETE /admin/accounts/:id 未被拦截 —— ReadonlyAdminGuard 可能没有挂载到 admin 路由链上")

	// 白名单内的读端点：必须放行（放行后会撞上 nil 处理器，状态非 403 即可）。
	require.NotEqual(t, http.StatusForbidden,
		statusFor(t, readonly, http.MethodGet, "/api/v1/admin/accounts"),
		"GET /admin/accounts 被拦截了 —— 白名单未生效")

	// 换成普通管理员：同一条写端点不应被拦（守卫只对 readonly_admin 生效）。
	require.NotEqual(t, http.StatusForbidden,
		statusFor(t, newAdminRouterWithRole(service.RoleAdmin), http.MethodDelete, "/api/v1/admin/accounts/1"),
		"普通 admin 被 ReadonlyAdminGuard 误伤")
}

// TestReadonlyAdminGuardIsMountedOnPaymentRoutes 同上，针对 routes/payment.go 里
// 那个独立的 v1.Group("/admin/payment")。它不经过 RegisterAdminRoutes，需要单独挂载，
// 也就需要单独断言 —— 此前正是这里漏挂，导致只读账号可读取未脱敏的支付渠道配置、
// 并能发起退款。
func TestReadonlyAdminGuardIsMountedOnPaymentRoutes(t *testing.T) {
	readonly := newAdminRouterWithRole(service.RoleReadonlyAdmin)
	for _, tc := range []struct{ method, path string }{
		{http.MethodGet, "/api/v1/admin/payment/config"}, // 未脱敏的支付渠道配置
		{http.MethodPut, "/api/v1/admin/payment/config"}, // 写
		{http.MethodGet, "/api/v1/admin/payment/orders"}, // 其他客户的订单
		{http.MethodPost, "/api/v1/admin/payment/orders/1/refund"},
		{http.MethodGet, "/api/v1/admin/payment/providers"}, // 渠道凭据
	} {
		require.Equalf(t, http.StatusForbidden, statusFor(t, readonly, tc.method, tc.path),
			"%s %s 未被拦截 —— payment 管理端路由组缺少 ReadonlyAdminGuard", tc.method, tc.path)
	}

	require.NotEqual(t, http.StatusForbidden,
		statusFor(t, newAdminRouterWithRole(service.RoleAdmin), http.MethodGet, "/api/v1/admin/payment/config"),
		"普通 admin 被 ReadonlyAdminGuard 误伤")
}

// TestReadonlyAdminAllowlistDoesNotOverlapDenyPrefixes 锁定"整体拒绝的模块里没有放行项"
// 这条不变量。
//
// 若某天有人往当前被整体拒绝的模块（如 proxies）里加了一条白名单端点，该模块就悄悄
// 变成了"部分放行"，却仍以前缀形式登记在 reviewedDenyPrefixes 中 —— 于是模块内新增
// 路由再次自动免于分类，正是已修复的那个 bug 从反方向复活。
// 触发本测试时的正确做法：把该模块根加入 partiallyAllowedModuleRoots，从
// reviewedDenyPrefixes 移除，并把模块内的拒绝项逐条写入 reviewedDenyExact。
func TestReadonlyAdminAllowlistDoesNotOverlapDenyPrefixes(t *testing.T) {
	for _, key := range servermiddleware.ReadonlyAdminAllowlistKeys() {
		parts := strings.SplitN(key, " ", 2)
		require.Lenf(t, parts, 2, "白名单条目格式应为 \"METHOD /path\"，实际为 %q", key)
		for _, prefix := range reviewedDenyPrefixes {
			require.Falsef(t, strings.HasPrefix(parts[1], prefix),
				"白名单端点 %q 落在了整体拒绝前缀 %q 之下。\n"+
					"该模块已变成『部分放行』，必须改用精确登记：把 %q 加入 partiallyAllowedModuleRoots，\n"+
					"从 reviewedDenyPrefixes 移除，模块内的拒绝项逐条写入 reviewedDenyExact。",
				key, prefix, prefix)
		}
	}
}

// TestReadonlyAdminDenyPrefixesAreAllLive 防止拒绝前缀变成"看着已审阅、实则谁也保护
// 不了"的死条目。
//
// 一条匹配不到任何已注册路由的前缀有两种成因，都需要人处理：
// (1) 模块已被删除或改名 —— 该条目应清理；
// (2) 该模块的路由压根没被 newFullAdminRouter 枚举到 —— 说明它在某个独立的路由组里
//
//	注册（payment 就是这样），完备性测试对它完全失明，必须补注册。
//
// 成因 (2) 正是支付管理端漏挂守卫时的现场：/api/v1/admin/payment 当时匹配 0 条路由。
func TestReadonlyAdminDenyPrefixesAreAllLive(t *testing.T) {
	routes := newFullAdminRouter().Routes()
	var dead []string
	for _, prefix := range reviewedDenyPrefixes {
		matched := false
		for _, r := range routes {
			if strings.HasPrefix(r.Path, prefix) {
				matched = true
				break
			}
		}
		if !matched {
			dead = append(dead, prefix)
		}
	}
	sort.Strings(dead)
	require.Emptyf(t, dead,
		"以下 reviewedDenyPrefixes 条目匹配不到任何已注册路由：\n  %s\n"+
			"要么该模块已删除/改名（清理掉），要么它的路由注册在独立的路由组里、\n"+
			"newFullAdminRouter 没有枚举到（补注册，否则整个模块对本测试隐身）。",
		strings.Join(dead, "\n  "))
}
