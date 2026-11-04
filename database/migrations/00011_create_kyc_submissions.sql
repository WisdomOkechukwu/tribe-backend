-- One KYC REQUEST per tier ("submission"), holding that tier's checks.
--
-- Before this, every check (BVN, NIN, selfie ...) was its own row and admins
-- decided them one by one. Nothing stopped a customer from sending the same
-- tier again while it was still in review.
--
-- Now:
--   1. kyc_submissions has one row per request: pending | approved | rejected.
--   2. open_key holds the user id while a request is pending and NULL after.
--      UNIQUE(open_key) means a customer can have at most ONE open request.
--      MySQL allows many NULLs in a unique index, so decided requests never
--      clash. This works the same on MySQL 5.7 and 8.4.
--   3. kyc_verifications.submission_id links each check to its request.
--   4. phone_verifications gets an index for "codes sent in the last hour".
--
-- Backfill: the checks of one old submit share user, tier and submitted_at,
-- so each such group becomes one request. Its id is the group's earliest check
-- id (ULIDs, so it is unique and stable). If a customer had several pending
-- requests, only the newest stays open; older ones are closed as superseded.

-- +goose Up
CREATE TABLE kyc_submissions (
    id            CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    user_id       CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NOT NULL,
    tier          TINYINT UNSIGNED NOT NULL,
    status        VARCHAR(16)  NOT NULL COMMENT 'pending | approved | rejected',
    reason        VARCHAR(255) NOT NULL DEFAULT '' COMMENT 'why it was rejected; the customer sees it',
    submitted_at  DATETIME(6)  NOT NULL,
    decided_at    DATETIME(6)  NULL,
    decided_by    VARCHAR(100) NULL,
    open_key      CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL COMMENT 'user_id while pending, NULL once decided',
    version       INT UNSIGNED NOT NULL DEFAULT 1,
    PRIMARY KEY (id),
    UNIQUE KEY kyc_submissions_open_unique (open_key),
    KEY kyc_submissions_user_index (user_id, submitted_at),
    KEY kyc_submissions_status_index (status, submitted_at)
) ENGINE=InnoDB DEFAULT CHARSET=utf8mb4 COLLATE=utf8mb4_unicode_ci ROW_FORMAT=DYNAMIC;

ALTER TABLE kyc_verifications
    ADD COLUMN submission_id CHAR(26) CHARACTER SET ascii COLLATE ascii_bin NULL AFTER user_id,
    ADD KEY kyc_verifications_submission_index (submission_id);

ALTER TABLE phone_verifications
    ADD KEY phone_verifications_sent_index (user_id, created_at);

-- 1. One request per (user, tier, submitted_at) group of old checks.
INSERT INTO kyc_submissions (id, user_id, tier, status, reason, submitted_at, decided_at, decided_by, open_key, version)
SELECT MIN(id), user_id, tier,
       CASE
           WHEN SUM(status IN ('failed', 'cancelled')) > 0 THEN 'rejected'
           WHEN SUM(status = 'pending') > 0 THEN 'pending'
           ELSE 'approved'
       END,
       COALESCE(MAX(reason), ''), submitted_at, MAX(decided_at), MAX(decided_by), NULL, 1
  FROM kyc_verifications
 GROUP BY user_id, tier, submitted_at;

-- 2. Link every check to its request.
UPDATE kyc_verifications verification
  JOIN kyc_submissions submission
    ON submission.user_id = verification.user_id
   AND submission.tier = verification.tier
   AND submission.submitted_at = verification.submitted_at
   SET verification.submission_id = submission.id;

-- 3. Only the newest pending request per customer stays open. (The derived
--    table is materialised first, so MySQL lets us update the same table.)
UPDATE kyc_submissions submission
  JOIN (SELECT user_id, MAX(submitted_at) AS newest FROM kyc_submissions WHERE status = 'pending' GROUP BY user_id) latest
    ON latest.user_id = submission.user_id
   SET submission.status = 'rejected', submission.reason = 'Superseded by a newer request',
       submission.decided_at = UTC_TIMESTAMP(6), submission.decided_by = 'system'
 WHERE submission.status = 'pending' AND submission.submitted_at < latest.newest;

-- Two pending requests with the very same time: keep the higher id.
UPDATE kyc_submissions submission
  JOIN (SELECT user_id, MAX(id) AS newest_id FROM kyc_submissions WHERE status = 'pending' GROUP BY user_id) latest
    ON latest.user_id = submission.user_id
   SET submission.status = 'rejected', submission.reason = 'Superseded by a newer request',
       submission.decided_at = UTC_TIMESTAMP(6), submission.decided_by = 'system'
 WHERE submission.status = 'pending' AND submission.id < latest.newest_id;

-- The checks of superseded requests are closed too.
UPDATE kyc_verifications verification
  JOIN kyc_submissions submission ON submission.id = verification.submission_id
   SET verification.status = 'failed', verification.reason = submission.reason,
       verification.decided_at = submission.decided_at, verification.decided_by = submission.decided_by
 WHERE submission.status = 'rejected' AND verification.status = 'pending';

-- 4. Open the pending ones (now at most one per customer).
UPDATE kyc_submissions SET open_key = user_id WHERE status = 'pending';

-- 5. Repair customers. A tier-1 resubmit used to reset kyc_tier to 1, so
--    restore the highest approved tier ...
UPDATE users user_row
  JOIN (SELECT user_id, MAX(tier) AS highest FROM kyc_submissions WHERE status = 'approved' GROUP BY user_id) approved
    ON approved.user_id = user_row.id
   SET user_row.kyc_tier = GREATEST(user_row.kyc_tier, approved.highest);

-- ... then take kyc_status from the latest request ...
UPDATE users user_row
  JOIN kyc_submissions submission ON submission.user_id = user_row.id
  JOIN (SELECT user_id, MAX(submitted_at) AS latest FROM kyc_submissions GROUP BY user_id) newest
    ON newest.user_id = submission.user_id AND newest.latest = submission.submitted_at
   SET user_row.kyc_status = CASE submission.status WHEN 'pending' THEN 'pending' WHEN 'approved' THEN 'approved' ELSE 'failed' END;

-- ... and an open request always means "pending".
UPDATE users user_row
  JOIN kyc_submissions submission ON submission.user_id = user_row.id AND submission.status = 'pending'
   SET user_row.kyc_status = 'pending';

-- +goose Down
ALTER TABLE phone_verifications DROP KEY phone_verifications_sent_index;
ALTER TABLE kyc_verifications DROP KEY kyc_verifications_submission_index, DROP COLUMN submission_id;
DROP TABLE IF EXISTS kyc_submissions;
