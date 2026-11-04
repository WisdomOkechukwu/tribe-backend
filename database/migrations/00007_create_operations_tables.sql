-- Scheduler bookkeeping, treasury funding and placeholder tables for later
-- finance features (settlement, payroll, period close).

-- +goose Up
-- One row per cron run. UNIQUE(job_name, window_key) means a job can only
-- run once per window, even if two scheduler processes start at once.
CREATE TABLE scheduled_job_runs (
    id           BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    job_name     VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    window_key   VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'e.g. 2026-10-23 for a daily job',
    status       VARCHAR(16)  NOT NULL COMMENT 'running | succeeded | failed',
    message      VARCHAR(500) NOT NULL DEFAULT '',
    started_at   DATETIME(6)  NOT NULL,
    finished_at  DATETIME(6)  NULL,
    PRIMARY KEY (id),
    UNIQUE KEY scheduled_job_runs_job_window_unique (job_name, window_key),
    KEY scheduled_job_runs_started_index (started_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Money the company adds to its own vault (admin "Top up inventory").
CREATE TABLE treasury_fundings (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    asset_code          VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    amount              DECIMAL(38,0) NOT NULL,
    external_reference  VARCHAR(64)  NOT NULL,
    note                VARCHAR(255) NOT NULL DEFAULT '',
    requested_by        CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    requested_by_name   VARCHAR(100) NOT NULL DEFAULT '',
    journal_entry_id    CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    created_at          DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY treasury_fundings_reference_unique (asset_code, external_reference)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Placeholders for later (see docs/ACCOUNTING.md "Later").
CREATE TABLE settlement_sessions (
    id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    asset_code  VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    status      VARCHAR(16) NOT NULL DEFAULT 'open',
    created_at  DATETIME(6) NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE payroll_runs (
    id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    period      VARCHAR(7)  NOT NULL COMMENT 'YYYY-MM',
    status      VARCHAR(16) NOT NULL DEFAULT 'draft',
    created_at  DATETIME(6) NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE period_close_runs (
    id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    period      VARCHAR(7)  NOT NULL COMMENT 'YYYY-MM',
    status      VARCHAR(16) NOT NULL DEFAULT 'draft',
    created_at  DATETIME(6) NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- +goose Down
DROP TABLE IF EXISTS period_close_runs;
DROP TABLE IF EXISTS payroll_runs;
DROP TABLE IF EXISTS settlement_sessions;
DROP TABLE IF EXISTS treasury_fundings;
DROP TABLE IF EXISTS scheduled_job_runs;
