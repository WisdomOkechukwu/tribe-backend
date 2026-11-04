-- THE ACCOUNTING CORE (Nexum double-entry).
--
-- ledger_accounts         = the "buckets" money can sit in (a user's NGN wallet,
--                           the company's BTC vault, fee revenue ...).
-- journal_entries         = one financial event ("Ada swapped 10 USD to NGN").
-- journal_legs            = the lines of that event. Every leg is a DEBIT or a
--                           CREDIT of one account. For every asset in an entry,
--                           total debits must equal total credits.
-- ledger_daily_snapshots  = each account's closing balance at the end of a day,
--                           so we never have to add up years of legs.
--
-- Balance of an account = latest snapshot + all legs posted after it.

-- +goose Up
CREATE TABLE ledger_accounts (
    id               BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    account_number   CHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    -- Who owns the account: "company", "user:<user id>" or "card:<card id>".
    owner_key        VARCHAR(100) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    code             VARCHAR(48)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL COMMENT 'e.g. user_primary, company_vault, fee_revenue',
    name             VARCHAR(150) NOT NULL,
    -- asset | liability | revenue | expense | equity | contra_asset |
    -- contra_liability | suspense_asset | suspense_liability | summary
    category         VARCHAR(24)  NOT NULL,
    -- debit or credit: the side that INCREASES this account (from the Nexum table).
    normal_side      VARCHAR(6)   NOT NULL,
    asset_code       VARCHAR(10)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    virtual_card_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    account_type     VARCHAR(24)  NOT NULL COMMENT 'user_primary | user_card | user_referral | company',
    allow_negative   TINYINT(1)   NOT NULL DEFAULT 0 COMMENT 'user accounts can never go below zero',
    status           VARCHAR(16)  NOT NULL DEFAULT 'active' COMMENT 'active | frozen | closed',
    created_at       DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY ledger_accounts_number_unique (account_number),
    UNIQUE KEY ledger_accounts_owner_code_asset_unique (owner_key, code, asset_code),
    KEY ledger_accounts_user_index (user_id),
    KEY ledger_accounts_card_index (virtual_card_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE journal_entries (
    id                        CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    -- UNIQUE reference = the idempotency guard. Posting the same reference
    -- twice is impossible, so a retried request can never double-post.
    reference                 VARCHAR(120) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    transaction_type          VARCHAR(48)  NOT NULL,
    description               VARCHAR(255) NOT NULL DEFAULT '',
    status                    VARCHAR(16)  NOT NULL DEFAULT 'posted',
    reverses_entry_id         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    business_transaction_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT 'transactions.id this entry belongs to',
    created_by                VARCHAR(64)  NOT NULL DEFAULT 'system',
    metadata                  JSON NULL,
    accounting_date           DATE NOT NULL COMMENT 'Lagos calendar day',
    posted_at                 DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY journal_entries_reference_unique (reference),
    UNIQUE KEY journal_entries_reverses_unique (reverses_entry_id),
    KEY journal_entries_business_transaction_index (business_transaction_id),
    KEY journal_entries_posted_at_index (posted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE journal_legs (
    id                 BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    journal_entry_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    ledger_account_id  BIGINT UNSIGNED NOT NULL,
    asset_code         VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    side               VARCHAR(6)  NOT NULL COMMENT 'debit | credit',
    amount             DECIMAL(38,0) NOT NULL COMMENT 'smallest units, always positive',
    usd_rate           DECIMAL(38,18) NULL COMMENT 'USD value of one unit at posting time',
    ngn_rate           DECIMAL(38,18) NULL COMMENT 'NGN value of one unit at posting time',
    memo               VARCHAR(150) NOT NULL DEFAULT '',
    created_at         DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    KEY journal_legs_account_index (ledger_account_id, id),
    KEY journal_legs_entry_index (journal_entry_id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

CREATE TABLE ledger_daily_snapshots (
    id                   BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    ledger_account_id    BIGINT UNSIGNED NOT NULL,
    snapshot_date        DATE NOT NULL COMMENT 'the Lagos day this balance closes',
    closing_balance      DECIMAL(38,0) NOT NULL COMMENT 'positive = on the normal side of the account',
    total_debits         DECIMAL(38,0) NOT NULL DEFAULT 0 COMMENT 'debits posted during this day',
    total_credits        DECIMAL(38,0) NOT NULL DEFAULT 0 COMMENT 'credits posted during this day',
    last_journal_leg_id  BIGINT UNSIGNED NOT NULL DEFAULT 0 COMMENT 'legs with a bigger id are NOT in this snapshot',
    created_at           DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY ledger_daily_snapshots_account_date_unique (ledger_account_id, snapshot_date)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Journal rows are permanent. Mistakes are fixed by posting a REVERSAL
-- entry, never by editing or deleting. These triggers make the database
-- refuse any UPDATE or DELETE, even if some code tries.

-- +goose StatementBegin
CREATE TRIGGER journal_legs_block_update BEFORE UPDATE ON journal_legs
FOR EACH ROW BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'journal_legs are insert-only';
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER journal_legs_block_delete BEFORE DELETE ON journal_legs
FOR EACH ROW BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'journal_legs are insert-only';
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER journal_entries_block_update BEFORE UPDATE ON journal_entries
FOR EACH ROW BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'journal_entries are insert-only';
END;
-- +goose StatementEnd

-- +goose StatementBegin
CREATE TRIGGER journal_entries_block_delete BEFORE DELETE ON journal_entries
FOR EACH ROW BEGIN
    SIGNAL SQLSTATE '45000' SET MESSAGE_TEXT = 'journal_entries are insert-only';
END;
-- +goose StatementEnd

-- +goose Down
DROP TRIGGER IF EXISTS journal_entries_block_delete;
DROP TRIGGER IF EXISTS journal_entries_block_update;
DROP TRIGGER IF EXISTS journal_legs_block_delete;
DROP TRIGGER IF EXISTS journal_legs_block_update;
DROP TABLE IF EXISTS ledger_daily_snapshots;
DROP TABLE IF EXISTS journal_legs;
DROP TABLE IF EXISTS journal_entries;
DROP TABLE IF EXISTS ledger_accounts;
