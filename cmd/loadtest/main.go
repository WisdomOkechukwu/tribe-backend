// Command loadtest sends realistic traffic to a DISPOSABLE Tribe deployment
// and reports latency, errors and whether the books still balance.
//
//	go run ./cmd/artisan loadtest:seed 200          (on the disposable server, once)
//	go run ./cmd/loadtest -url http://API:8080 -users 200 -concurrency 50 -duration 2m
//
// It never touches production: it signs in only the load-test customers made
// by `artisan loadtest:seed`, which refuses to run when APP_ENV=production.
// Run the API with RATE_LIMITS_OFF=true (refused in production), or every
// request from this one machine hits the per-IP limits. See docs/LOAD_TESTING.md.
//
// The mix of requests imitates the app: mostly reading wallets, rates and
// activity, some quotes, small Tribe-to-Tribe sends (which take the PIN and
// lock ledger rows) and a few bill payments (which go through the provider).
package main

import (
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math/big"
	"net/http"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/tribe-africa/tribe-backend/database/seeders"
)

// scenario is one kind of request and how often it is picked.
type scenario struct {
	name   string
	weight int
	run    func(runner *runner, user *loadUser) (int, string) // HTTP status and error code
}

// loadUser is one signed-in load-test customer.
type loadUser struct {
	index int
	tag   string
	mutex sync.Mutex
	token string
}

// envelope is the API's JSON shape.
type envelope struct {
	Success bool            `json:"success"`
	Message string          `json:"message"`
	Code    string          `json:"code"`
	Data    json.RawMessage `json:"data"`
}

// runner holds the settings and the collected measurements.
type runner struct {
	baseURL    string
	httpClient *http.Client
	users      []*loadUser

	mutex     sync.Mutex
	latencies map[string][]time.Duration
	statuses  map[string]map[string]int // scenario -> "200" / "422 INSUFFICIENT_FUNDS" -> count
}

func main() {
	baseURL := flag.String("url", "http://localhost:8080", "API base URL of the DISPOSABLE deployment")
	userCount := flag.Int("users", 200, "how many seeded load-test customers to sign in")
	concurrency := flag.Int("concurrency", 50, "requests in flight at the same time")
	duration := flag.Duration("duration", time.Minute, "how long to send traffic")
	adminEmail := flag.String("admin-email", "super@tribe.africa", "admin for the final ledger check (needs ledger.view)")
	adminPassword := flag.String("admin-password", seeders.DemoPassword, "that admin's password")
	flag.Parse()

	if strings.Contains(*baseURL, "tribe.africa") && !strings.Contains(*baseURL, "loadtest") {
		fmt.Fprintln(os.Stderr, "refusing: this looks like a real Tribe address. Load-test a disposable deployment only.")
		os.Exit(2)
	}
	load := &runner{
		baseURL:    strings.TrimRight(*baseURL, "/"),
		httpClient: &http.Client{Timeout: 30 * time.Second, Transport: &http.Transport{MaxIdleConnsPerHost: *concurrency * 2}},
		latencies:  map[string][]time.Duration{},
		statuses:   map[string]map[string]int{},
	}

	fmt.Printf("Signing in %d load-test customers...\n", *userCount)
	started := time.Now()
	if err := load.signInAll(*userCount); err != nil {
		fmt.Fprintln(os.Stderr, "sign-in failed:", err)
		fmt.Fprintln(os.Stderr, "did you run `artisan loadtest:seed` on that server, and is RATE_LIMITS_OFF=true there?")
		os.Exit(1)
	}
	fmt.Printf("Signed in %d customers in %s (password checks are deliberately slow)\n\n", len(load.users), time.Since(started).Round(time.Millisecond))

	scenarios := []scenario{
		{"GET /wallets", 30, func(runner *runner, user *loadUser) (int, string) {
			return runner.call(user, http.MethodGet, "/api/mobile/v1/wallets", nil, false)
		}},
		{"GET /rates", 20, func(runner *runner, user *loadUser) (int, string) {
			return runner.call(user, http.MethodGet, "/api/mobile/v1/rates", nil, false)
		}},
		{"GET /transactions", 15, func(runner *runner, user *loadUser) (int, string) {
			return runner.call(user, http.MethodGet, "/api/mobile/v1/transactions", nil, false)
		}},
		{"POST /quotes", 10, func(runner *runner, user *loadUser) (int, string) {
			return runner.call(user, http.MethodPost, "/api/mobile/v1/quotes",
				map[string]string{"transaction_type": "swap", "from_asset": "USD", "to_asset": "NGN", "amount": "1"}, false)
		}},
		{"POST /sends (Tribe)", 20, func(runner *runner, user *loadUser) (int, string) {
			recipient := runner.users[randomIndex(len(runner.users))]
			if recipient == user {
				recipient = runner.users[(user.index)%len(runner.users)]
			}
			return runner.call(user, http.MethodPost, "/api/mobile/v1/sends", map[string]string{
				"recipient_kind": "tribe", "handle": "@" + recipient.tag, "asset": "USD", "amount": "0.01", "pin": seeders.LoadTestPin,
			}, true)
		}},
		{"POST /bills", 5, func(runner *runner, user *loadUser) (int, string) {
			return runner.call(user, http.MethodPost, "/api/mobile/v1/bills", map[string]string{
				"biller_id": "phed", "target": fmt.Sprintf("4567%04d", user.index%10000), "amount_ngn": "100", "pay_with": "NGN", "pin": seeders.LoadTestPin,
			}, true)
		}},
	}
	totalWeight := 0
	for _, item := range scenarios {
		totalWeight += item.weight
	}

	fmt.Printf("Sending traffic for %s with %d requests in flight...\n", *duration, *concurrency)
	deadline := time.Now().Add(*duration)
	trafficStarted := time.Now()
	var waitGroup sync.WaitGroup
	for worker := 0; worker < *concurrency; worker++ {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			for time.Now().Before(deadline) {
				pick := randomIndex(totalWeight)
				var chosen scenario
				for _, item := range scenarios {
					if pick < item.weight {
						chosen = item
						break
					}
					pick -= item.weight
				}
				user := load.users[randomIndex(len(load.users))]
				requestStarted := time.Now()
				status, code := chosen.run(load, user)
				load.record(chosen.name, time.Since(requestStarted), status, code)
			}
		}()
	}
	waitGroup.Wait()
	load.report(time.Since(trafficStarted))

	fmt.Println("\nChecking the books (full ledger verification)...")
	if err := load.verifyLedger(*adminEmail, *adminPassword); err != nil {
		fmt.Fprintln(os.Stderr, "LEDGER CHECK FAILED:", err)
		os.Exit(1)
	}
}

// signInAll signs every load-test customer in on their already-active phone,
// eight at a time (sign-in is a deliberately slow password check).
func (runner *runner) signInAll(count int) error {
	users := make([]*loadUser, count)
	errorsSeen := make(chan error, count)
	slots := make(chan struct{}, 8)
	var waitGroup sync.WaitGroup
	for index := 1; index <= count; index++ {
		waitGroup.Add(1)
		slots <- struct{}{}
		go func(index int) {
			defer waitGroup.Done()
			defer func() { <-slots }()
			user := &loadUser{index: index, tag: fmt.Sprintf("lt%d", index)}
			if err := runner.signIn(user); err != nil {
				errorsSeen <- fmt.Errorf("%s: %w", seeders.LoadTestEmail(index), err)
				return
			}
			users[index-1] = user
		}(index)
	}
	waitGroup.Wait()
	close(errorsSeen)
	for err := range errorsSeen {
		return err
	}
	runner.users = users
	return nil
}

// signIn gets a fresh access token for one customer.
func (runner *runner) signIn(user *loadUser) error {
	body := map[string]any{
		"email": seeders.LoadTestEmail(user.index), "password": seeders.LoadTestPassword,
		"device": map[string]string{"imei": seeders.LoadTestDevice(user.index), "name": "Load test"},
	}
	status, response, err := runner.send(http.MethodPost, "/api/mobile/v1/auth/login", "", seeders.LoadTestDevice(user.index), body, newKey())
	if err != nil {
		return err
	}
	var data struct {
		Tokens struct {
			AccessToken string `json:"access_token"`
		} `json:"tokens"`
	}
	json.Unmarshal(response.Data, &data)
	if status != http.StatusOK || data.Tokens.AccessToken == "" {
		return fmt.Errorf("status %d %s %s", status, response.Code, response.Message)
	}
	user.mutex.Lock()
	user.token = data.Tokens.AccessToken
	user.mutex.Unlock()
	return nil
}

// call sends one request as a customer. An expired token is renewed once.
func (runner *runner) call(user *loadUser, method string, path string, body any, idempotent bool) (int, string) {
	for attempt := 0; attempt < 2; attempt++ {
		key := ""
		if idempotent {
			key = newKey()
		}
		user.mutex.Lock()
		token := user.token
		user.mutex.Unlock()
		status, response, err := runner.send(method, path, token, seeders.LoadTestDevice(user.index), body, key)
		if err != nil {
			return 0, "NETWORK: " + shorten(err.Error())
		}
		if status == http.StatusUnauthorized && attempt == 0 {
			if runner.signIn(user) == nil {
				continue
			}
		}
		return status, response.Code
	}
	return http.StatusUnauthorized, "UNAUTHENTICATED"
}

// send does the HTTP round trip and decodes the envelope.
func (runner *runner) send(method string, path string, token string, deviceImei string, body any, idempotencyKey string) (int, envelope, error) {
	var reader io.Reader
	if body != nil {
		encoded, _ := json.Marshal(body)
		reader = bytes.NewReader(encoded)
	}
	request, err := http.NewRequest(method, runner.baseURL+path, reader)
	if err != nil {
		return 0, envelope{}, err
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("User-Agent", "tribe-loadtest")
	if token != "" {
		request.Header.Set("Authorization", "Bearer "+token)
	}
	if deviceImei != "" {
		request.Header.Set("X-Device-Imei", deviceImei)
	}
	if idempotencyKey != "" {
		request.Header.Set("Idempotency-Key", idempotencyKey)
	}
	response, err := runner.httpClient.Do(request)
	if err != nil {
		return 0, envelope{}, err
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	var decoded envelope
	json.Unmarshal(raw, &decoded)
	return response.StatusCode, decoded, nil
}

// record saves one measurement.
func (runner *runner) record(name string, latency time.Duration, status int, code string) {
	runner.mutex.Lock()
	defer runner.mutex.Unlock()
	runner.latencies[name] = append(runner.latencies[name], latency)
	if runner.statuses[name] == nil {
		runner.statuses[name] = map[string]int{}
	}
	key := fmt.Sprintf("%d", status)
	if status >= 300 || status == 0 {
		key += " " + code
	}
	runner.statuses[name][key]++
}

// report prints throughput, latency percentiles and every non-2xx answer.
func (runner *runner) report(elapsed time.Duration) {
	names := make([]string, 0, len(runner.latencies))
	total := 0
	for name, latencies := range runner.latencies {
		names = append(names, name)
		total += len(latencies)
	}
	sort.Strings(names)
	fmt.Printf("\n%d requests in %s = %.1f requests/second\n\n", total, elapsed.Round(time.Second), float64(total)/elapsed.Seconds())
	fmt.Printf("%-22s %8s %8s %8s %8s %8s %8s\n", "scenario", "count", "p50", "p95", "p99", "max", "errors")
	for _, name := range names {
		latencies := runner.latencies[name]
		sort.Slice(latencies, func(first, second int) bool { return latencies[first] < latencies[second] })
		failures := 0
		for key, count := range runner.statuses[name] {
			if !strings.HasPrefix(key, "2") {
				failures += count
			}
		}
		fmt.Printf("%-22s %8d %8s %8s %8s %8s %7.2f%%\n", name, len(latencies),
			percentile(latencies, 50), percentile(latencies, 95), percentile(latencies, 99), latencies[len(latencies)-1].Round(time.Millisecond),
			100*float64(failures)/float64(len(latencies)))
	}
	fmt.Println("\nAnswers other than 2xx:")
	anyFailure := false
	for _, name := range names {
		for key, count := range runner.statuses[name] {
			if !strings.HasPrefix(key, "2") {
				anyFailure = true
				fmt.Printf("  %-22s %-40s %d\n", name, key, count)
			}
		}
	}
	if !anyFailure {
		fmt.Println("  none")
	}
}

// verifyLedger signs in as an admin and runs the FULL ledger check.
func (runner *runner) verifyLedger(email string, password string) error {
	status, response, err := runner.send(http.MethodPost, "/admin-api/sessions", "", "", map[string]string{"email": email, "password": password}, "")
	if err != nil {
		return err
	}
	var session struct {
		AccessToken string `json:"access_token"`
	}
	json.Unmarshal(response.Data, &session)
	if status >= 300 || session.AccessToken == "" {
		return fmt.Errorf("admin sign-in: %d %s", status, response.Message)
	}
	status, response, err = runner.send(http.MethodGet, "/admin-api/ledger/verify?full=true", session.AccessToken, "", nil, "")
	if err != nil {
		return err
	}
	var result struct {
		OK               bool  `json:"ok"`
		ProblemCount     int64 `json:"problem_count"`
		EntriesChecked   int64 `json:"entries_checked"`
		SnapshotsChecked int64 `json:"snapshots_checked"`
		Problems         []struct {
			Description string `json:"Description"`
		} `json:"problems"`
	}
	json.Unmarshal(response.Data, &result)
	if status != http.StatusOK {
		return fmt.Errorf("verify: %d %s", status, response.Message)
	}
	fmt.Printf("Ledger: %d entries and %d snapshots checked, %d problems\n", result.EntriesChecked, result.SnapshotsChecked, result.ProblemCount)
	if !result.OK {
		for _, problem := range result.Problems {
			fmt.Println("  PROBLEM:", problem.Description)
		}
		return fmt.Errorf("%d ledger problems", result.ProblemCount)
	}
	fmt.Println("Ledger OK: every entry balances and every snapshot matches the journal.")
	return nil
}

// percentile returns the p-th percentile of sorted latencies.
func percentile(sorted []time.Duration, p int) string {
	if len(sorted) == 0 {
		return "-"
	}
	index := (len(sorted) - 1) * p / 100
	return sorted[index].Round(time.Millisecond).String()
}

// randomIndex returns a random number from 0 to limit-1.
func randomIndex(limit int) int {
	number, _ := rand.Int(rand.Reader, big.NewInt(int64(limit)))
	return int(number.Int64())
}

// newKey returns a fresh Idempotency-Key.
func newKey() string {
	buffer := make([]byte, 12)
	rand.Read(buffer)
	return "load-" + hex.EncodeToString(buffer)
}

// shorten keeps error texts short in the report.
func shorten(text string) string {
	if len(text) > 60 {
		return text[:60]
	}
	return text
}
