// Package requests describes what each endpoint expects in its JSON body,
// like Laravel FormRequests. The `binding:"..."` tags are the validation
// rules (required, email, min, max, oneof ...). Bind() checks them and
// returns friendly per-field errors.
package requests

import (
	"errors"
	"io"
	"reflect"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/go-playground/validator/v10"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
)

// Bind reads the JSON body into input and validates it.
func Bind(httpContext *gin.Context, input any) error {
	err := httpContext.ShouldBindJSON(input)
	if err == nil {
		return nil
	}
	if errors.Is(err, io.EOF) {
		return apperrors.Validation(map[string][]string{"body": {"The request body is empty."}})
	}
	var validationErrors validator.ValidationErrors
	if errors.As(err, &validationErrors) {
		fieldErrors := map[string][]string{}
		for _, fieldError := range validationErrors {
			name := jsonFieldName(input, fieldError.StructField())
			fieldErrors[name] = append(fieldErrors[name], message(fieldError))
		}
		return apperrors.Validation(fieldErrors)
	}
	return apperrors.Validation(map[string][]string{"body": {"The request body is not valid JSON."}})
}

// message turns a validation rule into a sentence.
func message(fieldError validator.FieldError) string {
	switch fieldError.Tag() {
	case "required":
		return "This field is required."
	case "email":
		return "Enter a valid email address."
	case "min":
		return "Too short (minimum " + fieldError.Param() + ")."
	case "max":
		return "Too long (maximum " + fieldError.Param() + ")."
	case "len":
		return "Must be exactly " + fieldError.Param() + " characters."
	case "oneof":
		return "Must be one of: " + fieldError.Param() + "."
	case "numeric":
		return "Must contain only digits."
	}
	return "This value is not valid."
}

// jsonFieldName finds the json name of a struct field (for error messages).
func jsonFieldName(input any, structField string) string {
	inputType := reflect.TypeOf(input)
	for inputType.Kind() == reflect.Pointer {
		inputType = inputType.Elem()
	}
	if field, found := inputType.FieldByName(structField); found {
		if name, _, _ := strings.Cut(field.Tag.Get("json"), ","); name != "" {
			return name
		}
	}
	return strings.ToLower(structField)
}

// ─── Mobile: auth ────────────────────────────────────────────────────

// DeviceRequest describes the phone (sent with register and login).
type DeviceRequest struct {
	Imei       string `json:"imei" binding:"required,min=4,max=191"`
	Name       string `json:"name" binding:"max=100"`
	Platform   string `json:"platform" binding:"max=16"`
	Model      string `json:"model" binding:"max=100"`
	AppVersion string `json:"app_version" binding:"max=32"`
}

// EmailRequest is just an email.
type EmailRequest struct {
	Email string `json:"email" binding:"required,email,max=191"`
}

// VerifyEmailRequest checks a 6-digit email code.
type VerifyEmailRequest struct {
	Email   string `json:"email" binding:"required,email"`
	Code    string `json:"code" binding:"required,len=6,numeric"`
	Purpose string `json:"purpose" binding:"omitempty,oneof=signup password_reset"`
}

// RegisterRequest creates an account.
type RegisterRequest struct {
	Email             string        `json:"email" binding:"required,email,max=191"`
	VerificationToken string        `json:"verification_token" binding:"required"`
	Password          string        `json:"password" binding:"required,min=8,max=128"`
	FirstName         string        `json:"first_name" binding:"required,min=2,max=100"`
	LastName          string        `json:"last_name" binding:"required,min=2,max=100"`
	Tag               string        `json:"tag" binding:"required,min=3,max=32"`
	Phone             string        `json:"phone" binding:"max=32"`
	Country           string        `json:"country" binding:"required,len=2"`
	ReferralCode      string        `json:"referral_code" binding:"max=16"`
	HeardFrom         string        `json:"heard_from" binding:"max=64"`
	Device            DeviceRequest `json:"device" binding:"required"`
}

// LoginRequest signs in.
type LoginRequest struct {
	Email    string        `json:"email" binding:"required,email"`
	Password string        `json:"password" binding:"required"`
	Device   DeviceRequest `json:"device" binding:"required"`
}

// VerifyDeviceRequest finishes a new-phone sign-in.
type VerifyDeviceRequest struct {
	VerificationID string `json:"verification_id" binding:"required"`
	Code           string `json:"code" binding:"required,len=6,numeric"`
}

// RefreshRequest swaps a refresh token for new tokens.
type RefreshRequest struct {
	RefreshToken string `json:"refresh_token" binding:"required"`
}

// ResetPasswordRequest sets a new password after the reset code.
type ResetPasswordRequest struct {
	Email             string `json:"email" binding:"required,email"`
	VerificationToken string `json:"verification_token" binding:"required"`
	Password          string `json:"password" binding:"required,min=8,max=128"`
}

// ─── Mobile: profile ─────────────────────────────────────────────────

// UpdateProfileRequest changes profile fields (all optional).
type UpdateProfileRequest struct {
	FirstName         *string `json:"first_name" binding:"omitempty,min=2,max=100"`
	LastName          *string `json:"last_name" binding:"omitempty,min=2,max=100"`
	Address           *string `json:"address" binding:"omitempty,max=255"`
	DateOfBirth       *string `json:"date_of_birth"`
	World             *string `json:"world" binding:"omitempty,oneof=global crypto card bills"`
	BiometricsEnabled *bool   `json:"biometrics_enabled"`
	TwoFactorEnabled  *bool   `json:"two_factor_enabled"`
}

// TagRequest changes the @tag.
type TagRequest struct {
	Tag string `json:"tag" binding:"required,min=3,max=32"`
}

// PinRequest creates the PIN.
type PinRequest struct {
	Pin string `json:"pin" binding:"required,len=4,numeric"`
}

// ChangePinRequest changes the PIN.
type ChangePinRequest struct {
	CurrentPin string `json:"current_pin" binding:"required,len=4,numeric"`
	NewPin     string `json:"new_pin" binding:"required,len=4,numeric"`
}

// ChangePasswordRequest changes the password.
type ChangePasswordRequest struct {
	CurrentPassword string `json:"current_password" binding:"required"`
	NewPassword     string `json:"new_password" binding:"required,min=8,max=128"`
}

// PhoneRequest starts phone verification.
type PhoneRequest struct {
	Phone string `json:"phone" binding:"required,min=8,max=32"`
}

// CodeRequest sends a 6-digit SMS code.
type CodeRequest struct {
	Code string `json:"code" binding:"required,len=6,numeric"`
}

// OnboardingRequest finishes onboarding.
type OnboardingRequest struct {
	World string `json:"world" binding:"required,oneof=global crypto card bills"`
}

// PushTokenRequest saves a push token.
type PushTokenRequest struct {
	Token    string `json:"token" binding:"required,max=191"`
	Platform string `json:"platform" binding:"max=16"`
}

// KycRequest submits a KYC tier. Which fields are required depends on the
// tier, so the per-tier rules live in account.SubmitKyc; these are only limits.
type KycRequest struct {
	Bvn            string `json:"bvn" binding:"max=11"`
	DateOfBirth    string `json:"date_of_birth" binding:"max=10"`
	Nin            string `json:"nin" binding:"max=11"`
	DocumentType   string `json:"document_type" binding:"omitempty,oneof=drivers_licence voters_card nin_slip passport"`
	DocumentNumber string `json:"document_number" binding:"max=32"`
	Address        string `json:"address" binding:"max=255"`
}

// ─── Mobile: money ───────────────────────────────────────────────────

// QuoteRequest asks for a price. Send exactly ONE amount, in the unit the
// user typed it in: amount (what you pay), receive_amount (what you get) or
// usd_amount (what you pay, as US dollars' worth).
type QuoteRequest struct {
	TransactionType string `json:"transaction_type" binding:"required,oneof=swap send"`
	FromAsset       string `json:"from_asset" binding:"required,max=10"`
	ToAsset         string `json:"to_asset" binding:"required,max=10"`
	Amount          string `json:"amount" binding:"max=40"`
	ReceiveAmount   string `json:"receive_amount" binding:"max=40"`
	UsdAmount       string `json:"usd_amount" binding:"max=40"`
}

// SwapRequest confirms a swap.
type SwapRequest struct {
	QuoteID string `json:"quote_id" binding:"required"`
	Pin     string `json:"pin" binding:"required,len=4,numeric"`
}

// SendRequest sends money.
type SendRequest struct {
	RecipientKind   string            `json:"recipient_kind" binding:"required,oneof=tribe bank wallet"`
	Handle          string            `json:"handle" binding:"required,max=191"`
	RecipientName   string            `json:"recipient_name" binding:"max=150"`
	Asset           string            `json:"asset" binding:"max=10"`
	Amount          string            `json:"amount" binding:"max=40"`
	QuoteID         string            `json:"quote_id"`
	ChannelID       string            `json:"channel_id" binding:"max=32"`
	Network         string            `json:"network" binding:"max=32"`
	Country         string            `json:"country" binding:"max=2"`
	Details         map[string]string `json:"details"`
	SaveBeneficiary bool              `json:"save_beneficiary"`
	Pin             string            `json:"pin" binding:"required,len=4,numeric"`
}

// BeneficiaryRequest saves a recipient without sending money.
type BeneficiaryRequest struct {
	Kind      string            `json:"kind" binding:"required,oneof=tribe bank wallet"`
	Name      string            `json:"name" binding:"required,max=150"`
	Handle    string            `json:"handle" binding:"required,max=191"`
	Asset     string            `json:"asset" binding:"required,max=10"`
	ChannelID string            `json:"channel_id" binding:"max=32"`
	Network   string            `json:"network" binding:"max=32"`
	Country   string            `json:"country" binding:"max=2"`
	Details   map[string]string `json:"details"`
}

// VerifyBillCustomerRequest checks a meter/phone/smartcard.
type VerifyBillCustomerRequest struct {
	BillerID string `json:"biller_id" binding:"required"`
	Target   string `json:"target" binding:"required,max=64"`
}

// PayBillRequest pays a bill.
type PayBillRequest struct {
	BillerID  string `json:"biller_id" binding:"required"`
	Target    string `json:"target" binding:"required,max=64"`
	PlanID    string `json:"plan_id" binding:"max=64"`
	AmountNgn string `json:"amount_ngn" binding:"required,max=40"`
	PayWith   string `json:"pay_with" binding:"required,max=10"`
	Pin       string `json:"pin" binding:"required,len=4,numeric"`
}

// VaultTokenRequest marks a token loaded or not.
type VaultTokenRequest struct {
	Loaded bool `json:"loaded"`
}

// RecurringBillRequest creates or changes a schedule.
type RecurringBillRequest struct {
	BillerID  string `json:"biller_id"`
	Target    string `json:"target" binding:"max=64"`
	PlanID    string `json:"plan_id" binding:"max=64"`
	AmountNgn string `json:"amount_ngn" binding:"max=40"`
	PayWith   string `json:"pay_with" binding:"max=10"`
	Frequency string `json:"frequency" binding:"omitempty,oneof=weekly biweekly monthly"`
	StartDate string `json:"start_date"`
	Active    *bool  `json:"active"`
	Version   int    `json:"version"`
	Pin       string `json:"pin"`
}

// GiftCardRequest buys or sells a gift card.
type GiftCardRequest struct {
	BrandID string `json:"brand_id" binding:"required"`
	FaceUsd string `json:"face_usd" binding:"required,max=12"`
	Asset   string `json:"asset" binding:"required,max=10"`
	Code    string `json:"code" binding:"max=100"`
	Pin     string `json:"pin" binding:"required,len=4,numeric"`
}

// CashpointRequest creates a cashpoint code.
type CashpointRequest struct {
	AmountNgn string `json:"amount_ngn" binding:"required,max=40"`
	Pin       string `json:"pin" binding:"required,len=4,numeric"`
}

// RedeemCashpointRequest redeems a code.
type RedeemCashpointRequest struct {
	Code string `json:"code" binding:"required,len=12,numeric"`
}

// CreateCardRequest issues a card.
type CreateCardRequest struct {
	Label        string `json:"label" binding:"required,max=18"`
	Network      string `json:"network" binding:"required,oneof=visa mastercard"`
	DesignFinish string `json:"design_finish" binding:"required,max=64"`
	DesignShade  string `json:"design_shade" binding:"omitempty,oneof=light rich deep"`
	FirstTopUp   string `json:"first_top_up" binding:"required,max=20"`
	PayWith      string `json:"pay_with" binding:"required,max=10"`
	Pin          string `json:"pin" binding:"required,len=4,numeric"`
}

// CardMoneyRequest funds or withdraws a card.
type CardMoneyRequest struct {
	AmountUsd string `json:"amount_usd" binding:"required,max=20"`
	Asset     string `json:"asset" binding:"required,max=10"`
	Pin       string `json:"pin" binding:"required,len=4,numeric"`
}

// UpdateCardRequest changes card settings.
type UpdateCardRequest struct {
	Label          *string `json:"label" binding:"omitempty,max=18"`
	DesignFinish   *string `json:"design_finish" binding:"omitempty,max=64"`
	DesignShade    string  `json:"design_shade" binding:"omitempty,oneof=light rich deep"`
	MonthlyLimit   *string `json:"monthly_limit" binding:"omitempty,max=20"`
	OnlinePayments *bool   `json:"online_payments_enabled"`
	InWallet       *bool   `json:"in_wallet"`
	Version        int     `json:"version"`
}

// CardDesignChangeRequest changes only a card's look.
type CardDesignChangeRequest struct {
	DesignFinish string `json:"design_finish" binding:"required,max=64"`
	DesignShade  string `json:"design_shade" binding:"omitempty,oneof=light rich deep"`
	Version      int    `json:"version"`
}

// CardLimitChangeRequest changes only a card's monthly spending limit (USD).
type CardLimitChangeRequest struct {
	MonthlyLimit string `json:"monthly_limit" binding:"required,max=20"`
	Version      int    `json:"version"`
}

// PinOnlyRequest is a body with just the PIN (reveal card, delete card).
type PinOnlyRequest struct {
	Pin string `json:"pin" binding:"required,len=4,numeric"`
}

// ReferralWithdrawRequest moves referral money to the wallet.
type ReferralWithdrawRequest struct {
	AmountNgn string `json:"amount_ngn" binding:"required,max=40"`
	Pin       string `json:"pin" binding:"required,len=4,numeric"`
}

// StatementRequest asks for a statement.
type StatementRequest struct {
	Asset    string `json:"asset" binding:"max=10"`
	FromDate string `json:"from_date" binding:"required"`
	ToDate   string `json:"to_date" binding:"required"`
}

// TicketRequest opens a support ticket.
type TicketRequest struct {
	Subject       string `json:"subject" binding:"required,max=191"`
	Body          string `json:"body" binding:"required,max=2000"`
	TransactionID string `json:"transaction_id"`
}

// MessageRequest is a message in a thread.
type MessageRequest struct {
	Body string `json:"body" binding:"required,max=2000"`
}

// ─── Admin ───────────────────────────────────────────────────────────

// AdminLoginRequest signs an admin in.
type AdminLoginRequest struct {
	Email    string `json:"email" binding:"required,email"`
	Password string `json:"password" binding:"required"`
}

// CustomerStatusRequest freezes or unfreezes a customer.
type CustomerStatusRequest struct {
	Status  string `json:"status" binding:"required,oneof=active frozen"`
	Reason  string `json:"reason" binding:"required,min=3,max=200"`
	Version int    `json:"version"`
}

// CustomerLabelsRequest replaces labels.
type CustomerLabelsRequest struct {
	Labels []string `json:"labels" binding:"max=6"`
}

// VerificationDecisionRequest approves or rejects a KYC request. A reason is
// required when rejecting (min 3 characters, checked in backoffice.DecideSubmission
// so whitespace doesn't count); the customer sees it.
type VerificationDecisionRequest struct {
	Decision string `json:"decision" binding:"required,oneof=approve reject"`
	Reason   string `json:"reason" binding:"max=255"`
}

// RateMarginRequest changes an asset's margins.
type RateMarginRequest struct {
	BuyMarginPercent  string `json:"buy_margin_percent" binding:"required"`
	SellMarginPercent string `json:"sell_margin_percent" binding:"required"`
	Version           int    `json:"version"`
}

// TransactionFeeRequest changes a fee rule.
type TransactionFeeRequest struct {
	FeePercent   string  `json:"fee_percent" binding:"required"`
	FlatFee      string  `json:"flat_fee" binding:"required"`
	FlatFeeAsset string  `json:"flat_fee_asset" binding:"required,max=10"`
	MaximumFee   *string `json:"maximum_fee"`
	Active       bool    `json:"active"`
	Version      int     `json:"version"`
}

// TreasuryFundingRequest records a top-up.
type TreasuryFundingRequest struct {
	Asset             string `json:"asset" binding:"required,max=10"`
	Amount            string `json:"amount" binding:"required,max=40"`
	ExternalReference string `json:"external_reference" binding:"required,min=3,max=64"`
	Note              string `json:"note" binding:"max=200"`
}

// CustomerAdjustmentRequest asks to credit a customer from the company's free
// funds, or to debit money back out of their wallet.
type CustomerAdjustmentRequest struct {
	Kind   string `json:"kind" binding:"required,oneof=credit debit"`
	Asset  string `json:"asset" binding:"required,max=10"`
	Amount string `json:"amount" binding:"required,max=40"`
	Reason string `json:"reason" binding:"required,min=3,max=200"`
}

// RejectFundingRequest says why a treasury funding (or a customer credit) was refused.
type RejectFundingRequest struct {
	Reason string `json:"reason" binding:"required,min=3,max=255"`
}

// ProviderRequest flips provider switches.
type ProviderRequest struct {
	UsesNgnRail              *bool `json:"uses_ngn_rail"`
	SupportsDirectConversion *bool `json:"supports_direct_conversion"`
	Enabled                  *bool `json:"enabled"`
	Version                  int   `json:"version"`
}

// CurrencyCreateRequest adds a currency or coin. It is created switched off;
// an admin switches it on afterwards (CurrencyUpdateRequest).
type CurrencyCreateRequest struct {
	Code                 string `json:"code" binding:"required,max=10"`
	Name                 string `json:"name" binding:"required,max=64"`
	Kind                 string `json:"kind" binding:"required,oneof=fiat crypto"`
	Symbol               string `json:"symbol" binding:"max=8"`
	DecimalPlaces        *int   `json:"decimal_places" binding:"required,min=0,max=8"`
	ConversionProviderID string `json:"conversion_provider_id" binding:"required,max=26"`
	PriceSource          string `json:"price_source" binding:"required,oneof=fiat_feed coinbase usd_peg"`
}

// CurrencyUpdateRequest renames a currency, moves it in the list or switches
// it on or off. A field that is left out is not changed. version is the one
// the admin last saw (409 STALE_DATA if the row changed since).
type CurrencyUpdateRequest struct {
	Name         *string `json:"name" binding:"omitempty,max=64"`
	Symbol       *string `json:"symbol" binding:"omitempty,max=8"`
	DisplayOrder *int    `json:"display_order" binding:"omitempty,min=0"`
	Enabled      *bool   `json:"enabled"`
	Version      int     `json:"version" binding:"required,min=1"`
}

// ServiceRequest switches a customer-facing service on or off, and/or sets
// the message the app shows while it is off. A field that is left out is
// not changed; an empty disabled_message clears it (the app then shows a
// default). version is the one the admin last saw (409 STALE_DATA if the
// row changed since).
type ServiceRequest struct {
	Enabled         *bool   `json:"enabled"`
	DisabledMessage *string `json:"disabled_message" binding:"omitempty,max=200"`
	Version         int     `json:"version" binding:"required,min=1"`
}

// CardDesignRequest creates (POST) or updates (PUT) a card design.
// POST uses `id` as the new design's id (for example "theme-sunset"); PUT
// takes the id from the URL and needs the `version` last seen (1 or more).
// The per-kind rules (finish for themes, hex_color for swatches, pattern
// for patterns, sort_order 0..10000) are checked by the products service,
// which also clears the fields that do not belong to the kind.
type CardDesignRequest struct {
	ID        string  `json:"id" binding:"max=48"`
	Kind      string  `json:"kind" binding:"required,oneof=theme swatch pattern"`
	Name      string  `json:"name" binding:"required,max=64"`
	Finish    string  `json:"finish" binding:"max=64"`
	HexColor  *string `json:"hex_color" binding:"omitempty,max=16"`
	Pattern   *string `json:"pattern" binding:"omitempty,max=24"`
	Enabled   bool    `json:"enabled"`
	SortOrder int     `json:"sort_order"`
	Version   int     `json:"version"`
}

// CardDesignVersionRequest turns a card design back on: the version the
// admin last saw, so two people cannot flip it at once.
type CardDesignVersionRequest struct {
	Version int `json:"version" binding:"required,min=1"`
}

// TeamMemberRequest creates a teammate.
type TeamMemberRequest struct {
	Name        string   `json:"name" binding:"required,min=2,max=60"`
	Email       string   `json:"email" binding:"required,email"`
	Color       string   `json:"color" binding:"max=48"`
	Permissions []string `json:"permissions" binding:"required,min=1"`
}

// UpdateTeamMemberRequest changes a teammate.
type UpdateTeamMemberRequest struct {
	Name          *string  `json:"name" binding:"omitempty,min=2,max=60"`
	Color         *string  `json:"color" binding:"omitempty,max=48"`
	Status        *string  `json:"status" binding:"omitempty,oneof=active suspended"`
	Permissions   []string `json:"permissions"`
	ResetPassword bool     `json:"reset_password"`
	Version       int      `json:"version"`
}

// SupportStatusRequest changes a thread's status.
type SupportStatusRequest struct {
	Status string `json:"status" binding:"required,oneof=open pending resolved"`
}
