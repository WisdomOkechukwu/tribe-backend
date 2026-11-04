package providers

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"math/big"
	"math/rand/v2"
	"strings"
	"sync"
	"time"

	"github.com/tribe-africa/tribe-backend/pkg/ids"
)

// Simulator is a PLACEHOLDER provider used until real providers are chosen.
// It answers every call with believable fake data:
//   - prices move a little on every call (a "random walk"),
//   - payouts succeed (unless the destination contains "fail"),
//   - bills return a fake 20-digit token,
//   - cards get a fake card number derived from their reference.
type Simulator struct {
	mutex          sync.Mutex // prices are shared, so only one goroutine may change them at a time
	usdPrices      map[string]float64
	nairaPerDollar float64

	slowReferences sync.Map // references already "timed out" once (see PayBill)
}

// startingUsdPrices match the demo prices in the mobile app.
var startingUsdPrices = map[string]float64{
	"USD": 1, "USDT": 1, "USDC": 1,
	"BTC": 98_400, "ETH": 3_620, "SOL": 182,
	"GBP": 1.31, "EUR": 1.12, "CAD": 0.73,
	"KES": 1.0 / 129, "GHS": 1.0 / 15.4,
}

// NewSimulator creates the simulator with the starting prices.
func NewSimulator(nairaPerDollarSeed string) *Simulator {
	nairaPerDollar := 1540.0
	fmt.Sscanf(nairaPerDollarSeed, "%g", &nairaPerDollar)
	prices := make(map[string]float64, len(startingUsdPrices))
	for assetCode, price := range startingUsdPrices {
		prices[assetCode] = price
	}
	return &Simulator{usdPrices: prices, nairaPerDollar: nairaPerDollar}
}

// FetchPrices moves every price a tiny random step and returns them.
// (Floats are fine HERE because these are pretend market prices; they are
// turned into exact fractions before any money maths.)
func (simulator *Simulator) FetchPrices(requestContext context.Context, assetCodes []string) (map[string]AssetPrice, error) {
	simulator.mutex.Lock()
	defer simulator.mutex.Unlock()

	simulator.nairaPerDollar *= 1 + (rand.Float64()-0.5)*0.001 // ±0.05%
	prices := make(map[string]AssetPrice, len(assetCodes))
	for _, assetCode := range assetCodes {
		if assetCode == "NGN" {
			prices["NGN"] = AssetPrice{Usd: ratFromFloat(1 / simulator.nairaPerDollar), Ngn: big.NewRat(1, 1)}
			continue
		}
		price, known := simulator.usdPrices[assetCode]
		if !known {
			continue
		}
		switch {
		case assetCode == "USD" || assetCode == "USDT" || assetCode == "USDC":
			// Dollars and stablecoins stay at 1.
		case isCryptoCode(assetCode):
			price *= 1 + (rand.Float64()-0.5)*0.008 // ±0.4%
		default:
			price *= 1 + (rand.Float64()-0.5)*0.001 // ±0.05%
		}
		simulator.usdPrices[assetCode] = price
		prices[assetCode] = AssetPrice{Usd: ratFromFloat(price), Ngn: ratFromFloat(price * simulator.nairaPerDollar)}
	}
	return prices, nil
}

// SendPayout pretends to send money. Destinations containing "fail" fail,
// which lets you test the refund path.
func (simulator *Simulator) SendPayout(requestContext context.Context, reference string, assetCode string, amount string, destination map[string]string) (PayoutResult, error) {
	time.Sleep(300 * time.Millisecond) // pretend network time
	for _, value := range destination {
		if strings.Contains(strings.ToLower(value), "fail") {
			return PayoutResult{Status: "failed", ProviderReference: "SIM-" + ids.New(), FailureReason: "Simulated failure: the receiving account rejected the payment."}, nil
		}
	}
	return PayoutResult{Status: "succeeded", ProviderReference: "SIM-" + ids.New()}, nil
}

// VerifyCustomer pretends to look up the customer name for a meter/phone/smartcard.
func (simulator *Simulator) VerifyCustomer(requestContext context.Context, billerID string, target string) (string, error) {
	if len(target) < 6 {
		return "", fmt.Errorf("customer not found")
	}
	return "Simulated Customer " + target[len(target)-4:], nil
}

// PayBill pretends to pay a bill. Electricity gets a fake 20-digit token.
// Like a real provider it is idempotent: the provider reference and token
// are derived from OUR reference, so asking again gives the same answer.
// A target ending in "0000" is refused, to try the refund path. A target
// ending in "5555" times out the FIRST time each reference is tried and
// works after that, to try the "provider did not answer" path.
func (simulator *Simulator) PayBill(requestContext context.Context, reference string, billerID string, target string, amountNgn string) (BillResult, error) {
	if strings.HasSuffix(target, "0000") {
		return BillResult{}, &RejectedError{Reason: "Simulated failure: the biller rejected this customer number."}
	}
	if strings.HasSuffix(target, "5555") {
		if _, alreadyTried := simulator.slowReferences.LoadOrStore(reference, true); !alreadyTried {
			return BillResult{}, fmt.Errorf("simulated timeout: the biller did not answer")
		}
	}
	fingerprint := referenceDigits(reference)
	result := BillResult{ProviderReference: "SIM-" + fingerprint[:16]}
	if billerID == "phed" {
		digits := fingerprint[:20]
		result.Token = digits[0:4] + "-" + digits[4:8] + "-" + digits[8:12] + "-" + digits[12:16] + "-" + digits[16:20]
	}
	return result, nil
}

// BuyCard pretends to buy a gift card and returns a fake code, the same
// code every time for the same reference (idempotent, like a real one).
func (simulator *Simulator) BuyCard(requestContext context.Context, reference string, brandID string, faceValueUsd string) (string, error) {
	return strings.ToUpper(brandID[:min(3, len(brandID))]) + "-" + referenceDigits(reference)[20:26], nil
}

// referenceDigits turns a reference into 40 stable digits (a SHA-256 hash
// written as digits), so simulated results never change for a reference.
func referenceDigits(reference string) string {
	sum := sha256.Sum256([]byte("simulator:" + reference))
	digits := make([]byte, 0, 40)
	for _, value := range sum[:20] {
		digits = append(digits, '0'+value%10, '0'+(value/10)%10)
	}
	return string(digits)
}

// CheckSoldCard pretends to check a gift card the user is selling. Codes
// containing "bad" are rejected.
func (simulator *Simulator) CheckSoldCard(requestContext context.Context, reference string, brandID string, code string) (bool, error) {
	return !strings.Contains(strings.ToLower(code), "bad"), nil
}

// IssueCard pretends to issue a virtual card.
func (simulator *Simulator) IssueCard(requestContext context.Context, reference string, holderName string, network string) (IssuedCard, error) {
	number, _ := fakeCardNumber(reference, network)
	expiry := time.Now().AddDate(3, 0, 0)
	return IssuedCard{ProviderCardReference: "SIMCARD-" + reference, LastFour: number[len(number)-4:], ExpiryMonth: int(expiry.Month()), ExpiryYear: expiry.Year()}, nil
}

// RevealCard returns the fake full number and CVV (always the same for a card).
func (simulator *Simulator) RevealCard(requestContext context.Context, providerCardReference string) (string, string, error) {
	network := "visa"
	number, cvv := fakeCardNumber(strings.TrimPrefix(providerCardReference, "SIMCARD-"), network)
	return number, cvv, nil
}

// fakeCardNumber derives a stable fake 16-digit number and CVV from a reference.
func fakeCardNumber(reference string, network string) (string, string) {
	sum := sha256.Sum256([]byte(reference))
	digits := ""
	for _, character := range hex.EncodeToString(sum[:]) {
		if character >= '0' && character <= '9' {
			digits += string(character)
		}
	}
	digits += "0000000000000000"
	prefix := "4" // visa
	if network == "mastercard" {
		prefix = "5"
	}
	return prefix + digits[:15], digits[15:18]
}

// isCryptoCode is true for the crypto assets the simulator knows.
func isCryptoCode(assetCode string) bool {
	switch assetCode {
	case "BTC", "ETH", "SOL", "USDT", "USDC":
		return true
	}
	return false
}

// ratFromFloat turns a float price into an exact fraction (12 decimals kept).
func ratFromFloat(value float64) *big.Rat {
	rational, _ := new(big.Rat).SetString(fmt.Sprintf("%.12f", value))
	return rational
}
