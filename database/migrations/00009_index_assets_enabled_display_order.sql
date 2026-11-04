-- Index the assets lookup the mobile app and the rate collector run constantly.
-- Both the swap/send asset list (mobile money controller) and the collector read
-- "WHERE enabled = 1 ORDER BY display_order", which otherwise scans and sorts the
-- whole table. This composite index lets MySQL filter and return the rows already
-- ordered. Portable across MySQL 5.7 and 8.4 (a plain non-functional index).

-- +goose Up
CREATE INDEX assets_enabled_display_order_index ON assets (enabled, display_order);

-- +goose Down
DROP INDEX assets_enabled_display_order_index ON assets;
