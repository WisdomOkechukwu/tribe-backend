# Load testing on a disposable copy of production

This runbook load-tests a **throw-away** copy of the production shape: a $24 DigitalOcean droplet for the API (4 GB RAM, 2 vCPU) and a separate MySQL server. **Never point it at production.** Everything here uses fake customers, simulated providers and a database you delete afterwards.

## 1. What you build (and delete afterwards)

| Piece | Size | Notes |
|---|---|---|
| API droplet | Basic, 4 GB / 2 vCPU ($24) | Runs `server`, `worker`, `scheduler` and Redis, like production |
| MySQL | Managed MySQL 8.4 (or a second droplet) in the **same region** | Check `max_connections` (see section 3) |
| Load generator | Any other machine, e.g. a $6 droplet in the same region | Running it on the API droplet would steal the CPU you are measuring |

Tag everything `loadtest` so it is easy to find and destroy.

## 2. Set up the API droplet

```bash
# On the droplet: copy the code (or a built release), then:
cp .env.example .env
```

Edit `.env`:

```ini
APP_ENV=testing                 # NOT production: simulators are allowed, real money is impossible
APP_KEY=<any 32+ characters>
DB_HOST=<managed mysql host>
DB_PORT=25060
DB_DATABASE=tribe_core          # a fresh, EMPTY database on the disposable MySQL
DB_USERNAME=... / DB_PASSWORD=...
RATE_LIMITS_OFF=true            # one load generator = one IP; refused when APP_ENV=production
MEMORY_LIMIT_MB=1200            # soft limit for the API program
HASHING_CONCURRENCY=4           # 4 x 64 MB = 256 MB of password/PIN hashing at most
DB_MAX_OPEN_CONNECTIONS=20      # x3 programs = 60; must stay below MySQL max_connections
```

Then:

```bash
make migrate && make seed
make loadtest-seed USERS=200    # 200 customers with an active phone, PIN 2580, ₦5,000,000 + $5,000 each
make build && ./bin/server & ./bin/worker & ./bin/scheduler &
```

`loadtest-seed` refuses to run when `APP_ENV=production`, and `RATE_LIMITS_OFF=true` stops the server from starting in production.

## 3. Things to check before you start

- **MySQL connections.** Server, worker and scheduler each open up to `DB_MAX_OPEN_CONNECTIONS`. Keep 3 × that below MySQL's `max_connections` (`SHOW VARIABLES LIKE 'max_connections';`). The smallest managed plans allow about 75.
- **Timeouts.** `DB_CONNECT_TIMEOUT_SECONDS`, `DB_READ_TIMEOUT_SECONDS` and `DB_WRITE_TIMEOUT_SECONDS` make a stalled MySQL fail requests instead of hanging them.
- **Rates.** The scheduler must be running: quotes refuse rates older than 120 seconds.

## 4. Run the load

From the load-generator machine:

```bash
make loadtest URL=http://<api droplet ip>:8080 USERS=200 CONCURRENCY=50 DURATION=5m
```

The traffic mix imitates the app: 30% wallets, 20% rates, 15% activity, 10% quotes, 20% Tribe-to-Tribe sends (PIN + ledger locks) and 5% bill payments (PIN + provider). At the end, the tool signs in as `super@tribe.africa` and runs the **full** ledger check (`GET /admin-api/ledger/verify?full=true`). A load test only passes if the books still balance.

Try a few levels: `CONCURRENCY=10`, `50`, `100`, `200`. Look for where p95 latency or errors jump.

## 5. Watch resources while it runs

On the API droplet, in a second terminal:

```bash
vmstat 2                                   # r = CPU queue, free/si/so = memory and swapping
pidstat -r -u -p $(pgrep -d, -f bin/) 2    # CPU and RSS of server, worker, scheduler
redis-cli info memory | grep used_memory_human
```

On MySQL:

```sql
SHOW GLOBAL STATUS LIKE 'Threads_connected';
SHOW GLOBAL STATUS LIKE 'Innodb_row_lock%';     -- waits on hot rows (company vault)
SELECT * FROM sys.statements_with_runtimes_in_95th_percentile LIMIT 10;
```

Write down for each level: requests/second, p50/p95/p99 per scenario, error answers, peak RSS of `server`, CPU, MySQL connections and row-lock waits, and whether the ledger check passed.

## 6. What a first local run showed (laptop, not the droplet)

A 60-second run on a developer laptop (MySQL 5.7 in MAMP, 200 customers, 50 requests in flight) gave **0 errors** and a clean full ledger check. It also compared hashing with and without the slot limit:

| | `HASHING_CONCURRENCY=4` | no limit (old behaviour) |
|---|---|---|
| Throughput | 117 req/s | 152 req/s |
| Peak API memory | **1,050 MB** | **2,114 MB** |
| `GET /wallets` p50 / p95 | 5 ms / 43 ms | 96 ms / 246 ms |
| `POST /sends` p50 / p95 | 1.57 s / 2.40 s | 0.74 s / 1.17 s |

Without the limit the hashes used every CPU core (407% CPU) and doubled memory; on a 2-vCPU, 4 GB droplet that is the out-of-memory path. With the limit, reads stay fast and PIN-protected requests wait their turn. On the droplet, raise `HASHING_CONCURRENCY` only while peak memory stays well under 4 GB.

## 7. Tear down

Destroy the API droplet, the load generator and the disposable MySQL (or `DROP DATABASE` it). Nothing from the test may be copied to production: the load-test customers share a known password and PIN.
