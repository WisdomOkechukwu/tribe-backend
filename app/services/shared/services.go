package shared

import (
	"net/http"
	"strings"

	"gorm.io/gorm"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/models"
)

// ─── Service switches ────────────────────────────────────────────────
//
// An admin can switch any customer-facing money service off (admin panel →
// Services). While a service is off the API refuses it with 503
// SERVICE_DISABLED and the app greys it out (GET /services).
//
// Where the check lives:
//   - when the route alone says which service it is (POST /swaps), the
//     middleware.ServiceEnabled middleware on that route;
//   - when it depends on the request body (which biller, which kind of
//     recipient, what a quote is for), RequireService inside the service
//     method, as soon as the input is understood and BEFORE any money moves.
//
// Ways OUT of a half-finished state are never switched off: cancelling an
// unredeemed cashpoint, deleting a card (its balance goes back to the
// wallet), and the jobs and webhooks that settle or refund a payment.

// The service codes. Every customer money action belongs to exactly one.
// They are fixed here because code checks them; the `services` table only
// stores each one's switch.
const (
	ServiceSwap             = "swap"              // POST /swaps and swap quotes
	ServiceSendTribe        = "send_tribe"        // POST /sends to a Tribe @tag
	ServiceSendBank         = "send_bank"         // POST /sends to a bank account, paid out in Naira
	ServiceSendGlobal       = "send_global"       // POST /sends to a bank account or mobile money in any other currency
	ServiceSendCrypto       = "send_crypto"       // POST /sends to an outside crypto wallet
	ServiceBillAirtime      = "bill_airtime"      // POST /bills for an airtime biller
	ServiceBillData         = "bill_data"         // POST /bills for a data biller
	ServiceBillElectricity  = "bill_electricity"  // POST /bills for an electricity biller
	ServiceBillTv           = "bill_tv"           // POST /bills for a TV biller
	ServiceRecurringBills   = "recurring_bills"   // new or changed bill schedules, and the 07:00 scheduled payments
	ServiceGiftCardBuy      = "gift_card_buy"     // POST /gift-cards/buy
	ServiceGiftCardSell     = "gift_card_sell"    // POST /gift-cards/sell
	ServiceVirtualCards     = "virtual_cards"     // POST /cards, /cards/{id}/fund and /cards/{id}/withdraw
	ServiceCashpointCreate  = "cashpoint_create"  // POST /cashpoints
	ServiceCashpointRedeem  = "cashpoint_redeem"  // POST /cashpoints/redeem
	ServiceReferralWithdraw = "referral_withdraw" // POST /referrals/withdraw
	ServiceWalletOpen       = "wallet_open"       // POST /wallets/{code}/open
	ServiceReceive          = "receive"           // GET /wallets/{code}/receive
)

// ServiceDefinition is one entry of the fixed list of services.
type ServiceDefinition struct {
	Code        string
	Name        string
	Description string
}

// ServiceCatalog is every service, in the order the admin panel and the app
// list them.
//
// Adding a service takes three steps: a constant above, an entry here, and
// a NEW migration that inserts its row into `services` (see
// 00014_create_services.sql; the seeder also inserts missing rows, and
// services_test.go fails when a code has no migration). Until its row
// exists a service counts as enabled.
var ServiceCatalog = []ServiceDefinition{
	{ServiceSwap, "Swap", "Convert money between two of your own wallets."},
	{ServiceSendTribe, "Send to a Tribe tag", "Instant transfers to another Tribe customer (@tag)."},
	{ServiceSendBank, "Send to a Nigerian bank", "Naira payouts to Nigerian bank accounts."},
	{ServiceSendGlobal, "Global send", "Payouts to bank accounts and mobile money outside Nigeria."},
	{ServiceSendCrypto, "Crypto send", "Withdrawals to an outside crypto wallet address."},
	{ServiceBillAirtime, "Airtime", "Airtime top-ups."},
	{ServiceBillData, "Data bundles", "Mobile data bundles."},
	{ServiceBillElectricity, "Electricity", "Electricity tokens and meter payments."},
	{ServiceBillTv, "TV subscriptions", "Cable and satellite TV subscriptions."},
	{ServiceRecurringBills, "Recurring bills", "Creating bill schedules, and the scheduled payments themselves."},
	{ServiceGiftCardBuy, "Buy gift cards", "Buying gift cards with any wallet."},
	{ServiceGiftCardSell, "Sell gift cards", "Selling a gift card to Tribe for Naira."},
	{ServiceVirtualCards, "Virtual cards", "Creating, funding and withdrawing from virtual cards."},
	{ServiceCashpointCreate, "Create cashpoints", "Creating a cashpoint code from the Naira wallet."},
	{ServiceCashpointRedeem, "Redeem cashpoints", "Redeeming a cashpoint code into the Naira wallet."},
	{ServiceReferralWithdraw, "Referral withdrawals", "Moving referral earnings into the Naira wallet."},
	{ServiceWalletOpen, "Open a currency wallet", "Opening a wallet in a new currency or coin."},
	{ServiceReceive, "Receive and add money", "Account numbers and addresses for paying into a wallet."},
}

// IsService is true when the code is in the catalog.
func IsService(code string) bool {
	for _, definition := range ServiceCatalog {
		if definition.Code == code {
			return true
		}
	}
	return false
}

// CodeServiceDisabled is the error code of a service an admin switched off.
// The app uses it to show the admin's message instead of a generic error.
const CodeServiceDisabled = "SERVICE_DISABLED"

// DefaultServiceDisabledMessage is shown when the admin switched a service
// off without writing a message of their own.
const DefaultServiceDisabledMessage = "This service is not available right now. Please try again later."

// serviceSwitches returns the stored switches by code. They come from the
// shared configuration copy (no query), or from MySQL when no cache is
// installed, exactly like LoadAsset.
func serviceSwitches(query *gorm.DB) (map[string]models.Service, error) {
	if snapshot := configSnapshot(query); snapshot != nil {
		return snapshot.Services, nil // shared and read-only: never change it
	}
	var rows []models.Service
	if err := query.Find(&rows).Error; err != nil {
		return nil, err
	}
	switches := make(map[string]models.Service, len(rows))
	for _, row := range rows {
		switches[row.Code] = row
	}
	return switches, nil
}

// serviceDisabledError builds the 503 the customer gets for a switched-off
// service, carrying the admin's message (or the default one).
func serviceDisabledError(serviceSwitch models.Service) error {
	return apperrors.New(http.StatusServiceUnavailable, CodeServiceDisabled, disabledMessageOf(serviceSwitch))
}

// disabledMessageOf returns the admin's message, or the default when none is set.
func disabledMessageOf(serviceSwitch models.Service) string {
	if serviceSwitch.DisabledMessage != nil && strings.TrimSpace(*serviceSwitch.DisabledMessage) != "" {
		return strings.TrimSpace(*serviceSwitch.DisabledMessage)
	}
	return DefaultServiceDisabledMessage
}

// RequireService returns nil when the service is switched on, and a 503
// SERVICE_DISABLED error with the admin's message when it is off. Call it
// before any money moves. It reads the shared configuration copy, so it
// costs no query. A service with no row yet counts as enabled.
func RequireService(query *gorm.DB, code string) error {
	switches, err := serviceSwitches(query)
	if err != nil {
		return err
	}
	if serviceSwitch, found := switches[code]; found && !serviceSwitch.Enabled {
		return serviceDisabledError(serviceSwitch)
	}
	return nil
}

// RequireAnyService returns nil when AT LEAST ONE of the services is on,
// and the first one's SERVICE_DISABLED error when all are off. A quote for
// a send uses it: the quote does not say who the money is for yet, so it is
// refused only when no kind of send could use it.
func RequireAnyService(query *gorm.DB, codes ...string) error {
	var firstError error
	for _, code := range codes {
		err := RequireService(query, code)
		if err == nil {
			return nil
		}
		if firstError == nil {
			firstError = err
		}
	}
	return firstError
}

// ServiceStatus is what the app is told about one service, so it can grey
// out what is switched off. Message is empty while the service is on.
type ServiceStatus struct {
	Code    string `json:"code"`
	Enabled bool   `json:"enabled"`
	Message string `json:"message"`
}

// ServiceStatuses returns every service in catalog order with its switch.
func ServiceStatuses(query *gorm.DB) ([]ServiceStatus, error) {
	switches, err := serviceSwitches(query)
	if err != nil {
		return nil, err
	}
	statuses := make([]ServiceStatus, 0, len(ServiceCatalog))
	for _, definition := range ServiceCatalog {
		status := ServiceStatus{Code: definition.Code, Enabled: true}
		if serviceSwitch, found := switches[definition.Code]; found && !serviceSwitch.Enabled {
			status.Enabled, status.Message = false, disabledMessageOf(serviceSwitch)
		}
		statuses = append(statuses, status)
	}
	return statuses, nil
}

// SendServiceFor returns the service a send belongs to:
//
//	tribe  -> send_tribe   (another Tribe customer)
//	wallet -> send_crypto  (an outside crypto wallet)
//	bank   -> send_bank when the payout is in Naira, send_global otherwise
//
// payoutAssetCode is the currency that ARRIVES (the quote's to-asset on a
// cross-currency send).
func SendServiceFor(recipientKind string, payoutAssetCode string) string {
	switch recipientKind {
	case "tribe":
		return ServiceSendTribe
	case "wallet":
		return ServiceSendCrypto
	}
	if payoutAssetCode == "NGN" {
		return ServiceSendBank
	}
	return ServiceSendGlobal
}

// billServices maps a biller's category (billers.category) to its service.
var billServices = map[string]string{
	"airtime":     ServiceBillAirtime,
	"data":        ServiceBillData,
	"electricity": ServiceBillElectricity,
	"tv":          ServiceBillTv,
}

// RequireBillService checks the service of a biller's category (airtime,
// data, electricity or tv). A category with no service is refused too:
// every money action must have a switch, so a new biller category needs a
// service code before it can take payments.
func RequireBillService(query *gorm.DB, billerCategory string) error {
	code, found := billServices[billerCategory]
	if !found {
		return apperrors.New(http.StatusServiceUnavailable, CodeServiceDisabled, DefaultServiceDisabledMessage)
	}
	return RequireService(query, code)
}
