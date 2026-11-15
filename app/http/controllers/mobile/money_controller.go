package mobile

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/events"
	"github.com/tribe-africa/tribe-backend/app/http/middleware"
	"github.com/tribe-africa/tribe-backend/app/http/requests"
	"github.com/tribe-africa/tribe-backend/app/http/resources"
	"github.com/tribe-africa/tribe-backend/app/http/responses"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/account"
	"github.com/tribe-africa/tribe-backend/app/services/rates"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/app/services/swaps"
	"github.com/tribe-africa/tribe-backend/app/services/transfers"
)

// MoneyController handles wallets, rates, quotes, swaps, sends and the activity list.
type MoneyController struct {
	application *bootstrap.App
}

// NewMoneyController creates the controller.
func NewMoneyController(application *bootstrap.App) *MoneyController {
	return &MoneyController{application: application}
}

// decimals returns asset -> decimal places.
func (controller *MoneyController) decimals(httpContext *gin.Context) map[string]int {
	return shared.AssetDecimals(controller.application.Database.WithContext(httpContext.Request.Context()))
}

// Assets  GET /assets  -> every supported currency and coin.
func (controller *MoneyController) Assets(httpContext *gin.Context) {
	var assets []models.Asset
	if err := controller.application.Database.WithContext(httpContext.Request.Context()).Where("enabled = ?", true).Order("display_order").Find(&assets).Error; err != nil {
		responses.Error(httpContext, err)
		return
	}
	list := make([]gin.H, 0, len(assets))
	for _, asset := range assets {
		list = append(list, gin.H{"code": asset.Code, "name": asset.Name, "kind": asset.Kind, "symbol": asset.Symbol, "decimal_places": asset.DecimalPlaces})
	}
	responses.OK(httpContext, "Assets loaded.", list)
}

// Wallets  GET /wallets  -> balances from the ledger ({balances, opened}).
func (controller *MoneyController) Wallets(httpContext *gin.Context) {
	result, err := controller.application.Wallets.Wallets(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Wallets loaded.", result)
}

// OpenWallet  POST /wallets/:code/open
func (controller *MoneyController) OpenWallet(httpContext *gin.Context) {
	if err := controller.application.Wallets.OpenWallet(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("code")); err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.Wallets(httpContext)
}

// ReceiveDetails  GET /wallets/:code/receive
func (controller *MoneyController) ReceiveDetails(httpContext *gin.Context) {
	details, err := controller.application.Wallets.ReceiveDetails(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("code"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Receive details loaded.", details)
}

// Services  GET /services  -> every service with its on/off switch and, when
// it is off, the admin's message, so the app can grey it out.
func (controller *MoneyController) Services(httpContext *gin.Context) {
	statuses, err := shared.ServiceStatuses(controller.application.Database.WithContext(httpContext.Request.Context()))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Services loaded.", statuses)
}

// Rates  GET /rates  -> all rates now (first load, or when the stream is down).
func (controller *MoneyController) Rates(httpContext *gin.Context) {
	event, err := controller.application.Rates.LatestEvent(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Rates loaded.", event)
}

// RatesStream  GET /rates/stream  -> LIVE rates over Server-Sent Events (SSE).
//
// SSE is a long-lived HTTP response that the server keeps writing to:
//
//	event: rates.snapshot          <- sent once, straight away
//	data: {"version":12,"rates":{...}}
//
//	event: rates.updated           <- sent every time the collector publishes
//	data: {"version":13,"rates":{...}}
//
//	: heartbeat                    <- every 15 s so proxies keep the line open
//
// The phone opens this while it is on a screen that shows rates.
func (controller *MoneyController) RatesStream(httpContext *gin.Context) {
	writer := httpContext.Writer
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no") // tell nginx not to buffer

	// The snapshot: the hub's newest event when it has one (kept in memory, so no Redis or
	// database read per connection); otherwise, right after a restart, read it once.
	if latest := controller.application.RateHub.Latest(); latest != nil {
		writer.Write(events.RateFrame("rates.snapshot", latest))
		writer.Flush()
	} else if latest, err := controller.application.Rates.LatestEvent(httpContext.Request.Context()); err == nil {
		if encoded, err := json.Marshal(latest); err == nil {
			writer.Write(events.RateFrame("rates.snapshot", encoded))
			writer.Flush()
		} else {
			slog.Warn("could not encode the rate snapshot", "error", err)
		}
	}

	connection := controller.application.RateHub.Subscribe()
	defer controller.application.RateHub.Unsubscribe(connection)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-httpContext.Request.Context().Done():
			return // the phone left the screen or lost signal
		case frame := <-connection:
			// already a complete "rates.updated" SSE frame, built once by the hub for every phone
			writer.Write(frame)
			writer.Flush()
		case <-heartbeat.C:
			fmt.Fprint(writer, ": heartbeat\n\n")
			writer.Flush()
		}
	}
}

// CreateQuote  POST /quotes  -> a price locked for 30 seconds.
// 503 SERVICE_DISABLED when the swap or send it is for is switched off.
func (controller *MoneyController) CreateQuote(httpContext *gin.Context) {
	var input requests.QuoteRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	quote, price, err := controller.application.Rates.CreateQuote(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, rates.PriceInput{
		TransactionType: input.TransactionType, FromAsset: input.FromAsset, ToAsset: input.ToAsset,
		PayAmount: input.Amount, ReceiveAmount: input.ReceiveAmount, UsdAmount: input.UsdAmount,
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	// The app is shown ONE step, from the asset paid to the asset received.
	// price.Route (which may go through Naira) is saved on the quote for the
	// swap itself and for the admin panel, and never sent to the phone.
	decimals := controller.decimals(httpContext)
	responses.Created(httpContext, "Here's your price.", resources.QuoteResource{
		ID: quote.ID, FromAsset: quote.FromAsset, ToAsset: quote.ToAsset,
		FromAmount: price.TotalPaid.Decimal(decimals[quote.FromAsset]), Fee: price.Fee.Decimal(decimals[quote.FromAsset]),
		ToAmount: price.Received.Decimal(decimals[quote.ToAsset]), Rate: price.Rate.FloatString(8),
		Route: []resources.RouteLegResource{{
			Step: 1, FromAsset: quote.FromAsset, ToAsset: quote.ToAsset,
			FromAmount: price.Converted.Decimal(decimals[quote.FromAsset]), ToAmount: price.Received.Decimal(decimals[quote.ToAsset]), Rate: quote.Rate,
		}},
		ExpiresAt: quote.ExpiresAt,
	})
}

// Swap  POST /swaps
func (controller *MoneyController) Swap(httpContext *gin.Context) {
	var input requests.SwapRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	transaction, err := controller.application.Swaps.Swap(httpContext.Request.Context(), middleware.CurrentActor(httpContext), swaps.SwapInput{QuoteID: input.QuoteID, Pin: input.Pin, Reference: middleware.BusinessReference(httpContext, "SWP")})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Swap complete.", resources.Transaction(*transaction, controller.decimals(httpContext)))
}

// LookupRecipient  GET /recipients/lookup?tag=ada
func (controller *MoneyController) LookupRecipient(httpContext *gin.Context) {
	user, err := controller.application.Transfers.FindRecipient(httpContext.Request.Context(), httpContext.Query("tag"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Found.", gin.H{"name": user.FullName(), "tag": user.Tag})
}

// Channels  GET /channels?currency=GBP
func (controller *MoneyController) Channels(httpContext *gin.Context) {
	currency := httpContext.Query("currency")
	list := []transfers.Channel{}
	for _, channel := range transfers.Channels() {
		for _, code := range channel.Currencies {
			if currency == "" || code == currency {
				list = append(list, channel)
				break
			}
		}
	}
	responses.OK(httpContext, "Channels loaded.", list)
}

// Send  POST /sends
func (controller *MoneyController) Send(httpContext *gin.Context) {
	var input requests.SendRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	transaction, err := controller.application.Transfers.Send(httpContext.Request.Context(), middleware.CurrentActor(httpContext), transfers.SendInput{
		RecipientKind: input.RecipientKind, Handle: input.Handle, RecipientName: input.RecipientName,
		AssetCode: input.Asset, Amount: input.Amount, QuoteID: input.QuoteID, ChannelID: input.ChannelID,
		Network: input.Network, Country: input.Country, Details: input.Details, SaveBeneficiary: input.SaveBeneficiary,
		Pin: input.Pin, Reference: middleware.BusinessReference(httpContext, "SND"),
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	message := "Money sent."
	if transaction.Status == models.TransactionStatusPending {
		message = "Sending. We'll let you know when it arrives."
	}
	responses.Created(httpContext, message, resources.Transaction(*transaction, controller.decimals(httpContext)))
}

// Beneficiaries  GET /beneficiaries
func (controller *MoneyController) Beneficiaries(httpContext *gin.Context) {
	list, err := controller.application.Transfers.ListBeneficiaries(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Beneficiaries loaded.", list)
}

// SaveBeneficiary  POST /beneficiaries  -> save a recipient without sending
func (controller *MoneyController) SaveBeneficiary(httpContext *gin.Context) {
	var input requests.BeneficiaryRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	beneficiary, err := controller.application.Transfers.SaveBeneficiary(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, transfers.SaveBeneficiaryInput{
		Kind: input.Kind, Name: input.Name, Handle: input.Handle, AssetCode: input.Asset,
		ChannelID: input.ChannelID, Network: input.Network, Country: input.Country, Details: input.Details,
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Recipient saved.", beneficiary)
}

// DeleteBeneficiary  DELETE /beneficiaries/:id
func (controller *MoneyController) DeleteBeneficiary(httpContext *gin.Context) {
	if err := controller.application.Transfers.DeleteBeneficiary(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("id")); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Removed.", nil)
}

// Transactions  GET /transactions?category=&type=&asset=&card_id=&cursor=
func (controller *MoneyController) Transactions(httpContext *gin.Context) {
	transactions, nextCursor, err := controller.application.Account.Transactions(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, account.TransactionFilters{
		Category: httpContext.Query("category"), Type: httpContext.Query("type"), Asset: httpContext.Query("asset"),
		CardID: httpContext.Query("card_id"), Cursor: httpContext.Query("cursor"),
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OKWithMeta(httpContext, "Transactions loaded.", resources.Transactions(transactions, controller.decimals(httpContext)), responses.Pagination{NextCursor: nextCursor})
}

// Transaction  GET /transactions/:id  -> one transaction with its route (receipt).
// The route is the conversion as the customer did it, in one step (see resources.CustomerRoute).
func (controller *MoneyController) Transaction(httpContext *gin.Context) {
	transaction, legs, quotedRate, err := controller.application.Account.Transaction(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("id"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	decimals := controller.decimals(httpContext)
	responses.OK(httpContext, "Transaction loaded.", gin.H{"transaction": resources.Transaction(*transaction, decimals), "route": resources.CustomerRoute(legs, quotedRate, decimals)})
}
