package admin

import (
	"context"
	"encoding/csv"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/pkg/pagination"
	"github.com/Wei-Shaw/sub2api/internal/pkg/usagestats"
	"github.com/Wei-Shaw/sub2api/internal/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

// adminUsageExportCSVRepo is a minimal repo stub that only implements the two
// methods the ExportCSV handler touches. Everything else stays embedded as nil
// via the underlying UsageLogRepository interface.
type adminUsageExportCSVRepo struct {
	service.UsageLogRepository
	total    int64
	logs     []service.UsageLog
	listErr  error
	statsErr error

	lastFilters usagestats.UsageLogFilters
	lastParams  pagination.PaginationParams
	listCalls   int
}

func (r *adminUsageExportCSVRepo) GetStatsWithFilters(_ context.Context, filters usagestats.UsageLogFilters) (*usagestats.UsageStats, error) {
	if r.statsErr != nil {
		return nil, r.statsErr
	}
	_ = filters
	return &usagestats.UsageStats{TotalRequests: r.total}, nil
}

func (r *adminUsageExportCSVRepo) ListWithFilters(_ context.Context, params pagination.PaginationParams, filters usagestats.UsageLogFilters) ([]service.UsageLog, *pagination.PaginationResult, error) {
	r.listCalls++
	r.lastFilters = filters
	r.lastParams = params
	if r.listErr != nil {
		return nil, nil, r.listErr
	}
	// Return everything on the first page, empty thereafter, to exercise the
	// "stop when repo runs out of rows" branch.
	if params.Page > 1 {
		return nil, &pagination.PaginationResult{Total: int64(len(r.logs)), Page: params.Page, PageSize: params.PageSize}, nil
	}
	return r.logs, &pagination.PaginationResult{
		Total:    int64(len(r.logs)),
		Page:     params.Page,
		PageSize: params.PageSize,
	}, nil
}

func newAdminUsageExportCSVRouter(repo *adminUsageExportCSVRepo) *gin.Engine {
	gin.SetMode(gin.TestMode)
	usageSvc := service.NewUsageService(repo, nil, nil, nil)
	handler := NewUsageHandler(usageSvc, nil, nil, nil)
	router := gin.New()
	router.GET("/admin/usage.csv", handler.ExportCSV)
	return router
}

func TestAdminUsageExportCSV_BadStartDate(t *testing.T) {
	repo := &adminUsageExportCSVRepo{}
	router := newAdminUsageExportCSVRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/admin/usage.csv?start_date=not-a-date", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "start_date")
	require.Equal(t, 0, repo.listCalls, "list should not run when param parsing fails")
}

func TestAdminUsageExportCSV_BadAccountID(t *testing.T) {
	repo := &adminUsageExportCSVRepo{}
	router := newAdminUsageExportCSVRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/admin/usage.csv?account_id=nope", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
}

func TestAdminUsageExportCSV_TooManyRows(t *testing.T) {
	repo := &adminUsageExportCSVRepo{total: adminUsageCSVMaxRows + 1}
	router := newAdminUsageExportCSVRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/admin/usage.csv", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusBadRequest, rec.Code)
	require.Contains(t, rec.Body.String(), "narrow the time range")
}

func TestAdminUsageExportCSV_Success(t *testing.T) {
	ip := "10.0.0.1"
	dur := 1234
	repo := &adminUsageExportCSVRepo{
		total: 2,
		logs: []service.UsageLog{
			{
				ID:                  1,
				UserID:              100,
				APIKeyID:            200,
				AccountID:           300,
				RequestID:           "req-one",
				Model:               "claude-3-5-sonnet",
				InputTokens:         10,
				OutputTokens:        20,
				CacheCreationTokens: 1,
				CacheReadTokens:     2,
				TotalCost:           0.0125,
				Stream:              true,
				DurationMs:          &dur,
				IPAddress:           &ip,
				CreatedAt:           time.Date(2026, 4, 17, 12, 0, 0, 0, time.UTC),
			},
			{
				ID:           2,
				UserID:       101,
				APIKeyID:     201,
				AccountID:    300,
				RequestID:    "req-two",
				Model:        "claude-3-opus",
				InputTokens:  5,
				OutputTokens: 7,
				TotalCost:    0.0042,
				CreatedAt:    time.Date(2026, 4, 17, 12, 5, 0, 0, time.UTC),
			},
		},
	}
	router := newAdminUsageExportCSVRouter(repo)

	req := httptest.NewRequest(http.MethodGet, "/admin/usage.csv?account_id=300&start_date=2026-04-17&end_date=2026-04-17", nil)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Header().Get("Content-Type"), "text/csv")
	cd := rec.Header().Get("Content-Disposition")
	require.True(t, strings.HasPrefix(cd, "attachment;"), "Content-Disposition should start with attachment; got %q", cd)
	require.Contains(t, cd, "usage-")
	require.Contains(t, cd, ".csv")

	reader := csv.NewReader(strings.NewReader(rec.Body.String()))
	rows, err := reader.ReadAll()
	require.NoError(t, err)
	require.GreaterOrEqual(t, len(rows), 2, "expected header plus at least one data row")

	header := rows[0]
	require.Equal(t, []string{
		"created_at",
		"account_id",
		"api_key_id",
		"user_id",
		"ip_address",
		"model",
		"input_tokens",
		"output_tokens",
		"cache_creation_tokens",
		"cache_read_tokens",
		"total_cost_usd",
		"duration_ms",
		"stream",
		"request_id",
	}, header)

	// Filters threaded through to the repo.
	require.Equal(t, int64(300), repo.lastFilters.AccountID)
	require.NotNil(t, repo.lastFilters.StartTime)
	require.NotNil(t, repo.lastFilters.EndTime)

	// First data row sanity check.
	first := rows[1]
	require.Equal(t, "300", first[1])
	require.Equal(t, "200", first[2])
	require.Equal(t, "100", first[3])
	require.Equal(t, "10.0.0.1", first[4])
	require.Equal(t, "claude-3-5-sonnet", first[5])
	require.Equal(t, "10", first[6])
	require.Equal(t, "20", first[7])
	require.Equal(t, "true", first[12])
	require.Equal(t, "req-one", first[13])
}
