-- Three changes:
--   1. Treasury fundings need a SECOND admin to approve them before any money
--      is posted ("four eyes"). A request starts as pending; the journal entry
--      is only written when a different admin approves it.
--   2. provider_webhook_events stores every webhook a provider sends us.
--      UNIQUE(provider_slug, event_id) means a provider that sends the same
--      event twice is only processed once.
--   3. inbox_messages.pushed_at turns the inbox into an "outbox" for push
--      notifications: the scheduler pushes every message whose pushed_at is
--      still NULL, so every money movement gets a push without each service
--      having to remember to send one.

-- +goose Up
ALTER TABLE treasury_fundings
    MODIFY journal_entry_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '',
    ADD COLUMN status            VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending | approved | rejected' AFTER note,
    ADD COLUMN approved_by       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER requested_by_name,
    ADD COLUMN approved_by_name  VARCHAR(100) NOT NULL DEFAULT '' AFTER approved_by,
    ADD COLUMN decided_at        DATETIME(6)  NULL AFTER approved_by_name,
    ADD COLUMN rejection_reason  VARCHAR(255) NOT NULL DEFAULT '' AFTER decided_at,
    ADD KEY treasury_fundings_status_index (status, created_at);

-- Rows created before this change were posted straight away.
UPDATE treasury_fundings SET status = 'approved', approved_by_name = 'before approvals', decided_at = created_at WHERE journal_entry_id <> '';

-- Admins who could already change fees & margins may now approve fundings.
INSERT IGNORE INTO admin_user_permissions (admin_user_id, permission)
SELECT admin_user_id, 'inventory.approve' FROM admin_user_permissions WHERE permission = 'markups.edit';

CREATE TABLE provider_webhook_events (
    id              CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    provider_slug   VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    event_id        VARCHAR(100) NOT NULL,
    event_type      VARCHAR(64)  NOT NULL DEFAULT '',
    payload         JSON         NOT NULL,
    status          VARCHAR(16)  NOT NULL DEFAULT 'received' COMMENT 'received | processed | failed',
    error_message   VARCHAR(255) NOT NULL DEFAULT '',
    received_at     DATETIME(6)  NOT NULL,
    processed_at    DATETIME(6)  NULL,
    PRIMARY KEY (id),
    UNIQUE KEY provider_webhook_events_unique (provider_slug, event_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

ALTER TABLE inbox_messages
    ADD COLUMN pushed_at DATETIME(6) NULL AFTER read_at,
    ADD KEY inbox_messages_push_index (pushed_at, created_at);

-- Old messages are not pushed again.
UPDATE inbox_messages SET pushed_at = created_at;

-- +goose Down
ALTER TABLE inbox_messages DROP KEY inbox_messages_push_index, DROP COLUMN pushed_at;
DROP TABLE IF EXISTS provider_webhook_events;
DELETE FROM admin_user_permissions WHERE permission = 'inventory.approve';
ALTER TABLE treasury_fundings
    DROP KEY treasury_fundings_status_index,
    DROP COLUMN rejection_reason,
    DROP COLUMN decided_at,
    DROP COLUMN approved_by_name,
    DROP COLUMN approved_by,
    DROP COLUMN status;
