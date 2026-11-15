-- PROVIDER VAULTS: the books mirror what each provider holds (see docs/ACCOUNTING.md).
--
-- The money sits with the providers, not with Tribe. So the ledger now has:
--   provider vaults  one per provider per currency it handles (provider_assets),
--                    owner key "provider:<provider id>", code provider_vault
--   Tribe Funds      one per currency: Tribe's own share of the money in the
--                    vaults (fees, margins, treasury top-ups; credits and
--                    referral rewards are paid from it)
--   customer balances (wallets, cards, referral, and the new user_cashpoint)
-- Every other company account (Company Vault, Treasury Funding, Swap
-- Clearing, the in-transit accounts, Earnings and Referral Vaults, Provider
-- Expense, Cashpoint Escrow, Income Summary) is gone. The accounts are rows
-- opened by the seeder, so none is created here.
--
-- IMPORTANT: made with a fresh database. On a database that already holds
-- money in the old accounts, those balances are no longer read by the code.

-- +goose Up

-- Treasury funding now names the provider whose vault the money went into.
-- NULL means the currency's home provider (assets.conversion_provider_id).
ALTER TABLE treasury_fundings
    ADD COLUMN provider_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL
        COMMENT 'provider vault funded; NULL = the currency''s home provider' AFTER asset_code;

-- +goose Down
ALTER TABLE treasury_fundings DROP COLUMN provider_id;
