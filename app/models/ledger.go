package models

import (
	"time"

	"gorm.io/datatypes"
)

// Asset is a currency or coin (table: assets).
type Asset struct {
	Code                 string    `gorm:"primaryKey" json:"code"`
	Name                 string    `json:"name"`
	Kind                 string    `json:"kind"` // fiat | crypto
	Symbol               string    `json:"symbol"`
	DecimalPlaces        int       `json:"decimal_places"`
	ConversionProviderID *string   `json:"conversion_provider_id"`
	DisplayOrder         int       `json:"display_order"`
	Enabled              bool      `json:"enabled"`
	CreatedAt            time.Time `json:"created_at"`
	UpdatedAt            time.Time `json:"updated_at"`
}

// Provider is an outside company that moves money for us (table: providers).
type Provider struct {
	ID                       string    `gorm:"primaryKey" json:"id"`
	Slug                     string    `json:"slug"`
	Name                     string    `json:"name"`
	Category                 string    `json:"category"` // global | crypto | utilities | gift_card | virtual_card
	UsesNgnRail              bool      `json:"uses_ngn_rail"`
	SupportsDirectConversion bool      `json:"supports_direct_conversion"`
	IsSimulator              bool      `json:"is_simulator"`
	Enabled                  bool      `json:"enabled"`
	CredentialsEnvKey        *string   `json:"credentials_env_key"`
	Version                  int       `json:"version"`
	CreatedAt                time.Time `json:"created_at"`
	UpdatedAt                time.Time `json:"updated_at"`
}

// Provider categories.
const (
	ProviderCategoryGlobal      = "global"
	ProviderCategoryCrypto      = "crypto"
	ProviderCategoryUtilities   = "utilities"
	ProviderCategoryGiftCard    = "gift_card"
	ProviderCategoryVirtualCard = "virtual_card"
)

// ProviderAsset says a provider supports an asset (table: provider_assets).
type ProviderAsset struct {
	ProviderID string `gorm:"primaryKey" json:"provider_id"`
	AssetCode  string `gorm:"primaryKey" json:"asset_code"`
}

// ExchangeRate is one collected rate, kept forever as history (table: exchange_rates).
type ExchangeRate struct {
	ID          uint64    `gorm:"primaryKey" json:"id"`
	AssetCode   string    `json:"asset_code"`
	UsdRate     string    `json:"usd_rate"` // decimal text, e.g. "98400.000000000000000000"
	NgnRate     string    `json:"ngn_rate"`
	ProviderID  *string   `json:"provider_id"`
	CollectedAt time.Time `json:"collected_at"`
}

// ExchangeRateLatest is the newest rate per asset (table: exchange_rate_latest).
type ExchangeRateLatest struct {
	AssetCode      string    `gorm:"primaryKey" json:"asset_code"`
	UsdRate        string    `json:"usd_rate"`
	NgnRate        string    `json:"ngn_rate"`
	OpeningUsdRate string    `json:"opening_usd_rate"`
	ProviderID     *string   `json:"provider_id"`
	RatesVersion   uint64    `json:"rates_version"`
	CollectedAt    time.Time `json:"collected_at"`
}

// TableName tells GORM the table name (it would otherwise guess "exchange_rate_latests").
func (ExchangeRateLatest) TableName() string { return "exchange_rate_latest" }

// RateMargin is our buy/sell spread for an asset (table: rate_margins).
type RateMargin struct {
	AssetCode         string    `gorm:"primaryKey" json:"asset_code"`
	BuyMarginPercent  string    `json:"buy_margin_percent"`
	SellMarginPercent string    `json:"sell_margin_percent"`
	Version           int       `json:"version"`
	UpdatedBy         string    `json:"updated_by"`
	UpdatedAt         time.Time `json:"updated_at"`
}

// TransactionFee is the fee rule for one transaction type (table: transaction_fees).
type TransactionFee struct {
	TransactionType string    `gorm:"primaryKey" json:"transaction_type"`
	Category        string    `json:"category"`
	FeePercent      string    `json:"fee_percent"`
	FlatFee         string    `json:"flat_fee"`
	FlatFeeAsset    string    `json:"flat_fee_asset"`
	MaximumFee      *string   `json:"maximum_fee"`
	Active          bool      `json:"active"`
	Version         int       `json:"version"`
	UpdatedBy       string    `json:"updated_by"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// LedgerAccount is a bucket money can sit in (table: ledger_accounts).
type LedgerAccount struct {
	ID            uint64    `gorm:"primaryKey" json:"id"`
	AccountNumber string    `json:"account_number"`
	OwnerKey      string    `json:"owner_key"`
	Code          string    `json:"code"`
	Name          string    `json:"name"`
	Category      string    `json:"category"`
	NormalSide    string    `json:"normal_side"` // debit | credit
	AssetCode     string    `json:"asset_code"`
	UserID        *string   `json:"user_id"`
	VirtualCardID *string   `json:"virtual_card_id"`
	AccountType   string    `json:"account_type"`
	AllowNegative bool      `json:"allow_negative"`
	Status        string    `json:"status"`
	CreatedAt     time.Time `json:"created_at"`
}

// JournalEntry is one financial event (table: journal_entries).
type JournalEntry struct {
	ID                    string         `gorm:"primaryKey" json:"id"`
	Reference             string         `json:"reference"`
	TransactionType       string         `json:"transaction_type"`
	Description           string         `json:"description"`
	Status                string         `json:"status"`
	ReversesEntryID       *string        `json:"reverses_entry_id"`
	BusinessTransactionID *string        `json:"business_transaction_id"`
	CreatedBy             string         `json:"created_by"`
	Metadata              datatypes.JSON `json:"metadata"`
	AccountingDate        time.Time      `json:"accounting_date"`
	PostedAt              time.Time      `json:"posted_at"`
}

// JournalLeg is one debit or credit line of an entry (table: journal_legs).
type JournalLeg struct {
	ID              uint64    `gorm:"primaryKey" json:"id"`
	JournalEntryID  string    `json:"journal_entry_id"`
	LedgerAccountID uint64    `json:"ledger_account_id"`
	AssetCode       string    `json:"asset_code"`
	Side            string    `json:"side"`   // debit | credit
	Amount          string    `json:"amount"` // smallest units as text, always positive
	UsdRate         *string   `json:"usd_rate"`
	NgnRate         *string   `json:"ngn_rate"`
	Memo            string    `json:"memo"`
	CreatedAt       time.Time `json:"created_at"`
}

// LedgerVerifyRun is one run of the ledger checks (table: ledger_verify_runs).
// The latest CLEAN run tells the next incremental run where to start.
type LedgerVerifyRun struct {
	ID                uint64     `gorm:"primaryKey" json:"id"`
	Mode              string     `json:"mode"`   // full | incremental
	Status            string     `json:"status"` // running | clean | problems | failed
	EntriesFromID     string     `json:"entries_from_id"`
	SnapshotsFromID   uint64     `json:"snapshots_from_id"`
	SnapshotsToID     uint64     `json:"snapshots_to_id"`
	SnapshotsFromDate *time.Time `json:"snapshots_from_date"`
	SnapshotsToDate   *time.Time `json:"snapshots_to_date"`
	EntriesChecked    int64      `json:"entries_checked"`
	SnapshotsChecked  int64      `json:"snapshots_checked"`
	ProblemCount      int64      `json:"problem_count"`
	Message           string     `json:"message"`
	StartedAt         time.Time  `json:"started_at"`
	FinishedAt        *time.Time `json:"finished_at"`
}

// LedgerDailySnapshot is an account's closing balance for a day (table: ledger_daily_snapshots).
type LedgerDailySnapshot struct {
	ID               uint64    `gorm:"primaryKey" json:"id"`
	LedgerAccountID  uint64    `json:"ledger_account_id"`
	SnapshotDate     time.Time `json:"snapshot_date"`
	ClosingBalance   string    `json:"closing_balance"`
	TotalDebits      string    `json:"total_debits"`
	TotalCredits     string    `json:"total_credits"`
	LastJournalLegID uint64    `json:"last_journal_leg_id"`
	CreatedAt        time.Time `json:"created_at"`
}

// Transaction is the user-facing record of something that happened (table: transactions).
type Transaction struct {
	ID             string         `gorm:"primaryKey" json:"id"`
	Reference      string         `json:"reference"`
	UserID         string         `json:"user_id"`
	Category       string         `json:"category"`
	Type           string         `json:"type"`
	Direction      string         `json:"direction"`
	Status         string         `json:"status"`
	Title          string         `json:"title"`
	Subtitle       string         `json:"subtitle"`
	FromAsset      string         `json:"from_asset"`
	FromAmount     string         `json:"from_amount"`
	ToAsset        *string        `json:"to_asset"`
	ToAmount       *string        `json:"to_amount"`
	FeeAsset       *string        `json:"fee_asset"`
	FeeAmount      string         `json:"fee_amount"`
	UsdValue       string         `json:"usd_value"`
	JournalEntryID *string        `json:"journal_entry_id"`
	QuoteID        *string        `json:"quote_id"`
	Metadata       datatypes.JSON `json:"metadata"`
	FailureReason  *string        `json:"failure_reason"`
	Version        int            `json:"version"`
	CreatedAt      time.Time      `json:"created_at"`
	UpdatedAt      time.Time      `json:"updated_at"`
	CompletedAt    *time.Time     `json:"completed_at"`
}

// Transaction statuses.
const (
	TransactionStatusPending   = "pending"
	TransactionStatusCompleted = "completed"
	TransactionStatusFailed    = "failed"
	TransactionStatusCancelled = "cancelled"
	TransactionStatusReversed  = "reversed"
)

// Quote is a price we promise for a short time (table: quotes).
type Quote struct {
	ID              string         `gorm:"primaryKey" json:"id"`
	UserID          string         `json:"user_id"`
	TransactionType string         `json:"transaction_type"`
	FromAsset       string         `json:"from_asset"`
	ToAsset         string         `json:"to_asset"`
	FromAmount      string         `json:"from_amount"`
	FeeAmount       string         `json:"fee_amount"`
	ToAmount        string         `json:"to_amount"`
	Rate            string         `json:"rate"`
	UsesNgnRail     bool           `json:"uses_ngn_rail"`
	NairaInAmount   *string        `json:"naira_in_amount"`
	NairaOutAmount  *string        `json:"naira_out_amount"`
	Route           datatypes.JSON `json:"route"`
	ExpiresAt       time.Time      `json:"expires_at"`
	UsedAt          *time.Time     `json:"used_at"`
	CreatedAt       time.Time      `json:"created_at"`
}

// SwapLeg is one hop of a conversion route (table: swap_legs).
type SwapLeg struct {
	ID            uint64    `gorm:"primaryKey" json:"id"`
	TransactionID string    `json:"transaction_id"`
	QuoteID       *string   `json:"quote_id"`
	Step          int       `json:"step"`
	FromAsset     string    `json:"from_asset"`
	ToAsset       string    `json:"to_asset"`
	FromAmount    string    `json:"from_amount"`
	ToAmount      string    `json:"to_amount"`
	Rate          string    `json:"rate"`
	ProviderID    *string   `json:"provider_id"`
	ViaNgnRail    bool      `json:"via_ngn_rail"`
	CreatedAt     time.Time `json:"created_at"`
}

// IdempotencyKey remembers a request sent with an Idempotency-Key header (table: idempotency_keys).
type IdempotencyKey struct {
	ID             string    `gorm:"primaryKey" json:"id"`
	ActorType      string    `json:"actor_type"`
	ActorID        string    `json:"actor_id"`
	IdempotencyKey string    `gorm:"column:idempotency_key" json:"idempotency_key"`
	RequestMethod  string    `json:"request_method"`
	RequestPath    string    `json:"request_path"`
	RequestHash    string    `json:"-"`
	Status         string    `json:"status"` // processing | completed
	ResponseStatus *int      `json:"response_status"`
	ResponseBody   []byte    `json:"-"`
	LockedUntil    time.Time `json:"locked_until"`
	CreatedAt      time.Time `json:"created_at"`
	UpdatedAt      time.Time `json:"updated_at"`
}

// Beneficiary is someone the user paid before (table: beneficiaries).
type Beneficiary struct {
	ID        string         `gorm:"primaryKey" json:"id"`
	UserID    string         `json:"user_id"`
	Kind      string         `json:"kind"`
	Name      string         `json:"name"`
	Handle    string         `json:"handle"`
	AssetCode string         `json:"asset_code"`
	ChannelID *string        `json:"channel_id"`
	Network   *string        `json:"network"`
	Country   *string        `json:"country"`
	Details   datatypes.JSON `json:"details"`
	DedupeKey string         `json:"dedupe_key"`
	CreatedAt time.Time      `json:"created_at"`
}

// TableName tells GORM the table name.
func (Beneficiary) TableName() string { return "beneficiaries" }

// TreasuryFunding is money the company added to its vault (table: treasury_fundings).
type TreasuryFunding struct {
	ID                string     `gorm:"primaryKey" json:"id"`
	AssetCode         string     `json:"asset_code"`
	Amount            string     `json:"amount"`
	ExternalReference string     `json:"external_reference"`
	Note              string     `json:"note"`
	Status            string     `json:"status"` // pending | approved | rejected
	RequestedBy       string     `json:"requested_by"`
	RequestedByName   string     `json:"requested_by_name"`
	ApprovedBy        *string    `json:"approved_by"` // the second admin (also set on rejection)
	ApprovedByName    string     `json:"approved_by_name"`
	DecidedAt         *time.Time `json:"decided_at"`
	RejectionReason   string     `json:"rejection_reason"`
	JournalEntryID    string     `json:"journal_entry_id"` // empty until approved
	CreatedAt         time.Time  `json:"created_at"`
}

// CustomerAdjustment is a manual credit (money from the company's free funds
// into a customer's wallet) or debit (money back out of it) made by admins
// (table: customer_adjustments). Two admins: one requests, a different one
// approves, and only approval posts.
type CustomerAdjustment struct {
	ID              string     `gorm:"primaryKey" json:"id"`
	UserID          string     `json:"user_id"`
	Kind            string     `json:"kind"` // AdjustmentCredit | AdjustmentDebit
	AssetCode       string     `json:"asset_code"`
	Amount          string     `json:"amount"`
	Reason          string     `json:"reason"`
	Status          string     `json:"status"` // pending | approved | rejected (same values as FundingPending...)
	RequestedBy     string     `json:"requested_by"`
	RequestedByName string     `json:"requested_by_name"`
	ApprovedBy      *string    `json:"approved_by"` // the second admin (also set on rejection)
	ApprovedByName  string     `json:"approved_by_name"`
	DecidedAt       *time.Time `json:"decided_at"`
	RejectionReason string     `json:"rejection_reason"`
	TransactionID   string     `json:"transaction_id"`   // empty until approved
	JournalEntryID  string     `json:"journal_entry_id"` // empty until approved
	CreatedAt       time.Time  `json:"created_at"`
}

// The kinds of customer adjustment.
const (
	AdjustmentCredit = "credit"
	AdjustmentDebit  = "debit"
)

// The states of a treasury funding request.
const (
	FundingPending  = "pending"
	FundingApproved = "approved"
	FundingRejected = "rejected"
)

// ProviderWebhookEvent is one webhook received from a provider (table: provider_webhook_events).
type ProviderWebhookEvent struct {
	ID           string     `gorm:"primaryKey" json:"id"`
	ProviderSlug string     `json:"provider_slug"`
	EventID      string     `json:"event_id"`
	EventType    string     `json:"event_type"`
	Payload      []byte     `json:"payload"`
	Status       string     `json:"status"`
	ErrorMessage string     `json:"error_message"`
	ReceivedAt   time.Time  `json:"received_at"`
	ProcessedAt  *time.Time `json:"processed_at"`
}

// ScheduledJobRun is one cron run (table: scheduled_job_runs).
type ScheduledJobRun struct {
	ID         uint64     `gorm:"primaryKey" json:"id"`
	JobName    string     `json:"job_name"`
	WindowKey  string     `json:"window_key"`
	Status     string     `json:"status"`
	Message    string     `json:"message"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at"`
}
