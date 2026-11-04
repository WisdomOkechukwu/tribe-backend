-- Indexes found missing with EXPLAIN while reviewing slow queries.
-- All are plain (non-functional) indexes, so they work on MySQL 5.7 and 8.4.
--
-- ledger_accounts (asset_code, code)
--   Free funds adds up every customer wallet, referral and card account of
--   ONE currency. Without this, every call scanned the whole accounts table
--   (EXPLAIN: type ALL). With it MySQL jumps straight to that currency's
--   accounts of those kinds.
--
-- ledger_daily_snapshots (snapshot_date)
--   The admin Snapshots page asks for "the latest day" (MAX(snapshot_date))
--   and then every snapshot of that day. Both read the whole table before.
--
-- transactions (status, created_at)
--   The dashboard's "recent failures" (status = 'failed' ORDER BY
--   created_at DESC LIMIT 8) was sorting every failed transaction in memory
--   (EXPLAIN: Using filesort). This index returns them already in order.
--
-- transactions (created_at, status, category, usd_value)
--   The dashboard's 24-hour, 7-day and 30-day totals read only these four
--   columns. A "covering" index holds all of them, so MySQL answers from the
--   index alone and never opens the full transaction rows.
--
-- transactions (category, created_at)
--   The admin transactions list filtered by category, newest first.
--
-- support_threads (updated_at)
--   The support inbox without a status filter (ORDER BY updated_at DESC).
--
-- Clean-up job (app/console/cleanup.go) deletes old rows in batches; these
-- let each batch find its rows without scanning the table:
--   provider_webhook_events (status, received_at), user_sessions (expires_at),
--   admin_sessions (expires_at), and created_at on the three code tables.

-- +goose Up
CREATE INDEX ledger_accounts_asset_code_index ON ledger_accounts (asset_code, code);
CREATE INDEX ledger_daily_snapshots_date_index ON ledger_daily_snapshots (snapshot_date);
CREATE INDEX transactions_status_created_index ON transactions (status, created_at);
CREATE INDEX transactions_dashboard_index ON transactions (created_at, status, category, usd_value);
CREATE INDEX transactions_category_created_index ON transactions (category, created_at);
CREATE INDEX support_threads_updated_index ON support_threads (updated_at);
CREATE INDEX provider_webhook_events_status_received_index ON provider_webhook_events (status, received_at);
CREATE INDEX user_sessions_expires_index ON user_sessions (expires_at);
CREATE INDEX admin_sessions_expires_index ON admin_sessions (expires_at);
CREATE INDEX email_verifications_created_index ON email_verifications (created_at);
CREATE INDEX phone_verifications_created_index ON phone_verifications (created_at);
CREATE INDEX device_verifications_created_index ON device_verifications (created_at);

-- +goose Down
DROP INDEX device_verifications_created_index ON device_verifications;
DROP INDEX phone_verifications_created_index ON phone_verifications;
DROP INDEX email_verifications_created_index ON email_verifications;
DROP INDEX admin_sessions_expires_index ON admin_sessions;
DROP INDEX user_sessions_expires_index ON user_sessions;
DROP INDEX provider_webhook_events_status_received_index ON provider_webhook_events;
DROP INDEX support_threads_updated_index ON support_threads;
DROP INDEX transactions_category_created_index ON transactions;
DROP INDEX transactions_dashboard_index ON transactions;
DROP INDEX transactions_status_created_index ON transactions;
DROP INDEX ledger_daily_snapshots_date_index ON ledger_daily_snapshots;
DROP INDEX ledger_accounts_asset_code_index ON ledger_accounts;
