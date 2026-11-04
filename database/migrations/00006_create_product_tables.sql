-- Products: bills, gift cards, virtual cards (and their designs), cashpoints,
-- referrals, inbox, support, KYC and the API request log.

-- +goose Up
CREATE TABLE billers (
    id           VARCHAR(48) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name         VARCHAR(100) NOT NULL,
    category     VARCHAR(24)  NOT NULL COMMENT 'airtime | data | tv | electricity',
    field        VARCHAR(16)  NOT NULL COMMENT 'what the customer is identified by: phone | smartcard | meter',
    plans        JSON NULL COMMENT 'list of {id, name, price, period}; prices in whole Naira',
    background   VARCHAR(16)  NOT NULL DEFAULT '',
    foreground   VARCHAR(16)  NOT NULL DEFAULT '',
    provider_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    enabled      TINYINT(1)   NOT NULL DEFAULT 1,
    sort_order   INT NOT NULL DEFAULT 0,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE bill_payments (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id             CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    transaction_id      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    biller_id           VARCHAR(48) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target              VARCHAR(64)  NOT NULL COMMENT 'phone, smartcard or meter number',
    plan_id             VARCHAR(64)  NULL,
    amount_ngn          DECIMAL(38,0) NOT NULL COMMENT 'kobo',
    pay_with_asset      VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    provider_reference  VARCHAR(100) NULL,
    token               VARCHAR(64)  NULL COMMENT 'prepaid electricity token',
    units               VARCHAR(32)  NULL,
    status              VARCHAR(16)  NOT NULL,
    created_at          DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY bill_payments_user_index (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Prepaid electricity tokens the user has bought (the "vault").
CREATE TABLE vault_tokens (
    id               CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    bill_payment_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    provider         VARCHAR(64)  NOT NULL,
    meter            VARCHAR(32)  NOT NULL,
    amount_ngn       DECIMAL(38,0) NOT NULL,
    kwh              VARCHAR(16)  NOT NULL,
    token            VARCHAR(64)  NOT NULL,
    paid_with        VARCHAR(10)  NOT NULL,
    loaded_on        DATETIME(6)  NULL,
    bought_at        DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY vault_tokens_user_index (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE recurring_bills (
    id              CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    biller_id       VARCHAR(48) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    target          VARCHAR(64)  NOT NULL,
    plan_id         VARCHAR(64)  NULL,
    amount_ngn      DECIMAL(38,0) NOT NULL,
    pay_with_asset  VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    frequency       VARCHAR(16)  NOT NULL COMMENT 'weekly | biweekly | monthly',
    next_run_at     DATETIME(6)  NOT NULL,
    active          TINYINT(1)   NOT NULL DEFAULT 1,
    last_paid_at    DATETIME(6)  NULL,
    last_result     VARCHAR(255) NULL,
    version         INT UNSIGNED NOT NULL DEFAULT 1,
    created_at      DATETIME(6)  NOT NULL,
    updated_at      DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY recurring_bills_due_index (active, next_run_at),
    KEY recurring_bills_user_index (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE gift_card_brands (
    id                  VARCHAR(48) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name                VARCHAR(100) NOT NULL,
    sell_rate_ngn       DECIMAL(20,4) NOT NULL COMMENT 'Naira we pay per 1 USD of card when the user SELLS',
    buy_markup_percent  DECIMAL(10,6) NOT NULL DEFAULT 1.5,
    minimum_usd         INT NOT NULL DEFAULT 5,
    maximum_usd         INT NOT NULL DEFAULT 500,
    background          VARCHAR(16)  NOT NULL DEFAULT '',
    foreground          VARCHAR(16)  NOT NULL DEFAULT '',
    provider_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    enabled             TINYINT(1)   NOT NULL DEFAULT 1,
    sort_order          INT NOT NULL DEFAULT 0,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE gift_card_orders (
    id              CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    transaction_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    brand_id        VARCHAR(48) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    direction       VARCHAR(4)  NOT NULL COMMENT 'buy | sell',
    face_value_usd  DECIMAL(38,0) NOT NULL COMMENT 'cents',
    code_encrypted  BLOB NULL COMMENT 'the card code, encrypted with APP_KEY (AES-256-GCM)',
    status          VARCHAR(16) NOT NULL,
    created_at      DATETIME(6) NOT NULL,
    updated_at      DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    KEY gift_card_orders_user_index (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Card looks the admin offers in the app.
-- kind = theme   : a ready-made look (finish "midnight")
-- kind = swatch  : a colour for "make your own" (hex "FF7A5C")
-- kind = pattern : a pattern that can go on any card ("waves")
CREATE TABLE card_designs (
    id          VARCHAR(48) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    kind        VARCHAR(8)   NOT NULL COMMENT 'theme | swatch | pattern',
    name        VARCHAR(64)  NOT NULL,
    finish      VARCHAR(64)  NOT NULL DEFAULT '' COMMENT 'theme id, used as the card finish string',
    hex_color   CHAR(6)      NULL,
    pattern     VARCHAR(24)  NULL,
    enabled     TINYINT(1)   NOT NULL DEFAULT 1,
    sort_order  INT NOT NULL DEFAULT 0,
    version     INT UNSIGNED NOT NULL DEFAULT 1,
    created_at  DATETIME(6)  NOT NULL,
    updated_at  DATETIME(6)  NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- A user's virtual dollar card. Its balance lives in the ledger
-- (account type user_card), not here. The full card number is never stored.
CREATE TABLE virtual_cards (
    id                        CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id                   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    label                     VARCHAR(32)  NOT NULL,
    network                   VARCHAR(16)  NOT NULL COMMENT 'visa | mastercard',
    -- The look, exactly as the app writes it: "midnight", "sunset:waves"
    -- or "custom:FF7A5C:dots". design_settings keeps the parsed pieces.
    design_finish             VARCHAR(64)  NOT NULL,
    design_settings           JSON NULL COMMENT '{theme, pattern, hex, shade}',
    holder_name               VARCHAR(150) NOT NULL,
    currency                  VARCHAR(10)  NOT NULL DEFAULT 'USD',
    last_four                 CHAR(4)      NOT NULL,
    expiry_month              TINYINT UNSIGNED NOT NULL,
    expiry_year               SMALLINT UNSIGNED NOT NULL,
    provider_card_reference   VARCHAR(100) NOT NULL,
    frozen                    TINYINT(1)   NOT NULL DEFAULT 0,
    online_payments_enabled   TINYINT(1)   NOT NULL DEFAULT 1,
    monthly_limit             DECIMAL(38,0) NOT NULL DEFAULT 0 COMMENT 'cents, 0 = no limit',
    billing_address           JSON NULL,
    in_wallet                 TINYINT(1)   NOT NULL DEFAULT 0,
    status                    VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active | deleted',
    version                   INT UNSIGNED NOT NULL DEFAULT 1,
    created_at                DATETIME(6)  NOT NULL,
    updated_at                DATETIME(6)  NOT NULL,
    deleted_at                DATETIME(6)  NULL,
    PRIMARY KEY (id),
    KEY virtual_cards_user_index (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- A 12-digit code worth some Naira that another Tribe user can redeem.
CREATE TABLE cashpoints (
    id                      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    creator_user_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code_hash               CHAR(64) CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'for lookup; the code itself is encrypted',
    code_encrypted          BLOB NOT NULL,
    amount                  DECIMAL(38,0) NOT NULL COMMENT 'kobo',
    status                  VARCHAR(16) NOT NULL COMMENT 'active | redeemed | cancelled',
    transaction_id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    redeemed_by_user_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    redeem_transaction_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    redeemed_at             DATETIME(6) NULL,
    cancelled_at            DATETIME(6) NULL,
    version                 INT UNSIGNED NOT NULL DEFAULT 1,
    created_at              DATETIME(6) NOT NULL,
    updated_at              DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY cashpoints_code_hash_unique (code_hash),
    KEY cashpoints_creator_index (creator_user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Money a user earned because a friend they invited did something.
CREATE TABLE referral_earnings (
    id              CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    friend_user_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    friend_name     VARCHAR(150) NOT NULL DEFAULT '',
    description     VARCHAR(150) NOT NULL DEFAULT '',
    amount_ngn      DECIMAL(38,0) NOT NULL,
    created_at      DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    KEY referral_earnings_user_index (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE inbox_messages (
    id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    kind        VARCHAR(16)  NOT NULL COMMENT 'money | swap | security | cashpoint | referral | vault | card | promo | support',
    title       VARCHAR(150) NOT NULL,
    body        VARCHAR(500) NOT NULL DEFAULT '',
    href        VARCHAR(191) NULL,
    data        JSON NULL,
    read_at     DATETIME(6)  NULL,
    created_at  DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY inbox_messages_user_index (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE support_threads (
    id                CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id           CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    channel           VARCHAR(8)   NOT NULL COMMENT 'ticket | chat',
    subject           VARCHAR(191) NOT NULL,
    transaction_id    CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    status            VARCHAR(16)  NOT NULL DEFAULT 'open' COMMENT 'open | pending | resolved',
    priority          VARCHAR(8)   NOT NULL DEFAULT 'normal' COMMENT 'normal | high | urgent',
    unread_for_admin  INT UNSIGNED NOT NULL DEFAULT 0,
    version           INT UNSIGNED NOT NULL DEFAULT 1,
    created_at        DATETIME(6)  NOT NULL,
    updated_at        DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY support_threads_user_index (user_id),
    KEY support_threads_status_index (status, updated_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE support_messages (
    id           CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    thread_id    CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    sender_type  VARCHAR(8)   NOT NULL COMMENT 'customer | agent | system',
    author_name  VARCHAR(100) NOT NULL DEFAULT '',
    body         TEXT NOT NULL,
    created_at   DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY support_messages_thread_index (thread_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- One row per identity check (BVN, NIN, selfie, ID document, address ...).
CREATE TABLE kyc_verifications (
    id                CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id           CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tier              TINYINT UNSIGNED NOT NULL,
    check_type        VARCHAR(16)  NOT NULL COMMENT 'bvn | dob | nin | liveness | id_document | address',
    provider          VARCHAR(32)  NOT NULL DEFAULT 'simulator',
    reference         VARCHAR(64)  NOT NULL,
    status            VARCHAR(16)  NOT NULL COMMENT 'pending | approved | failed | cancelled',
    document_type     VARCHAR(32)  NULL,
    document_masked   VARCHAR(64)  NULL,
    match_score       TINYINT UNSIGNED NULL,
    data_encrypted    BLOB NULL COMMENT 'BVN/NIN etc, encrypted',
    reason            VARCHAR(255) NULL,
    submitted_at      DATETIME(6)  NOT NULL,
    decided_at        DATETIME(6)  NULL,
    decided_by        VARCHAR(100) NULL,
    version           INT UNSIGNED NOT NULL DEFAULT 1,
    PRIMARY KEY (id),
    KEY kyc_verifications_user_index (user_id),
    KEY kyc_verifications_status_index (status, submitted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE global_account_applications (
    id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id     CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    answers     JSON NOT NULL,
    status      VARCHAR(16) NOT NULL DEFAULT 'review',
    created_at  DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    KEY global_account_applications_user_index (user_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Every API call (bodies redacted) for the admin "API trail" page.
CREATE TABLE api_request_logs (
    id              BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    request_id      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    actor_type      VARCHAR(8)   NOT NULL DEFAULT '' COMMENT 'user | admin | guest',
    user_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    method          VARCHAR(8)   NOT NULL,
    path            VARCHAR(191) NOT NULL,
    status          SMALLINT UNSIGNED NOT NULL,
    latency_ms      INT UNSIGNED NOT NULL,
    ip_address      VARCHAR(45)  NOT NULL DEFAULT '',
    device          VARCHAR(191) NOT NULL DEFAULT '',
    transaction_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    request_body    MEDIUMTEXT NULL,
    response_body   MEDIUMTEXT NULL,
    error_message   VARCHAR(255) NULL,
    created_at      DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    KEY api_request_logs_user_index (user_id, created_at),
    KEY api_request_logs_created_index (created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- +goose Down
DROP TABLE IF EXISTS api_request_logs;
DROP TABLE IF EXISTS global_account_applications;
DROP TABLE IF EXISTS kyc_verifications;
DROP TABLE IF EXISTS support_messages;
DROP TABLE IF EXISTS support_threads;
DROP TABLE IF EXISTS inbox_messages;
DROP TABLE IF EXISTS referral_earnings;
DROP TABLE IF EXISTS cashpoints;
DROP TABLE IF EXISTS virtual_cards;
DROP TABLE IF EXISTS card_designs;
DROP TABLE IF EXISTS gift_card_orders;
DROP TABLE IF EXISTS gift_card_brands;
DROP TABLE IF EXISTS recurring_bills;
DROP TABLE IF EXISTS vault_tokens;
DROP TABLE IF EXISTS bill_payments;
DROP TABLE IF EXISTS billers;
