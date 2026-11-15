package products

import (
	"context"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/rates"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/app/services/swaps"
	"github.com/tribe-africa/tribe-backend/database"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// ─── Card designs (stored customisations) ────────────────────────────

// CardDesignCatalog is what the app needs to draw the designer screen.
type CardDesignCatalog struct {
	Themes   []models.CardDesign `json:"themes"`
	Swatches []models.CardDesign `json:"swatches"`
	Patterns []models.CardDesign `json:"patterns"`
}

// ListCardDesigns returns the themes, colours and patterns, in sort order
// (then by name, so equal sort orders always come back the same way).
// includeDisabled is true for the admin panel; the app only gets enabled ones.
func (service *Service) ListCardDesigns(requestContext context.Context, includeDisabled bool) (*CardDesignCatalog, error) {
	query := service.database.WithContext(requestContext).Order("sort_order, name")
	if !includeDisabled {
		query = query.Where("enabled = ?", true)
	}
	var designs []models.CardDesign
	if err := query.Find(&designs).Error; err != nil {
		return nil, err
	}
	catalog := &CardDesignCatalog{Themes: []models.CardDesign{}, Swatches: []models.CardDesign{}, Patterns: []models.CardDesign{}}
	for _, design := range designs {
		switch design.Kind {
		case "theme":
			catalog.Themes = append(catalog.Themes, design)
		case "swatch":
			catalog.Swatches = append(catalog.Swatches, design)
		case "pattern":
			catalog.Patterns = append(catalog.Patterns, design)
		}
	}
	return catalog, nil
}

// DesignSettings is the parsed form of a finish string.
type DesignSettings struct {
	Theme   string `json:"theme,omitempty"`
	Pattern string `json:"pattern"`
	Hex     string `json:"hex,omitempty"`
	Shade   string `json:"shade,omitempty"`
}

var (
	hexColor = regexp.MustCompile(`^[0-9A-Fa-f]{6}$`)
	// designID is the shape of a card design id: lowercase words joined by
	// dashes, where later words may hold capital hex letters
	// ("theme-midnight", "swatch-FF7A5C", "pattern-dots").
	designID = regexp.MustCompile(`^[a-z0-9]+(-[a-z0-9A-F]+)*$`)
	// designWord is a theme finish or a pattern name ("sunset", "dots").
	designWord = regexp.MustCompile(`^[a-z]+$`)
)

// ErrDesignExists is returned when a new card design reuses an id, or the
// same theme finish, colour or pattern as another design.
var ErrDesignExists = apperrors.New(http.StatusConflict, "DESIGN_EXISTS", "A card design with that id or look already exists.")

// splitFinish reads a finish string into its parts WITHOUT checking what
// is enabled. patternGiven is true when the string names a pattern itself
// (a bare theme like "sunset" carries its own built-in look). ok is false
// when the string is not one of the three forms ParseFinish describes.
func splitFinish(finish string, shade string) (settings DesignSettings, patternGiven bool, ok bool) {
	parts := strings.Split(finish, ":")
	settings = DesignSettings{Pattern: "plain"}
	switch {
	case parts[0] == "custom" && (len(parts) == 2 || len(parts) == 3):
		if !hexColor.MatchString(parts[1]) {
			return settings, false, false
		}
		settings.Hex = strings.ToUpper(parts[1])
		settings.Shade = shade
		if len(parts) == 3 {
			settings.Pattern, patternGiven = parts[2], true
		}
	case parts[0] != "" && parts[0] != "custom" && len(parts) <= 2:
		settings.Theme = parts[0]
		if len(parts) == 2 {
			settings.Pattern, patternGiven = parts[1], true
		}
	default:
		return settings, false, false
	}
	if settings.Pattern == "" {
		return settings, false, false
	}
	return settings, patternGiven, true
}

// ParseFinish checks a finish string the customer just PICKED (new card or
// design change) and splits it into parts. The app writes three forms:
//
//	"midnight"               a theme with its own built-in look: only the theme must be enabled
//	"sunset:waves"           a theme with a pattern: the theme AND the pattern must be enabled
//	"custom:FF7A5C:dots"     make-your-own colour: the pattern must be enabled ("custom:FF7A5C"
//	                         means the solid "plain" pattern). Any hex is allowed (the app has a slider).
//
// Cards that already wear a design that was later turned off keep it:
// only new picks come through here.
func (service *Service) ParseFinish(requestContext context.Context, finish string, shade string) (DesignSettings, error) {
	invalid := apperrors.Validation(map[string][]string{"design_finish": {"That card design is not available."}})
	settings, patternGiven, ok := splitFinish(finish, shade)
	if !ok {
		return settings, invalid
	}
	catalog, err := service.ListCardDesigns(requestContext, false)
	if err != nil {
		return DesignSettings{}, err
	}
	themes, patterns := map[string]bool{}, map[string]bool{}
	for _, design := range catalog.Themes {
		themes[design.Finish] = true
	}
	for _, design := range catalog.Patterns {
		if design.Pattern != nil {
			patterns[*design.Pattern] = true
		}
	}
	if settings.Theme != "" && !themes[settings.Theme] {
		return settings, invalid
	}
	// A bare theme brings its own look, so no pattern row is needed for it.
	needsPattern := settings.Theme == "" || patternGiven
	if needsPattern && !patterns[settings.Pattern] {
		return settings, invalid
	}
	return settings, nil
}

// validateCardDesign checks the fields of one design for its kind and
// clears the fields that belong to other kinds: a theme keeps only
// finish, a swatch only hex_color (upper-cased), a pattern only pattern.
func validateCardDesign(design *models.CardDesign) error {
	fieldErrors := map[string][]string{}
	design.Name = strings.TrimSpace(design.Name)
	if design.Name == "" {
		fieldErrors["name"] = append(fieldErrors["name"], "Give the design a name.")
	}
	if design.SortOrder < 0 || design.SortOrder > 10000 {
		fieldErrors["sort_order"] = append(fieldErrors["sort_order"], "Sort order must be between 0 and 10000.")
	}
	switch design.Kind {
	case "theme":
		if !designWord.MatchString(design.Finish) || len(design.Finish) > 64 || design.Finish == "custom" {
			fieldErrors["finish"] = append(fieldErrors["finish"], "A theme needs a finish of lowercase letters (for example sunset).")
		}
		design.HexColor, design.Pattern = nil, nil
	case "swatch":
		if design.HexColor == nil || !hexColor.MatchString(*design.HexColor) {
			fieldErrors["hex_color"] = append(fieldErrors["hex_color"], "A colour needs a 6-digit hex code (for example FF7A5C).")
		} else {
			upper := strings.ToUpper(*design.HexColor)
			design.HexColor = &upper
		}
		design.Finish, design.Pattern = "", nil
	case "pattern":
		if design.Pattern == nil || !designWord.MatchString(*design.Pattern) || len(*design.Pattern) > 24 {
			fieldErrors["pattern"] = append(fieldErrors["pattern"], "A pattern needs a name of lowercase letters (for example dots).")
		}
		design.Finish, design.HexColor = "", nil
	default:
		fieldErrors["kind"] = append(fieldErrors["kind"], "Kind must be theme, swatch or pattern.")
	}
	if len(fieldErrors) > 0 {
		return apperrors.Validation(fieldErrors)
	}
	return nil
}

// sameLookExists is true when ANOTHER design (not excludeID) already has
// this design's kind and look: the same finish for themes, the same hex
// for swatches, the same pattern for patterns. Two rows with one look
// would make ParseFinish and the app's picker ambiguous.
func sameLookExists(databaseTransaction *gorm.DB, design models.CardDesign, excludeID string) (bool, error) {
	lookQuery := databaseTransaction.Model(&models.CardDesign{}).Where("kind = ? AND id <> ?", design.Kind, excludeID)
	switch design.Kind {
	case "theme":
		lookQuery = lookQuery.Where("finish = ?", design.Finish)
	case "swatch":
		lookQuery = lookQuery.Where("hex_color = ?", *design.HexColor)
	case "pattern":
		lookQuery = lookQuery.Where("pattern = ?", *design.Pattern)
	}
	var total int64
	err := lookQuery.Count(&total).Error
	return total > 0, err
}

// CreateCardDesign stores a new design under the id the admin chose
// ("theme-midnight", "swatch-FF7A5C", "pattern-dots"). It returns 409
// DESIGN_EXISTS when the id is taken or another design already has the
// same look.
func (service *Service) CreateCardDesign(requestContext context.Context, design models.CardDesign) (*models.CardDesign, error) {
	if len(design.ID) < 2 || len(design.ID) > 48 || !designID.MatchString(design.ID) {
		return nil, apperrors.Validation(map[string][]string{"id": {"Use 2 to 48 characters: lowercase words joined by dashes (for example theme-sunset)."}})
	}
	if err := validateCardDesign(&design); err != nil {
		return nil, err
	}
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		var existing models.CardDesign
		lookupError := databaseTransaction.Where("id = ?", design.ID).Take(&existing).Error
		if lookupError == nil {
			return ErrDesignExists
		}
		if !database.IsNotFound(lookupError) {
			return lookupError
		}
		taken, err := sameLookExists(databaseTransaction, design, design.ID)
		if err != nil {
			return err
		}
		if taken {
			return ErrDesignExists
		}
		now := time.Now().UTC()
		design.Version, design.CreatedAt, design.UpdatedAt = 1, now, now
		err = databaseTransaction.Create(&design).Error
		// Two admins creating the same id at the same moment: the primary
		// key stops the second insert.
		if database.IsDuplicateEntry(err) {
			return ErrDesignExists
		}
		return err
	})
	if err != nil {
		return nil, err
	}
	return &design, nil
}

// UpdateCardDesign changes an existing design with lock + version: the
// caller sends the version it last saw (1 or more), and a different
// current version returns STALE_DATA (409). Cards already wearing the
// design keep their own finish string, so they are not affected.
func (service *Service) UpdateCardDesign(requestContext context.Context, design models.CardDesign, expectedVersion int) (*models.CardDesign, error) {
	if expectedVersion < 1 {
		return nil, apperrors.Validation(map[string][]string{"version": {"Send the version you are editing (1 or more)."}})
	}
	if err := validateCardDesign(&design); err != nil {
		return nil, err
	}
	var saved models.CardDesign
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := database.LockForUpdate(databaseTransaction, &saved, "id = ?", design.ID); err != nil {
			if database.IsNotFound(err) {
				return apperrors.NotFound("Card design")
			}
			return err
		}
		if saved.Version != expectedVersion {
			return apperrors.ErrStaleData
		}
		taken, err := sameLookExists(databaseTransaction, design, design.ID)
		if err != nil {
			return err
		}
		if taken {
			return ErrDesignExists
		}
		design.Version, design.CreatedAt, design.UpdatedAt = saved.Version+1, saved.CreatedAt, time.Now().UTC()
		saved = design
		return databaseTransaction.Save(&saved).Error
	})
	if err != nil {
		return nil, err
	}
	return &saved, nil
}

// SetCardDesignEnabled turns a design off (DELETE) or back on (enable),
// with lock + version like every other edit. It bumps the version and
// updated_at and returns the changed row. Turning a design off only stops
// NEW picks; cards already wearing it keep their look.
func (service *Service) SetCardDesignEnabled(requestContext context.Context, designID string, enabled bool, expectedVersion int) (*models.CardDesign, error) {
	if expectedVersion < 1 {
		return nil, apperrors.Validation(map[string][]string{"version": {"Send the version you are changing (1 or more)."}})
	}
	var design models.CardDesign
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := database.LockForUpdate(databaseTransaction, &design, "id = ?", designID); err != nil {
			if database.IsNotFound(err) {
				return apperrors.NotFound("Card design")
			}
			return err
		}
		if design.Version != expectedVersion {
			return apperrors.ErrStaleData
		}
		design.Enabled, design.Version, design.UpdatedAt = enabled, design.Version+1, time.Now().UTC()
		return databaseTransaction.Model(&models.CardDesign{}).Where("id = ?", designID).
			Updates(map[string]any{"enabled": enabled, "version": design.Version, "updated_at": design.UpdatedAt}).Error
	})
	if err != nil {
		return nil, err
	}
	return &design, nil
}

// ─── Virtual cards ───────────────────────────────────────────────────

// CardWithBalance is a card plus its ledger balance.
type CardWithBalance struct {
	Card    models.VirtualCard
	Balance money.Amount
}

// ListCards returns the user's active cards with balances.
func (service *Service) ListCards(requestContext context.Context, userID string) ([]CardWithBalance, error) {
	var cards []models.VirtualCard
	if err := service.database.WithContext(requestContext).Where("user_id = ? AND status = ?", userID, "active").Order("created_at").Find(&cards).Error; err != nil {
		return nil, err
	}
	if len(cards) == 0 {
		return []CardWithBalance{}, nil
	}
	// Load every card's ledger account in one query, then all balances in
	// one batched query, instead of three queries per card.
	cardIDs := make([]string, 0, len(cards))
	for _, card := range cards {
		cardIDs = append(cardIDs, card.ID)
	}
	var accounts []models.LedgerAccount
	if err := service.database.WithContext(requestContext).
		Where("virtual_card_id IN ? AND code = ?", cardIDs, ledger.CodeUserCard).
		Find(&accounts).Error; err != nil {
		return nil, err
	}
	accountBalances, err := service.ledger.BalancesForAccounts(requestContext, accounts)
	if err != nil {
		return nil, err
	}
	balanceOfCard := map[string]money.Amount{} // key: card id + currency
	for _, accountBalance := range accountBalances {
		if accountBalance.Account.VirtualCardID != nil {
			balanceOfCard[*accountBalance.Account.VirtualCardID+":"+accountBalance.Account.AssetCode] = accountBalance.Balance
		}
	}
	result := make([]CardWithBalance, 0, len(cards))
	for _, card := range cards {
		balance, found := balanceOfCard[card.ID+":"+card.Currency]
		if !found {
			balance = money.Zero() // the card account is opened on first funding
		}
		result = append(result, CardWithBalance{Card: card, Balance: balance})
	}
	return result, nil
}

// conversionInto prices "the target account must receive exactly this
// amount of toAsset, paid with fromAsset". Same-asset: no conversion, only a fee.
func (service *Service) conversionInto(requestContext context.Context, transactionType string, fromAsset string, toAsset string, receiveAmount money.Amount, toDecimals int) (converted money.Amount, fee money.Amount, price *rates.Price, err error) {
	if fromAsset == toAsset {
		fee, err = service.rates.FeeFor(service.database.WithContext(requestContext), transactionType, toAsset, receiveAmount)
		return receiveAmount, fee, nil, err
	}
	price, err = service.rates.PriceConversion(requestContext, rates.PriceInput{
		TransactionType: transactionType, FromAsset: fromAsset, ToAsset: toAsset, ReceiveAmount: receiveAmount.Decimal(toDecimals),
	})
	if err != nil {
		return money.Zero(), money.Zero(), nil, err
	}
	return price.Converted, price.Fee, price, nil
}

// CreateCardInput is what the new-card flow sends.
type CreateCardInput struct {
	Label        string
	Network      string
	DesignFinish string
	DesignShade  string
	FirstTopUp   string // USD, at least 5
	PayWith      string
	Pin          string
	Reference    string
}

// CreateCard issues a card, stores its design and loads the first top-up.
func (service *Service) CreateCard(requestContext context.Context, actor shared.Actor, input CreateCardInput) (*CardWithBalance, *models.Transaction, error) {
	if err := service.auth.VerifyPin(requestContext, actor.UserID, input.Pin); err != nil {
		return nil, nil, err
	}
	input.Label = strings.TrimSpace(input.Label)
	if input.Label == "" || len(input.Label) > 18 {
		return nil, nil, apperrors.Validation(map[string][]string{"label": {"Give the card a name (up to 18 characters)."}})
	}
	if input.Network != "visa" && input.Network != "mastercard" {
		return nil, nil, apperrors.Validation(map[string][]string{"network": {"Choose Visa or Mastercard."}})
	}
	design, err := service.ParseFinish(requestContext, input.DesignFinish, input.DesignShade)
	if err != nil {
		return nil, nil, err
	}
	topUp, err := money.ParseDecimal(input.FirstTopUp, 2)
	if err != nil || topUp.Compare(money.FromUnits(500)) < 0 {
		return nil, nil, apperrors.Validation(map[string][]string{"first_top_up": {"The first top-up must be at least $5."}})
	}
	var user models.User
	if err := service.database.WithContext(requestContext).Where("id = ?", actor.UserID).Take(&user).Error; err != nil {
		return nil, nil, err
	}

	converted, fee, price, err := service.conversionInto(requestContext, "card_create", input.PayWith, "USD", topUp, 2)
	if err != nil {
		return nil, nil, err
	}
	cardID := ids.New()
	sourceWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, input.PayWith)
	if err != nil {
		return nil, nil, err
	}
	cardAccount, err := service.ledger.EnsureCardAccount(requestContext, actor.UserID, cardID, "USD")
	if err != nil {
		return nil, nil, err
	}
	issuer, err := service.providers.Cards()
	if err != nil {
		return nil, nil, err
	}
	issued, err := issuer.IssueCard(requestContext, cardID, user.FullName(), input.Network)
	if err != nil {
		return nil, nil, apperrors.New(http.StatusBadGateway, "PROVIDER_FAILED", "The card issuer did not respond. Try again.")
	}

	now := time.Now().UTC()
	card := models.VirtualCard{
		ID: cardID, UserID: actor.UserID, Label: input.Label, Network: input.Network,
		DesignFinish: input.DesignFinish, DesignSettings: shared.JSON(design), HolderName: user.FullName(),
		Currency: "USD", LastFour: issued.LastFour, ExpiryMonth: issued.ExpiryMonth, ExpiryYear: issued.ExpiryYear,
		ProviderCardReference: issued.ProviderCardReference, OnlinePaymentsEnabled: true, MonthlyLimit: "0",
		BillingAddress: shared.JSON(map[string]string{"street": "1 Simulated Way", "city": "Wilmington", "state": "DE", "postal": "19801"}),
		Status:         "active", Version: 1, CreatedAt: now, UpdatedAt: now,
	}
	transaction := &models.Transaction{ID: ids.New(), Reference: input.Reference, UserID: actor.UserID}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		if err := databaseTransaction.Create(&card).Error; err != nil {
			return err
		}
		posting := ledger.ConversionPosting{
			Reference: input.Reference + ":POST", TransactionType: "card_create", Description: "New card " + input.Label,
			CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &transaction.ID,
			SourceAccountID: sourceWallet.ID, TargetAccountID: cardAccount.ID,
			FromAsset: input.PayWith, ToAsset: "USD", Converted: converted, Fee: fee, Received: topUp,
			Rates: service.rates.RatesFor(databaseTransaction, input.PayWith, "USD", "NGN"),
		}
		if price != nil {
			posting.NairaIn, posting.NairaOut = price.NairaIn, price.NairaOut
		}
		postResult, err := service.ledger.PostConversion(databaseTransaction, posting)
		if err != nil {
			return err
		}
		usd, topUpText, feeAsset := "USD", topUp.UnitsString(), input.PayWith
		*transaction = models.Transaction{
			ID: transaction.ID, Reference: transaction.Reference, UserID: actor.UserID,
			Category: "virtual_card", Type: "card_create", Direction: "out", Status: models.TransactionStatusCompleted,
			Title: "New card · " + input.Label, Subtitle: strings.ToUpper(input.Network[:1]) + input.Network[1:] + " •• " + issued.LastFour,
			FromAsset: input.PayWith, FromAmount: converted.Add(fee).UnitsString(), ToAsset: &usd, ToAmount: &topUpText,
			FeeAsset: &feeAsset, FeeAmount: fee.UnitsString(), JournalEntryID: &postResult.Entry.ID, UsdValue: topUp.Decimal(2),
			Metadata: shared.JSON(map[string]any{"card_id": card.ID, "card_op": "created", "network": input.Network}),
		}
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		if price != nil {
			if err := swaps.SaveSwapLegs(databaseTransaction, transaction.ID, nil, price.Route); err != nil {
				return err
			}
		}
		return shared.Notify(databaseTransaction, actor.UserID, "card", "Your card is ready", input.Label+" •• "+issued.LastFour+" has $"+topUp.Display(2), "/cards")
	})
	if err != nil {
		return nil, nil, err
	}
	return &CardWithBalance{Card: card, Balance: topUp}, transaction, nil
}

// MoveCardMoneyInput funds or withdraws a card.
type MoveCardMoneyInput struct {
	CardID    string
	AmountUsd string
	Asset     string // pay with (fund) or receive in (withdraw)
	Pin       string
	Reference string
}

// FundCard loads money onto a card from any wallet.
func (service *Service) FundCard(requestContext context.Context, actor shared.Actor, input MoveCardMoneyInput) (*models.Transaction, error) {
	if err := service.auth.VerifyPin(requestContext, actor.UserID, input.Pin); err != nil {
		return nil, err
	}
	card, cardAccount, err := service.loadCardAccount(requestContext, actor.UserID, input.CardID)
	if err != nil {
		return nil, err
	}
	amount, err := money.ParseDecimal(input.AmountUsd, 2)
	if err != nil || !amount.IsPositive() {
		return nil, apperrors.Validation(map[string][]string{"amount_usd": {"Enter a valid amount."}})
	}
	converted, fee, price, err := service.conversionInto(requestContext, "card_fund", input.Asset, "USD", amount, 2)
	if err != nil {
		return nil, err
	}
	sourceWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, input.Asset)
	if err != nil {
		return nil, err
	}
	return service.postCardMovement(requestContext, actor, card, "card_fund", "funding", "Card top-up · "+card.Label, input.Reference,
		sourceWallet.ID, cardAccount.ID, input.Asset, "USD", converted, fee, amount, price)
}

// WithdrawCard moves money from a card back to a wallet.
func (service *Service) WithdrawCard(requestContext context.Context, actor shared.Actor, input MoveCardMoneyInput) (*models.Transaction, error) {
	if err := service.auth.VerifyPin(requestContext, actor.UserID, input.Pin); err != nil {
		return nil, err
	}
	card, cardAccount, err := service.loadCardAccount(requestContext, actor.UserID, input.CardID)
	if err != nil {
		return nil, err
	}
	amount, err := money.ParseDecimal(input.AmountUsd, 2)
	if err != nil || !amount.IsPositive() {
		return nil, apperrors.Validation(map[string][]string{"amount_usd": {"Enter a valid amount."}})
	}
	targetWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, input.Asset)
	if err != nil {
		return nil, err
	}
	received, price := amount, (*rates.Price)(nil)
	if input.Asset != "USD" {
		price, err = service.rates.PriceConversion(requestContext, rates.PriceInput{TransactionType: "card_withdraw", FromAsset: "USD", ToAsset: input.Asset, PayAmount: amount.Decimal(2)})
		if err != nil {
			return nil, err
		}
		received = price.Received
		return service.postCardMovement(requestContext, actor, card, "card_withdraw", "withdrawal", "Card withdrawal · "+card.Label, input.Reference,
			cardAccount.ID, targetWallet.ID, "USD", input.Asset, price.Converted, price.Fee, received, price)
	}
	return service.postCardMovement(requestContext, actor, card, "card_withdraw", "withdrawal", "Card withdrawal · "+card.Label, input.Reference,
		cardAccount.ID, targetWallet.ID, "USD", "USD", amount, money.Zero(), received, nil)
}

// postCardMovement is the shared "move money to/from a card" transaction.
func (service *Service) postCardMovement(requestContext context.Context, actor shared.Actor, card *models.VirtualCard, transactionType string, cardOperation string, title string, reference string,
	sourceAccountID uint64, targetAccountID uint64, fromAsset string, toAsset string, converted money.Amount, fee money.Amount, received money.Amount, price *rates.Price) (*models.Transaction, error) {
	transaction := &models.Transaction{ID: ids.New(), Reference: reference, UserID: actor.UserID}
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		// Lock the card row: freezing, deleting and moving money on the same card happen one at a time.
		var lockedCard models.VirtualCard
		if err := database.LockForUpdate(databaseTransaction, &lockedCard, "id = ? AND status = ?", card.ID, "active"); err != nil {
			return apperrors.NotFound("Card")
		}
		if lockedCard.Frozen && transactionType == "card_fund" {
			return apperrors.New(http.StatusUnprocessableEntity, "CARD_FROZEN", "Unfreeze the card first.")
		}
		posting := ledger.ConversionPosting{
			Reference: reference + ":POST", TransactionType: transactionType, Description: title,
			CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &transaction.ID,
			SourceAccountID: sourceAccountID, TargetAccountID: targetAccountID,
			FromAsset: fromAsset, ToAsset: toAsset, Converted: converted, Fee: fee, Received: received,
			Rates: service.rates.RatesFor(databaseTransaction, fromAsset, toAsset, "NGN"),
		}
		if price != nil {
			posting.NairaIn, posting.NairaOut = price.NairaIn, price.NairaOut
		}
		postResult, err := service.ledger.PostConversion(databaseTransaction, posting)
		if err != nil {
			return err
		}
		direction := "out"
		if transactionType == "card_withdraw" {
			direction = "in"
		}
		toAssetCopy, receivedText, feeAsset := toAsset, received.UnitsString(), fromAsset
		*transaction = models.Transaction{
			ID: transaction.ID, Reference: reference, UserID: actor.UserID,
			Category: "virtual_card", Type: transactionType, Direction: direction, Status: models.TransactionStatusCompleted,
			Title: title, Subtitle: "•• " + lockedCard.LastFour,
			FromAsset: fromAsset, FromAmount: converted.Add(fee).UnitsString(), ToAsset: &toAssetCopy, ToAmount: &receivedText,
			FeeAsset: &feeAsset, FeeAmount: fee.UnitsString(), JournalEntryID: &postResult.Entry.ID,
			Metadata: shared.JSON(map[string]any{"card_id": lockedCard.ID, "card_op": cardOperation}),
		}
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		if err := shared.NotifyTransaction(databaseTransaction, transaction, "card", title); err != nil {
			return err
		}
		if price != nil {
			return swaps.SaveSwapLegs(databaseTransaction, transaction.ID, nil, price.Route)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return transaction, nil
}

// loadCardAccount loads a user's card and its balance account.
func (service *Service) loadCardAccount(requestContext context.Context, userID string, cardID string) (*models.VirtualCard, *models.LedgerAccount, error) {
	var card models.VirtualCard
	if err := service.database.WithContext(requestContext).Where("id = ? AND user_id = ? AND status = ?", cardID, userID, "active").Take(&card).Error; err != nil {
		return nil, nil, apperrors.NotFound("Card")
	}
	account, err := service.ledger.EnsureCardAccount(requestContext, userID, card.ID, card.Currency)
	return &card, account, err
}

// CardChanges are the settings a user can change on a card.
type CardChanges struct {
	Frozen         *bool
	OnlinePayments *bool
	Label          *string
	DesignFinish   *string
	DesignShade    string
	MonthlyLimit   *string // USD
	InWallet       *bool
	Version        int // version the app last saw (0 = don't check)
}

// UpdateCard changes card settings with the LOCK + VERSION pattern.
func (service *Service) UpdateCard(requestContext context.Context, userID string, cardID string, changes CardChanges) (*models.VirtualCard, error) {
	updates := map[string]any{}
	keptFinish := "" // set when the card re-sends its current finish (checked again under the lock)
	if changes.DesignFinish != nil {
		// A NEW pick must be an enabled design. Re-sending the finish the
		// card already wears (for example with a new shade, or a settings
		// screen that posts every field) is allowed even if that design was
		// turned off since: the card keeps the look it has.
		var current models.VirtualCard
		if err := service.database.WithContext(requestContext).Select("design_finish").
			Where("id = ? AND user_id = ? AND status = ?", cardID, userID, "active").Take(&current).Error; err != nil {
			return nil, apperrors.NotFound("Card")
		}
		var design DesignSettings
		if *changes.DesignFinish == current.DesignFinish {
			parsed, _, ok := splitFinish(*changes.DesignFinish, changes.DesignShade)
			if !ok {
				return nil, apperrors.Validation(map[string][]string{"design_finish": {"That card design is not available."}})
			}
			design, keptFinish = parsed, current.DesignFinish
		} else {
			parsed, err := service.ParseFinish(requestContext, *changes.DesignFinish, changes.DesignShade)
			if err != nil {
				return nil, err
			}
			design = parsed
		}
		updates["design_finish"], updates["design_settings"] = *changes.DesignFinish, shared.JSON(design)
	}
	if changes.Frozen != nil {
		updates["frozen"] = *changes.Frozen
	}
	if changes.OnlinePayments != nil {
		updates["online_payments_enabled"] = *changes.OnlinePayments
	}
	if changes.InWallet != nil {
		updates["in_wallet"] = *changes.InWallet
	}
	if changes.Label != nil {
		label := strings.TrimSpace(*changes.Label)
		if label == "" || len(label) > 18 {
			return nil, apperrors.Validation(map[string][]string{"label": {"Up to 18 characters."}})
		}
		updates["label"] = label
	}
	if changes.MonthlyLimit != nil {
		limit, err := money.ParseDecimal(*changes.MonthlyLimit, 2)
		if err != nil || limit.IsNegative() {
			return nil, apperrors.Validation(map[string][]string{"monthly_limit": {"Enter a valid limit."}})
		}
		updates["monthly_limit"] = limit.UnitsString()
	}

	var card models.VirtualCard
	err := database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if err := database.LockForUpdate(databaseTransaction, &card, "id = ? AND user_id = ? AND status = ?", cardID, userID, "active"); err != nil {
			return apperrors.NotFound("Card")
		}
		if changes.Version != 0 && changes.Version != card.Version {
			return apperrors.ErrStaleData
		}
		// The unchecked "same finish" shortcut only holds if nobody changed
		// the card's design between our first read and this lock.
		if keptFinish != "" && card.DesignFinish != keptFinish {
			return apperrors.ErrStaleData
		}
		updates["version"], updates["updated_at"] = card.Version+1, time.Now().UTC()
		if err := databaseTransaction.Model(&models.VirtualCard{}).Where("id = ?", cardID).Updates(updates).Error; err != nil {
			return err
		}
		return databaseTransaction.Where("id = ?", cardID).Take(&card).Error
	})
	return &card, err
}

// RevealCard returns the full number and CVV. Needs the PIN; the result is
// never stored or logged.
func (service *Service) RevealCard(requestContext context.Context, userID string, cardID string, pin string) (map[string]string, error) {
	if err := service.auth.VerifyPin(requestContext, userID, pin); err != nil {
		return nil, err
	}
	card, _, err := service.loadCardAccount(requestContext, userID, cardID)
	if err != nil {
		return nil, err
	}
	issuer, err := service.providers.Cards()
	if err != nil {
		return nil, err
	}
	number, cvv, err := issuer.RevealCard(requestContext, card.ProviderCardReference)
	if err != nil {
		return nil, err
	}
	return map[string]string{"number": number, "cvv": cvv, "expiry": twoDigits(card.ExpiryMonth) + "/" + twoDigits(card.ExpiryYear%100)}, nil
}

// DeleteCard empties the card into the USD wallet and closes it.
func (service *Service) DeleteCard(requestContext context.Context, actor shared.Actor, cardID string, pin string, reference string) (*models.Transaction, error) {
	if err := service.auth.VerifyPin(requestContext, actor.UserID, pin); err != nil {
		return nil, err
	}
	card, cardAccount, err := service.loadCardAccount(requestContext, actor.UserID, cardID)
	if err != nil {
		return nil, err
	}
	usdWallet, err := service.ledger.EnsureUserAccount(requestContext, actor.UserID, ledger.CodeUserPrimary, "USD")
	if err != nil {
		return nil, err
	}
	transaction := &models.Transaction{ID: ids.New(), Reference: reference, UserID: actor.UserID}
	err = database.WithTransaction(requestContext, service.database, func(databaseTransaction *gorm.DB) error {
		if _, err := shared.LockActor(databaseTransaction, actor); err != nil {
			return err
		}
		var lockedCard models.VirtualCard
		if err := database.LockForUpdate(databaseTransaction, &lockedCard, "id = ? AND status = ?", card.ID, "active"); err != nil {
			return apperrors.NotFound("Card")
		}
		balance, err := service.ledger.BalanceInTransaction(databaseTransaction, cardAccount.ID)
		if err != nil {
			return err
		}
		var journalEntryID *string
		if balance.IsPositive() {
			result, err := service.ledger.Post(databaseTransaction, ledger.Entry{
				Reference: reference + ":POST", TransactionType: "card_delete", Description: "Card closed, balance returned",
				CreatedBy: "user:" + actor.UserID, BusinessTransactionID: &transaction.ID,
				Legs: ledger.MoveLegs(cardAccount.ID, usdWallet.ID, balance, "Card closed"),
			})
			if err != nil {
				return err
			}
			journalEntryID = &result.Entry.ID
		}
		now := time.Now().UTC()
		if err := databaseTransaction.Model(&models.VirtualCard{}).Where("id = ?", card.ID).
			Updates(map[string]any{"status": "deleted", "deleted_at": now, "updated_at": now, "version": lockedCard.Version + 1}).Error; err != nil {
			return err
		}
		*transaction = models.Transaction{
			ID: transaction.ID, Reference: reference, UserID: actor.UserID,
			Category: "virtual_card", Type: "card_delete", Direction: "in", Status: models.TransactionStatusCompleted,
			Title: "Card deleted · " + lockedCard.Label, Subtitle: "Balance returned to USD",
			FromAsset: "USD", FromAmount: balance.UnitsString(), JournalEntryID: journalEntryID,
			Metadata: shared.JSON(map[string]any{"card_id": lockedCard.ID, "card_op": "refund"}),
		}
		if err := shared.RecordTransaction(databaseTransaction, transaction); err != nil {
			return err
		}
		return shared.NotifyTransaction(databaseTransaction, transaction, "card", "Card deleted")
	})
	if err != nil {
		return nil, err
	}
	return transaction, nil
}

// twoDigits formats 7 as "07".
func twoDigits(number int) string {
	return fmt.Sprintf("%02d", number)
}
