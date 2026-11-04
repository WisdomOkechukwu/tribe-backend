-- Tokens (assets), the providers that move them, and exchange rates.

-- +goose Up
-- Every currency or coin Tribe supports (NGN, USD, BTC ...).
CREATE TABLE assets (
    code                    VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name                    VARCHAR(64)  NOT NULL,
    kind                    VARCHAR(8)   NOT NULL COMMENT 'fiat | crypto',
    symbol                  VARCHAR(8)   NOT NULL DEFAULT '',
    decimal_places          TINYINT UNSIGNED NOT NULL COMMENT '2 for NGN (kobo), 8 for BTC (satoshi) ...',
    -- The provider that buys and sells this asset for us. The swap router
    -- compares the providers of the two assets to decide the route.
    conversion_provider_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    display_order           INT NOT NULL DEFAULT 0,
    enabled                 TINYINT(1) NOT NULL DEFAULT 1,
    created_at              DATETIME(6) NOT NULL,
    updated_at              DATETIME(6) NOT NULL,
    PRIMARY KEY (code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Outside companies we work with.
-- category: global (fiat money abroad), crypto, utilities (bills),
--           gift_card, virtual_card.
-- uses_ngn_rail: when ON, every conversion through this provider is done in
--           two steps through Naira (USD -> NGN -> GBP) instead of directly.
CREATE TABLE providers (
    id                          CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    slug                        VARCHAR(64)  CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name                        VARCHAR(100) NOT NULL,
    category                    VARCHAR(32)  NOT NULL COMMENT 'global | crypto | utilities | gift_card | virtual_card',
    uses_ngn_rail               TINYINT(1)   NOT NULL DEFAULT 0,
    supports_direct_conversion  TINYINT(1)   NOT NULL DEFAULT 1,
    is_simulator                TINYINT(1)   NOT NULL DEFAULT 1 COMMENT 'placeholder adapter; refused in production',
    enabled                     TINYINT(1)   NOT NULL DEFAULT 1,
    credentials_env_key         VARCHAR(64)  NULL COMMENT 'name of the env variable holding the secret (never the secret itself)',
    version                     INT UNSIGNED NOT NULL DEFAULT 1,
    created_at                  DATETIME(6)  NOT NULL,
    updated_at                  DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    UNIQUE KEY providers_slug_unique (slug)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Which assets each provider can handle.
CREATE TABLE provider_assets (
    provider_id  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    asset_code   VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    PRIMARY KEY (provider_id, asset_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- History: every rate the collector ever received.
-- usd_rate = how many US dollars ONE unit of the asset is worth.
-- ngn_rate = how many Naira ONE unit of the asset is worth.
CREATE TABLE exchange_rates (
    id            BIGINT UNSIGNED NOT NULL AUTO_INCREMENT,
    asset_code    VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    usd_rate      DECIMAL(38,18) NOT NULL,
    ngn_rate      DECIMAL(38,18) NOT NULL,
    provider_id   CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    collected_at  DATETIME(6) NOT NULL,
    PRIMARY KEY (id),
    KEY exchange_rates_asset_time_index (asset_code, collected_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- The newest rate for each asset (one row per asset, updated in place).
CREATE TABLE exchange_rate_latest (
    asset_code         VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    usd_rate           DECIMAL(38,18) NOT NULL,
    ngn_rate           DECIMAL(38,18) NOT NULL,
    opening_usd_rate   DECIMAL(38,18) NOT NULL COMMENT 'rate at the start of the day, for the "% today" figure',
    provider_id        CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL,
    rates_version      BIGINT UNSIGNED NOT NULL DEFAULT 0,
    collected_at       DATETIME(6) NOT NULL,
    PRIMARY KEY (asset_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Our spread per asset. When a user SELLS us an asset we value it a little
-- lower (sell margin); when they BUY it from us we charge a little more (buy margin).
CREATE TABLE rate_margins (
    asset_code           VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    buy_margin_percent   DECIMAL(10,6) NOT NULL DEFAULT 0,
    sell_margin_percent  DECIMAL(10,6) NOT NULL DEFAULT 0,
    version              INT UNSIGNED NOT NULL DEFAULT 1,
    updated_by           VARCHAR(100) NOT NULL DEFAULT '',
    updated_at           DATETIME(6) NOT NULL,
    PRIMARY KEY (asset_code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Fees per kind of transaction (the admin "Markups" screen).
CREATE TABLE transaction_fees (
    transaction_type  VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    category          VARCHAR(32)   NOT NULL,
    fee_percent       DECIMAL(10,6) NOT NULL DEFAULT 0,
    flat_fee          DECIMAL(20,8) NOT NULL DEFAULT 0 COMMENT 'in whole units of flat_fee_asset',
    flat_fee_asset    VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT 'USD',
    maximum_fee       DECIMAL(20,8) NULL COMMENT 'cap, in whole units of flat_fee_asset',
    active            TINYINT(1)    NOT NULL DEFAULT 1,
    version           INT UNSIGNED  NOT NULL DEFAULT 1,
    updated_by        VARCHAR(100)  NOT NULL DEFAULT '',
    updated_at        DATETIME(6)   NOT NULL,
    PRIMARY KEY (transaction_type)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- +goose Down
DROP TABLE IF EXISTS transaction_fees;
DROP TABLE IF EXISTS rate_margins;
DROP TABLE IF EXISTS exchange_rate_latest;
DROP TABLE IF EXISTS exchange_rates;
DROP TABLE IF EXISTS provider_assets;
DROP TABLE IF EXISTS providers;
DROP TABLE IF EXISTS assets;
