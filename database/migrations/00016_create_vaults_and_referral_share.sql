-- VAULTS: the books are now organised around pots (see docs/ACCOUNTING.md).
--
--   Company Vault    real money at providers and banks (unchanged)
--   Earnings Vault   every fee and every conversion margin Tribe makes,
--                    one per currency. It replaces Fee Revenue, FX Margin
--                    Revenue and Customer Adjustments. Manual credits to
--                    customers are now paid OUT of it.
--   Referral Vault   (Naira) the share of an invited customer's earnings
--                    that belongs to the person who invited them. It
--                    replaces Referral Expense: rewards are no longer a
--                    flat amount, they are a percentage of what Tribe earned.
--
-- The vault accounts themselves are rows in ledger_accounts, opened by the
-- seeder from ledger.CompanyAccountTemplates, so no account is created here.
--
-- IMPORTANT: this change was made with a fresh database (make fresh). On a
-- database that already has money in fee_revenue, fx_margin_revenue,
-- referral_expense or customer_adjustments accounts, those balances are no
-- longer read by the code and would have to be moved by hand.

-- +goose Up

-- Where an earnings leg came from: fee | margin | referral_share | adjustment.
-- One Earnings Vault holds them all; this tag keeps reports able to split them.
ALTER TABLE journal_legs
    ADD COLUMN earning_source VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NULL
        COMMENT 'fee | margin | referral_share | adjustment (earnings vault legs only)' AFTER memo;

-- The market value of what a quote converts, in the to-asset, at the moment
-- it was priced. Received is lower by our margin; the difference is posted
-- to the Earnings Vault. NULL on the Naira rail, whose margin is the
-- naira_in_amount - naira_out_amount gap instead.
ALTER TABLE quotes
    ADD COLUMN market_to_amount DECIMAL(38,0) NULL
        COMMENT 'to_amount at market rate, before our margin (direct routes only)' AFTER naira_out_amount;

-- A referral earning now belongs to ONE transaction of the invited friend.
--   accrued    the share sits in the Referral Vault (the friend's transaction is
--              still waiting for a provider)
--   paid       it moved from the Referral Vault into the inviter's referral balance
--   cancelled  the friend's transaction was refused and reversed, share and all
ALTER TABLE referral_earnings
    ADD COLUMN transaction_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL
        COMMENT 'the invited friend''s transaction that earned it' AFTER friend_user_id,
    ADD COLUMN status VARCHAR(16) NOT NULL DEFAULT 'paid' COMMENT 'accrued | paid | cancelled' AFTER amount_ngn,
    ADD COLUMN share_percent DECIMAL(9,6) NULL COMMENT 'referral share in force when it was earned' AFTER status,
    ADD COLUMN accrual_entry_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER share_percent,
    ADD COLUMN payout_entry_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER accrual_entry_id,
    ADD COLUMN paid_at DATETIME(6) NULL AFTER payout_entry_id,
    ADD UNIQUE KEY referral_earnings_transaction_unique (transaction_id),
    ADD KEY referral_earnings_status_index (user_id, status, created_at);

-- The referral share: which percentage of what Tribe earns on an invited
-- customer's transaction goes to the person who invited them. One row (id 1),
-- edited by an admin (lock + version, like a fee rule).
CREATE TABLE referral_settings (
    id             TINYINT UNSIGNED NOT NULL,
    share_percent  DECIMAL(9,6) NOT NULL COMMENT '0 to 100: percent of earnings paid to the inviter',
    version        INT UNSIGNED NOT NULL DEFAULT 1,
    updated_by     VARCHAR(150) NOT NULL DEFAULT '',
    updated_at     DATETIME(6) NOT NULL,
    PRIMARY KEY (id)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

INSERT IGNORE INTO referral_settings (id, share_percent, version, updated_by, updated_at)
VALUES (1, 2.000000, 1, 'migration', UTC_TIMESTAMP(6));

-- +goose Down
DROP TABLE IF EXISTS referral_settings;
ALTER TABLE referral_earnings
    DROP KEY referral_earnings_status_index,
    DROP KEY referral_earnings_transaction_unique,
    DROP COLUMN paid_at,
    DROP COLUMN payout_entry_id,
    DROP COLUMN accrual_entry_id,
    DROP COLUMN share_percent,
    DROP COLUMN status,
    DROP COLUMN transaction_id;
ALTER TABLE quotes DROP COLUMN market_to_amount;
ALTER TABLE journal_legs DROP COLUMN earning_source;
