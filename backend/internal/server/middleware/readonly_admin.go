package middleware

import (
	"net/http"

	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
)

// readonlyAdminAllowlist 列出 readonly_admin 角色可访问的管理端端点。
//
// key 形如 "GET /api/v1/admin/accounts/:id"，即 HTTP 方法 + gin 的路由模板
// (c.FullPath())。用模板而非实际 URL，使 /accounts/1 与 /accounts/2 归一到同一
// 条目，避免路径参数绕过。
//
// 这是一张【默认拒绝】的白名单，逐条枚举，禁止通配符或前缀匹配：前缀匹配会让
// 未来新增的写端点自动获得放行。routes 包的完备性测试
// (readonly_admin_coverage_test.go) 会遍历全部已注册的 admin 路由，断言每条要么
// 在本表中、要么在该测试的显式拒绝清单中，两者皆无则测试失败。
//
// 新增管理端端点时，必须在这两处之一登记，否则 CI 红。
var readonlyAdminAllowlist = map[string]struct{}{
	// ---- 上游账号（凭据已由 dto.RedactCredentials 脱敏）----
	"GET /api/v1/admin/accounts":                        {},
	"GET /api/v1/admin/accounts/:id":                    {},
	"GET /api/v1/admin/accounts/:id/stats":              {},
	"GET /api/v1/admin/accounts/:id/usage":              {},
	"GET /api/v1/admin/accounts/:id/today-stats":        {},
	"GET /api/v1/admin/accounts/:id/models":             {},
	"GET /api/v1/admin/accounts/:id/temp-unschedulable": {},
	"GET /api/v1/admin/accounts/:id/stats/overview":     {},
	"GET /api/v1/admin/accounts/:id/stats/ips":          {},
	"GET /api/v1/admin/accounts/:id/stats/users":        {},
	// 账号列表页依赖该端点渲染今日统计。它是 POST，但语义为批量读。
	"POST /api/v1/admin/accounts/today-stats/batch": {},

	// ---- 分组与倍率 ----
	"GET /api/v1/admin/groups":                                {},
	"GET /api/v1/admin/groups/all":                            {},
	"GET /api/v1/admin/groups/usage-summary":                  {},
	"GET /api/v1/admin/groups/capacity-summary":               {},
	"GET /api/v1/admin/groups/live-capability":                {},
	"GET /api/v1/admin/groups/:id":                            {},
	"GET /api/v1/admin/groups/:id/stats":                      {},
	"GET /api/v1/admin/groups/:id/rate-multipliers":           {},
	"GET /api/v1/admin/groups/:id/composite-routes":           {},
	"GET /api/v1/admin/groups/:id/model-allowlist-candidates": {},
	"GET /api/v1/admin/groups/:id/stats/overview":             {},
	"GET /api/v1/admin/groups/:id/stats/ips":                  {},
	"GET /api/v1/admin/groups/:id/stats/users":                {},
	"GET /api/v1/admin/groups/:id/stats/accounts":             {},

	// ---- 渠道与模型定价 ----
	"GET /api/v1/admin/channels":               {},
	"GET /api/v1/admin/channels/:id":           {},
	"GET /api/v1/admin/channels/model-pricing": {},

	// ---- 用户（handler 层强制裁剪为仅自身那条，见 user_handler.go）----
	"GET /api/v1/admin/users":     {},
	"GET /api/v1/admin/users/:id": {},

	// ---- 运维监控（读取端点；写端点不在表中，自动 403）----
	"GET /api/v1/admin/ops/concurrency":                        {},
	"GET /api/v1/admin/ops/user-concurrency":                   {},
	"GET /api/v1/admin/ops/account-availability":               {},
	"GET /api/v1/admin/ops/realtime-traffic":                   {},
	"GET /api/v1/admin/ops/alert-rules":                        {},
	"GET /api/v1/admin/ops/alert-events":                       {},
	"GET /api/v1/admin/ops/alert-events/:id":                   {},
	"GET /api/v1/admin/ops/email-notification/config":          {},
	"GET /api/v1/admin/ops/runtime/alert":                      {},
	"GET /api/v1/admin/ops/runtime/logging":                    {},
	"GET /api/v1/admin/ops/advanced-settings":                  {},
	"GET /api/v1/admin/ops/performance-settings":               {},
	"GET /api/v1/admin/ops/settings/metric-thresholds":         {},
	"GET /api/v1/admin/ops/ws/qps":                             {},
	"GET /api/v1/admin/ops/errors":                             {},
	"GET /api/v1/admin/ops/errors/:id":                         {},
	"GET /api/v1/admin/ops/request-errors":                     {},
	"GET /api/v1/admin/ops/request-errors/:id":                 {},
	"GET /api/v1/admin/ops/request-errors/:id/upstream-errors": {},
	"GET /api/v1/admin/ops/ingress-rejections":                 {},
	"GET /api/v1/admin/ops/ingress-rejections/health":          {},
	"GET /api/v1/admin/ops/auth-cache-invalidation/health":     {},
	"GET /api/v1/admin/ops/upstream-errors":                    {},
	"GET /api/v1/admin/ops/upstream-errors/:id":                {},
	"GET /api/v1/admin/ops/requests":                           {},
	"GET /api/v1/admin/ops/system-logs":                        {},
	"GET /api/v1/admin/ops/system-logs/health":                 {},
	"GET /api/v1/admin/ops/dashboard/snapshot-v2":              {},
	"GET /api/v1/admin/ops/dashboard/overview":                 {},
	"GET /api/v1/admin/ops/dashboard/throughput-trend":         {},
	"GET /api/v1/admin/ops/dashboard/latency-histogram":        {},
	"GET /api/v1/admin/ops/dashboard/error-trend":              {},
	"GET /api/v1/admin/ops/dashboard/error-distribution":       {},
	"GET /api/v1/admin/ops/dashboard/openai-token-stats":       {},
}

// ReadonlyAdminGuard 对 readonly_admin 角色强制执行白名单授权。
//
// 必须挂在 adminAuth 之后（依赖 context 中的 role），且挂在 auditLog 之后
// （使被拒绝的尝试也进入审计日志）。对非 readonly_admin 角色直接放行，零影响。
func ReadonlyAdminGuard() gin.HandlerFunc {
	return func(c *gin.Context) {
		role, ok := GetUserRoleFromContext(c)
		if !ok || role != service.RoleReadonlyAdmin {
			c.Next()
			return
		}
		if _, allowed := readonlyAdminAllowlist[c.Request.Method+" "+c.FullPath()]; !allowed {
			AbortWithError(c, http.StatusForbidden, "FORBIDDEN",
				"Read-only account cannot perform this action")
			return
		}
		c.Next()
	}
}

// ReadonlyAdminAllowlistKeys 返回白名单中全部 "METHOD FullPath" 条目的副本。
// 供 routes 包的完备性测试使用；返回副本以防调用方篡改白名单。
func ReadonlyAdminAllowlistKeys() []string {
	keys := make([]string, 0, len(readonlyAdminAllowlist))
	for k := range readonlyAdminAllowlist {
		keys = append(keys, k)
	}
	return keys
}
