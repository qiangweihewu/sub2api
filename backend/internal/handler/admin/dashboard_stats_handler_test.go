package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// Compile-time assertion that the concrete repository implementation
// satisfies the handler's StatsRepo interface. If the repo ever drifts, this
// line fails to compile, catching the mismatch before runtime.
var _ StatsRepo = (*repository.DashboardStatsRepo)(nil)

// fakeStatsRepo implements StatsRepo and captures the last call args so tests
// can assert the handler wired the filter, limit and time window correctly.
type fakeStatsRepo struct {
	// last call inputs
	lastFilter            repository.StatsFilter
	lastLimit             int
	lastAccountBreakGroup int64
	lastAccountBreakFrom  time.Time
	lastAccountBreakTo    time.Time

	// canned outputs
	overview         *repository.Overview
	ipRows           []repository.IPBreakdownRow
	userRows         []repository.UserBreakdownRow
	accountBreakRows []repository.AccountBreakdownRow
}

func (f *fakeStatsRepo) Overview(_ context.Context, filter repository.StatsFilter) (*repository.Overview, error) {
	f.lastFilter = filter
	return f.overview, nil
}

func (f *fakeStatsRepo) IPBreakdown(_ context.Context, filter repository.StatsFilter, limit int) ([]repository.IPBreakdownRow, error) {
	f.lastFilter = filter
	f.lastLimit = limit
	return f.ipRows, nil
}

func (f *fakeStatsRepo) UserBreakdown(_ context.Context, filter repository.StatsFilter, limit int) ([]repository.UserBreakdownRow, error) {
	f.lastFilter = filter
	f.lastLimit = limit
	return f.userRows, nil
}

func (f *fakeStatsRepo) AccountBreakdown(_ context.Context, groupID int64, from, to time.Time) ([]repository.AccountBreakdownRow, error) {
	f.lastAccountBreakGroup = groupID
	f.lastAccountBreakFrom = from
	f.lastAccountBreakTo = to
	return f.accountBreakRows, nil
}

// newDashboardStatsRouter registers the full route set on a bare gin engine.
// No middleware, no auth — tests drive the handler methods directly.
func newDashboardStatsRouter(repo StatsRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	r := gin.New()
	h := NewDashboardStatsHandler(repo)
	r.GET("/admin/accounts/:id/stats/overview", h.AccountOverview)
	r.GET("/admin/accounts/:id/stats/ips", h.AccountIPBreakdown)
	r.GET("/admin/accounts/:id/stats/users", h.AccountUserBreakdown)
	r.GET("/admin/groups/:id/stats/overview", h.GroupOverview)
	r.GET("/admin/groups/:id/stats/ips", h.GroupIPBreakdown)
	r.GET("/admin/groups/:id/stats/users", h.GroupUserBreakdown)
	r.GET("/admin/groups/:id/stats/accounts", h.GroupAccountBreakdown)
	return r
}

func TestDashboardStatsHandler_AccountOverview(t *testing.T) {
	want := &repository.Overview{
		RequestCount: 42,
		InputTokens:  1000,
		OutputTokens: 2000,
		TotalCostUSD: 0.5,
		UniqueIPs:    3,
		UniqueUsers:  2,
	}
	repo := &fakeStatsRepo{overview: want}
	r := newDashboardStatsRouter(repo)

	from := time.Date(2026, 4, 1, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 4, 2, 0, 0, 0, 0, time.UTC)
	url := "/admin/accounts/42/stats/overview?from=" + from.Format(time.RFC3339) + "&to=" + to.Format(time.RFC3339)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, url, nil))

	require.Equal(t, http.StatusOK, rec.Code)

	require.NotNil(t, repo.lastFilter.AccountID)
	require.Equal(t, int64(42), *repo.lastFilter.AccountID)
	require.Nil(t, repo.lastFilter.GroupID)
	require.True(t, from.Equal(repo.lastFilter.From), "from mismatch: got %v want %v", repo.lastFilter.From, from)
	require.True(t, to.Equal(repo.lastFilter.To), "to mismatch: got %v want %v", repo.lastFilter.To, to)

	var envelope struct {
		Code int                 `json:"code"`
		Data repository.Overview `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, 0, envelope.Code)
	require.Equal(t, *want, envelope.Data)
}

func TestDashboardStatsHandler_GroupOverview(t *testing.T) {
	want := &repository.Overview{RequestCount: 7}
	repo := &fakeStatsRepo{overview: want}
	r := newDashboardStatsRouter(repo)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/groups/7/stats/overview", nil))

	require.Equal(t, http.StatusOK, rec.Code)

	require.NotNil(t, repo.lastFilter.GroupID)
	require.Equal(t, int64(7), *repo.lastFilter.GroupID)
	require.Nil(t, repo.lastFilter.AccountID)

	var envelope struct {
		Code int                 `json:"code"`
		Data repository.Overview `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, 0, envelope.Code)
	require.Equal(t, *want, envelope.Data)
}

func TestDashboardStatsHandler_GroupAccountBreakdown(t *testing.T) {
	rows := []repository.AccountBreakdownRow{
		{AccountID: 11, RequestCount: 100},
		{AccountID: 12, RequestCount: 50},
	}
	repo := &fakeStatsRepo{accountBreakRows: rows}
	r := newDashboardStatsRouter(repo)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/groups/7/stats/accounts", nil))

	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, int64(7), repo.lastAccountBreakGroup)

	var envelope struct {
		Code int `json:"code"`
		Data struct {
			Rows []repository.AccountBreakdownRow `json:"rows"`
		} `json:"data"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &envelope))
	require.Equal(t, 0, envelope.Code)
	require.Len(t, envelope.Data.Rows, 2)
	require.Equal(t, int64(11), envelope.Data.Rows[0].AccountID)
	require.Equal(t, int64(12), envelope.Data.Rows[1].AccountID)
}

func TestDashboardStatsHandler_InvalidID(t *testing.T) {
	repo := &fakeStatsRepo{overview: &repository.Overview{}}
	r := newDashboardStatsRouter(repo)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/accounts/notanumber/stats/overview", nil))

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestDashboardStatsHandler_DefaultTimeWindow(t *testing.T) {
	repo := &fakeStatsRepo{overview: &repository.Overview{}}
	r := newDashboardStatsRouter(repo)

	before := time.Now().UTC()
	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/accounts/42/stats/overview", nil))
	after := time.Now().UTC()

	require.Equal(t, http.StatusOK, rec.Code)

	// Expected: to ~= now, from ~= now - 24h, bracketed by our before/after
	// observations with a couple seconds of tolerance for scheduling jitter.
	tol := 2 * time.Second
	require.WithinDuration(t, before, repo.lastFilter.To, after.Sub(before)+tol, "To should be roughly now")
	require.True(t, !repo.lastFilter.To.Before(before.Add(-tol)), "To before observation window")
	require.True(t, !repo.lastFilter.To.After(after.Add(tol)), "To after observation window")

	wantFrom := repo.lastFilter.To.Add(-24 * time.Hour)
	require.WithinDuration(t, wantFrom, repo.lastFilter.From, tol, "From should be To - 24h")
}

func TestDashboardStatsHandler_CustomLimit(t *testing.T) {
	repo := &fakeStatsRepo{
		ipRows:   []repository.IPBreakdownRow{},
		userRows: []repository.UserBreakdownRow{},
	}
	r := newDashboardStatsRouter(repo)

	rec := httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/accounts/42/stats/ips?limit=50", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 50, repo.lastLimit)

	repo.lastLimit = 0
	rec = httptest.NewRecorder()
	r.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/admin/accounts/42/stats/users?limit=50", nil))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, 50, repo.lastLimit)
}
