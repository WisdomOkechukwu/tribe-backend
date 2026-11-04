package integration

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/tests/testenv"
)

// kycProfile is the part of the profile the KYC tests read.
type kycProfile struct {
	ID             string     `json:"id"`
	KycTier        int        `json:"kyc_tier"`
	KycStatus      string     `json:"kyc_status"`
	KycPendingTier *int       `json:"kyc_pending_tier"`
	KycSubmittedAt *time.Time `json:"kyc_submitted_at"`
}

func profileOf(test *testing.T, client *testenv.Client) kycProfile {
	test.Helper()
	var profile kycProfile
	client.Call(test, http.MethodGet, "/api/mobile/v1/profile", nil, "").Decode(test, &profile)
	return profile
}

// tierStates returns GET /kyc tiers[].state by tier, plus the rejection reasons.
func tierStates(test *testing.T, client *testenv.Client) (map[int]string, map[int]string, *struct {
	ID   string `json:"id"`
	Tier int    `json:"tier"`
}) {
	test.Helper()
	var overview struct {
		Open *struct {
			ID   string `json:"id"`
			Tier int    `json:"tier"`
		} `json:"open"`
		Tiers []struct {
			Tier   int     `json:"tier"`
			State  string  `json:"state"`
			Reason *string `json:"reason"`
		} `json:"tiers"`
	}
	client.Call(test, http.MethodGet, "/api/mobile/v1/kyc", nil, "").Decode(test, &overview)
	states, reasons := map[int]string{}, map[int]string{}
	for _, tier := range overview.Tiers {
		states[tier.Tier] = tier.State
		if tier.Reason != nil {
			reasons[tier.Tier] = *tier.Reason
		}
	}
	return states, reasons, overview.Open
}

func submitTier1(test *testing.T, client *testenv.Client) *testenv.Response {
	test.Helper()
	return client.Call(test, http.MethodPost, "/api/mobile/v1/kyc/tier-1", map[string]string{"bvn": "22212345678", "date_of_birth": "1995-04-12"}, testenv.NewKey())
}

func submitTier2(test *testing.T, client *testenv.Client) *testenv.Response {
	test.Helper()
	return client.Call(test, http.MethodPost, "/api/mobile/v1/kyc/tier-2", map[string]string{"nin": "12345678901"}, testenv.NewKey())
}

// pendingSubmissionOf finds the customer's request in the admin queue.
func pendingSubmissionOf(test *testing.T, admin *testenv.Client, userID string) (string, int) {
	test.Helper()
	var page struct {
		Rows []struct {
			ID      string `json:"id"`
			UserID  string `json:"user_id"`
			Attempt int    `json:"attempt"`
			Checks  []any  `json:"checks"`
		} `json:"rows"`
	}
	admin.Call(test, http.MethodGet, "/admin-api/verifications?status=pending", nil, "").Decode(test, &page)
	for _, row := range page.Rows {
		if row.UserID == userID {
			if len(row.Checks) != 2 {
				test.Fatalf("a tier 2 request should hold 2 checks, got %d", len(row.Checks))
			}
			return row.ID, row.Attempt
		}
	}
	test.Fatalf("no pending request for %s in the queue", userID)
	return "", 0
}

// TestKycRequestFlow walks one customer through the whole flow: tier 1 is
// auto-approved and can't be sent again, tier 3 must wait for tier 2, a tier 2
// request can't be sent twice, a rejection needs a reason the customer sees,
// they may then try again, and approval moves them up and frees the lock.
func TestKycRequestFlow(test *testing.T) {
	environment := testenv.Start(test)
	customer := environment.RegisterUser(test, "kycflow")
	reviewer := environment.SignInAdmin(test, "compliance@tribe.africa")

	if response := submitTier1(test, customer); !response.Success {
		test.Fatalf("tier 1: %d %s %s", response.Status, response.Code, response.Message)
	}
	if profile := profileOf(test, customer); profile.KycTier != 1 || profile.KycStatus != "approved" || profile.KycPendingTier != nil {
		test.Fatalf("after tier 1: %+v", profile)
	}
	if again := submitTier1(test, customer); again.Code != "ALREADY_VERIFIED" {
		test.Fatalf("tier 1 again gave %d %s, want ALREADY_VERIFIED", again.Status, again.Code)
	}
	if early := customer.Call(test, http.MethodPost, "/api/mobile/v1/kyc/tier-3", map[string]string{
		"document_type": "passport", "document_number": "A12345678", "address": "12 Admiralty Way, Lekki",
	}, testenv.NewKey()); early.Code != "TIER_ORDER" {
		test.Fatalf("tier 3 before tier 2 gave %d %s, want TIER_ORDER", early.Status, early.Code)
	}

	if first := submitTier2(test, customer); !first.Success {
		test.Fatalf("tier 2: %d %s %s", first.Status, first.Code, first.Message)
	}
	profile := profileOf(test, customer)
	if profile.KycStatus != "pending" || profile.KycPendingTier == nil || *profile.KycPendingTier != 2 || profile.KycSubmittedAt == nil {
		test.Fatalf("after tier 2 submit: %+v", profile)
	}
	if states, _, open := tierStates(test, customer); states[1] != "verified" || states[2] != "in_review" || states[3] != "locked" || open == nil || open.Tier != 2 {
		test.Fatalf("states while in review: %v open=%v", states, open)
	}
	if second := submitTier2(test, customer); second.Status != http.StatusConflict || second.Code != "VERIFICATION_IN_REVIEW" {
		test.Fatalf("second tier 2 gave %d %s, want 409 VERIFICATION_IN_REVIEW", second.Status, second.Code)
	}

	submissionID, attempt := pendingSubmissionOf(test, reviewer, profile.ID)
	if attempt != 1 {
		test.Errorf("first request attempt = %d, want 1", attempt)
	}
	decide := func(body map[string]string) *testenv.Response {
		return reviewer.Call(test, http.MethodPost, "/admin-api/verifications/"+submissionID+"/decisions", body, testenv.NewKey())
	}
	if noReason := decide(map[string]string{"decision": "reject", "reason": "  "}); noReason.Status != http.StatusUnprocessableEntity || len(noReason.Errors["reason"]) == 0 {
		test.Fatalf("reject without a reason gave %d %s %v, want 422 on reason", noReason.Status, noReason.Code, noReason.Errors)
	}
	if rejected := decide(map[string]string{"decision": "reject", "reason": "Name doesn't match your BVN"}); !rejected.Success {
		test.Fatalf("reject: %d %s %s", rejected.Status, rejected.Code, rejected.Message)
	}
	if twice := decide(map[string]string{"decision": "approve"}); twice.Code != "ALREADY_DECIDED" {
		test.Fatalf("deciding twice gave %d %s, want ALREADY_DECIDED", twice.Status, twice.Code)
	}
	states, reasons, open := tierStates(test, customer)
	if states[2] != "needs_attention" || reasons[2] != "Name doesn't match your BVN" || open != nil {
		test.Fatalf("after reject: states=%v reasons=%v open=%v", states, reasons, open)
	}
	var inbox models.InboxMessage
	environment.App.Database.Where("user_id = ? AND kind = ?", profile.ID, "security").Order("created_at DESC").Take(&inbox)
	if inbox.Title != "Tier 2 needs attention" {
		test.Errorf("reject inbox title = %q", inbox.Title)
	}

	if retry := submitTier2(test, customer); !retry.Success {
		test.Fatalf("retry after reject: %d %s %s", retry.Status, retry.Code, retry.Message)
	}
	submissionID, attempt = pendingSubmissionOf(test, reviewer, profile.ID)
	if attempt != 2 {
		test.Errorf("retry attempt = %d, want 2", attempt)
	}
	if approved := decide(map[string]string{"decision": "approve"}); !approved.Success {
		test.Fatalf("approve: %d %s %s", approved.Status, approved.Code, approved.Message)
	}
	if profile := profileOf(test, customer); profile.KycTier != 2 || profile.KycStatus != "approved" || profile.KycPendingTier != nil {
		test.Fatalf("after approve: %+v", profile)
	}
	if states, _, _ := tierStates(test, customer); states[2] != "verified" || states[3] != "available" {
		test.Fatalf("after approve states: %v", states)
	}
	// the lock is free again: tier 3 can be sent (and its fields are checked one by one)
	if bad := customer.Call(test, http.MethodPost, "/api/mobile/v1/kyc/tier-3", map[string]string{
		"document_type": "passport", "document_number": "123", "address": "short",
	}, testenv.NewKey()); len(bad.Errors["document_number"]) == 0 || len(bad.Errors["address"]) == 0 {
		test.Fatalf("bad tier 3 gave %d %v, want errors on document_number and address", bad.Status, bad.Errors)
	}
	if tier3 := customer.Call(test, http.MethodPost, "/api/mobile/v1/kyc/tier-3", map[string]string{
		"document_type": "passport", "document_number": "a1234 5678", "address": "12 Admiralty Way, Lekki",
	}, testenv.NewKey()); !tier3.Success {
		test.Fatalf("tier 3: %d %s %s %v", tier3.Status, tier3.Code, tier3.Message, tier3.Errors)
	}
}

// TestKycDoubleSubmitRace sends the same tier twice at the same moment with
// different Idempotency-Keys: exactly one request may be created.
func TestKycDoubleSubmitRace(test *testing.T) {
	environment := testenv.Start(test)
	customer := environment.RegisterUser(test, "kycrace")
	if response := submitTier1(test, customer); !response.Success {
		test.Fatalf("tier 1: %s", response.Message)
	}

	var group sync.WaitGroup
	responses := make([]*testenv.Response, 2)
	start := make(chan struct{})
	for index := range responses {
		group.Add(1)
		go func(slot int) {
			defer group.Done()
			<-start
			responses[slot] = submitTier2(test, customer)
		}(index)
	}
	close(start)
	group.Wait()

	successes, refused := 0, 0
	for _, response := range responses {
		switch {
		case response.Success:
			successes++
		case response.Code == "VERIFICATION_IN_REVIEW":
			refused++
		default:
			test.Errorf("unexpected answer %d %s %s", response.Status, response.Code, response.Message)
		}
	}
	if successes != 1 || refused != 1 {
		test.Fatalf("got %d successes and %d refusals, want 1 and 1", successes, refused)
	}
	var open int64
	environment.App.Database.Model(&models.KycSubmission{}).
		Where("user_id = ? AND status = ?", profileOf(test, customer).ID, models.KycSubmissionPending).Count(&open)
	if open != 1 {
		test.Fatalf("%d open requests, want 1", open)
	}
}

// TestPhoneCodeLimits: codes can be asked for back to back (no wait), each new
// code kills the old one, only 6-digit codes are accepted, and more than 10 an
// hour is refused with the wait time.
func TestPhoneCodeLimits(test *testing.T) {
	environment := testenv.Start(test)
	customer := environment.RegisterUser(test, "phonelimits")
	userID := profileOf(test, customer).ID
	send := func() *testenv.Response {
		return customer.Call(test, http.MethodPost, "/api/mobile/v1/phone/send-otp", map[string]string{"phone": "+234 8031234567"}, testenv.NewKey())
	}
	var sent struct {
		DebugCode string `json:"debug_code"`
	}
	send().Decode(test, &sent)
	oldCode := sent.DebugCode

	// straight away, no waiting
	again := send()
	if !again.Success {
		test.Fatalf("second send right away gave %d %s, want success", again.Status, again.Code)
	}
	again.Decode(test, &sent)
	if short := customer.Call(test, http.MethodPost, "/api/mobile/v1/phone/verify", map[string]string{"code": "123"}, testenv.NewKey()); short.Status != http.StatusUnprocessableEntity {
		test.Errorf("3-digit code gave %d, want 422", short.Status)
	}
	if oldCode != sent.DebugCode {
		if stale := customer.Call(test, http.MethodPost, "/api/mobile/v1/phone/verify", map[string]string{"code": oldCode}, testenv.NewKey()); stale.Success {
			test.Fatal("the old code still worked after a new one was sent")
		}
	}
	if verified := customer.Call(test, http.MethodPost, "/api/mobile/v1/phone/verify", map[string]string{"code": sent.DebugCode}, testenv.NewKey()); !verified.Success {
		test.Fatalf("newest code: %d %s %s", verified.Status, verified.Code, verified.Message)
	}

	// ten codes in the last hour -> the eleventh is refused
	for count := 0; count < 8; count++ {
		environment.App.Database.Create(&models.PhoneVerification{
			ID: ids.New(), UserID: userID, Phone: "+2348031234567", CodeHash: "x",
			ExpiresAt: time.Now().UTC(), CreatedAt: time.Now().UTC().Add(-5 * time.Minute),
		})
	}
	if tooMany := send(); tooMany.Code != "TOO_MANY_CODES" || len(tooMany.Errors["retry_after"]) != 1 {
		test.Fatalf("eleventh code in an hour gave %d %s %v, want TOO_MANY_CODES", tooMany.Status, tooMany.Code, tooMany.Errors)
	}
}
