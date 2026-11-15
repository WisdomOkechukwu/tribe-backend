// Package resources turns database models into the JSON the apps receive,
// like Laravel API Resources. Keeping this separate means:
//   - secrets (password hashes, encrypted codes) can never leak by accident,
//   - amounts are always sent as decimal STRINGS ("12.50"), never floats.
package resources

import (
	"encoding/json"
	"math/big"
	"time"

	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// UserResource is the signed-in user's profile.
type UserResource struct {
	ID                   string     `json:"id"`
	FirstName            string     `json:"first_name"`
	LastName             string     `json:"last_name"`
	Tag                  string     `json:"tag"`
	Email                string     `json:"email"`
	Phone                *string    `json:"phone"`
	PhoneVerified        bool       `json:"phone_verified"`
	Country              string     `json:"country"`
	Status               string     `json:"status"`
	KycTier              int        `json:"kyc_tier"`
	KycStatus            string     `json:"kyc_status"`
	KycPendingTier       *int       `json:"kyc_pending_tier"` // the tier with an open request, or null
	KycSubmittedAt       *time.Time `json:"kyc_submitted_at"` // when that request was sent, or null
	GlobalAccountsStatus string     `json:"global_accounts_status"`
	World                *string    `json:"world"`
	OnboardingCompleted  bool       `json:"onboarding_completed"`
	ReferralCode         string     `json:"referral_code"`
	BiometricsEnabled    bool       `json:"biometrics_enabled"`
	TwoFactorEnabled     bool       `json:"two_factor_enabled"`
	HasPin               bool       `json:"has_pin"`
	TagChangedAt         *time.Time `json:"tag_changed_at"`
	CreatedAt            time.Time  `json:"created_at"`
	Version              int        `json:"version"`
}

// User builds a UserResource.
func User(user *models.User, hasPin bool) UserResource {
	return UserResource{
		ID: user.ID, FirstName: user.FirstName, LastName: user.LastName, Tag: user.Tag, Email: user.Email,
		Phone: user.Phone, PhoneVerified: user.PhoneVerifiedAt != nil, Country: user.Country, Status: user.Status,
		KycTier: user.KycTier, KycStatus: user.KycStatus, KycPendingTier: user.KycPendingTier, KycSubmittedAt: user.KycSubmittedAt,
		GlobalAccountsStatus: user.GlobalAccountsStatus,
		World:                user.World, OnboardingCompleted: user.OnboardingCompletedAt != nil, ReferralCode: user.ReferralCode,
		BiometricsEnabled: user.BiometricsEnabled, TwoFactorEnabled: user.TwoFactorEnabled, HasPin: hasPin,
		TagChangedAt: user.TagChangedAt, CreatedAt: user.CreatedAt, Version: user.Version,
	}
}

// TransactionResource is one item in the activity list.
type TransactionResource struct {
	ID            string         `json:"id"`
	Reference     string         `json:"reference"`
	Kind          string         `json:"kind"` // detailed backend type, e.g. card_fund
	Type          string         `json:"type"` // the app's simpler type: receive|send|swap|bill|giftcard|card|cashpoint|referral
	Category      string         `json:"category"`
	Direction     string         `json:"direction"`
	Status        string         `json:"status"`
	Title         string         `json:"title"`
	Subtitle      string         `json:"subtitle"`
	Asset         string         `json:"asset"`
	Amount        string         `json:"amount"`
	ToAsset       *string        `json:"to_asset"`
	ToAmount      *string        `json:"to_amount"`
	FeeAsset      *string        `json:"fee_asset"`
	Fee           string         `json:"fee"`
	UsdValue      string         `json:"usd_value"`
	FailureReason *string        `json:"failure_reason"`
	Meta          map[string]any `json:"meta"`
	Date          time.Time      `json:"date"`
	CompletedAt   *time.Time     `json:"completed_at"`
}

// appTypes maps backend transaction types to the app's types.
var appTypes = map[string]string{
	"deposit": "receive", "receive": "receive", "send": "send", "swap": "swap", "bill": "bill",
	"gift_buy": "giftcard", "gift_sell": "giftcard",
	"card_create": "card", "card_fund": "card", "card_withdraw": "card", "card_delete": "card",
	"cashpoint_create": "cashpoint", "cashpoint_redeem": "cashpoint", "cashpoint_cancel": "cashpoint",
	"referral_withdraw": "referral",
}

// internalMetaKeys are keys of transactions.metadata that describe HOW we
// routed a conversion. They stay in the database for the admin panel but
// are removed from what the app receives.
var internalMetaKeys = []string{"uses_ngn_rail"}

// Transaction builds a TransactionResource. decimals maps asset -> decimal places.
func Transaction(transaction models.Transaction, decimals map[string]int) TransactionResource {
	amount, _ := money.ParseUnits(transaction.FromAmount)
	fee, _ := money.ParseUnits(transaction.FeeAmount)
	resource := TransactionResource{
		ID: transaction.ID, Reference: transaction.Reference, Kind: transaction.Type, Type: appTypes[transaction.Type],
		Category: transaction.Category, Direction: transaction.Direction, Status: transaction.Status,
		Title: transaction.Title, Subtitle: transaction.Subtitle,
		Asset: transaction.FromAsset, Amount: amount.Decimal(decimals[transaction.FromAsset]),
		ToAsset: transaction.ToAsset, FeeAsset: transaction.FeeAsset, UsdValue: transaction.UsdValue,
		FailureReason: transaction.FailureReason, Date: transaction.CreatedAt, CompletedAt: transaction.CompletedAt,
		Meta: map[string]any{},
	}
	if transaction.FeeAsset != nil {
		resource.Fee = fee.Decimal(decimals[*transaction.FeeAsset])
	}
	if transaction.ToAsset != nil && transaction.ToAmount != nil {
		toAmount, _ := money.ParseUnits(*transaction.ToAmount)
		text := toAmount.Decimal(decimals[*transaction.ToAsset])
		resource.ToAmount = &text
	}
	json.Unmarshal(transaction.Metadata, &resource.Meta)
	for _, key := range internalMetaKeys {
		delete(resource.Meta, key)
	}
	return resource
}

// Transactions builds a list.
func Transactions(transactions []models.Transaction, decimals map[string]int) []TransactionResource {
	list := make([]TransactionResource, 0, len(transactions))
	for _, transaction := range transactions {
		list = append(list, Transaction(transaction, decimals))
	}
	return list
}

// RouteLegResource is the conversion on a quote or a receipt, as the
// CUSTOMER did it: one step from the asset they paid with to the asset that
// arrived. How we got there (for example through Naira) is an internal
// routing detail and is never sent to the app.
type RouteLegResource struct {
	Step       int    `json:"step"` // always 1: the app sees one step
	FromAsset  string `json:"from_asset"`
	ToAsset    string `json:"to_asset"`
	FromAmount string `json:"from_amount"`
	ToAmount   string `json:"to_amount"`
	Rate       string `json:"rate"` // to-units per from-unit, for the whole conversion
}

// routeRateDecimalPlaces is how many decimal places a route rate is sent
// with (the same as the rates stored in swap_legs and quotes).
const routeRateDecimalPlaces = 18

// CustomerRoute collapses the stored hops of a conversion (swap_legs, in
// step order) into the single step the customer did: from the first hop's
// asset and amount to the last hop's asset and amount. A conversion that
// went USD -> NGN -> GBP is sent as USD -> GBP, so the app never learns that
// Naira was in the middle. An empty list stays empty (nothing was converted).
//
// The rate is the overall one:
//   - one stored hop: that hop's own rate;
//   - several hops: quotedRate, the rate of the quote the customer accepted;
//   - several hops and no quote (a card or gift card paid from another
//     currency): the amount that arrived divided by the amount converted.
func CustomerRoute(legs []models.SwapLeg, quotedRate string, decimals map[string]int) []RouteLegResource {
	if len(legs) == 0 {
		return []RouteLegResource{}
	}
	first, last := legs[0], legs[len(legs)-1]
	fromAmount, _ := money.ParseUnits(first.FromAmount)
	toAmount, _ := money.ParseUnits(last.ToAmount)
	rate := first.Rate
	if len(legs) > 1 {
		rate = quotedRate
		if rate == "" && fromAmount.IsPositive() {
			overall := new(big.Rat).Quo(toAmount.ToRational(decimals[last.ToAsset]), fromAmount.ToRational(decimals[first.FromAsset]))
			rate = money.FormatRate(overall, routeRateDecimalPlaces)
		}
	}
	return []RouteLegResource{{
		Step: 1, FromAsset: first.FromAsset, ToAsset: last.ToAsset,
		FromAmount: fromAmount.Decimal(decimals[first.FromAsset]), ToAmount: toAmount.Decimal(decimals[last.ToAsset]), Rate: rate,
	}}
}

// QuoteResource is a price the user can accept before it expires. Route is
// always ONE step, from the asset paid to the asset received (see
// RouteLegResource): the hops in between stay on the server.
type QuoteResource struct {
	ID         string             `json:"id"`
	FromAsset  string             `json:"from_asset"`
	ToAsset    string             `json:"to_asset"`
	FromAmount string             `json:"from_amount"` // total paid, fee included
	Fee        string             `json:"fee"`
	ToAmount   string             `json:"to_amount"`
	Rate       string             `json:"rate"`
	Route      []RouteLegResource `json:"route"`
	ExpiresAt  time.Time          `json:"expires_at"`
}

// CardResource is a virtual card (never the full number or CVV).
type CardResource struct {
	ID                    string          `json:"id"`
	Label                 string          `json:"label"`
	Network               string          `json:"network"`
	DesignFinish          string          `json:"design_finish"`
	DesignSettings        json.RawMessage `json:"design_settings"`
	HolderName            string          `json:"holder_name"`
	Currency              string          `json:"currency"`
	LastFour              string          `json:"last_four"`
	Expiry                string          `json:"expiry"`
	Balance               string          `json:"balance"`
	Frozen                bool            `json:"frozen"`
	OnlinePaymentsEnabled bool            `json:"online_payments_enabled"`
	MonthlyLimit          string          `json:"monthly_limit"`
	BillingAddress        json.RawMessage `json:"billing_address"`
	InWallet              bool            `json:"in_wallet"`
	CreatedAt             time.Time       `json:"created_at"`
	Version               int             `json:"version"`
}

// Card builds a CardResource.
func Card(card models.VirtualCard, balance money.Amount) CardResource {
	limit, _ := money.ParseUnits(card.MonthlyLimit)
	return CardResource{
		ID: card.ID, Label: card.Label, Network: card.Network, DesignFinish: card.DesignFinish,
		DesignSettings: json.RawMessage(orEmptyObject(card.DesignSettings)), HolderName: card.HolderName, Currency: card.Currency,
		LastFour: card.LastFour, Expiry: twoDigits(card.ExpiryMonth) + "/" + twoDigits(card.ExpiryYear%100),
		Balance: balance.Decimal(2), Frozen: card.Frozen, OnlinePaymentsEnabled: card.OnlinePaymentsEnabled,
		MonthlyLimit: limit.Decimal(2), BillingAddress: json.RawMessage(orEmptyObject(card.BillingAddress)),
		InWallet: card.InWallet, CreatedAt: card.CreatedAt, Version: card.Version,
	}
}

// DeviceResource is one phone in the devices list.
type DeviceResource struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Platform   string     `json:"platform"`
	Model      string     `json:"model"`
	Status     string     `json:"status"`
	IsCurrent  bool       `json:"is_current"`
	LastSeenAt *time.Time `json:"last_seen_at"`
	CreatedAt  time.Time  `json:"created_at"`
}

// Devices builds the device list (the IMEI itself is never sent back).
func Devices(devices []models.UserDevice, currentDeviceID string) []DeviceResource {
	list := make([]DeviceResource, 0, len(devices))
	for _, device := range devices {
		list = append(list, DeviceResource{ID: device.ID, Name: device.Name, Platform: device.Platform, Model: device.Model,
			Status: device.Status, IsCurrent: device.ID == currentDeviceID, LastSeenAt: device.LastSeenAt, CreatedAt: device.CreatedAt})
	}
	return list
}

// orEmptyObject returns "{}" for an empty JSON column.
func orEmptyObject(value []byte) []byte {
	if len(value) == 0 {
		return []byte("{}")
	}
	return value
}

// twoDigits formats 7 as "07".
func twoDigits(number int) string {
	if number < 10 {
		return "0" + string(rune('0'+number))
	}
	return string(rune('0'+number/10)) + string(rune('0'+number%10))
}
