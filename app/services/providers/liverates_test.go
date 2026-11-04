package providers

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// fakeFeeds is a pretend Coinbase + open.er-api.com server. Tests flip the
// failing switches to make one feed return HTTP 500.
type fakeFeeds struct {
	server         *httptest.Server
	fiatCallCount  atomic.Int64
	fiatFailing    atomic.Bool
	cryptoFailing  atomic.Bool
	bitcoinDollars atomic.Int64
}

// startFakeFeeds starts the fake server. BTC starts at $70,000 and the Naira at ₦1,500 per dollar.
func startFakeFeeds(test *testing.T) *fakeFeeds {
	feeds := &fakeFeeds{}
	feeds.bitcoinDollars.Store(70_000)
	feeds.server = httptest.NewServer(http.HandlerFunc(func(responseWriter http.ResponseWriter, request *http.Request) {
		if strings.HasPrefix(request.URL.Path, "/fiat") {
			feeds.fiatCallCount.Add(1)
			if feeds.fiatFailing.Load() {
				responseWriter.WriteHeader(http.StatusInternalServerError)
				return
			}
			fmt.Fprint(responseWriter, `{"result":"success","rates":{"USD":1,"NGN":1500,"GBP":0.8}}`)
			return
		}
		if feeds.cryptoFailing.Load() {
			responseWriter.WriteHeader(http.StatusInternalServerError)
			return
		}
		fmt.Fprintf(responseWriter, `{"data":{"amount":"%d"}}`, feeds.bitcoinDollars.Load())
	}))
	test.Cleanup(feeds.server.Close)
	return feeds
}

// markerFallback stands in for the simulator. Its NGN price is 999, so a test
// can tell at a glance whether the fallback was used.
type markerFallback struct{}

// FetchPrices returns the marker price for NGN only.
func (markerFallback) FetchPrices(requestContext context.Context, assetCodes []string) (map[string]AssetPrice, error) {
	return map[string]AssetPrice{"NGN": {Usd: big.NewRat(1, 999), Ngn: big.NewRat(999, 1)}}, nil
}

// testClock is a clock the test moves forward by hand.
type testClock struct{ now time.Time }

// newLiveRatesForTest points a LiveRates at the fake feeds and a hand-moved clock.
func newLiveRatesForTest(feeds *fakeFeeds, fallback RateProvider) (*LiveRates, *testClock) {
	clock := &testClock{now: time.Date(2026, 11, 3, 9, 0, 0, 0, time.UTC)}
	live := NewLiveRates(fallback)
	live.coinbaseAddressPattern = feeds.server.URL + "/coinbase/%s"
	live.fiatAddress = feeds.server.URL + "/fiat"
	live.timeNow = func() time.Time { return clock.now }
	return live, clock
}

// fetch calls FetchPrices for NGN, BTC and GBP and fails the test on an error.
func fetch(test *testing.T, live *LiveRates) map[string]AssetPrice {
	test.Helper()
	prices, err := live.FetchPrices(context.Background(), []string{"NGN", "BTC", "GBP"})
	if err != nil {
		test.Fatalf("FetchPrices: %v", err)
	}
	return prices
}

// TestFiatFeedIsAskedAtMostHourly proves the fiat feed is not called on every
// collection (it only updates daily and rate-limits frequent callers).
func TestFiatFeedIsAskedAtMostHourly(test *testing.T) {
	feeds := startFakeFeeds(test)
	live, clock := newLiveRatesForTest(feeds, nil)

	for collection := 0; collection < 5; collection++ {
		fetch(test, live)
		clock.now = clock.now.Add(10 * time.Second)
	}
	if calls := feeds.fiatCallCount.Load(); calls != 1 {
		test.Fatalf("fiat feed called %d times in 50 seconds, want 1", calls)
	}

	clock.now = clock.now.Add(time.Hour)
	fetch(test, live)
	if calls := feeds.fiatCallCount.Load(); calls != 2 {
		test.Fatalf("fiat feed called %d times after an hour, want 2", calls)
	}
}

// TestFailedCallReusesRecentLivePrices proves a failed call keeps the last
// real prices instead of switching to the simulator.
func TestFailedCallReusesRecentLivePrices(test *testing.T) {
	feeds := startFakeFeeds(test)
	live, clock := newLiveRatesForTest(feeds, markerFallback{})
	fetch(test, live)

	// Coinbase fails 30 seconds later: BTC keeps its last real price.
	feeds.cryptoFailing.Store(true)
	clock.now = clock.now.Add(30 * time.Second)
	prices := fetch(test, live)
	if prices["BTC"].Usd.Cmp(big.NewRat(70_000, 1)) != 0 {
		test.Fatalf("BTC = %s, want the last live price 70000", prices["BTC"].Usd.FloatString(2))
	}
	if prices["NGN"].Ngn.Cmp(big.NewRat(1, 1)) != 0 || prices["BTC"].Ngn.Cmp(big.NewRat(105_000_000, 1)) != 0 {
		test.Fatalf("NGN prices came from the simulator, want live: BTC in NGN = %s", prices["BTC"].Ngn.FloatString(2))
	}

	// The fiat feed fails at its next refresh: the old fiat rates are reused.
	feeds.cryptoFailing.Store(false)
	feeds.fiatFailing.Store(true)
	clock.now = clock.now.Add(2 * time.Hour)
	prices = fetch(test, live)
	if prices["GBP"].Usd.Cmp(big.NewRat(5, 4)) != 0 {
		test.Fatalf("GBP = %s USD, want the last live 1.25", prices["GBP"].Usd.FloatString(4))
	}
}

// TestOldCryptoPriceIsNotReused proves a crypto price older than
// cryptoReuseLimit is never quoted on: production gets an error, and a
// developer machine gets the simulator.
func TestOldCryptoPriceIsNotReused(test *testing.T) {
	feeds := startFakeFeeds(test)

	production, productionClock := newLiveRatesForTest(feeds, nil)
	fetch(test, production)
	feeds.cryptoFailing.Store(true)
	productionClock.now = productionClock.now.Add(cryptoReuseLimit + time.Second)
	if _, err := production.FetchPrices(context.Background(), []string{"NGN", "BTC"}); err == nil {
		test.Fatal("production reused a crypto price older than the limit; want an error")
	}

	feeds.cryptoFailing.Store(false)
	local, localClock := newLiveRatesForTest(feeds, markerFallback{})
	fetch(test, local)
	feeds.cryptoFailing.Store(true)
	localClock.now = localClock.now.Add(cryptoReuseLimit + time.Second)
	prices := fetch(test, local)
	if prices["NGN"].Ngn.Cmp(big.NewRat(999, 1)) != 0 {
		test.Fatal("a developer machine did not fall back to the simulator once the price was too old")
	}
}
