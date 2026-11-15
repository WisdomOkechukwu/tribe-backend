-- +goose Up
-- Admins can now ADD a currency or coin from the admin panel (Currencies),
-- and switch any currency on or off. Two things an asset needs for that:
--
--   price_source  where its live price comes from, so the rate collector no
--                 longer needs a list of codes written into the Go code:
--                   fiat_feed  the daily fiat feed (open.er-api.com), by code
--                   coinbase   Coinbase's spot price for CODE-USD
--                   usd_peg    always exactly one US dollar (stablecoins, USD)
--   version       the edit counter for "someone changed it first" (409 STALE_DATA)
ALTER TABLE assets
    ADD COLUMN price_source VARCHAR(16) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'fiat_feed'
        COMMENT 'fiat_feed | coinbase | usd_peg' AFTER conversion_provider_id,
    ADD COLUMN version INT UNSIGNED NOT NULL DEFAULT 1 AFTER enabled;

-- The currencies that already exist keep the sources the code used for them.
UPDATE assets SET price_source = 'usd_peg'  WHERE code IN ('USD', 'USDT', 'USDC');
UPDATE assets SET price_source = 'coinbase' WHERE code IN ('BTC', 'ETH', 'SOL');

-- Adding a currency touches what every customer can hold, so it follows the
-- people who already manage the providers (and keeps super admins super:
-- "super admin" means holding EVERY permission).
INSERT IGNORE INTO admin_user_permissions (admin_user_id, permission)
SELECT admin_user_id, 'currencies.manage' FROM admin_user_permissions WHERE permission = 'providers.manage';

-- +goose Down
DELETE FROM admin_user_permissions WHERE permission = 'currencies.manage';
ALTER TABLE assets DROP COLUMN version, DROP COLUMN price_source;
