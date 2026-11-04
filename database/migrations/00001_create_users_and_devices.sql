-- Users, their devices, sessions, and the one-time codes we send them.
--
-- Rules that every migration follows (so it works on MySQL 5.7 and 8.4):
--   * IDs are 26-character ULIDs stored as CHAR(26) ascii.
--   * Times are DATETIME(6) in UTC.
--   * No CHECK constraints (MySQL 5.7 ignores them); Go validates instead.

-- +goose Up
CREATE TABLE users (
    id                        CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    email                     VARCHAR(191) NOT NULL,
    password_hash             VARCHAR(255) NOT NULL,
    first_name                VARCHAR(100) NOT NULL,
    last_name                 VARCHAR(100) NOT NULL,
    tag                       VARCHAR(32)  NOT NULL COMMENT 'public @handle',
    phone                     VARCHAR(32)  NULL,
    phone_verified_at         DATETIME(6)  NULL,
    country                   CHAR(2)      NOT NULL,
    date_of_birth             DATE         NULL,
    address                   VARCHAR(255) NULL,
    status                    VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active | frozen | closed',
    kyc_tier                  TINYINT UNSIGNED NOT NULL DEFAULT 0,
    kyc_status                VARCHAR(16)  NOT NULL DEFAULT 'none' COMMENT 'none | pending | approved | failed',
    global_accounts_status    VARCHAR(16)  NOT NULL DEFAULT 'none' COMMENT 'none | review | ready',
    world                     VARCHAR(16)  NULL COMMENT 'global | crypto | card | bills (picked in onboarding)',
    onboarding_completed_at   DATETIME(6)  NULL,
    referral_code             VARCHAR(16)  NOT NULL,
    referred_by_user_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    heard_from                VARCHAR(64)  NULL,
    labels                    JSON         NULL,
    biometrics_enabled        TINYINT(1)   NOT NULL DEFAULT 0,
    two_factor_enabled        TINYINT(1)   NOT NULL DEFAULT 0,
    tag_changed_at            DATETIME(6)  NULL,
    email_verified_at         DATETIME(6)  NULL,
    last_active_at            DATETIME(6)  NULL,
    version                   INT UNSIGNED NOT NULL DEFAULT 1 COMMENT 'bumped on every change; stops two admins overwriting each other',
    created_at                DATETIME(6)  NOT NULL,
    updated_at                DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY users_email_unique (email),
    UNIQUE KEY users_tag_unique (tag),
    UNIQUE KEY users_referral_code_unique (referral_code),
    KEY users_created_at_index (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- One row per phone a user has signed in with.
-- device_imei holds the most stable ID the phone allows (Android ID, iOS
-- identifierForVendor, or an install ID); real IMEIs are blocked by iOS/Android.
CREATE TABLE user_devices (
    id              CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    device_imei     VARCHAR(191) NOT NULL,
    name            VARCHAR(100) NOT NULL DEFAULT '',
    platform        VARCHAR(16)  NOT NULL DEFAULT '' COMMENT 'ios | android | web',
    model           VARCHAR(100) NOT NULL DEFAULT '',
    app_version     VARCHAR(32)  NOT NULL DEFAULT '',
    status          VARCHAR(16)  NOT NULL COMMENT 'pending | active | replaced | revoked',
    -- The trick that enforces "one active device per user" INSIDE the database:
    -- this column equals user_id only while the device is active (NULL otherwise),
    -- and it has a UNIQUE index. MySQL allows many NULLs but only one copy of
    -- each user_id, so a second active device is impossible, even under a race.
    active_user_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin
                    GENERATED ALWAYS AS (CASE WHEN status = 'active' THEN user_id ELSE NULL END) STORED,
    last_seen_at    DATETIME(6) NULL,
    activated_at    DATETIME(6) NULL,
    replaced_at     DATETIME(6) NULL,
    created_at      DATETIME(6) NOT NULL,
    updated_at      DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY user_devices_user_imei_unique (user_id, device_imei),
    UNIQUE KEY user_devices_one_active_per_user (active_user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- A signed-in session on one device. Revoking it signs that phone out.
CREATE TABLE user_sessions (
    id              CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    device_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ip_address      VARCHAR(45)  NOT NULL DEFAULT '',
    user_agent      VARCHAR(255) NOT NULL DEFAULT '',
    last_used_at    DATETIME(6)  NOT NULL,
    expires_at      DATETIME(6)  NOT NULL,
    revoked_at      DATETIME(6)  NULL,
    revoked_reason  VARCHAR(64)  NULL,
    created_at      DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY user_sessions_user_index (user_id),
    KEY user_sessions_device_index (device_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Long-lived tokens used to get a new short-lived access token.
-- We only store a SHA-256 hash, never the token itself.
CREATE TABLE refresh_tokens (
    id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    session_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token_hash  CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    expires_at  DATETIME(6) NOT NULL,
    used_at     DATETIME(6) NULL,
    created_at  DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY refresh_tokens_hash_unique (token_hash),
    KEY refresh_tokens_session_index (session_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- 4-digit email codes for sign-up and password reset.
CREATE TABLE email_verifications (
    id                       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    email                    VARCHAR(191) NOT NULL,
    purpose                  VARCHAR(24)  NOT NULL COMMENT 'signup | password_reset',
    code_hash                CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    attempts                 INT UNSIGNED NOT NULL DEFAULT 0,
    expires_at               DATETIME(6)  NOT NULL,
    verified_at              DATETIME(6)  NULL,
    verification_token_hash  CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NULL,
    consumed_at              DATETIME(6)  NULL,
    created_at               DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY email_verifications_email_index (email, purpose),
    UNIQUE KEY email_verifications_token_unique (verification_token_hash)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- 6-digit SMS codes. The phone number is only saved on the user once verified.
CREATE TABLE phone_verifications (
    id           CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    phone        VARCHAR(32)  NOT NULL,
    code_hash    CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    attempts     INT UNSIGNED NOT NULL DEFAULT 0,
    expires_at   DATETIME(6)  NOT NULL,
    verified_at  DATETIME(6)  NULL,
    created_at   DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY phone_verifications_user_index (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Codes sent by email when someone signs in on a NEW phone.
CREATE TABLE device_verifications (
    id           CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    device_id    CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code_hash    CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    attempts     INT UNSIGNED NOT NULL DEFAULT 0,
    expires_at   DATETIME(6)  NOT NULL,
    verified_at  DATETIME(6)  NULL,
    created_at   DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY device_verifications_user_index (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- The 4-digit payment PIN (hashed) and its lock-out counter.
CREATE TABLE user_pins (
    user_id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    pin_hash         VARCHAR(255) NOT NULL,
    failed_attempts  INT UNSIGNED NOT NULL DEFAULT 0,
    locked_until     DATETIME(6)  NULL,
    updated_at       DATETIME(6)  NOT NULL,
    PRIMARY KEY (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Expo push tokens so we can send notifications.
CREATE TABLE push_tokens (
    id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    token       VARCHAR(191) NOT NULL,
    platform    VARCHAR(16)  NOT NULL DEFAULT '',
    created_at  DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY push_tokens_token_unique (token),
    KEY push_tokens_user_index (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- +goose Down
DROP TABLE IF EXISTS push_tokens;
DROP TABLE IF EXISTS user_pins;
DROP TABLE IF EXISTS device_verifications;
DROP TABLE IF EXISTS phone_verifications;
DROP TABLE IF EXISTS email_verifications;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS user_sessions;
DROP TABLE IF EXISTS user_devices;
DROP TABLE IF EXISTS users;
