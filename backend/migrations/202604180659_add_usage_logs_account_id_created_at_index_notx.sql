-- Add composite index on usage_logs(account_id, created_at) to accelerate per-account dashboard queries.
CREATE INDEX CONCURRENTLY IF NOT EXISTS usagelog_account_id_created_at
    ON usage_logs (account_id, created_at);
