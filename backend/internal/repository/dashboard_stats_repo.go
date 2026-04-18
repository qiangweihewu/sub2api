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
// Implementation note: consolidated to 2 round-trips.
//   - Query 1: single Aggregate(...).Scan(...) producing the 4 numeric metrics
//     plus the row count, using As(...) aliases matched to JSON tags on the
//     scan target (same pattern the Breakdown methods use). For non-grouped
//     aggregates Ent returns a single row; we scan into a []struct of length 1
//     to stay robust across dialects.
//   - Query 2: single GROUP BY (ip_address, api_key_id) scan; distinct IPs
//     and distinct api_key_ids are counted in Go memory.
func (r *DashboardStatsRepo) Overview(ctx context.Context, f StatsFilter) (*Overview, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}

	// Query 1: numeric metrics + row count in one aggregate scan.
	type overviewAgg struct {
		RequestCount int64   `json:"request_count"`
		InputTokens  int64   `json:"input_tokens"`
		OutputTokens int64   `json:"output_tokens"`
		TotalCostUSD float64 `json:"total_cost_usd"`
	}
	var aggs []overviewAgg
	if err := r.baseQuery(f).
		Aggregate(
			dbent.As(dbent.Count(), "request_count"),
			dbent.As(dbent.Sum(dbusagelog.FieldInputTokens), "input_tokens"),
			dbent.As(dbent.Sum(dbusagelog.FieldOutputTokens), "output_tokens"),
			dbent.As(dbent.Sum(dbusagelog.FieldTotalCost), "total_cost_usd"),
		).Scan(ctx, &aggs); err != nil {
		return nil, fmt.Errorf("dashboard stats: overview aggregate scan: %w", err)
	}

	ov := &Overview{}
	if len(aggs) > 0 {
		ov.RequestCount = aggs[0].RequestCount
		ov.InputTokens = aggs[0].InputTokens
		ov.OutputTokens = aggs[0].OutputTokens
		ov.TotalCostUSD = aggs[0].TotalCostUSD
	}

	// Empty window short-circuit: skip the pairs query when there are no rows.
	if ov.RequestCount == 0 {
		return ov, nil
	}

	// Query 2: distinct (ip_address, api_key_id) pairs in scope. Unique IPs
	// and unique api_key_ids are derived in Go memory. Empty IP addresses are
	// excluded so they don't inflate UniqueIPs.
	var pairs []struct {
		IPAddress string `json:"ip_address"`
		APIKeyID  int64  `json:"api_key_id"`
	}
	if err := r.baseQuery(f).
		Where(dbusagelog.IPAddressNEQ("")).
		GroupBy(dbusagelog.FieldIPAddress, dbusagelog.FieldAPIKeyID).
		Scan(ctx, &pairs); err != nil {
		return nil, fmt.Errorf("dashboard stats: overview pairs: %w", err)
	}

	ipSet := make(map[string]struct{}, len(pairs))
	keySet := make(map[int64]struct{}, len(pairs))
	for _, p := range pairs {
		ipSet[p.IPAddress] = struct{}{}
		keySet[p.APIKeyID] = struct{}{}
	}
	ov.UniqueIPs = len(ipSet)
	ov.UniqueUsers = len(keySet)
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
// Implementation note: unlike Overview(), this method consolidates all
// per-IP aggregates (count, token sums, cost, min/max created_at) into a
// single GROUP BY ip_address scan using explicit Ent As(...) aliases with
// matching JSON tags on the scan target. A second query fetches distinct
// (ip, api_key_id) pairs for the trimmed set so UniqueUsers is computed in
// Go memory. Total round-trips: 2, regardless of N.
func (r *DashboardStatsRepo) IPBreakdown(ctx context.Context, f StatsFilter, limit int) ([]IPBreakdownRow, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	// Query 1: single GROUP BY ip_address producing all per-IP numeric
	// aggregates in one round-trip. Explicit As(...) aliases line up with
	// JSON tags on ipAgg so Scan decodes rows correctly across dialects.
	type ipAgg struct {
		IPAddress    string    `json:"ip_address"`
		RequestCount int64     `json:"request_count"`
		InputTokens  int64     `json:"input_tokens"`
		OutputTokens int64     `json:"output_tokens"`
		TotalCostUSD float64   `json:"total_cost_usd"`
		FirstSeenAt  time.Time `json:"first_seen_at"`
		LastSeenAt   time.Time `json:"last_seen_at"`
	}
	var aggs []ipAgg
	if err := r.baseQuery(f).
		Where(dbusagelog.IPAddressNEQ("")).
		GroupBy(dbusagelog.FieldIPAddress).
		Aggregate(
			dbent.As(dbent.Count(), "request_count"),
			dbent.As(dbent.Sum(dbusagelog.FieldInputTokens), "input_tokens"),
			dbent.As(dbent.Sum(dbusagelog.FieldOutputTokens), "output_tokens"),
			dbent.As(dbent.Sum(dbusagelog.FieldTotalCost), "total_cost_usd"),
			dbent.As(dbent.Min(dbusagelog.FieldCreatedAt), "first_seen_at"),
			dbent.As(dbent.Max(dbusagelog.FieldCreatedAt), "last_seen_at"),
		).Scan(ctx, &aggs); err != nil {
		return nil, fmt.Errorf("dashboard stats: ip aggregate scan: %w", err)
	}

	rows := make([]IPBreakdownRow, 0, len(aggs))
	for _, a := range aggs {
		rows = append(rows, IPBreakdownRow{
			IPAddress:    a.IPAddress,
			RequestCount: a.RequestCount,
			InputTokens:  a.InputTokens,
			OutputTokens: a.OutputTokens,
			TotalCostUSD: a.TotalCostUSD,
			FirstSeenAt:  a.FirstSeenAt,
			LastSeenAt:   a.LastSeenAt,
		})
	}

	// Sort by RequestCount desc, then trim to limit BEFORE issuing the
	// second query so the pairs query is bounded by limit, not N.
	sort.Slice(rows, func(i, j int) bool {
		return rows[i].RequestCount > rows[j].RequestCount
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}

	if len(rows) == 0 {
		return rows, nil
	}

	// Query 2: GROUP BY (ip_address, api_key_id) constrained to the trimmed
	// IP set. Counting distinct api_key_id per ip happens in Go memory. The
	// known NULL-api_key_id undercount is accepted here (see Task 2 review).
	topIPs := make([]string, len(rows))
	for i, r := range rows {
		topIPs[i] = r.IPAddress
	}
	var pairs []struct {
		IPAddress string `json:"ip_address"`
		APIKeyID  int64  `json:"api_key_id"`
	}
	if err := r.baseQuery(f).
		Where(
			dbusagelog.IPAddressNEQ(""),
			dbusagelog.IPAddressIn(topIPs...),
		).
		GroupBy(dbusagelog.FieldIPAddress, dbusagelog.FieldAPIKeyID).
		Scan(ctx, &pairs); err != nil {
		return nil, fmt.Errorf("dashboard stats: ip/api_key_id pairs: %w", err)
	}

	users := make(map[string]map[int64]struct{}, len(rows))
	for _, p := range pairs {
		set, ok := users[p.IPAddress]
		if !ok {
			set = make(map[int64]struct{})
			users[p.IPAddress] = set
		}
		set[p.APIKeyID] = struct{}{}
	}
	for i := range rows {
		rows[i].UniqueUsers = len(users[rows[i].IPAddress])
	}

	return rows, nil
}

// UserBreakdownRow is one row of per-(api_key, user) aggregates produced by
// UserBreakdown.
type UserBreakdownRow struct {
	APIKeyID     int64     `json:"api_key_id"`
	UserID       int64     `json:"user_id"`
	RequestCount int64     `json:"request_count"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	TotalCostUSD float64   `json:"total_cost_usd"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

// UserBreakdown returns per-(api_key, user) aggregates for the given scope +
// time window, sorted by RequestCount descending and trimmed to limit. If
// limit <= 0 or > 500 it is reset to 100.
//
// Implementation note: consolidates all per-(api_key_id, user_id) aggregates
// (count, token sums, cost, max created_at) into a single GROUP BY scan using
// explicit Ent As(...) aliases that line up with the JSON tags on the scan
// target. Total round-trips: 1, regardless of N.
func (r *DashboardStatsRepo) UserBreakdown(ctx context.Context, f StatsFilter, limit int) ([]UserBreakdownRow, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if limit <= 0 || limit > 500 {
		limit = 100
	}

	type userAgg struct {
		APIKeyID     int64     `json:"api_key_id"`
		UserID       int64     `json:"user_id"`
		RequestCount int64     `json:"request_count"`
		InputTokens  int64     `json:"input_tokens"`
		OutputTokens int64     `json:"output_tokens"`
		TotalCostUSD float64   `json:"total_cost_usd"`
		LastSeenAt   time.Time `json:"last_seen_at"`
	}
	var aggs []userAgg
	if err := r.baseQuery(f).
		GroupBy(dbusagelog.FieldAPIKeyID, dbusagelog.FieldUserID).
		Aggregate(
			dbent.As(dbent.Count(), "request_count"),
			dbent.As(dbent.Sum(dbusagelog.FieldInputTokens), "input_tokens"),
			dbent.As(dbent.Sum(dbusagelog.FieldOutputTokens), "output_tokens"),
			dbent.As(dbent.Sum(dbusagelog.FieldTotalCost), "total_cost_usd"),
			dbent.As(dbent.Max(dbusagelog.FieldCreatedAt), "last_seen_at"),
		).Scan(ctx, &aggs); err != nil {
		return nil, fmt.Errorf("dashboard stats: user aggregate scan: %w", err)
	}

	rows := make([]UserBreakdownRow, 0, len(aggs))
	for _, a := range aggs {
		rows = append(rows, UserBreakdownRow{
			APIKeyID:     a.APIKeyID,
			UserID:       a.UserID,
			RequestCount: a.RequestCount,
			InputTokens:  a.InputTokens,
			OutputTokens: a.OutputTokens,
			TotalCostUSD: a.TotalCostUSD,
			LastSeenAt:   a.LastSeenAt,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].RequestCount > rows[j].RequestCount
	})
	if len(rows) > limit {
		rows = rows[:limit]
	}

	return rows, nil
}

// AccountBreakdownRow is one row of per-account aggregates produced by
// AccountBreakdown.
type AccountBreakdownRow struct {
	AccountID    int64     `json:"account_id"`
	RequestCount int64     `json:"request_count"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
	TotalCostUSD float64   `json:"total_cost_usd"`
	LastSeenAt   time.Time `json:"last_seen_at"`
}

// AccountBreakdown splits a group's consumption across its accounts over the
// window. Sorted by RequestCount desc (no limit — number of accounts per group
// is expected to be small, typically 1–10).
//
// This is group-only and intentionally does not take a StatsFilter because the
// account_id dimension must be unconstrained. Total round-trips: 1.
func (r *DashboardStatsRepo) AccountBreakdown(ctx context.Context, groupID int64, from, to time.Time) ([]AccountBreakdownRow, error) {
	if groupID <= 0 {
		return nil, errors.New("dashboard stats: AccountBreakdown requires groupID > 0")
	}
	if to.Before(from) {
		return nil, errors.New("dashboard stats: AccountBreakdown to must be >= from")
	}

	type accountAgg struct {
		AccountID    int64     `json:"account_id"`
		RequestCount int64     `json:"request_count"`
		InputTokens  int64     `json:"input_tokens"`
		OutputTokens int64     `json:"output_tokens"`
		TotalCostUSD float64   `json:"total_cost_usd"`
		LastSeenAt   time.Time `json:"last_seen_at"`
	}
	var aggs []accountAgg
	if err := r.client.UsageLog.Query().
		Where(
			dbusagelog.GroupIDEQ(groupID),
			dbusagelog.CreatedAtGTE(from),
			dbusagelog.CreatedAtLTE(to),
		).
		GroupBy(dbusagelog.FieldAccountID).
		Aggregate(
			dbent.As(dbent.Count(), "request_count"),
			dbent.As(dbent.Sum(dbusagelog.FieldInputTokens), "input_tokens"),
			dbent.As(dbent.Sum(dbusagelog.FieldOutputTokens), "output_tokens"),
			dbent.As(dbent.Sum(dbusagelog.FieldTotalCost), "total_cost_usd"),
			dbent.As(dbent.Max(dbusagelog.FieldCreatedAt), "last_seen_at"),
		).Scan(ctx, &aggs); err != nil {
		return nil, fmt.Errorf("dashboard stats: account aggregate scan: %w", err)
	}

	rows := make([]AccountBreakdownRow, 0, len(aggs))
	for _, a := range aggs {
		rows = append(rows, AccountBreakdownRow{
			AccountID:    a.AccountID,
			RequestCount: a.RequestCount,
			InputTokens:  a.InputTokens,
			OutputTokens: a.OutputTokens,
			TotalCostUSD: a.TotalCostUSD,
			LastSeenAt:   a.LastSeenAt,
		})
	}

	sort.Slice(rows, func(i, j int) bool {
		return rows[i].RequestCount > rows[j].RequestCount
	})

	return rows, nil
}
