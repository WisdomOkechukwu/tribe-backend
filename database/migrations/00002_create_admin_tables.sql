-- Admin panel users, what each one is allowed to do, and the audit trail.

-- +goose Up
CREATE TABLE admin_users (
    id             CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name           VARCHAR(100) NOT NULL,
    email          VARCHAR(191) NOT NULL,
    password_hash  VARCHAR(255) NOT NULL,
    color          VARCHAR(48)  NOT NULL DEFAULT '',
    status         VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active | suspended',
    last_login_at  DATETIME(6)  NULL,
    created_by     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    version        INT UNSIGNED NOT NULL DEFAULT 1,
    created_at     DATETIME(6)  NOT NULL,
    updated_at     DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY admin_users_email_unique (email)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- One row per permission an admin has, e.g. ("adm_1", "customers.view").
-- The list of possible permissions lives in Go (app/services/adminauth/permissions.go).
CREATE TABLE admin_user_permissions (
    admin_user_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    permission     VARCHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (admin_user_id, permission)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE admin_sessions (
    id             CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    admin_user_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ip_address     VARCHAR(45)  NOT NULL DEFAULT '',
    user_agent     VARCHAR(255) NOT NULL DEFAULT '',
    expires_at     DATETIME(6)  NOT NULL,
    revoked_at     DATETIME(6)  NULL,
    created_at     DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY admin_sessions_admin_index (admin_user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Every admin write is recorded here with what it looked like before and after.
CREATE TABLE audit_logs (
    id             CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    admin_user_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    admin_name     VARCHAR(100) NOT NULL DEFAULT '',
    action         VARCHAR(64)  NOT NULL,
    target         VARCHAR(191) NOT NULL DEFAULT '',
    summary        VARCHAR(255) NOT NULL DEFAULT '',
    before_data    JSON NULL,
    after_data     JSON NULL,
    created_at     DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY audit_logs_action_index (action),
    KEY audit_logs_created_at_index (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- +goose Down
DROP TABLE IF EXISTS audit_logs;
DROP TABLE IF EXISTS admin_sessions;
DROP TABLE IF EXISTS admin_user_permissions;
DROP TABLE IF EXISTS admin_users;
