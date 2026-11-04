# Go for Laravel developers: a tour of the Tribe backend

This guide is for someone who knows PHP and Laravel but has never written Go. It covers:

1. The few Go ideas you need, each compared with Laravel.
2. One request, **"send $5 to @ada"**, followed through the real code.
3. The ideas that keep money safe: locking, idempotency, the journal, snapshots, the NGN rail, queues, cron and live updates.

Keep the code open next to this page. Every file named here exists.

---

## Part 1: Go in ten ideas

| Go | Laravel / PHP | Where you see it |
|---|---|---|
| **package** (one folder = one package) | namespace | `app/services/transfers` is the `transfers` package |
| **struct** | a class with only properties | `models.User`, `requests.SendRequest` |
| **method** `func (service *Service) Send(...)` | a class method; `service` is `$this` | every service |
| **interface** | interface | `providers.PayoutProvider` (real or simulator) |
| **`err` return values** | exceptions | almost every function |
| **pointer** `*models.User` | an object passed by reference | services return `*models.Transaction` |
| **goroutine** `go doSomething()` | a helper running at the same time as you | the rate hub, the collector |
| **channel** `chan []byte` | a safe pipe between goroutines | the SSE connections |
| **`context.Context`** | "this request is still alive" + deadline | the first argument, `requestContext` |
| **`defer`** | `finally` | `defer application.Close()` |

### Errors instead of exceptions
Go has no `try/catch`. A function that can fail returns its result **and** an error, and the caller checks it straight away:

```go
transaction, err := controller.application.Transfers.Send(httpContext.Request.Context(), actor, input)
if err != nil {
	responses.Error(httpContext, err) // turns the error into the JSON error envelope
	return
}
```

`err` is the only short variable name this codebase allows, because every Go programmer recognises it. Everything else uses full words: `httpContext`, `databaseTransaction`, `walletBalance`.

Our own errors are `apperrors.AppError`. It carries an HTTP status, a `code` for the app (such as `INSUFFICIENT_FUNDS`) and a human message. `responses.Error` knows how to send it. Any other error becomes a 500 and is logged.

### Structs with tags = FormRequest rules
```go
type SendRequest struct {
	RecipientKind string `json:"recipient_kind" binding:"required,oneof=tribe bank wallet"`
	Amount        string `json:"amount" binding:"max=40"`
	Pin           string `json:"pin" binding:"required,len=4,numeric"`
}
```
- `json:"..."` is the field name in the JSON body.
- `binding:"..."` holds the validation rules, like Laravel's `'required|in:tribe,bank,wallet'`.
- `requests.Bind` runs them and returns a 422 with per-field messages.

### Goroutines and channels
A **goroutine** is a function started with `go`. It runs alongside everything else, like a helper working in the background. A **channel** is how goroutines hand each other messages without stepping on each other. You will only meet them in two places:

- `app/events/hub.go`: one goroutine listens to Redis and pushes each event into one channel per open SSE connection.
- `cmd/scheduler/main.go`: the rate collector and the push outbox each run in their own goroutine.

Everything else is ordinary top-to-bottom code. The web server already runs each request in its own goroutine for you, just as PHP-FPM gives each request its own process.

---

## Part 2: one request, "send $5 to @ada"

The phone sends:

```
POST /api/mobile/v1/sends
Authorization: Bearer <access token>
X-Device-Imei: 3f9a...            (which phone this is)
Idempotency-Key: 6c1e...          (a new UUID made when the user tapped "Send")

{"recipient_kind":"tribe","handle":"@ada","asset":"USD","amount":"5","pin":"2580"}
```

### Step 1: the route (`routes/mobile.go`)
```go
signedIn.POST("/sends", idempotent, moneyController.Send)
```
Read it as a sentence: *a signed-in POST to /sends must carry an Idempotency-Key, then MoneyController.Send runs.* The `signedIn` group already added the authentication, single-device and rate-limit middleware.

### Step 2: middleware (`app/http/middleware`)
They run in order, like Laravel's middleware stack:

1. **`RequestLogger`** records the call for the admin "API trail", with passwords, PINs and codes hidden.
2. **`AuthenticateUser`** checks the token.
3. **`EnsureSingleDevice`** checks that `X-Device-Imei` is the phone the token belongs to **and** is still the user's active phone. If the user has since signed in on another phone, this one gets `401 DEVICE_NOT_ACTIVE`.
4. **`RateLimit`** allows at most 240 requests a minute.
5. **`RequireIdempotencyKey`** (Part 3) makes the request safe to retry.

### Step 3: the controller (`app/http/controllers/mobile/money_controller.go`)
Controllers are deliberately boring: bind, call one service, respond.

```go
func (controller *MoneyController) Send(httpContext *gin.Context) {
	var input requests.SendRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	transaction, err := controller.application.Transfers.Send(httpContext.Request.Context(), middleware.CurrentActor(httpContext), transfers.SendInput{
		RecipientKind: input.RecipientKind, Handle: input.Handle, AssetCode: input.Asset, Amount: input.Amount,
		Pin: input.Pin, Reference: middleware.BusinessReference(httpContext, "SND"),
		// ...
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Money sent.", resources.Transaction(*transaction, controller.decimals(httpContext)))
}
```

`BusinessReference` gives `SND-<idempotency record id>`. It is the **same on every retry** of this request, and that matters in Step 5.

### Step 4: the service (`app/services/transfers/service.go`)
This is where the business logic lives (Laravel: a service class or action). `Send` checks the PIN, then `sendToTribeUser` does the work. The important part runs inside **one database transaction**:

```go
err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
	// a) Lock the sender's user row and re-check: not frozen, and THIS phone is still the active one.
	sender, err := shared.LockActor(databaseTransaction, actor)
	if err != nil {
		return err
	}
	// b) Work out the fee, then post the journal entry.
	postResult, err = service.ledger.Post(databaseTransaction, ledger.Entry{
		Reference: input.Reference + ":POST", TransactionType: "send_tribe",
		Legs: ledger.InternalTransferLegs(senderWallet.ID, receiverWallet.ID, feeRevenue.ID, converted, fee),
	})
	// c) Save the user-facing transaction rows (one "sent", one "received").
	// d) Write the inbox messages. The scheduler turns them into push notifications later.
	return nil
})
```

`database.WithTransaction` is like `DB::transaction(function () {...})`, with two extras:

- It uses the READ COMMITTED isolation level.
- If MySQL reports a deadlock or lock timeout, it **retries the whole function** up to 3 times.

If the function returns an error, everything inside is rolled back.

### Step 5: the ledger (`app/services/ledger/posting.go`)
`ledger.Post` is the **only** way money moves. The rule is:

```
Dr Sender wallet   5.00 + fee
Cr Ada's wallet    5.00
Cr Fee Revenue     fee
```

The steps, in order:

1. Validate the legs: at least two, and every amount above zero.
2. Insert the journal entry header. Its `reference` column is UNIQUE. If this exact reference was already posted (a retry), stop and return the old entry, so money moves **once**.
3. Lock every account the legs touch with `SELECT … FOR UPDATE`, **in ascending id order**.
4. Check that debits equal credits for each currency.
5. Check that no user wallet would go below zero (`INSUFFICIENT_FUNDS`).
6. Insert the legs. They can never be updated or deleted.

### Step 6: the response
`resources.Transaction` turns the model into JSON, like a Laravel API Resource, with money as **strings**:

```json
{"success":true,"message":"Money sent.","data":{"id":"01J...","status":"completed","from_amount":"5.00", ...}}
```

The idempotency middleware saves this exact response under the key before it leaves.

---

## Part 3: what keeps the money safe

### Row locking: "wait your turn"
`SELECT … FOR UPDATE` puts a lock on the rows. A second transaction that wants the same rows **waits** until the first commits. That is why 50 simultaneous $3 sends from a $100 wallet give exactly 33 successes and a final balance of $1.00 (see `tests/integration/concurrency_test.go`). Each send waits, sees the new balance, and the 34th is refused.

**Why ascending order?**

- Suppose request 1 locks Alice then Bob, while request 2 locks Bob then Alice. Each holds one lock and waits for the other forever. That is a deadlock.
- If everyone locks the lower id first, this cannot happen.
- `TestOppositeTransfersDoNotDeadlock` sends A→B and B→A 20 times at once to prove it.

**Editing information** (a profile, a card setting, a fee) follows the same steps: lock, re-read, check, write. Admin-edited rows also carry a `version` number. If you saved version 3 but the row is now version 4, someone else changed it first, and you get `409 STALE_DATA` instead of silently overwriting their change.

### Idempotency: retrying is always safe
Phones lose signal. If "Send" times out, the app retries with the **same `Idempotency-Key`**. The guard has four layers:

1. **A Redis `SETNX` front guard** turns away a second copy that arrives while the first is still running, such as a double tap.
2. **The `idempotency_keys` table** has UNIQUE(user, key). The first request inserts a row; a finished retry gets the **saved response** back (header `Idempotent-Replay: true`) and nothing runs again.
3. **The journal reference** is built from the key, and `journal_entries.reference` is UNIQUE, so even a bug cannot post twice.
4. **Quotes** are used with `UPDATE quotes SET used_at = NOW() WHERE id = ? AND used_at IS NULL`, which must change exactly one row. That makes a quote single-use.

### The journal and snapshots: how balances work
There is no `balance` column. A wallet's balance is:

```
latest daily snapshot (closing balance at midnight)
+ every leg posted after that snapshot
```

Worked example:

- Yesterday's snapshot says Ada had $40.00, and the last leg it counted was #1000.
- Today she receives $5 (leg #1003) and spends $2.50 (leg #1010).
- Her balance is 40 + 5 − 2.50 = **$42.50**.

Every night at 00:05 Lagos time, the scheduler writes a new snapshot per account, so the "legs after" list stays short. `artisan ledger:verify` proves the books are consistent: every entry balances, and every snapshot equals the full sum of its legs. See `docs/ACCOUNTING.md` for every posting.

### The NGN rail
Many currency pairs are converted **through Naira**, because that is how the providers price them.

- **Example:** USD → GBP with the global provider's `uses_ngn_rail` switched on.
  1. Sell $20 for ₦30,773.
  2. Buy £15.03 for ₦30,606.
  3. The ₦167 difference is our FX margin.
- The route is decided in `app/services/rates/router.go`:
  - Same provider, rail off, direct conversion supported: one leg (e.g. USDT → USDC).
  - Different providers (e.g. BTC → USD), or the rail is on: two legs through NGN.
- Each hop is saved in `swap_legs`, and the journal records the Naira received and paid.
- Admins flip the rail per provider on the Providers page.

### Redis, queues and cron
- **Redis** holds the session cache, rate limits, locks, the queue and live events.
- **Queue (`cmd/worker`, library `asynq`)** works like Laravel queues. The API calls `application.Dispatcher.ProcessPayout(...)` and returns at once; the worker runs the job and retries it with back-off if it fails. Jobs:
  - email codes
  - provider payouts
  - statements
  - push notifications
- **Cron (`cmd/scheduler`, `app/console/schedule.go`)** works like `Kernel::schedule`:

  | When (Lagos) | Job |
  |---|---|
  | 00:05 | daily snapshot |
  | 00:30 | ledger check |
  | hourly | cleanup |
  | 07:00 | recurring bills |

  Each run is recorded in `scheduled_job_runs`, with UNIQUE(job, window), so two schedulers can't both run the same night.
- **Push outbox.** Every 3 seconds the scheduler pushes inbox messages that have not been pushed yet. Services only write the inbox row inside their money transaction, so a push is sent only for money that really moved.

### Live rates and the support inbox (Server-Sent Events)
SSE is one long HTTP response that the server keeps writing to:

```
event: rates.updated
data: {"version":13,"rates":{"BTC":{"usd":"67000.00","ngn":"103180000.00"}, ...}}
```

1. Every 10 seconds the scheduler's collector asks the providers for rates. It saves them, then publishes **one** message to Redis.
2. Every API server runs a `Hub` goroutine that hears that message.
3. The hub copies it to every connected phone through its channel.

A slow phone keeps only the newest event, so it never slows the others down. The admin support inbox works the same way through `support-threads/stream`.

### Single-device sign-in
- `user_devices` has a generated column, `active_user_id`, that is filled only while a device is active. A UNIQUE index on it means the **database itself** allows one active phone per user.
- Signing in on a new phone emails a code. Verifying it does three things in one transaction:
  1. Marks the old phone as `replaced`.
  2. Revokes its sessions.
  3. Activates the new phone.
- If two new phones race, the UNIQUE index lets exactly one win.

---

## Part 4: running it

```bash
make migrate && make seed   # database + demo data
make dev                    # server + worker + scheduler
make try                    # the curl tour: sign-up, swap on the NGN rail, replay, second phone, ...
make test                   # unit + integration tests (each gets its own throw-away database)
```

When you add something, follow the checklist in `CLAUDE.md` ("Adding an endpoint"). If you're unsure where code belongs, ask: "is this HTTP plumbing (controller), a business rule (service), or a money movement (ledger rule)?"
