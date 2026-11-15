-- Service switches: an admin can turn any customer-facing money service off
-- (for maintenance, a provider outage, a fraud wave) without a deploy.
--
--   code              fixed in Go (app/services/shared/services.go), because
--                     code enforces it; this table only stores the SWITCH
--   enabled           0 = the API refuses the service with 503 SERVICE_DISABLED
--   disabled_message  what the app shows while it is off (NULL = a default)
--   version           goes up on every admin edit (409 STALE_DATA on a clash)
--
-- A service with no row counts as ENABLED, so a missing row can never
-- switch something off by accident.

-- +goose Up
CREATE TABLE services (
    code              VARCHAR(32) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    name              VARCHAR(64)  NOT NULL,
    description       VARCHAR(255) NOT NULL DEFAULT '',
    enabled           TINYINT(1)   NOT NULL DEFAULT 1,
    disabled_message  VARCHAR(200) NULL COMMENT 'shown in the app while the service is off',
    version           INT UNSIGNED NOT NULL DEFAULT 1,
    created_at        DATETIME(6)  NOT NULL,
    updated_at        DATETIME(6)  NOT NULL,
    PRIMARY KEY (code)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Every service starts switched ON. The same list lives in Go
-- (shared.ServiceCatalog); a test checks the two never drift apart.
INSERT IGNORE INTO services (code, name, description, enabled, version, created_at, updated_at) VALUES
    ('swap',              'Swap',                       'Convert money between two of your own wallets.',                  1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('send_tribe',        'Send to a Tribe tag',        'Instant transfers to another Tribe customer (@tag).',             1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('send_bank',         'Send to a Nigerian bank',    'Naira payouts to Nigerian bank accounts.',                        1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('send_global',       'Global send',                'Payouts to bank accounts and mobile money outside Nigeria.',      1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('send_crypto',       'Crypto send',                'Withdrawals to an outside crypto wallet address.',                1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('bill_airtime',      'Airtime',                    'Airtime top-ups.',                                                1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('bill_data',         'Data bundles',               'Mobile data bundles.',                                            1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('bill_electricity',  'Electricity',                'Electricity tokens and meter payments.',                          1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('bill_tv',           'TV subscriptions',           'Cable and satellite TV subscriptions.',                           1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('recurring_bills',   'Recurring bills',            'Creating bill schedules, and the scheduled payments themselves.', 1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('gift_card_buy',     'Buy gift cards',             'Buying gift cards with any wallet.',                              1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('gift_card_sell',    'Sell gift cards',            'Selling a gift card to Tribe for Naira.',                         1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('virtual_cards',     'Virtual cards',              'Creating, funding and withdrawing from virtual cards.',           1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('cashpoint_create',  'Create cashpoints',          'Creating a cashpoint code from the Naira wallet.',                1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('cashpoint_redeem',  'Redeem cashpoints',          'Redeeming a cashpoint code into the Naira wallet.',               1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('referral_withdraw', 'Referral withdrawals',       'Moving referral earnings into the Naira wallet.',                 1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('wallet_open',       'Open a currency wallet',     'Opening a wallet in a new currency or coin.',                     1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6)),
    ('receive',           'Receive and add money',      'Account numbers and addresses for paying into a wallet.',         1, 1, UTC_TIMESTAMP(6), UTC_TIMESTAMP(6));

-- Admins who can see or manage providers get the matching new permission,
-- so the people who run the partners can run the switches too. It also keeps
-- super admins super: "super admin" means holding EVERY permission
-- (adminauth.IsSuperAdmin), and they already hold both provider ones.
INSERT IGNORE INTO admin_user_permissions (admin_user_id, permission)
SELECT admin_user_id, 'services.view' FROM admin_user_permissions WHERE permission = 'providers.view';
INSERT IGNORE INTO admin_user_permissions (admin_user_id, permission)
SELECT admin_user_id, 'services.manage' FROM admin_user_permissions WHERE permission = 'providers.manage';

-- +goose Down
DELETE FROM admin_user_permissions WHERE permission IN ('services.view', 'services.manage');
DROP TABLE IF EXISTS services;
