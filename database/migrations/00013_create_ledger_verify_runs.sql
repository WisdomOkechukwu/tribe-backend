-- Every run of ledger verification, so the nightly check can be
-- INCREMENTAL: it only re-checks what was added since the last clean run.
--
--   entries_from_id     the first journal entry id (a ULID, which sorts by
--                       time) this run checked; "" for a full run
--   snapshots_from_id   the first snapshot id this run checked (0 = all)
--   snapshots_to_id     the highest snapshot id that existed when it started
--   snapshots_from_date snapshots on or after this day were re-checked too
--   started_at          the next incremental run starts an hour before this
--
-- Only runs with status 'clean' move the starting point forward. A run that
-- found problems leaves it where it was, so the problems keep showing up
-- until someone fixes them.

-- +goose Up
CREATE TABLE ledger_verify_runs (
    id                   BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    mode                 VARCHAR(16)  NOT NULL COMMENT 'full | incremental',
    status               VARCHAR(16)  NOT NULL COMMENT 'running | clean | problems | failed',
    entries_from_id      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    snapshots_from_id    BIGINT UNSIGNED NOT NULL DEFAULT 0,
    snapshots_to_id      BIGINT UNSIGNED NOT NULL DEFAULT 0,
    snapshots_from_date  DATE NULL,
    snapshots_to_date    DATE NULL,
    entries_checked      BIGINT UNSIGNED NOT NULL DEFAULT 0,
    snapshots_checked    BIGINT UNSIGNED NOT NULL DEFAULT 0,
    problem_count        BIGINT UNSIGNED NOT NULL DEFAULT 0,
    message              VARCHAR(500) NOT NULL DEFAULT '',
    started_at           DATETIME(6)  NOT NULL,
    finished_at          DATETIME(6)  NULL,
    PRIMARY KEY (id),
    KEY ledger_verify_runs_status_index (status, id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- +goose Down
DROP TABLE ledger_verify_runs;
