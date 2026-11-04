-- Manual customer adjustments (replace the mobile "test top-up"):
--   credit = an admin gives a customer money from the company's FREE funds
--   debit  = an admin takes money back out of a customer's wallet
--
-- Like treasury funding they need two admins: one requests, a DIFFERENT one
-- approves, and only approval posts. Only a SUPER ADMIN may approve. A credit is refused when the vault's
-- free funds (vault minus what we already owe customers in that currency)
-- are too low; a debit is refused when the customer's wallet is too low.

-- +goose Up
CREATE TABLE customer_adjustments (
    id                  CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id             CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    kind                VARCHAR(8)   NOT NULL COMMENT 'credit | debit',
    asset_code          VARCHAR(10) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    amount              DECIMAL(38,0) NOT NULL,
    reason              VARCHAR(200) NOT NULL,
    status              VARCHAR(16)  NOT NULL DEFAULT 'pending' COMMENT 'pending | approved | rejected',
    requested_by        CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    requested_by_name   VARCHAR(100) NOT NULL DEFAULT '',
    approved_by         CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT 'the second admin (also set on rejection)',
    approved_by_name    VARCHAR(100) NOT NULL DEFAULT '',
    decided_at          DATETIME(6)  NULL,
    rejection_reason    VARCHAR(255) NOT NULL DEFAULT '',
    transaction_id      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' COMMENT 'the customer-facing transaction, empty until approved',
    journal_entry_id    CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL DEFAULT '' COMMENT 'empty until approved',
    created_at          DATETIME(6)  NOT NULL,
    PRIMARY KEY (id),
    KEY customer_adjustments_status_index (status, created_at),
    KEY customer_adjustments_user_index (user_id, created_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

-- Super admins (admins holding EVERY permission that existed before this
-- migration) get both new permissions, so they stay super admins. Approving
-- an adjustment is for super admins only (wallets.ApproveCustomerAdjustment).
INSERT IGNORE INTO admin_user_permissions (admin_user_id, permission)
SELECT admin_user_id, 'adjustments.approve' FROM admin_user_permissions
 WHERE permission IN ('overview.view','customers.view','customers.pii','customers.labels','customers.freeze','kyc.view','kyc.media','kyc.decide','transactions.view','currencies.view','inventory.view','inventory.topup','inventory.approve','markups.view','markups.edit','support.view','support.reply','support.resolve','support.simulate','api.view','api.bodies','audit.view','team.view','team.manage','ledger.view','providers.view','providers.manage','rates.view','card_designs.manage','jobs.view','jobs.manage')
 GROUP BY admin_user_id HAVING COUNT(DISTINCT permission) = 31;
INSERT IGNORE INTO admin_user_permissions (admin_user_id, permission)
SELECT admin_user_id, 'adjustments.request' FROM admin_user_permissions WHERE permission = 'adjustments.approve';

-- Admins who could request treasury funding may request adjustments too.
INSERT IGNORE INTO admin_user_permissions (admin_user_id, permission)
SELECT admin_user_id, 'adjustments.request' FROM admin_user_permissions WHERE permission = 'inventory.topup';

-- +goose Down
DELETE FROM admin_user_permissions WHERE permission IN ('adjustments.request', 'adjustments.approve');
DROP TABLE IF EXISTS customer_adjustments;
