-- 125_temp_unsched_backoff_fields.sql
-- Adds exponential-backoff state to accounts.temp_unschedulable_*.
--
-- See docs/superpowers/plans/2026-04-23-temp-unsched-backoff-thinking-filter.md
-- Two nullable columns, no backfill needed; next trigger treated as fresh-start.

ALTER TABLE accounts
    ADD COLUMN IF NOT EXISTS temp_unsched_step_index INTEGER,
    ADD COLUMN IF NOT EXISTS temp_unsched_last_recovered_at TIMESTAMPTZ;
