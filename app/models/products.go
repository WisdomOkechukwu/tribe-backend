package models

import (
	"time"

	"gorm.io/datatypes"
)

// Biller is a company you can pay a bill to (table: billers).
type Biller struct {
	ID         string         `gorm:"primaryKey" json:"id"`
	Name       string         `json:"name"`
	Category   string         `json:"category"`
	Field      string         `json:"field"`
	Plans      datatypes.JSON `json:"plans"`
	Background string         `json:"background"`
	Foreground string         `json:"foreground"`
	ProviderID *string        `json:"provider_id"`
	Enabled    bool           `json:"enabled"`
	SortOrder  int            `json:"sort_order"`
}

// BillPayment is one bill paid (table: bill_payments).
type BillPayment struct {
	ID                string    `gorm:"primaryKey" json:"id"`
	UserID            string    `json:"user_id"`
	TransactionID     string    `json:"transaction_id"`
	BillerID          string    `json:"biller_id"`
	Target            string    `json:"target"`
	PlanID            *string   `json:"plan_id"`
	AmountNgn         string    `json:"amount_ngn"`
	PayWithAsset      string    `json:"pay_with_asset"`
	ProviderReference *string   `json:"provider_reference"`
	Token             *string   `json:"token"`
	Units             *string   `json:"units"`
	Status            string    `json:"status"`
	CreatedAt         time.Time `json:"created_at"`
}

// VaultToken is a prepaid electricity token (table: vault_tokens).
type VaultToken struct {
	ID            string     `gorm:"primaryKey" json:"id"`
	UserID        string     `json:"user_id"`
	BillPaymentID string     `json:"bill_payment_id"`
	Provider      string     `json:"provider"`
	Meter         string     `json:"meter"`
	AmountNgn     string     `json:"amount_ngn"`
	Kwh           string     `json:"kwh"`
	Token         string     `json:"token"`
	PaidWith      string     `json:"paid_with"`
	LoadedOn      *time.Time `json:"loaded_on"`
	BoughtAt      time.Time  `json:"bought_at"`
}

// RecurringBill is a bill paid automatically on a schedule (table: recurring_bills).
type RecurringBill struct {
	ID           string     `gorm:"primaryKey" json:"id"`
	UserID       string     `json:"user_id"`
	BillerID     string     `json:"biller_id"`
	Target       string     `json:"target"`
	PlanID       *string    `json:"plan_id"`
	AmountNgn    string     `json:"amount_ngn"`
	PayWithAsset string     `json:"pay_with_asset"`
	Frequency    string     `json:"frequency"`
	NextRunAt    time.Time  `json:"next_run_at"`
	Active       bool       `json:"active"`
	LastPaidAt   *time.Time `json:"last_paid_at"`
	LastResult   *string    `json:"last_result"`
	Version      int        `json:"version"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// GiftCardBrand is a gift card we buy or sell (table: gift_card_brands).
type GiftCardBrand struct {
	ID               string  `gorm:"primaryKey" json:"id"`
	Name             string  `json:"name"`
	SellRateNgn      string  `json:"sell_rate_ngn"`
	BuyMarkupPercent string  `json:"buy_markup_percent"`
	MinimumUsd       int     `json:"minimum_usd"`
	MaximumUsd       int     `json:"maximum_usd"`
	Background       string  `json:"background"`
	Foreground       string  `json:"foreground"`
	ProviderID       *string `json:"provider_id"`
	Enabled          bool    `json:"enabled"`
	SortOrder        int     `json:"sort_order"`
}

// GiftCardOrder is one gift card bought or sold (table: gift_card_orders).
type GiftCardOrder struct {
	ID            string    `gorm:"primaryKey" json:"id"`
	UserID        string    `json:"user_id"`
	TransactionID string    `json:"transaction_id"`
	BrandID       string    `json:"brand_id"`
	Direction     string    `json:"direction"`
	FaceValueUsd  string    `json:"face_value_usd"`
	CodeEncrypted []byte    `json:"-"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
	UpdatedAt     time.Time `json:"updated_at"`
}

// CardDesign is a card look offered in the app (table: card_designs).
type CardDesign struct {
	ID        string    `gorm:"primaryKey" json:"id"`
	Kind      string    `json:"kind"` // theme | swatch | pattern
	Name      string    `json:"name"`
	Finish    string    `json:"finish"`
	HexColor  *string   `json:"hex_color"`
	Pattern   *string   `json:"pattern"`
	Enabled   bool      `json:"enabled"`
	SortOrder int       `json:"sort_order"`
	Version   int       `json:"version"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// VirtualCard is a user's dollar card (table: virtual_cards).
type VirtualCard struct {
	ID                    string         `gorm:"primaryKey" json:"id"`
	UserID                string         `json:"user_id"`
	Label                 string         `json:"label"`
	Network               string         `json:"network"`
	DesignFinish          string         `json:"design_finish"`
	DesignSettings        datatypes.JSON `json:"design_settings"`
	HolderName            string         `json:"holder_name"`
	Currency              string         `json:"currency"`
	LastFour              string         `json:"last_four"`
	ExpiryMonth           int            `json:"expiry_month"`
	ExpiryYear            int            `json:"expiry_year"`
	ProviderCardReference string         `json:"provider_card_reference"`
	Frozen                bool           `json:"frozen"`
	OnlinePaymentsEnabled bool           `json:"online_payments_enabled"`
	MonthlyLimit          string         `json:"monthly_limit"`
	BillingAddress        datatypes.JSON `json:"billing_address"`
	InWallet              bool           `json:"in_wallet"`
	Status                string         `json:"status"`
	Version               int            `json:"version"`
	CreatedAt             time.Time      `json:"created_at"`
	UpdatedAt             time.Time      `json:"updated_at"`
	DeletedAt             *time.Time     `json:"deleted_at"`
}

// Cashpoint is a redeemable Naira code (table: cashpoints).
type Cashpoint struct {
	ID                  string     `gorm:"primaryKey" json:"id"`
	CreatorUserID       string     `json:"creator_user_id"`
	CodeHash            string     `json:"-"`
	CodeEncrypted       []byte     `json:"-"`
	Amount              string     `json:"amount"`
	Status              string     `json:"status"`
	TransactionID       string     `json:"transaction_id"`
	RedeemedByUserID    *string    `json:"redeemed_by_user_id"`
	RedeemTransactionID *string    `json:"redeem_transaction_id"`
	RedeemedAt          *time.Time `json:"redeemed_at"`
	CancelledAt         *time.Time `json:"cancelled_at"`
	Version             int        `json:"version"`
	CreatedAt           time.Time  `json:"created_at"`
	UpdatedAt           time.Time  `json:"updated_at"`
}

// ReferralEarning is money earned from an invited friend (table: referral_earnings).
type ReferralEarning struct {
	ID           string    `gorm:"primaryKey" json:"id"`
	UserID       string    `json:"user_id"`
	FriendUserID *string   `json:"friend_user_id"`
	FriendName   string    `json:"friend_name"`
	Description  string    `json:"description"`
	AmountNgn    string    `json:"amount_ngn"`
	CreatedAt    time.Time `json:"created_at"`
}

// InboxMessage is a notification in the app inbox (table: inbox_messages).
type InboxMessage struct {
	ID        string         `gorm:"primaryKey" json:"id"`
	UserID    string         `json:"user_id"`
	Kind      string         `json:"kind"`
	Title     string         `json:"title"`
	Body      string         `json:"body"`
	Href      *string        `json:"href"`
	Data      datatypes.JSON `json:"data"`
	ReadAt    *time.Time     `json:"read_at"`
	PushedAt  *time.Time     `json:"-"` // set once the push notification is queued
	CreatedAt time.Time      `json:"created_at"`
}

// SupportThread is a support conversation (table: support_threads).
type SupportThread struct {
	ID             string    `gorm:"primaryKey" json:"id"`
	UserID         string    `json:"user_id"`
	Channel        string    `json:"channel"`
	Subject        string    `json:"subject"`
	TransactionID  *string   `json:"transaction_id"`
	Status         string    `json:"status"`
	Priority       string    `json:"priority"`
	UnreadForAdmin int       `json:"unread_for_admin"`
	Version        int       `json:"version"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// SupportMessage is one message in a thread (table: support_messages).
type SupportMessage struct {
	ID         string    `gorm:"primaryKey" json:"id"`
	ThreadID   string    `json:"thread_id"`
	SenderType string    `json:"sender_type"`
	AuthorName string    `json:"author_name"`
	Body       string    `json:"body"`
	CreatedAt  time.Time `json:"created_at"`
}

// KycVerification is one identity check (table: kyc_verifications).
type KycVerification struct {
	ID             string     `gorm:"primaryKey" json:"id"`
	UserID         string     `json:"user_id"`
	SubmissionID   *string    `json:"submission_id"` // the request (KycSubmission) this check belongs to
	Tier           int        `json:"tier"`
	CheckType      string     `json:"check_type"`
	Provider       string     `json:"provider"`
	Reference      string     `json:"reference"`
	Status         string     `json:"status"`
	DocumentType   *string    `json:"document_type"`
	DocumentMasked *string    `json:"document_masked"`
	MatchScore     *int       `json:"match_score"`
	DataEncrypted  []byte     `json:"-"`
	Reason         *string    `json:"reason"`
	SubmittedAt    time.Time  `json:"submitted_at"`
	DecidedAt      *time.Time `json:"decided_at"`
	DecidedBy      *string    `json:"decided_by"`
	Version        int        `json:"version"`
}

// KycSubmission is ONE verification request for a tier, holding that tier's
// checks (table: kyc_submissions). Admins approve or reject the whole request.
// OpenKey is the user id while the request is pending and NULL once decided;
// a unique index on it means a customer can have only one open request.
type KycSubmission struct {
	ID          string     `gorm:"primaryKey" json:"id"`
	UserID      string     `json:"user_id"`
	Tier        int        `json:"tier"`
	Status      string     `json:"status"` // KycSubmissionPending | KycSubmissionApproved | KycSubmissionRejected
	Reason      string     `json:"reason"` // why it was rejected; the customer sees it
	SubmittedAt time.Time  `json:"submitted_at"`
	DecidedAt   *time.Time `json:"decided_at"`
	DecidedBy   *string    `json:"decided_by"`
	OpenKey     *string    `json:"-"`
	Version     int        `json:"version"`
}

// The states of a KYC submission.
const (
	KycSubmissionPending  = "pending"
	KycSubmissionApproved = "approved"
	KycSubmissionRejected = "rejected"
)

// GlobalAccountApplication is a request for USD/GBP/EUR accounts (table: global_account_applications).
type GlobalAccountApplication struct {
	ID        string         `gorm:"primaryKey" json:"id"`
	UserID    string         `json:"user_id"`
	Answers   datatypes.JSON `json:"answers"`
	Status    string         `json:"status"`
	CreatedAt time.Time      `json:"created_at"`
}

// ApiRequestLog is one logged API call (table: api_request_logs).
type ApiRequestLog struct {
	ID            uint64    `gorm:"primaryKey" json:"id"`
	RequestID     string    `json:"request_id"`
	ActorType     string    `json:"actor_type"`
	UserID        *string   `json:"user_id"`
	Method        string    `json:"method"`
	Path          string    `json:"path"`
	Status        int       `json:"status"`
	LatencyMs     int       `json:"latency_ms"`
	IPAddress     string    `gorm:"column:ip_address" json:"ip_address"`
	Device        string    `json:"device"`
	TransactionID *string   `json:"transaction_id"`
	RequestBody   *string   `json:"request_body"`
	ResponseBody  *string   `json:"-"`
	ErrorMessage  *string   `json:"error_message"`
	CreatedAt     time.Time `json:"created_at"`
}
