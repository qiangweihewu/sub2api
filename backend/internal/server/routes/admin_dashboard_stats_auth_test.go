package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/config"
	"github.com/Wei-Shaw/sub2api/internal/handler"
	"github.com/Wei-Shaw/sub2api/internal/handler/admin"
	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/repository"
	servermiddleware "github.com/Wei-Shaw/sub2api/internal/server/middleware"
	"github.com/Wei-Shaw/sub2api/internal/service"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// dashboardStatsRoutes is the canonical set of 7 dashboard stats endpoints
// registered by registerDashboardStatsRoutes. These are the routes the test
// verifies are admin-gated.
var dashboardStatsRoutes = []struct {
	method string
	path   string
}{
	{http.MethodGet, "/api/v1/admin/accounts/42/stats/overview"},
	{http.MethodGet, "/api/v1/admin/accounts/42/stats/ips"},
	{http.MethodGet, "/api/v1/admin/accounts/42/stats/users"},
	{http.MethodGet, "/api/v1/admin/groups/7/stats/overview"},
	{http.MethodGet, "/api/v1/admin/groups/7/stats/ips"},
	{http.MethodGet, "/api/v1/admin/groups/7/stats/users"},
	{http.MethodGet, "/api/v1/admin/groups/7/stats/accounts"},
}

// TestAdminDashboardStatsRoutesRunAdminMiddleware proves that every dashboard
// stats route is registered under an admin-gated group by swapping in a spy
// middleware and asserting it fires for each route.
//
// This is a route-inspection test (deliberately less invasive than a full
// JWT/DB stack). It catches the common "forgot to put the new routes inside
// the admin group" class of bug at the route-table wiring level.
func TestAdminDashboardStatsRoutesRunAdminMiddleware(t *testing.T) {
	gin.SetMode(gin.TestMode)

	var calls int32
	spy := servermiddleware.AdminAuthMiddleware(func(c *gin.Context) {
		atomic.AddInt32(&calls, 1)
		c.Next()
	})

	router := gin.New()
	v1 := router.Group("/api/v1")

	h := &handler.Handlers{
		Admin: &handler.AdminHandlers{
			DashboardStats: admin.NewDashboardStatsHandler(noopStatsRepo{}),
		},
	}
	RegisterAdminRoutes(v1, h, spy)

	// Assert all 7 routes are present in the route table, nested under
	// /api/v1/admin where the admin middleware is applied.
	registered := make(map[string]bool, len(router.Routes()))
	for _, r := range router.Routes() {
		registered[r.Method+" "+r.Path] = true
	}
	wantRoutes := []string{
		"GET /api/v1/admin/accounts/:id/stats/overview",
		"GET /api/v1/admin/accounts/:id/stats/ips",
		"GET /api/v1/admin/accounts/:id/stats/users",
		"GET /api/v1/admin/groups/:id/stats/overview",
		"GET /api/v1/admin/groups/:id/stats/ips",
		"GET /api/v1/admin/groups/:id/stats/users",
		"GET /api/v1/admin/groups/:id/stats/accounts",
	}
	for _, want := range wantRoutes {
		require.Truef(t, registered[want], "route not registered: %s", want)
	}

	// Hit each dashboard stats route and confirm the spy middleware ran.
	for _, r := range dashboardStatsRoutes {
		atomic.StoreInt32(&calls, 0)
		w := httptest.NewRecorder()
		router.ServeHTTP(w, httptest.NewRequest(r.method, r.path, nil))
		require.Equalf(t, int32(1), atomic.LoadInt32(&calls),
			"admin middleware did not run for %s %s", r.method, r.path)
	}
}

// TestAdminDashboardStatsRoutesRejectUnauthenticated wires the real admin
// middleware (JWT path) and confirms dashboard stats routes reject unauthed
// and non-admin requests with 401/403 respectively, and allow admins through
// with 200.
//
// Scaffolding is intentionally minimal: a stub UserRepository is sufficient to
// drive NewUserService + NewAdminAuthMiddleware; the SettingService arg is nil
// because we exercise only the JWT path (no x-api-key header).
func TestAdminDashboardStatsRoutesRejectUnauthenticated(t *testing.T) {
	gin.SetMode(gin.TestMode)

	cfg := &config.Config{JWT: config.JWTConfig{Secret: "test-secret", ExpireHour: 1}}
	authService := service.NewAuthService(nil, nil, nil, nil, cfg, nil, nil, nil, nil, nil, nil)

	adminUser := &service.User{
		ID:           1,
		Email:        "admin@example.com",
		Role:         service.RoleAdmin,
		Status:       service.StatusActive,
		TokenVersion: 1,
		Concurrency:  1,
	}
	regularUser := &service.User{
		ID:           2,
		Email:        "user@example.com",
		Role:         service.RoleUser,
		Status:       service.StatusActive,
		TokenVersion: 1,
		Concurrency:  1,
	}

	userRepo := &statsAuthUserRepo{
		users: map[int64]*service.User{
			adminUser.ID:   adminUser,
			regularUser.ID: regularUser,
		},
	}
	userService := service.NewUserService(userRepo, nil, nil, nil)
	adminMW := servermiddleware.NewAdminAuthMiddleware(authService, userService, nil)

	router := gin.New()
	v1 := router.Group("/api/v1")
	h := &handler.Handlers{
		Admin: &handler.AdminHandlers{
			DashboardStats: admin.NewDashboardStatsHandler(noopStatsRepo{}),
		},
	}
	RegisterAdminRoutes(v1, h, adminMW)

	adminToken, err := authService.GenerateToken(adminUser)
	require.NoError(t, err)
	userToken, err := authService.GenerateToken(regularUser)
	require.NoError(t, err)

	for _, r := range dashboardStatsRoutes {
		t.Run(r.method+" "+r.path, func(t *testing.T) {
			// No auth header → 401
			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(r.method, r.path, nil))
			require.Equal(t, http.StatusUnauthorized, w.Code)
			require.True(t,
				strings.Contains(w.Body.String(), "UNAUTHORIZED") ||
					strings.Contains(w.Body.String(), "Authorization required"),
				"missing-auth body should surface auth error, got %s", w.Body.String())

			// Non-admin JWT → 403
			w = httptest.NewRecorder()
			req := httptest.NewRequest(r.method, r.path, nil)
			req.Header.Set("Authorization", "Bearer "+userToken)
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusForbidden, w.Code)
			require.Contains(t, w.Body.String(), "FORBIDDEN")

			// Admin JWT → 200 (proves the middleware lets admins through and
			// the downstream handler actually runs; uses the noop stats repo).
			w = httptest.NewRecorder()
			req = httptest.NewRequest(r.method, r.path, nil)
			req.Header.Set("Authorization", "Bearer "+adminToken)
			router.ServeHTTP(w, req)
			require.Equal(t, http.StatusOK, w.Code,
				"admin request should pass the middleware: body=%s", w.Body.String())
		})
	}
}

// noopStatsRepo is the smallest admin.StatsRepo that lets the handler return
// 200 for every route. Enough to exercise the middleware chain end-to-end.
type noopStatsRepo struct{}

func (noopStatsRepo) Overview(_ context.Context, _ repository.StatsFilter) (*repository.Overview, error) {
	return &repository.Overview{}, nil
}

func (noopStatsRepo) IPBreakdown(_ context.Context, _ repository.StatsFilter, _ int) ([]repository.IPBreakdownRow, error) {
	return nil, nil
}

func (noopStatsRepo) UserBreakdown(_ context.Context, _ repository.StatsFilter, _ int) ([]repository.UserBreakdownRow, error) {
	return nil, nil
}

func (noopStatsRepo) AccountBreakdown(_ context.Context, _ int64, _, _ time.Time) ([]repository.AccountBreakdownRow, error) {
	return nil, nil
}

// statsAuthUserRepo is the minimum UserRepository surface needed to drive
// NewUserService + NewAdminAuthMiddleware. Only GetByID is exercised by the
// admin auth middleware's JWT path; every other method panics if touched so
// accidental reliance is caught loudly.
type statsAuthUserRepo struct {
	users map[int64]*service.User
}

func (s *statsAuthUserRepo) GetByID(_ context.Context, id int64) (*service.User, error) {
	u, ok := s.users[id]
	if !ok {
		return nil, service.ErrUserNotFound
	}
	clone := *u
	return &clone, nil
}

func (s *statsAuthUserRepo) Create(context.Context, *service.User) error {
	panic("unexpected Create call")
}
func (s *statsAuthUserRepo) GetByEmail(context.Context, string) (*service.User, error) {
	panic("unexpected GetByEmail call")
}
func (s *statsAuthUserRepo) GetFirstAdmin(context.Context) (*service.User, error) {
	panic("unexpected GetFirstAdmin call")
}
func (s *statsAuthUserRepo) Update(context.Context, *service.User) error {
	panic("unexpected Update call")
}
func (s *statsAuthUserRepo) Delete(context.Context, int64) error {
	panic("unexpected Delete call")
}
func (s *statsAuthUserRepo) List(context.Context, pagination.PaginationParams) ([]service.User, *pagination.PaginationResult, error) {
	panic("unexpected List call")
}
func (s *statsAuthUserRepo) ListWithFilters(context.Context, pagination.PaginationParams, service.UserListFilters) ([]service.User, *pagination.PaginationResult, error) {
	panic("unexpected ListWithFilters call")
}
func (s *statsAuthUserRepo) UpdateBalance(context.Context, int64, float64) error {
	panic("unexpected UpdateBalance call")
}
func (s *statsAuthUserRepo) DeductBalance(context.Context, int64, float64) error {
	panic("unexpected DeductBalance call")
}
func (s *statsAuthUserRepo) UpdateConcurrency(context.Context, int64, int) error {
	panic("unexpected UpdateConcurrency call")
}
func (s *statsAuthUserRepo) ExistsByEmail(context.Context, string) (bool, error) {
	panic("unexpected ExistsByEmail call")
}
func (s *statsAuthUserRepo) RemoveGroupFromAllowedGroups(context.Context, int64) (int64, error) {
	panic("unexpected RemoveGroupFromAllowedGroups call")
}
func (s *statsAuthUserRepo) RemoveGroupFromUserAllowedGroups(context.Context, int64, int64) error {
	panic("unexpected RemoveGroupFromUserAllowedGroups call")
}
func (s *statsAuthUserRepo) AddGroupToAllowedGroups(context.Context, int64, int64) error {
	panic("unexpected AddGroupToAllowedGroups call")
}
func (s *statsAuthUserRepo) UpdateTotpSecret(context.Context, int64, *string) error {
	panic("unexpected UpdateTotpSecret call")
}
func (s *statsAuthUserRepo) EnableTotp(context.Context, int64) error {
	panic("unexpected EnableTotp call")
}
func (s *statsAuthUserRepo) DisableTotp(context.Context, int64) error {
	panic("unexpected DisableTotp call")
}

func (s *statsAuthUserRepo) GetUserAvatar(context.Context, int64) (*service.UserAvatar, error) {
	// Auth middleware hydrates avatars on GetByID; return "no avatar" rather than panic.
	return nil, nil
}

func (s *statsAuthUserRepo) UpsertUserAvatar(context.Context, int64, service.UpsertUserAvatarInput) (*service.UserAvatar, error) {
	panic("unexpected UpsertUserAvatar call")
}

func (s *statsAuthUserRepo) DeleteUserAvatar(context.Context, int64) error {
	panic("unexpected DeleteUserAvatar call")
}

func (s *statsAuthUserRepo) GetLatestUsedAtByUserIDs(context.Context, []int64) (map[int64]*time.Time, error) {
	panic("unexpected GetLatestUsedAtByUserIDs call")
}

func (s *statsAuthUserRepo) GetLatestUsedAtByUserID(context.Context, int64) (*time.Time, error) {
	panic("unexpected GetLatestUsedAtByUserID call")
}

func (s *statsAuthUserRepo) UpdateUserLastActiveAt(context.Context, int64, time.Time) error {
	panic("unexpected UpdateUserLastActiveAt call")
}

func (s *statsAuthUserRepo) ListUserAuthIdentities(context.Context, int64) ([]service.UserAuthIdentityRecord, error) {
	panic("unexpected ListUserAuthIdentities call")
}

func (s *statsAuthUserRepo) UnbindUserAuthProvider(context.Context, int64, string) error {
	panic("unexpected UnbindUserAuthProvider call")
}
