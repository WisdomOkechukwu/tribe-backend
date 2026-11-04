package ledger

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// Leg is one line of a journal entry: debit or credit one account.
type Leg struct {
	AccountID uint64
	Side      string // Debit or Credit
	Amount    money.Amount
	Memo      string
}

// AssetRate is the USD and NGN value of one whole unit of an asset, stored
// on each leg so reports can show values at the time of posting.
type AssetRate struct {
	Usd string
	Ngn string
}

// Entry is everything needed to post one journal entry.
type Entry struct {
	// Reference must be unique and the SAME every time the same business
	// event is retried (for example "SWP-01J...:POST"). This is what makes
	// posting idempotent: a second post with the same reference does nothing.
	Reference             string
	TransactionType       string
	Description           string
	CreatedBy             string
	BusinessTransactionID *string
	ReversesEntryID       *string
	Metadata              map[string]any
	Rates                 map[string]AssetRate // optional: asset code -> rates at posting time
	Legs                  []Leg
}

// PostResult is what Post returns.
type PostResult struct {
	Entry         *models.JournalEntry
	AlreadyPosted bool // true when this reference was posted before (nothing new was written)
}

// Post writes a journal entry and its legs. It MUST be called inside
// database.WithTransaction so that it is all-or-nothing together with the
// caller's other changes (like creating the transactions row).
//
// Steps:
//  1. Check the legs make sense (at least 2, amounts > 0, debits = credits per asset).
//  2. Insert the entry header. If the reference already exists, stop: it was
//     already posted (idempotency).
//  3. Lock every account the legs touch, in ascending id order (FOR UPDATE).
//     Always locking in the same order means two transfers A->B and B->A can
//     never wait for each other forever (a deadlock).
//  4. For user accounts, make sure the balance will not go below zero.
//  5. Insert the legs.
func (service *Service) Post(databaseTransaction *gorm.DB, entry Entry) (*PostResult, error) {
	if err := validateLegs(entry.Legs); err != nil {
		return nil, err
	}

	now := time.Now().UTC().Truncate(time.Microsecond)
	metadataJSON, err := json.Marshal(entry.Metadata)
	if err != nil {
		return nil, err
	}
	if entry.CreatedBy == "" {
		entry.CreatedBy = "system"
	}

	// Step 2: insert the header first. The UNIQUE index on reference makes
	// a duplicate fail immediately, before we lock or change anything.
	header := models.JournalEntry{
		ID:                    ids.New(),
		Reference:             entry.Reference,
		TransactionType:       entry.TransactionType,
		Description:           entry.Description,
		Status:                "posted",
		ReversesEntryID:       entry.ReversesEntryID,
		BusinessTransactionID: entry.BusinessTransactionID,
		CreatedBy:             entry.CreatedBy,
		Metadata:              metadataJSON,
		AccountingDate:        lagosDate(now, service.timeZone),
		PostedAt:              now,
	}
	if err := databaseTransaction.Create(&header).Error; err != nil {
		if database.IsDuplicateEntry(err) {
			var existing models.JournalEntry
			if findError := databaseTransaction.Where("reference = ?", entry.Reference).Take(&existing).Error; findError != nil {
				return nil, findError
			}
			return &PostResult{Entry: &existing, AlreadyPosted: true}, nil
		}
		return nil, err
	}

	// Step 3: lock the accounts in ascending id order.
	accountIDs := uniqueAccountIDs(entry.Legs)
	var accounts []models.LedgerAccount
	err = databaseTransaction.
		Clauses(clause.Locking{Strength: "UPDATE"}).
		Where("id IN ?", accountIDs).
		Order("id ASC").
		Find(&accounts).Error
	if err != nil {
		return nil, err
	}
	if len(accounts) != len(accountIDs) {
		return nil, fmt.Errorf("posting %s: some ledger accounts do not exist", entry.Reference)
	}
	accountsByID := make(map[uint64]models.LedgerAccount, len(accounts))
	for _, account := range accounts {
		accountsByID[account.ID] = account
	}

	// Step 1 (continued, needs the accounts): debits must equal credits for EVERY asset.
	if err := checkBalancedPerAsset(entry.Legs, accountsByID); err != nil {
		return nil, err
	}

	// Step 4: work out how much each account changes and protect user balances.
	changeByAccount := make(map[uint64]money.Amount)
	for _, leg := range entry.Legs {
		account := accountsByID[leg.AccountID]
		change := leg.Amount
		if leg.Side != account.NormalSide {
			change = change.Negate()
		}
		current, found := changeByAccount[leg.AccountID]
		if !found {
			current = money.Zero()
		}
		changeByAccount[leg.AccountID] = current.Add(change)
	}
	for _, accountID := range accountIDs {
		account := accountsByID[accountID]
		change := changeByAccount[accountID]
		if account.Status != "active" && change.IsNegative() {
			return nil, apperrors.ErrAccountFrozen
		}
		if account.AllowNegative || !change.IsNegative() {
			continue
		}
		balance, err := service.balanceOfAccount(databaseTransaction, account)
		if err != nil {
			return nil, err
		}
		if balance.Add(change).IsNegative() {
			return nil, apperrors.ErrInsufficientFunds
		}
	}

	// Step 5: insert the legs.
	legRows := make([]models.JournalLeg, 0, len(entry.Legs))
	for _, leg := range entry.Legs {
		account := accountsByID[leg.AccountID]
		row := models.JournalLeg{
			JournalEntryID:  header.ID,
			LedgerAccountID: leg.AccountID,
			AssetCode:       account.AssetCode,
			Side:            leg.Side,
			Amount:          leg.Amount.UnitsString(),
			Memo:            truncate(leg.Memo, 150),
			CreatedAt:       now,
		}
		if rate, found := entry.Rates[account.AssetCode]; found {
			usdRate, ngnRate := rate.Usd, rate.Ngn
			row.UsdRate, row.NgnRate = &usdRate, &ngnRate
		}
		legRows = append(legRows, row)
	}
	if err := databaseTransaction.Create(&legRows).Error; err != nil {
		return nil, err
	}
	return &PostResult{Entry: &header}, nil
}

// Reverse posts the exact opposite of an existing entry (every debit becomes
// a credit and the other way round). The UNIQUE reverses_entry_id column
// means an entry can only ever be reversed once.
func (service *Service) Reverse(databaseTransaction *gorm.DB, originalEntryID string, reason string, createdBy string) (*PostResult, error) {
	var original models.JournalEntry
	if err := databaseTransaction.Where("id = ?", originalEntryID).Take(&original).Error; err != nil {
		return nil, err
	}
	var originalLegs []models.JournalLeg
	if err := databaseTransaction.Where("journal_entry_id = ?", originalEntryID).Order("id ASC").Find(&originalLegs).Error; err != nil {
		return nil, err
	}
	legs := make([]Leg, 0, len(originalLegs))
	for _, originalLeg := range originalLegs {
		amount, err := money.ParseUnits(originalLeg.Amount)
		if err != nil {
			return nil, err
		}
		legs = append(legs, Leg{AccountID: originalLeg.LedgerAccountID, Side: originalLeg.Side, Amount: amount, Memo: originalLeg.Memo})
	}
	reversedLegs := ReversalLegs(legs)
	originalID := original.ID
	result, err := service.Post(databaseTransaction, Entry{
		Reference:             original.Reference + ":REVERSAL",
		TransactionType:       original.TransactionType + "_reversal",
		Description:           "Reversal: " + reason,
		CreatedBy:             createdBy,
		BusinessTransactionID: original.BusinessTransactionID,
		ReversesEntryID:       &originalID,
		Legs:                  reversedLegs,
	})
	if err != nil && database.IsDuplicateEntry(err) {
		return nil, apperrors.New(http.StatusConflict, "ALREADY_REVERSED", "This entry was already reversed.")
	}
	return result, err
}

// validateLegs checks the simple rules that do not need the database.
func validateLegs(legs []Leg) error {
	if len(legs) < 2 {
		return errors.New("a journal entry needs at least two legs")
	}
	for _, leg := range legs {
		if leg.Side != Debit && leg.Side != Credit {
			return fmt.Errorf("leg side must be debit or credit, got %q", leg.Side)
		}
		if !leg.Amount.IsPositive() {
			return errors.New("every leg amount must be greater than zero")
		}
		if leg.AccountID == 0 {
			return errors.New("every leg needs an account")
		}
	}
	return nil
}

// checkBalancedPerAsset makes sure that, for each asset, total debits equal total credits.
func checkBalancedPerAsset(legs []Leg, accountsByID map[uint64]models.LedgerAccount) error {
	netByAsset := map[string]money.Amount{}
	for _, leg := range legs {
		assetCode := accountsByID[leg.AccountID].AssetCode
		net, found := netByAsset[assetCode]
		if !found {
			net = money.Zero()
		}
		if leg.Side == Debit {
			net = net.Add(leg.Amount)
		} else {
			net = net.Subtract(leg.Amount)
		}
		netByAsset[assetCode] = net
	}
	for assetCode, net := range netByAsset {
		if !net.IsZero() {
			return fmt.Errorf("journal entry does not balance for %s: debits minus credits = %s", assetCode, net.UnitsString())
		}
	}
	return nil
}

// uniqueAccountIDs returns the distinct account ids, sorted smallest first.
func uniqueAccountIDs(legs []Leg) []uint64 {
	seen := map[uint64]bool{}
	accountIDs := []uint64{}
	for _, leg := range legs {
		if !seen[leg.AccountID] {
			seen[leg.AccountID] = true
			accountIDs = append(accountIDs, leg.AccountID)
		}
	}
	sort.Slice(accountIDs, func(first, second int) bool { return accountIDs[first] < accountIDs[second] })
	return accountIDs
}

// lagosDate returns the calendar day in Lagos as a date (time at midnight UTC).
func lagosDate(moment time.Time, timeZone *time.Location) time.Time {
	local := moment.In(timeZone)
	return time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, time.UTC)
}

// truncate cuts text to a maximum length (so it fits its column).
func truncate(text string, maximumLength int) string {
	if len(text) <= maximumLength {
		return text
	}
	return text[:maximumLength]
}
