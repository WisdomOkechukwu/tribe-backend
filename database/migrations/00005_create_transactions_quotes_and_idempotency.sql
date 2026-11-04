-- The user-facing transaction list, price quotes, swap route legs and the
-- idempotency table that stops a double-tap from paying twice.

-- +goose Up
-- What the user sees in their activity list. The money itself lives in the
-- journal; journal_entry_id links the two.
CREATE TABLE transactions (
    id                CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    reference         VARCHAR(100) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id           CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    category          VARCHAR(24)  NOT NULL COMMENT 'currency | virtual_card | cashpoint | utility | giftcard | referral',
    type              VARCHAR(32)  NOT NULL COMMENT 'swap | send | receive | deposit | bill | gift_buy | gift_sell | card_create | card_fund | card_withdraw | card_delete | cashpoint_create | cashpoint_redeem | cashpoint_cancel | referral_withdraw',
    direction         VARCHAR(3)   NOT NULL COMMENT 'in | out',
    status            VARCHAR(16)  NOT NULL COMMENT 'pending | completed | failed | cancelled | reversed',
    title             VARCHAR(150) NOT NULL DEFAULT '',
    subtitle          VARCHAR(150) NOT NULL DEFAULT '',
    from_asset        VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    from_amount       DECIMAL(38,0) NOT NULL,
    to_asset          VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NULL,
    to_amount         DECIMAL(38,0) NULL,
    fee_asset         VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NULL,
    fee_amount        DECIMAL(38,0) NOT NULL DEFAULT 0,
    usd_value         DECIMAL(24,2) NOT NULL DEFAULT 0,
    journal_entry_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    quote_id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    metadata          JSON NULL,
    failure_reason    VARCHAR(255) NULL,
    version           INT UNSIGNED NOT NULL DEFAULT 1,
    created_at        DATETIME(6) NOT NULL,
    updated_at        DATETIME(6) NOT NULL,
    completed_at      DATETIME(6) NULL,
    PRIMARY KEY (id),
    UNIQUE KEY transactions_reference_unique (reference),
    KEY transactions_user_created_index (user_id, created_at),
    KEY transactions_status_index (status),
    KEY transactions_type_index (type),
    KEY transactions_created_index (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- A price we promise for 30 seconds. Using it sets used_at, so it works once.
CREATE TABLE quotes (
    id                CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id           CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    transaction_type  VARCHAR(32) NOT NULL,
    from_asset        VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    to_asset          VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    from_amount       DECIMAL(38,0) NOT NULL COMMENT 'what the user pays, fee included',
    fee_amount        DECIMAL(38,0) NOT NULL DEFAULT 0 COMMENT 'in from_asset',
    to_amount         DECIMAL(38,0) NOT NULL COMMENT 'what the user receives',
    rate              DECIMAL(38,18) NOT NULL COMMENT 'to units per from unit, after margins',
    uses_ngn_rail     TINYINT(1) NOT NULL DEFAULT 0,
    naira_in_amount   DECIMAL(38,0) NULL COMMENT 'kobo received selling from_asset (NGN rail only)',
    naira_out_amount  DECIMAL(38,0) NULL COMMENT 'kobo spent buying to_asset (NGN rail only)',
    route             JSON NOT NULL COMMENT 'list of legs with their rates and providers',
    expires_at        DATETIME(6) NOT NULL,
    used_at           DATETIME(6) NULL,
    created_at        DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    KEY quotes_user_index (user_id),
    KEY quotes_expires_index (expires_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Each hop of a conversion route, logged with the rate used.
-- USD -> GBP on the NGN rail is two rows: USD -> NGN, then NGN -> GBP.
CREATE TABLE swap_legs (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    transaction_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    quote_id        CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    step            TINYINT UNSIGNED NOT NULL,
    from_asset      VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    to_asset        VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    from_amount     DECIMAL(38,0) NOT NULL,
    to_amount       DECIMAL(38,0) NOT NULL,
    rate            DECIMAL(38,18) NOT NULL,
    provider_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    via_ngn_rail    TINYINT(1) NOT NULL DEFAULT 0,
    created_at      DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    KEY swap_legs_transaction_index (transaction_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Remembers every request sent with an Idempotency-Key header.
-- If the same key comes again we replay the saved answer instead of doing
-- the work twice. The UNIQUE key makes this safe even if both requests
-- arrive at the same millisecond.
CREATE TABLE idempotency_keys (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    actor_type          VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'user | admin',
    actor_id            CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    idempotency_key     VARCHAR(100) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    request_method      VARCHAR(8)   NOT NULL,
    request_path        VARCHAR(191) NOT NULL,
    request_hash        CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'SHA-256 of the raw body',
    status              VARCHAR(16)  NOT NULL COMMENT 'processing | completed',
    response_status     INT NULL,
    response_body       MEDIUMBLOB NULL,
    locked_until        DATETIME(6) NOT NULL,
    created_at          DATETIME(6) NOT NULL,
    updated_at          DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY idempotency_keys_actor_key_unique (actor_type, actor_id, idempotency_key),
    KEY idempotency_keys_created_index (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- People a user has paid before.
CREATE TABLE beneficiaries (
    id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    kind        VARCHAR(8)   NOT NULL COMMENT 'tribe | bank | wallet',
    name        VARCHAR(150) NOT NULL,
    handle      VARCHAR(191) NOT NULL COMMENT '@tag, account number or wallet address',
    asset_code  VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    channel_id  VARCHAR(32)  NULL,
    network     VARCHAR(32)  NULL,
    country     VARCHAR(2)   NULL,
    details     JSON NULL,
    dedupe_key  VARCHAR(191) NOT NULL COMMENT 'kind|asset|handle so the same person is saved once',
    created_at  DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY beneficiaries_user_dedupe_unique (user_id, dedupe_key)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- +goose Down
DROP TABLE IF EXISTS beneficiaries;
DROP TABLE IF EXISTS idempotency_keys;
DROP TABLE IF EXISTS swap_legs;
DROP TABLE IF EXISTS quotes;
DROP TABLE IF EXISTS transactions;
