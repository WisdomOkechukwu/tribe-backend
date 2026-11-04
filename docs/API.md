# Tribe API guide

This guide explains how the Tribe API works and how to call every endpoint. It assumes you have never used it before. For a hands-on tour, run `./scripts/try-the-api.sh` (see the README), which goes through most of these steps with real `curl` commands.

---

## 1. The big picture

```
 ┌──────────────┐  /api/mobile/v1/...   ┌──────────────────────┐      ┌─────────┐
 │ tribe-mobile │ ────────────────────▶ │                      │ ───▶ │  MySQL  │  the truth: users, journal, balances
 └──────────────┘                       │  cmd/server (Go API) │      └─────────┘
 ┌──────────────┐  /admin-api/...       │                      │      ┌─────────┐
 │ tribe-admin  │ ────────────────────▶ │                      │ ───▶ │  Redis  │  cache, locks, queue, live rates
 └──────────────┘                       └──────────────────────┘      └─────────┘
                                             ▲            │ jobs              ▲
                         rates.updated event │            ▼                   │
                                    ┌────────────────┐  ┌────────────┐         │
                                    │ cmd/scheduler  │  │ cmd/worker │ ────────┘
                                    │ cron + rates   │  │ queue jobs │
                                    └────────────────┘  └────────────┘
```

Three programs run side by side:

| Program | Laravel equivalent | What it does |
|---|---|---|
| `cmd/server` | `php artisan serve` | Answers HTTP requests |
| `cmd/worker` | `php artisan queue:work` | Runs queued jobs: emails, provider payouts, statements, push notifications |
| `cmd/scheduler` | `php artisan schedule:work` | Runs cron jobs (daily snapshot, cleanup, recurring bills) and the **live rate collector** |

## 2. Two URL styles

| App | Base URL | Style |
|---|---|---|
| Mobile app | `/api/mobile/v1` | **Versioned** (old app versions in people's pockets keep working when we ship `v2`) and **kebab-case** paths, e.g. `/gift-cards/buy`, `/recurring-bills/{id}` |
| Admin panel | `/admin-api` | **No version** (the admin site and the API are always deployed together). Plain REST resources, e.g. `/customers/{id}/status`, `/team-members/{id}` |

## 3. Every response has the same shape

Success:

```json
{ "success": true, "message": "Swap complete.", "data": { ... }, "meta": { "next_cursor": "..." } }
```

Error:

```json
{ "success": false, "message": "You don't have enough balance for this.", "code": "INSUFFICIENT_FUNDS",
  "errors": { "amount": ["Enter a valid amount."] } }
```

- `message` is written for people, so the app can show it as-is.
- `code` is for code. Use it to decide what to do (e.g. `DEVICE_NOT_ACTIVE` means "sign out").
- `errors` only appears for form problems (HTTP 422) and lists messages per field.
- **Money is always a string**, e.g. `"12.50"` or `"0.00500000"`. Never use a JS `number` for money.

| HTTP status | Meaning |
|---|---|
| 200 / 201 | OK / created |
| 202 | Accepted: started, finishes later (e.g. a statement, or a sign-in on a new phone) |
| 400 | A required header is missing (e.g. `Idempotency-Key`) |
| 401 | Not signed in (`UNAUTHENTICATED`) or this phone is no longer the active one (`DEVICE_NOT_ACTIVE`) |
| 403 | Not allowed (frozen account, missing admin permission) |
| 404 | Not found |
| 408 | `BODY_INCOMPLETE`: the request body did not arrive in full within `REQUEST_BODY_TIMEOUT_SECONDS` (15 s) |
| 409 | Conflict: `REQUEST_IN_PROGRESS`, `STALE_DATA` (someone changed it first), `QUOTE_EXPIRED` |
| 422 | Validation or business rule (`INSUFFICIENT_FUNDS`, `WRONG_PIN`, ...) |
| 413 | `BODY_TOO_LARGE`: the body is over `REQUEST_BODY_MAX_BYTES` (256 KB), refused before it is read |
| 429 | Too many requests / PIN locked. Sign-in allows 10 attempts per email per 15 minutes (admin: 5), password change 10 per user, PIN 5 then a lock. Attempts are counted **before** the password or PIN is checked, so parallel guesses cannot slip past the limit. |
| 5xx | Our fault. Safe to retry **with the same Idempotency-Key**. |

## 4. Headers the mobile app sends

| Header | When | Why |
|---|---|---|
| `Authorization: Bearer <access_token>` | Every signed-in request | Who you are (the token lasts 15 minutes) |
| `X-Device-Imei: <device id>` | **Every** request | Which phone this is (single-device rule, §5) |
| `Idempotency-Key: <uuid>` | Every request that **changes** something | Makes retries safe (§6) |
| `Content-Type: application/json` | Requests with a body | |

Admin requests send `Authorization: Bearer <admin token>`, plus an `Idempotency-Key` on the routes marked ⓘ.

## 5. Single-device sign-in

iOS and modern Android don't let apps read the real IMEI. The app sends the most stable ID the phone allows instead: Android ID or iOS identifierForVendor, falling back to an ID kept in SecureStore. The backend calls this value `device_imei`.

```
Register (phone A)  ─────────────▶ phone A becomes ACTIVE ("new entry", no check)

Login on phone A    ─────────────▶ 200 tokens            (same active phone)
Login, no active phone ──────────▶ 200 tokens            ("new entry")
Login on phone B    ─────────────▶ 202 { next: "verify_new_device", verification_id }
                                   + a 6-digit code is emailed (queued job)
POST auth/device/verify (phone B) ▶ 200 tokens
                                   phone B ACTIVE, phone A "replaced", every session of A revoked
Phone A's next request ──────────▶ 401 DEVICE_NOT_ACTIVE  → the app signs out
```

How it is enforced:

- **In the database.** `user_devices.active_user_id` is a generated column with a UNIQUE index, so two active phones per user is impossible, even if two phones verify at the same millisecond.
- **On every request.** The `EnsureSingleDevice` middleware checks that the session is alive, that its phone is still active, and that `X-Device-Imei` matches.
- **Inside money operations.** The user and device rows are re-checked with a shared lock, so a phone that is being replaced can't slip a payment through.

## 6. Idempotency: why retrying is always safe

1. Create a new UUID **when the user taps the button**, and keep it until you get a final answer.
2. Send it as `Idempotency-Key`. If the network fails, retry with **the same key and the same body**.
3. The server remembers every key:

| You send | You get |
|---|---|
| A new key | The work is done and the answer saved |
| The same key, finished | The **saved answer** again (header `Idempotent-Replay: true`). Nothing runs twice. |
| The same key, still running | `409 REQUEST_IN_PROGRESS`. Wait and retry. |
| The same key, different body | `422 IDEMPOTENCY_KEY_REUSED` (a bug in the app) |

Behind it are four layers:

- A Redis `SETNX` front guard turns away a second copy that arrives while the first is still running (a double tap), before MySQL is touched. If Redis is down, the guard is skipped.
- The `idempotency_keys` table, with UNIQUE(actor, key).
- A journal reference derived from the key, with a UNIQUE index on `journal_entries.reference`.
- Quotes that can be used only once.

## 7. Prices, quotes and the NGN rail

Swaps and cross-currency sends work in two steps:

1. `POST /quotes`. The server prices it and **locks the price for 30 seconds**.
2. `POST /swaps` (or `/sends`) with the `quote_id` and the PIN. The quote can be used **once**.

**The NGN rail.** Every provider has a `uses_ngn_rail` switch (admin → Providers).

| Situation | Route |
|---|---|
| One side is NGN | One leg, e.g. `USD → NGN` |
| Different providers (e.g. BTC on the crypto provider, GBP on the global one) | **Always through Naira**: `BTC → NGN → GBP` |
| Same provider, `uses_ngn_rail = ON` | Through Naira: `USD → NGN → GBP` |
| Same provider, rail OFF | Direct: `USDT → USDC` |

Every leg is saved in `swap_legs` with its rate. On the NGN rail the journal also records the Naira received and paid; the difference is posted to *FX Margin Revenue*.

## 8. Live rates (Server-Sent Events)

`GET /api/mobile/v1/rates/stream` keeps the connection open and pushes events:

```
event: rates.snapshot
id: 41
data: {"version":41,"collected_at":"...","is_stale":false,"rates":{"BTC":{"usd":"98410.12","ngn":"151551584.80","change_today_percent":"0.42"}, ...}}

event: rates.updated
id: 42
data: { ...all rates again... }

: heartbeat
```

How the events get to the phone:

1. The scheduler's collector goroutine asks the provider for **every asset in USD and NGN** every `RATE_REFRESH_SECONDS` (10 s).
2. It stores them in `exchange_rates` (history) and `exchange_rate_latest`.
3. It publishes one event with **all** rates to Redis.
4. Each API server's `RateHub` goroutine forwards that event to every open stream.

The app opens the stream only on screens that show rates (swap, send, card funding, gift cards), saves each event on the device, and redraws. `GET /rates` returns the same data without streaming, for the first paint or as a fallback.

## 9. Pagination

| App | How it works |
|---|---|
| Mobile | `GET /transactions?cursor=<meta.next_cursor>`. Keep calling until `next_cursor` is empty. |
| Admin | `?page=2`. Responses look like `{ rows, total, page, page_size: 50, pages }`. |

---

## 10. Mobile endpoints: `/api/mobile/v1`

🔓 = no token needed · 🔑 = signed in on the active phone · ⓘ = needs `Idempotency-Key` · 📌 = needs `"pin"` in the body

### Sign-up and sign-in
| Method & path | Body | Returns |
|---|---|---|
| 🔓 `POST /auth/email/check` | `{email}` | `{available}` |
| 🔓 `POST /auth/email/send-code` | `{email, purpose?: signup\|password_reset}` | `{sent, debug_code?}`. **Queue example**: the worker sends the email. `debug_code` is only returned locally. A new code cancels the older ones, and there is no wait between codes. Limits: 10 per day from one IP address, 50 per day per email (`429 TOO_MANY_CODES`). `/auth/email/verify` allows 5 guesses per code and 20 wrong guesses per email per day (`429 TOO_MANY_ATTEMPTS`). |
| 🔓 `POST /auth/email/verify` | `{email, code (6 digits), purpose?}` | `{verification_token}` |
| 🔓 `GET /auth/tags/{tag}/available` | – | `{available}` |
| 🔓 `POST /auth/register` | `{email, verification_token, password, first_name, last_name, tag, phone?, country, referral_code?, heard_from?, device:{imei, name, platform, model, app_version}}` | `201 {user, tokens}` |
| 🔓 `POST /auth/login` | `{email, password, device:{imei,...}}` | `200 {next:"home", user, tokens}` **or** `202 {next:"verify_new_device", verification_id}` |
| 🔓 `POST /auth/device/verify` | `{verification_id, code (6 digits)}` + header `X-Device-Imei` | `{user, tokens}` |
| 🔓 `POST /auth/refresh` | `{refresh_token}` + header `X-Device-Imei` | `{tokens}`. Each refresh token works once. |
| 🔑 `POST /auth/logout` | – | – |
| 🔓 `POST /auth/password/forgot` | `{email}` | always `{sent:true}` |
| 🔓 `POST /auth/password/reset` | `{email, verification_token, password}` | signs every phone out |

`tokens` = `{access_token, access_token_expires_at, refresh_token, refresh_token_expires_at, session_id}`

### Profile, security and KYC
| Method & path | Body |
|---|---|
| 🔑 `GET /profile` | – (returns the user with `has_pin`, `kyc_tier`, `kyc_status`, `kyc_pending_tier` (tier with an open request, or null), `kyc_submitted_at`, `version`...) |
| 🔑ⓘ `PATCH /profile` | `{first_name?, last_name?, address?, date_of_birth?, world?, biometrics_enabled?, two_factor_enabled?}` |
| 🔑ⓘ `PATCH /profile/tag` | `{tag}` (once every 30 days) |
| 🔑ⓘ `POST /onboarding/complete` | `{world: global\|crypto\|card\|bills}` |
| 🔑ⓘ `POST /pin` | `{pin}` |
| 🔑ⓘ `POST /pin/change` | `{current_pin, new_pin}` |
| 🔑ⓘ `POST /password/change` | `{current_password, new_password}` |
| 🔑ⓘ `POST /phone/send-otp` | `{phone}` → `{sent, debug_code (local only)}`. The number is saved only after it is verified. A new code kills every earlier one, and there is no wait between codes. `429 TOO_MANY_CODES` after 10 in an hour, with `errors.retry_after[0]` = seconds until another is allowed |
| 🔑ⓘ `POST /phone/verify` | `{code}` (exactly 6 digits). `WRONG_CODE`, `CODE_EXPIRED`, `429 TOO_MANY_ATTEMPTS` after 5 wrong tries |
| 🔑 `GET /devices` | – (active phone + history, `is_current`) |
| 🔑 `POST /push-tokens` | `{token, platform}` |
| 🔑 `GET /kyc` | – → `{tier, status, open: {id, tier, submitted_at} \| null, tiers: [{tier, state, submitted_at, decided_at, reason}] (always tiers 1-3), checks}`. `state` is `verified \| in_review \| needs_attention \| available \| locked`; `reason` is the rejection reason for `needs_attention` |
| 🔑ⓘ `POST /kyc/tier-1` | `{bvn (11 digits), date_of_birth (YYYY-MM-DD, 18+)}` (approved immediately by the simulator) |
| 🔑ⓘ `POST /kyc/tier-2` | `{nin (11 digits)}` (waits for an admin decision) |
| 🔑ⓘ `POST /kyc/tier-3` | `{document_type: drivers_licence \| voters_card \| nin_slip \| passport, document_number, address (8+ chars)}`. Number formats: NIN slip 11 digits, voter's card 19 letters/digits, driver's licence 8-15 letters/digits, passport a letter + 8 digits (spaces ignored) |
| 🔑ⓘ `POST /global-accounts/apply` | free-form answers |

**KYC requests.** Each submit is ONE request for that tier. Only one request can be open at a time (a unique index backs this up): `409 VERIFICATION_IN_REVIEW` while one is waiting, `409 ALREADY_VERIFIED` for a tier the customer already has, `422 TIER_ORDER` when the previous tier isn't done, `422 VALIDATION_FAILED` with errors on `bvn`, `date_of_birth`, `nin`, `document_type`, `document_number` or `address`. When an admin decides, the customer gets a `security` inbox message ("Tier 2 approved" / "Tier 2 needs attention", linking to `/verify`).

### Wallets, rates and transactions
| Method & path | Body / query |
|---|---|
| 🔑 `GET /assets` | – |
| 🔑 `GET /wallets` | – → `{balances:{NGN:"250000.00",...}, opened:[...], wallets:[...], referral_balance}` |
| 🔑ⓘ `POST /wallets/{code}/open` | – |
| 🔑 `GET /wallets/{code}/receive` | – (placeholder account / address) |
| 🔑 `GET /rates` | – |
| 🔑 `GET /rates/stream` | SSE (§8) |
| 🔑 `POST /quotes` | `{transaction_type: swap\|send, from_asset, to_asset, amount}` (what you pay) **or** `receive_amount` |
| 🔑 `GET /transactions` | `?category=&type=&asset=&card_id=&cursor=` |
| 🔑 `GET /transactions/{id}` | – → `{transaction, route:[legs]}` |

### Swap and send
| Method & path | Body |
|---|---|
| 🔑ⓘ📌 `POST /swaps` | `{quote_id, pin}` |
| 🔑 `GET /recipients/lookup?tag=ada` | – |
| 🔑 `GET /channels?currency=GBP` | – |
| 🔑ⓘ📌 `POST /sends` | `{recipient_kind: tribe\|bank\|wallet, handle, recipient_name?, asset, amount}`, **or** `quote_id` for a different currency, plus `{channel_id?, network?, country?, details?, save_beneficiary?, pin}`. **Queue example**: bank/wallet sends return `pending`; the worker pays out and completes or refunds them. |
| 🔑 `GET /beneficiaries`, 🔑ⓘ `DELETE /beneficiaries/{id}` | |
| 🔑ⓘ `POST /beneficiaries` | `{kind: tribe\|bank\|wallet, name, handle, asset, channel_id?, network?, country?, details?}`. Returns `409 BENEFICIARY_EXISTS` if this recipient is already saved. |

### Bills
| Method & path | Body |
|---|---|
| 🔑 `GET /billers` | – (with plans) |
| 🔑 `POST /bills/verify-customer` | `{biller_id, target}` |
| 🔑ⓘ📌 `POST /bills` | `{biller_id, target, plan_id?, amount_ngn, pay_with, pin}`. The money is taken first and the biller is asked **after** the commit (never inside a database transaction). Usually the answer is immediate and the transaction comes back `completed` with the token in `meta`. If the biller is slow it comes back `pending` (message says so); the worker finishes it and an inbox message follows. If the biller refuses: `422 PROVIDER_REFUSED` and the money (fee included) is already back in the wallet. Simulator: a target ending in `0000` is refused, one ending in `5555` times out once. |
| 🔑 `GET /vault/tokens`, 🔑ⓘ `PATCH /vault/tokens/{id}` | `{loaded}` |
| 🔑 `GET /recurring-bills`, 🔑ⓘ `POST` (`pin` = consent), `PATCH /recurring-bills/{id}` (send `version`), `DELETE /recurring-bills/{id}` | **Cron example**: due schedules are paid at 07:00. `PATCH` needs `pin` again when it changes `amount_ngn`, `pay_with` or `frequency`, or turns a schedule back on (`active:true`), because that is new consent. Pausing needs no PIN. |

### Gift cards, cashpoints and referrals
| Method & path | Body |
|---|---|
| 🔑 `GET /gift-cards/brands` | – |
| 🔑ⓘ📌 `POST /gift-cards/buy` | `{brand_id, face_usd, asset, pin}` → `{transaction, code}`. Same two steps as bills: `code` is empty when the transaction is still `pending`; `422 PROVIDER_REFUSED` means refunded. |
| 🔑 `POST /gift-cards/{transaction_id}/reveal` | `{pin}` → `{code}`. Shows a bought card's code again (for purchases that finished after the buy screen). `409 GIFT_CARD_PENDING` while it is still on its way. Not stored or cached (`Cache-Control: no-store`). |
| 🔑ⓘ📌 `POST /gift-cards/sell` | `{brand_id, face_usd, asset, code, pin}` |
| 🔑 `GET /cashpoints` | – |
| 🔑ⓘ📌 `POST /cashpoints` | `{amount_ngn, pin}` → `{code (12 digits), amount}` |
| 🔑ⓘ `POST /cashpoints/redeem` | `{code}` (the row is locked, so only one person can redeem a code) |
| 🔑ⓘ `POST /cashpoints/{id}/cancel` | – |
| 🔑 `GET /referrals`, 🔑ⓘ📌 `POST /referrals/withdraw` | `{amount_ngn, pin}` |

### Virtual cards and their stored designs
| Method & path | Body |
|---|---|
| 🔑 `GET /card-designs` | – → `{themes, swatches, patterns}` (managed by admins in `card_designs`) |
| 🔑 `GET /cards` | – (balance from the ledger; never the full number) |
| 🔑ⓘ📌 `POST /cards` | `{label, network: visa\|mastercard, design_finish, design_shade?, first_top_up, pay_with, pin}` |
| 🔑ⓘ📌 `POST /cards/{id}/fund` / `withdraw` | `{amount_usd, asset, pin}` |
| 🔑ⓘ `POST /cards/{id}/freeze` / `unfreeze` | – |
| 🔑ⓘ `PATCH /cards/{id}` | `{label?, design_finish?, design_shade?, monthly_limit?, online_payments_enabled?, in_wallet?, version?}` |
| 🔑ⓘ `PATCH /cards/{id}/design` | `{design_finish, design_shade?, version?}`. Changes only the look. |
| 🔑ⓘ `PATCH /cards/{id}/limit` | `{monthly_limit, version?}`. Changes only the monthly limit (USD). |
| 🔑📌 `POST /cards/{id}/reveal` | `{pin}` → `{number, cvv, expiry}` (never stored) |
| 🔑ⓘ📌 `DELETE /cards/{id}` | `{pin}` (the balance goes back to the USD wallet) |

`design_finish` uses the app's format: `"midnight"`, `"sunset:waves"` or `"custom:FF7A5C:dots"`. The server checks it against the ENABLED stored designs and saves both the string and the parsed parts (`design_settings`) on the card. A bare theme (`"sunset"`) needs only that theme; `"sunset:waves"` needs the theme and the pattern; `"custom:FF7A5C:dots"` needs the pattern (`plain` when left out) and allows any colour. Only new picks are checked: a card keeps a design an admin later turned off, and re-sending the finish it already wears (for example in `PATCH /cards/{id}`) is accepted.

### Inbox, support and statements
| Method & path | Body |
|---|---|
| 🔑 `GET /inbox`, `POST /inbox/{id}/read` (`id` = `all` marks everything) | |
| 🔑 `GET /support/tickets`, `GET /support/tickets/{id}` | |
| 🔑ⓘ `POST /support/tickets` | `{subject, body, transaction_id?}` |
| 🔑ⓘ `POST /support/tickets/{id}/messages` | `{body}` |
| 🔑 `GET /support/stream` | **WebSocket** (live support chat). Same headers as any signed-in call (`Authorization`, `X-Device-Imei`). Sends `{"type":"connected"}`, then one JSON text frame per change to **this customer's** threads: `{"type":"thread.updated\|thread.status\|chat.message\|ticket.new","thread_id","user_id"}`. Reload that ticket when one arrives. The server pings every 25 s; nothing is read from the phone (messages are still sent with `POST`). Other customers' events are never sent. |
| 🔑ⓘ `POST /statements` | `{asset?, from_date, to_date}` → `202`. **Queue example.** |

---

## 11. Admin endpoints: `/admin-api`

Sign in with `POST /admin-api/sessions {email, password}`, which returns `{admin, access_token, expires_at}`. Every other route needs the permission shown, and **every change is written to the audit log** with before and after values. **Every admin write (POST, PUT, PATCH, DELETE) needs an `Idempotency-Key`**, just like the mobile app.

| Area | Endpoints (permission) |
|---|---|
| Session | `DELETE /sessions/current`, `GET /me`, `GET /permissions` |
| Team | `GET /team-members` (team.view) · `POST`, `PATCH /team-members/{id}`, `DELETE /team-members/{id}` (team.manage) |
| Overview | `GET /overview` (overview.view) → everything below in one object (the stats keys plus `series`, `by_category_7`, `failed_recent`, `inventory`). The dashboard can instead load each card on its own, same permission: `GET /overview/stats` → `{as_of, customers, signups_7, kyc_pending, open_tickets, transactions_24, failed_24, fail_rate_24, pending_all, volume_1_usd, volume_7_usd}` · `GET /overview/series` → `[{day, volume_usd, count, failed}]` (last 30 days) · `GET /overview/by-category` → `[{category, volume_usd, count}]` (last 7 days) · `GET /overview/failures` → the 8 newest failed transactions (`customer_name`/`customer_tag` empty without customers.view). Lists are `[]`, never `null` |
| Customers | `GET /customers?search=&status=&kyc=&label=&page=`, `GET /customers/{id}` (customers.view; personal data is masked without customers.pii) · `GET /customers/{id}/transactions` (transactions.view) · `/verifications` (kyc.view) · `/api-requests` (api.view) · `PATCH /customers/{id}/status {status, reason, version}` (customers.freeze) · `PUT /customers/{id}/labels {labels}` (customers.labels) |
| KYC | `GET /verifications?status=pending\|approved\|rejected&page=` (kyc.view): one row per verification REQUEST (oldest first while pending) with `{id, user_id, tier, status, reason, submitted_at, decided_at, decided_by, attempt, previous_reason, checks: [{id, check_type, status, provider, reference, document_type, document_masked, match_score}], customer_name, customer_tag, customer_email, current_tier}`. `GET /customers/{id}/verifications` returns the same rows for one customer, newest first · `POST /verifications/{id}/decisions {decision: approve\|reject, reason}` (kyc.decide): `{id}` is the request id; it decides every check in it. `reason` is required (3+ characters) when rejecting and the customer sees it. Approving moves the customer up to that tier (never down). `409 ALREADY_DECIDED` when it was already decided · `GET /verifications/counts` (kyc.view) → `{pending, approved, rejected, all}` (every status, 0 when none; `all` is the total) for the tabs |
| Transactions | `GET /transactions?category=&type=&status=&asset=&search=&from=&to=&page=` (each row has `customer_name` and `customer_tag`, empty without customers.view) · `GET /transactions/{id}` → `{transaction, customer, journal:[{entry, legs}], swap_legs}` (transactions.view) |
| Rates and pricing | `GET /currencies`, `GET /rates`, `GET /rates/history?asset=BTC&hours=24` (currencies.view) · `GET /rate-margins`, `GET /transaction-fees` (markups.view) · `PUT /rate-margins/{asset}`, `PUT /transaction-fees/{type}` (markups.edit; send `version`) |
| Inventory | `GET /inventory`, `GET /inventory/entries?asset=&page=` (journal legs on the company vaults), `GET /treasury-fundings?status=pending\|approved\|rejected` (inventory.view) · `POST /treasury-fundings {asset, amount, external_reference, note}` creates a **pending** request (inventory.topup) · `POST /treasury-fundings/{id}/approve` and `POST /treasury-fundings/{id}/reject {reason}` (inventory.approve). Only a **different** admin can decide (`403 FOUR_EYES_REQUIRED`), and the money is posted only on approval. Deciding twice returns `409 ALREADY_DECIDED`. |
| Free funds | `GET /free-funds` (inventory.view) → per currency `{asset_code, vault, owed, free}`. `free` = vault − what customers are owed (wallets, referral and card balances, cashpoint escrow, payouts and purchases in transit). It is the most admins can credit, and may be negative. Every currency is computed with a fixed number of queries (`ledger.FreeFundsForAssets`), giving the same numbers as `ledger.FreeFundsOf` one currency at a time. |
| Manual credits & debits | `POST /customers/{id}/adjustments {kind: credit\|debit, asset, amount, reason}` creates a **pending** request (adjustments.request) · `GET /customer-adjustments?status=&kind=&customer=&page=` (inventory.view; rows have `amount_decimal`, `customer_name`, `customer_tag`) · `POST /customer-adjustments/{id}/approve` and `POST /customer-adjustments/{id}/reject {reason}` (adjustments.approve **and** a super admin, i.e. every permission: `403 SUPER_ADMIN_REQUIRED`). Never your own (`403 FOUR_EYES_REQUIRED`). A credit bigger than the free funds is refused with `422 INSUFFICIENT_FREE_FUNDS`, a debit bigger than the wallet with `422 INSUFFICIENT_FUNDS`, both at request time and again at approval. Deciding twice returns `409 ALREADY_DECIDED`. |
| Ledger | `GET /ledger-accounts?owner=company\|<user id>&asset=`, `GET /ledger-accounts/{id}/legs`, `GET /journal-entries/{id}`, `GET /ledger-snapshots?date=`, `GET /ledger/verify[?full=true]` (ledger.view): by default checks only what changed since the last clean check (`mode: incremental`); `full=true` re-proves the whole ledger. Returns `{ok, mode, problems (first 1000), problem_count, entries_checked, snapshots_checked}` |
| Providers | `GET /providers` (providers.view) · `PUT /providers/{id} {uses_ngn_rail?, supports_direct_conversion?, enabled?, version}` (providers.manage) |
| Card designs | All need card_designs.manage. `GET /card-designs` → `{themes, swatches, patterns}`, disabled ones included, ordered by `sort_order` then `name` · `POST /card-designs {id, kind: theme\|swatch\|pattern, name, finish?, hex_color?, pattern?, enabled, sort_order}` → `201` with the row. `id` is kept as sent (2–48 characters like `theme-midnight`, `swatch-FF7A5C`, `pattern-dots`); a taken id, or the same look as another design of that kind (same theme `finish`, same `hex_color`, same `pattern`), is `409 DESIGN_EXISTS` · `PUT /card-designs/{id} {kind, name, finish?, hex_color?, pattern?, enabled, sort_order, version}` → the row; `version` (1 or more) must be the current one (`409 STALE_DATA`; `422` for 0; `404` for an unknown id) · `DELETE /card-designs/{id}?version=N` turns it off, `POST /card-designs/{id}/enable {version}` turns it back on; both need the current version, bump it and return the row. Per kind: a theme needs `finish` (lowercase letters), a swatch `hex_color` (6 hex digits, stored upper case), a pattern `pattern` (lowercase letters); `sort_order` is 0–10000; fields of other kinds are stored empty (`422 VALIDATION_FAILED` with `errors.finish`, `hex_color`, `pattern`, `sort_order` or `id`). Turning a design off only stops NEW picks: cards already wearing it keep it. The app may pick a bare theme (`sunset`, needs only that theme enabled), `theme:pattern` (needs both) or `custom:RRGGBB[:pattern]` (needs the pattern, `plain` when left out; any colour) |
| Support | `GET /support-threads?status=&channel=` (`meta.counts` = `{open, pending, resolved}`), `GET /support-threads/{id}` (support.view) · `GET /support-threads/stream`: **live inbox over SSE**. It sends `event: support` with `data: {"type":"ticket.new\|thread.updated\|thread.status\|...","thread_id":"..."}` on every change, plus a heartbeat every 15 s. Reload the thread when an event arrives. · `POST /support-threads/{id}/messages` (support.reply) · `PATCH /support-threads/{id}/status` (support.resolve) |
| Logs | `GET /api-requests?customer_id=&status=2xx\|4xx\|5xx&method=&path=&min_latency=` (api.view; bodies hidden without api.bodies) · `GET /audit-logs?action=` (audit.view) |
| Jobs | `GET /jobs` → `{schedule, cron_runs, queues, failed_jobs}` in one object, or one list at a time: `GET /jobs/runs` (latest 100 cron runs), `GET /jobs/queues` (`[{name, pending, active, scheduled, retry, failed, processed_today}]`), `GET /jobs/failed` (`[{id, queue, type, error, failed_at}]`), `GET /jobs/schedule` (`[{name, cron}]`) (all jobs.view; lists are `[]`, never `null`) · `POST /jobs/{queue}/{id}/retry` · `POST /jobs/daily-snapshot/run` (**cron example**) (jobs.manage) |

Seeded admins all use the password `Tribe@2026` (local only): `super@`, `ops@`, `support@`, `compliance@`, `finance@` and `viewer@tribe.africa`.

## 12. System endpoints

| Path | Use |
|---|---|
| `GET /health/live` | The process is running |
| `GET /health/ready` | MySQL and Redis are reachable, plus how many rate and support streams are open |
| `POST /webhooks/{provider}` | A provider (by slug, e.g. `global-simulator`) tells us about a payout. See below. |

### Provider webhooks

The provider sends JSON and signs the **raw body** with HMAC-SHA256:

```
POST /webhooks/global-simulator
X-Tribe-Signature: sha256=<hex HMAC of the body>
{"event_id":"evt_123","type":"payout.succeeded","data":{"transaction_id":"01J..."}}
```

- The secret comes from the env var `WEBHOOK_SECRET_<SLUG>`, e.g. `WEBHOOK_SECRET_GLOBAL_SIMULATOR`. Locally, simulators fall back to `simulator-webhook-secret`.
- A wrong signature gets `401 BAD_SIGNATURE`.
- Each `(provider, event_id)` is stored once in `provider_webhook_events`. A repeat returns `{duplicate:true}` and does nothing.
- `payout.succeeded` and `payout.failed` do **not** trust the body: they queue the payout job again, which asks the provider for the real status before anything is posted.

Try it:

```bash
BODY='{"event_id":"evt-1","type":"payout.succeeded","data":{"transaction_id":"<an external send id>"}}'
SIG="sha256=$(printf '%s' "$BODY" | openssl dgst -sha256 -hmac simulator-webhook-secret | sed 's/^.* //')"
curl -X POST localhost:8080/webhooks/global-simulator -H "X-Tribe-Signature: $SIG" -d "$BODY"
```

## 13. Worked example: a swap from start to finish

```bash
# 1. price it (locked for 30 s)
curl -X POST $API/api/mobile/v1/quotes -H "Authorization: Bearer $TOKEN" -H "X-Device-Imei: $PHONE" \
  -H 'Content-Type: application/json' -d '{"transaction_type":"swap","from_asset":"USD","to_asset":"GBP","amount":"20"}'
# -> data.id = quote id, data.route = USD->NGN, NGN->GBP (NGN rail)

# 2. confirm it (the PIN is checked, the quote is used once, the journal is posted)
curl -X POST $API/api/mobile/v1/swaps -H "Authorization: Bearer $TOKEN" -H "X-Device-Imei: $PHONE" \
  -H "Idempotency-Key: 6c1f3a8e-1b7e-4b7e-9d1c-2f2d1a0b9c11" -H 'Content-Type: application/json' \
  -d '{"quote_id":"<id from step 1>","pin":"2580"}'
```

What the server did in step 2, all in ONE database transaction:

1. Checked the PIN, counting the attempt even if it was wrong.
2. Locked the user and device rows in **share** mode. A phone switch now has to wait.
3. Used the quote: `UPDATE quotes SET used_at=NOW() WHERE id=? AND used_at IS NULL AND expires_at>NOW()` must change exactly 1 row.
4. Posted the journal entry:
   - **locked** the wallet accounts `FOR UPDATE` in id order;
   - checked the USD balance, as the snapshot plus legs after it;
   - wrote the legs:
     - Dr User USD 20.00
     - Cr Swap Clearing USD 19.90
     - Cr Fee Revenue USD 0.10
     - Dr Swap Clearing NGN (Naira received)
     - Cr Swap Clearing NGN (Naira paid)
     - Cr FX Margin Revenue NGN (the difference)
     - Dr Swap Clearing GBP 15.03
     - Cr User GBP 15.03
5. Saved the `transactions` row, two `swap_legs` rows and an inbox message.
6. Committed, then queued a push notification job.

If anything failed, **nothing** was saved.
