// Package repository contains data-access layer code for sub2api.
//
// dashboard_stats_repo.go provides DashboardStatsRepo — a single repository
// that serves both the per-account admin dashboard
// (/admin/accounts/:id/dashboard) and the per-group admin dashboard
// (/admin/groups/:id/dashboard). Scope selection is carried by StatsFilter:
// exactly one of AccountID or GroupID must be set, plus a [From, To] time
// window. Queries rely on the composite indexes
// usage_logs(account_id, created_at) and usage_logs(group_id, created_at).
package repository

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"

	dbent "github.com/Wei-Shaw/sub2api/ent"
	dbusagelog "github.com/Wei-Shaw/sub2api/ent/usagelog"
)

// StatsFilter selects a scope for dashboard queries. Exactly one of
// AccountID or GroupID must be set. The [From, To] time range is applied as
// created_at >= From AND created_at <= To.
type StatsFilter struct {
	AccountID *int64
	GroupID   *int64
	From      time.Time
	To        time.Time
}

// Validate returns an error if the filter is not usable: neither scope set,
// both scopes set, or an inverted time range.
func (f StatsFilter) Validate() error {
	hasAccount := f.AccountID != nil
	hasGroup := f.GroupID != nil
	switch {
	case !hasAccount && !hasGroup:
		return errors.New("dashboard stats: StatsFilter requires AccountID or GroupID")
	case hasAccount && hasGroup:
		return errors.New("dashboard stats: StatsFilter cannot set both AccountID and GroupID")
	}
	if f.From.IsZero() || f.To.IsZero() {
		return errors.New("dashboard stats: StatsFilter requires non-zero From and To")
	}
	if f.To.Before(f.From) {
		return errors.New("dashboard stats: StatsFilter To must be >= From")
	}
	return nil
}

// Overview holds aggregated usage metrics for a dashboard scope + window.
type Overview struct {
	RequestCount int64   `json:"request_count"`
	InputTokens  int64   `json:"input_tokens"`
	OutputTokens int64   `json:"output_tokens"`
	TotalCostUSD float64 `json:"total_cost_usd"`
	UniqueIPs    int     `json:"unique_ips"`
	UniqueUsers  int     `json:"unique_users"`
}

// DashboardStatsRepo aggregates usage_logs rows for the admin dashboards.
// It is safe for concurrent use.
type DashboardStatsRepo struct {
	client *dbent.Client
}

// NewDashboardStatsRepo builds a DashboardStatsRepo backed by the given Ent
// client.
func NewDashboardStatsRepo(client *dbent.Client) *DashboardStatsRepo {
	return &DashboardStatsRepo{client: client}
}

// baseQuery builds a UsageLog query with the filter's scope predicate
// (account OR group) and the [From, To] window applied. The filter MUST have
// already been validated by the caller.
func (r *DashboardStatsRepo) baseQuery(f StatsFilter) *dbent.UsageLogQuery {
	q := r.client.UsageLog.Query().Where(
		dbusagelog.CreatedAtGTE(f.From),
		dbusagelog.CreatedAtLTE(f.To),
	)
	if f.AccountID != nil {
		q = q.Where(dbusagelog.AccountIDEQ(*f.AccountID))
	}
	if f.GroupID != nil {
		q = q.Where(dbusagelog.GroupIDEQ(*f.GroupID))
	}
	return q
}

// Overview returns aggregated Overview metrics for the filter's scope and
// time window.
//
// Implementation note: we run the aggregates as separate queries instead of a
// single Aggregate(...).Scan(...) call to avoid Ent struct-tag/alias
// mismatches between dialects. Dashboard windows are bounded, so the extra
// round-trips are acceptable.
func (r *DashboardStatsRepo) Overview(ctx context.Context, f StatsFilter) (*Overview, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}

	// 1) Request count.
	count, err := r.baseQuery(f).Count(ctx)
	if err != nil {
		return nil, fmt.Errorf("dashboard stats: count: %w", err)
	}

	// 2) Sum(input_tokens).
	var sumInput []struct {
		Sum int64 `json:"sum"`
	}
	if err := r.baseQuery(f).
		Aggregate(dbent.As(dbent.Sum(dbusagelog.FieldInputTokens), "sum")).
		Scan(ctx, &sumInput); err != nil {
		return nil, fmt.Errorf("dashboard stats: sum input_tokens: %w", err)
	}

	// 3) Sum(output_tokens).
	var sumOutput []struct {
		Sum int64 `json:"sum"`
	}
	if err := r.baseQuery(f).
		Aggregate(dbent.As(dbent.Sum(dbusagelog.FieldOutputTokens), "sum")).
		Scan(ctx, &sumOutput); err != nil {
		return nil, fmt.Errorf("dashboard stats: sum output_tokens: %w", err)
	}

	// 4) Sum(total_cost).
	var sumCost []struct {
		Sum float64 `json:"sum"`
	}
	if err := r.baseQuery(f).
		Aggregate(dbent.As(dbent.Sum(dbusagelog.FieldTotalCost), "sum")).
		Scan(ctx, &sumCost); err != nil {
		return nil, fmt.Errorf("dashboard stats: sum total_cost: %w", err)
	}

	// 5) Distinct IP addresses (len of GroupBy result).
	ips, err := r.baseQuery(f).
		GroupBy(dbusagelog.FieldIPAddress).
		Strings(ctx)
	if err != nil {
		return nil, fmt.Errorf("dashboard stats: distinct ip_address: %w", err)
	}

	// 6) Distinct api_key_id values (len of GroupBy result).
	apiKeyIDs, err := r.baseQuery(f).
		GroupBy(dbusagelog.FieldAPIKeyID).
		Ints(ctx)
	if err != nil {
		return nil, fmt.Errorf("dashboard stats: distinct api_key_id: %w", err)
	}

	ov := &Overview{
		RequestCount: int64(count),
		UniqueIPs:    len(ips),
		UniqueUsers:  len(apiKeyIDs),
	}
	if len(sumInput) > 0 {
		ov.InputTokens = sumInput[0].Sum
	}
	if len(sumOutput) > 0 {
		ov.OutputTokens = sumOutput[0].Sum
	}
	if len(sumCost) > 0 {
		ov.TotalCostUSD = sumCost[0].Sum
	}
	return ov, nil
}

// IPBreakdownRow is one row of per-IP aggregates produced by IPBreakdown.
type IPBreakdownRow struct {
	IPAddress    string    `json:"ip_address"`
	RequestCount int64     `json:"request_count"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	TotalCostUSD float64   `json:"total_cost_usd"`
	FirstSeenAt  time.Time `json:"first_seen_at"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	UniqueUsers  int       `json:"unique_users"`
}

// IPBreakdown returns per-IP aggregates for the given scope + window, sorted
// by RequestCount descending and trimmed to limit. If limit <= 0 or > 500 it
// is reset to 100. Empty IP addresses are excluded.
//
// Implementation note: same as Overview(), aggregates are issued as separate
// queries to sidestep Ent struct-tag/alias mismatches between dialects. A
// follow-up GroupBy runs per surviving row to count distinct api_key_id. The
// known NULL-api_key_id undercount is accepted here (see Task 2 review).
func (r *DashboardStatsRepo) IPBreakdown(ctx context.Context, f StatsFilter, limit int) ([]IPBreakdownRow, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	// 1) Distinct IPs (excluding empty) with request count via GroupBy+Count.
	type ipCountRow struct {
		IPAddress string `json:"ip_address"`
		Count     int64  `json:"count"`
	}
	var ipCounts []ipCountRow
	if err := r.baseQuery(f).
		Where(dbusagelog.IPAddressNEQ("")).
		GroupBy(dbusagelog.FieldIPAddress).
		Aggregate(dbent.As(dbent.Count(), "count")).
		Scan(ctx, &ipCounts); err != nil {
		return nil, fmt.Errorf("dashboard stats: ip group+count: %w", err)
	}

	rows := make([]IPBreakdownRow, 0, len(ipCounts))
	for _, ic := range ipCounts {
		rows = append(rows, IPBreakdownRow{
			IPAddress:    ic.IPAddress,
			RequestCount: ic.Count,
		})
	}

	// 2) Sort by RequestCount desc, then trim to limit before issuing further
	// per-IP queries to bound work.
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].RequestCount > rows[j].RequestCount
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}

	// 3) For each surviving row, fetch per-IP aggregates (token sums, cost,
	// first/last seen, unique users) via scoped queries.
	for i := range rows {
		ip := rows[i].IPAddress
		scoped := r.baseQuery(f).Where(dbusagelog.IPAddressEQ(ip))

		// Sum(input_tokens).
		var sumInput []struct {
			Sum int64 `json:"sum"`
		}
		if err := r.baseQuery(f).Where(dbusagelog.IPAddressEQ(ip)).
			Aggregate(dbent.As(dbent.Sum(dbusagelog.FieldInputTokens), "sum")).
			Scan(ctx, &sumInput); err != nil {
			return nil, fmt.Errorf("dashboard stats: ip %q sum input_tokens: %w", ip, err)
		}
		if len(sumInput) > 0 {
			rows[i].InputTokens = sumInput[0].Sum
		}

		// Sum(output_tokens).
		var sumOutput []struct {
			Sum int64 `json:"sum"`
		}
		if err := r.baseQuery(f).Where(dbusagelog.IPAddressEQ(ip)).
			Aggregate(dbent.As(dbent.Sum(dbusagelog.FieldOutputTokens), "sum")).
			Scan(ctx, &sumOutput); err != nil {
			return nil, fmt.Errorf("dashboard stats: ip %q sum output_tokens: %w", ip, err)
		}
		if len(sumOutput) > 0 {
			rows[i].OutputTokens = sumOutput[0].Sum
		}

		// Sum(total_cost).
		var sumCost []struct {
			Sum float64 `json:"sum"`
		}
		if err := r.baseQuery(f).Where(dbusagelog.IPAddressEQ(ip)).
			Aggregate(dbent.As(dbent.Sum(dbusagelog.FieldTotalCost), "sum")).
			Scan(ctx, &sumCost); err != nil {
			return nil, fmt.Errorf("dashboard stats: ip %q sum total_cost: %w", ip, err)
		}
		if len(sumCost) > 0 {
			rows[i].TotalCostUSD = sumCost[0].Sum
		}

		// Min(created_at) -> FirstSeenAt.
		first, err := scoped.Clone().Order(dbent.Asc(dbusagelog.FieldCreatedAt)).First(ctx)
		if err != nil {
			return nil, fmt.Errorf("dashboard stats: ip %q first_seen_at: %w", ip, err)
		}
		rows[i].FirstSeenAt = first.CreatedAt

		// Max(created_at) -> LastSeenAt.
		last, err := scoped.Clone().Order(dbent.Desc(dbusagelog.FieldCreatedAt)).First(ctx)
		if err != nil {
			return nil, fmt.Errorf("dashboard stats: ip %q last_seen_at: %w", ip, err)
		}
		rows[i].LastSeenAt = last.CreatedAt

		// Distinct api_key_id count.
		apiKeyIDs, err := r.baseQuery(f).Where(dbusagelog.IPAddressEQ(ip)).
			GroupBy(dbusagelog.FieldAPIKeyID).
			Ints(ctx)
		if err != nil {
			return nil, fmt.Errorf("dashboard stats: ip %q distinct api_key_id: %w", ip, err)
		}
		rows[i].UniqueUsers = len(apiKeyIDs)
	}

	return rows, nil
}
