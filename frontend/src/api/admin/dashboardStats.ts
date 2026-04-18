/**
 * Admin Dashboard Stats API endpoints
 * Provides account- and group-level breakdown statistics
 * (overview, IPs, users, accounts) for the admin dashboard views.
 */

import { apiClient } from '../client'

// ==================== Types ====================

/**
 * Query parameters accepted by all dashboard stats endpoints.
 * - `from` / `to` are RFC3339 timestamps; default window is the last 24h.
 * - `limit` is clamped server-side to 1..500 (default 100).
 */
export interface StatsQuery {
  from?: string
  to?: string
  limit?: number
}

/**
 * Aggregated overview metrics for an account or a group within the
 * requested time window.
 */
export interface Overview {
  request_count: number
  input_tokens: number
  output_tokens: number
  total_cost_usd: number
  unique_ips: number
  unique_users: number
}

/**
 * Per-IP breakdown row.
 */
export interface IPBreakdownRow {
  ip_address: string
  request_count: number
  input_tokens: number
  output_tokens: number
  total_cost_usd: number
  first_seen_at: string
  last_seen_at: string
  unique_users: number
}

/**
 * Per-user (API key) breakdown row.
 */
export interface UserBreakdownRow {
  api_key_id: number
  user_id: number
  request_count: number
  input_tokens: number
  output_tokens: number
  total_cost_usd: number
  last_seen_at: string
}

/**
 * Per-account breakdown row (group dashboards only).
 */
export interface AccountBreakdownRow {
  account_id: number
  request_count: number
  input_tokens: number
  output_tokens: number
  total_cost_usd: number
  last_seen_at: string
}

// ==================== API Functions ====================

function toParams(q?: StatsQuery): Record<string, string | number> | undefined {
  if (!q) return undefined
  const params: Record<string, string | number> = {}
  if (q.from) params.from = q.from
  if (q.to) params.to = q.to
  if (typeof q.limit === 'number') params.limit = q.limit
  return Object.keys(params).length > 0 ? params : undefined
}

/**
 * Get overview metrics for a single account.
 * @param id - Account ID
 * @param q - Optional time window and limit
 * @returns Overview aggregates
 */
export async function accountOverview(id: number, q?: StatsQuery): Promise<Overview> {
  const { data } = await apiClient.get<Overview>(`/admin/accounts/${id}/stats/overview`, {
    params: toParams(q)
  })
  return data
}

/**
 * Get per-IP breakdown for a single account.
 * @param id - Account ID
 * @param q - Optional time window and limit
 * @returns Rows of IP-level aggregates
 */
export async function accountIPs(
  id: number,
  q?: StatsQuery
): Promise<{ rows: IPBreakdownRow[] }> {
  const { data } = await apiClient.get<{ rows: IPBreakdownRow[] }>(
    `/admin/accounts/${id}/stats/ips`,
    { params: toParams(q) }
  )
  return data
}

/**
 * Get per-user breakdown for a single account.
 * @param id - Account ID
 * @param q - Optional time window and limit
 * @returns Rows of user/API-key-level aggregates
 */
export async function accountUsers(
  id: number,
  q?: StatsQuery
): Promise<{ rows: UserBreakdownRow[] }> {
  const { data } = await apiClient.get<{ rows: UserBreakdownRow[] }>(
    `/admin/accounts/${id}/stats/users`,
    { params: toParams(q) }
  )
  return data
}

/**
 * Get overview metrics for a single group.
 * @param id - Group ID
 * @param q - Optional time window and limit
 * @returns Overview aggregates
 */
export async function groupOverview(id: number, q?: StatsQuery): Promise<Overview> {
  const { data } = await apiClient.get<Overview>(`/admin/groups/${id}/stats/overview`, {
    params: toParams(q)
  })
  return data
}

/**
 * Get per-IP breakdown for a single group.
 * @param id - Group ID
 * @param q - Optional time window and limit
 * @returns Rows of IP-level aggregates
 */
export async function groupIPs(
  id: number,
  q?: StatsQuery
): Promise<{ rows: IPBreakdownRow[] }> {
  const { data } = await apiClient.get<{ rows: IPBreakdownRow[] }>(
    `/admin/groups/${id}/stats/ips`,
    { params: toParams(q) }
  )
  return data
}

/**
 * Get per-user breakdown for a single group.
 * @param id - Group ID
 * @param q - Optional time window and limit
 * @returns Rows of user/API-key-level aggregates
 */
export async function groupUsers(
  id: number,
  q?: StatsQuery
): Promise<{ rows: UserBreakdownRow[] }> {
  const { data } = await apiClient.get<{ rows: UserBreakdownRow[] }>(
    `/admin/groups/${id}/stats/users`,
    { params: toParams(q) }
  )
  return data
}

/**
 * Get per-account breakdown for a single group.
 * @param id - Group ID
 * @param q - Optional time window and limit
 * @returns Rows of account-level aggregates
 */
export async function groupAccounts(
  id: number,
  q?: StatsQuery
): Promise<{ rows: AccountBreakdownRow[] }> {
  const { data } = await apiClient.get<{ rows: AccountBreakdownRow[] }>(
    `/admin/groups/${id}/stats/accounts`,
    { params: toParams(q) }
  )
  return data
}

export const dashboardStatsApi = {
  accountOverview,
  accountIPs,
  accountUsers,
  groupOverview,
  groupIPs,
  groupUsers,
  groupAccounts
}

export default dashboardStatsApi
