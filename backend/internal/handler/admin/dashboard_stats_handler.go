package admin

import (
	"context"
	"net/http"
	"strconv"
	"time"

	"github.com/Wei-Shaw/sub2api/internal/repository"

	"github.com/gin-gonic/gin"
)

// StatsRepo is the repository surface the dashboard stats handler needs.
// Declared here so tests can fake it without touching the repository package.
type StatsRepo interface {
	Overview(ctx context.Context, f repository.StatsFilter) (*repository.Overview, error)
	IPBreakdown(ctx context.Context, f repository.StatsFilter, limit int) ([]repository.IPBreakdownRow, error)
	UserBreakdown(ctx context.Context, f repository.StatsFilter, limit int) ([]repository.UserBreakdownRow, error)
	AccountBreakdown(ctx context.Context, groupID int64, from, to time.Time) ([]repository.AccountBreakdownRow, error)
}

// DashboardStatsHandler exposes per-account and per-group usage aggregates for
// the admin dashboards.
type DashboardStatsHandler struct {
	repo StatsRepo
}

// NewDashboardStatsHandler constructs a DashboardStatsHandler.
func NewDashboardStatsHandler(repo StatsRepo) *DashboardStatsHandler {
	return &DashboardStatsHandler{repo: repo}
}

// statsParams is the normalized set of query inputs used by every dashboard
// stats endpoint.
type statsParams struct {
	ID    int64
	From  time.Time
	To    time.Time
	Limit int
}

// parseParams reads the :id path param plus optional from/to/limit query
// params. Defaults: to = now, from = now - 24h, limit = 100. Valid limit range
// is 1..500; out-of-range values fall back to 100. Malformed inputs return an
// error — callers should respond 400.
func parseParams(c *gin.Context) (statsParams, error) {
	var p statsParams

	id, err := strconv.ParseInt(c.Param("id"), 10, 64)
	if err != nil {
		return p, err
	}
	p.ID = id

	now := time.Now().UTC()
	p.To = now
	p.From = now.Add(-24 * time.Hour)

	if fromStr := c.Query("from"); fromStr != "" {
		t, err := time.Parse(time.RFC3339, fromStr)
		if err != nil {
			return p, err
		}
		p.From = t
	}
	if toStr := c.Query("to"); toStr != "" {
		t, err := time.Parse(time.RFC3339, toStr)
		if err != nil {
			return p, err
		}
		p.To = t
	}

	p.Limit = 100
	if limStr := c.Query("limit"); limStr != "" {
		n, err := strconv.Atoi(limStr)
		if err != nil {
			return p, err
		}
		if n >= 1 && n <= 500 {
			p.Limit = n
		}
	}

	return p, nil
}

// AccountOverview returns aggregated usage metrics for a single account over
// the requested window.
// GET /admin/accounts/:id/stats/overview?from=&to=
func (h *DashboardStatsHandler) AccountOverview(c *gin.Context) {
	p, err := parseParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id := p.ID
	ov, err := h.repo.Overview(c.Request.Context(), repository.StatsFilter{
		AccountID: &id,
		From:      p.From,
		To:        p.To,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, ov)
}

// AccountIPBreakdown returns per-IP aggregates for a single account.
// GET /admin/accounts/:id/stats/ips?from=&to=&limit=
func (h *DashboardStatsHandler) AccountIPBreakdown(c *gin.Context) {
	p, err := parseParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id := p.ID
	rows, err := h.repo.IPBreakdown(c.Request.Context(), repository.StatsFilter{
		AccountID: &id,
		From:      p.From,
		To:        p.To,
	}, p.Limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rows": rows})
}

// AccountUserBreakdown returns per-(api_key, user) aggregates for a single
// account.
// GET /admin/accounts/:id/stats/users?from=&to=&limit=
func (h *DashboardStatsHandler) AccountUserBreakdown(c *gin.Context) {
	p, err := parseParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id := p.ID
	rows, err := h.repo.UserBreakdown(c.Request.Context(), repository.StatsFilter{
		AccountID: &id,
		From:      p.From,
		To:        p.To,
	}, p.Limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rows": rows})
}

// GroupOverview returns aggregated usage metrics for a single group.
// GET /admin/groups/:id/stats/overview?from=&to=
func (h *DashboardStatsHandler) GroupOverview(c *gin.Context) {
	p, err := parseParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id := p.ID
	ov, err := h.repo.Overview(c.Request.Context(), repository.StatsFilter{
		GroupID: &id,
		From:    p.From,
		To:      p.To,
	})
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, ov)
}

// GroupIPBreakdown returns per-IP aggregates scoped to a single group.
// GET /admin/groups/:id/stats/ips?from=&to=&limit=
func (h *DashboardStatsHandler) GroupIPBreakdown(c *gin.Context) {
	p, err := parseParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id := p.ID
	rows, err := h.repo.IPBreakdown(c.Request.Context(), repository.StatsFilter{
		GroupID: &id,
		From:    p.From,
		To:      p.To,
	}, p.Limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rows": rows})
}

// GroupUserBreakdown returns per-(api_key, user) aggregates scoped to a single
// group.
// GET /admin/groups/:id/stats/users?from=&to=&limit=
func (h *DashboardStatsHandler) GroupUserBreakdown(c *gin.Context) {
	p, err := parseParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	id := p.ID
	rows, err := h.repo.UserBreakdown(c.Request.Context(), repository.StatsFilter{
		GroupID: &id,
		From:    p.From,
		To:      p.To,
	}, p.Limit)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rows": rows})
}

// GroupAccountBreakdown splits a group's consumption across the accounts that
// serve it.
// GET /admin/groups/:id/stats/accounts?from=&to=
func (h *DashboardStatsHandler) GroupAccountBreakdown(c *gin.Context) {
	p, err := parseParams(c)
	if err != nil {
		c.JSON(http.StatusBadRequest, gin.H{"error": err.Error()})
		return
	}
	rows, err := h.repo.AccountBreakdown(c.Request.Context(), p.ID, p.From, p.To)
	if err != nil {
		c.JSON(http.StatusInternalServerError, gin.H{"error": err.Error()})
		return
	}
	c.JSON(http.StatusOK, gin.H{"rows": rows})
}
