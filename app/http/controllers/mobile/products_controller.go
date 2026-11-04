package mobile

import (
	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/middleware"
	"github.com/tribe-africa/tribe-backend/app/http/requests"
	"github.com/tribe-africa/tribe-backend/app/http/resources"
	"github.com/tribe-africa/tribe-backend/app/http/responses"
	"github.com/tribe-africa/tribe-backend/app/jobs"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/products"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
	"github.com/tribe-africa/tribe-backend/pkg/money"
)

// ProductsController handles bills, gift cards, virtual cards, cashpoints,
// referrals, the inbox, support tickets and statements.
type ProductsController struct {
	application *bootstrap.App
}

// NewProductsController creates the controller.
func NewProductsController(application *bootstrap.App) *ProductsController {
	return &ProductsController{application: application}
}

// decimals returns asset -> decimal places.
func (controller *ProductsController) decimals(httpContext *gin.Context) map[string]int {
	return shared.AssetDecimals(controller.application.Database.WithContext(httpContext.Request.Context()))
}

// ─── Bills ───────────────────────────────────────────────────────────

// Billers  GET /billers
func (controller *ProductsController) Billers(httpContext *gin.Context) {
	billers, err := controller.application.Products.ListBillers(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Billers loaded.", billers)
}

// VerifyBillCustomer  POST /bills/verify-customer
func (controller *ProductsController) VerifyBillCustomer(httpContext *gin.Context) {
	var input requests.VerifyBillCustomerRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	name, err := controller.application.Products.VerifyBillCustomer(httpContext.Request.Context(), input.BillerID, input.Target)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Customer found.", gin.H{"customer_name": name})
}

// PayBill  POST /bills
func (controller *ProductsController) PayBill(httpContext *gin.Context) {
	var input requests.PayBillRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	transaction, err := controller.application.Products.PayBill(httpContext.Request.Context(), middleware.CurrentActor(httpContext), products.PayBillInput{
		BillerID: input.BillerID, Target: input.Target, PlanID: input.PlanID, AmountNgn: input.AmountNgn,
		PayWith: input.PayWith, Pin: input.Pin, Reference: middleware.BusinessReference(httpContext, "BIL"),
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	message := "Bill paid."
	if transaction.Status == models.TransactionStatusPending {
		message = "Payment sent to the biller. We'll let you know when it's confirmed."
	}
	responses.Created(httpContext, message, resources.Transaction(*transaction, controller.decimals(httpContext)))
}

// VaultTokens  GET /vault/tokens
func (controller *ProductsController) VaultTokens(httpContext *gin.Context) {
	tokens, err := controller.application.Products.ListVaultTokens(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	list := make([]gin.H, 0, len(tokens))
	for _, token := range tokens {
		amount, _ := money.ParseUnits(token.AmountNgn)
		list = append(list, gin.H{"id": token.ID, "provider": token.Provider, "meter": token.Meter, "amount": amount.Decimal(2),
			"kwh": token.Kwh, "token": token.Token, "paid_with": token.PaidWith, "loaded_on": token.LoadedOn, "bought": token.BoughtAt})
	}
	responses.OK(httpContext, "Tokens loaded.", list)
}

// UpdateVaultToken  PATCH /vault/tokens/:id
func (controller *ProductsController) UpdateVaultToken(httpContext *gin.Context) {
	var input requests.VaultTokenRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	if err := controller.application.Products.MarkVaultTokenLoaded(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("id"), input.Loaded); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Saved.", nil)
}

// RecurringBills  GET /recurring-bills
func (controller *ProductsController) RecurringBills(httpContext *gin.Context) {
	schedules, err := controller.application.Products.ListRecurring(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Schedules loaded.", schedules)
}

// CreateRecurringBill  POST /recurring-bills
func (controller *ProductsController) CreateRecurringBill(httpContext *gin.Context) {
	var input requests.RecurringBillRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	schedule, err := controller.application.Products.CreateRecurring(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, input.Pin, recurringInput(input))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Scheduled.", schedule)
}

// UpdateRecurringBill  PATCH /recurring-bills/:id
func (controller *ProductsController) UpdateRecurringBill(httpContext *gin.Context) {
	var input requests.RecurringBillRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	schedule, err := controller.application.Products.UpdateRecurring(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("id"), input.Pin, recurringInput(input))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Saved.", schedule)
}

// DeleteRecurringBill  DELETE /recurring-bills/:id
func (controller *ProductsController) DeleteRecurringBill(httpContext *gin.Context) {
	if err := controller.application.Products.DeleteRecurring(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("id")); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Removed.", nil)
}

// recurringInput converts the request.
func recurringInput(input requests.RecurringBillRequest) products.RecurringInput {
	return products.RecurringInput{BillerID: input.BillerID, Target: input.Target, PlanID: input.PlanID, AmountNgn: input.AmountNgn,
		PayWith: input.PayWith, Frequency: input.Frequency, StartDate: input.StartDate, Active: input.Active, Version: input.Version}
}

// ─── Gift cards ──────────────────────────────────────────────────────

// GiftCardBrands  GET /gift-cards/brands
func (controller *ProductsController) GiftCardBrands(httpContext *gin.Context) {
	brands, err := controller.application.Products.ListGiftCardBrands(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Brands loaded.", brands)
}

// BuyGiftCard  POST /gift-cards/buy
func (controller *ProductsController) BuyGiftCard(httpContext *gin.Context) {
	var input requests.GiftCardRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	transaction, code, err := controller.application.Products.BuyGiftCard(httpContext.Request.Context(), middleware.CurrentActor(httpContext), products.GiftCardInput{
		BrandID: input.BrandID, FaceUsd: input.FaceUsd, Asset: input.Asset, Pin: input.Pin, Reference: middleware.BusinessReference(httpContext, "GFB"),
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	message := "Gift card bought."
	if transaction.Status == models.TransactionStatusPending {
		message = "Gift card on its way. We'll let you know when it's ready."
	}
	httpContext.Header("Cache-Control", "no-store")
	responses.Created(httpContext, message, gin.H{"transaction": resources.Transaction(*transaction, controller.decimals(httpContext)), "code": code})
}

// RevealGiftCard  POST /gift-cards/:id/reveal  (needs the PIN; :id is the purchase's transaction id)
// Shows the code of a bought gift card, for purchases the provider finished
// after the buy screen had closed.
func (controller *ProductsController) RevealGiftCard(httpContext *gin.Context) {
	var input requests.PinOnlyRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	code, err := controller.application.Products.RevealGiftCardCode(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("id"), input.Pin)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	httpContext.Header("Cache-Control", "no-store")
	responses.OK(httpContext, "Gift card code.", gin.H{"code": code})
}

// SellGiftCard  POST /gift-cards/sell
func (controller *ProductsController) SellGiftCard(httpContext *gin.Context) {
	var input requests.GiftCardRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	transaction, err := controller.application.Products.SellGiftCard(httpContext.Request.Context(), middleware.CurrentActor(httpContext), products.GiftCardInput{
		BrandID: input.BrandID, FaceUsd: input.FaceUsd, Asset: input.Asset, Code: input.Code, Pin: input.Pin, Reference: middleware.BusinessReference(httpContext, "GFS"),
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Gift card sold.", resources.Transaction(*transaction, controller.decimals(httpContext)))
}

// ─── Virtual cards ───────────────────────────────────────────────────

// CardDesigns  GET /card-designs  -> the stored customisation options.
func (controller *ProductsController) CardDesigns(httpContext *gin.Context) {
	catalog, err := controller.application.Products.ListCardDesigns(httpContext.Request.Context(), false)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Card designs loaded.", catalog)
}

// Cards  GET /cards
func (controller *ProductsController) Cards(httpContext *gin.Context) {
	cards, err := controller.application.Products.ListCards(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	list := make([]resources.CardResource, 0, len(cards))
	for _, card := range cards {
		list = append(list, resources.Card(card.Card, card.Balance))
	}
	responses.OK(httpContext, "Cards loaded.", list)
}

// CreateCard  POST /cards
func (controller *ProductsController) CreateCard(httpContext *gin.Context) {
	var input requests.CreateCardRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	card, transaction, err := controller.application.Products.CreateCard(httpContext.Request.Context(), middleware.CurrentActor(httpContext), products.CreateCardInput{
		Label: input.Label, Network: input.Network, DesignFinish: input.DesignFinish, DesignShade: input.DesignShade,
		FirstTopUp: input.FirstTopUp, PayWith: input.PayWith, Pin: input.Pin, Reference: middleware.BusinessReference(httpContext, "CRD"),
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Your card is ready.", gin.H{"card": resources.Card(card.Card, card.Balance), "transaction": resources.Transaction(*transaction, controller.decimals(httpContext))})
}

// FundCard  POST /cards/:id/fund
func (controller *ProductsController) FundCard(httpContext *gin.Context) {
	controller.moveCardMoney(httpContext, true)
}

// WithdrawCard  POST /cards/:id/withdraw
func (controller *ProductsController) WithdrawCard(httpContext *gin.Context) {
	controller.moveCardMoney(httpContext, false)
}

// moveCardMoney is shared by fund and withdraw.
func (controller *ProductsController) moveCardMoney(httpContext *gin.Context, isFunding bool) {
	var input requests.CardMoneyRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	cardInput := products.MoveCardMoneyInput{CardID: httpContext.Param("id"), AmountUsd: input.AmountUsd, Asset: input.Asset, Pin: input.Pin}
	var err error
	var transactionResult any
	if isFunding {
		cardInput.Reference = middleware.BusinessReference(httpContext, "CFD")
		transaction, fundError := controller.application.Products.FundCard(httpContext.Request.Context(), middleware.CurrentActor(httpContext), cardInput)
		err = fundError
		if transaction != nil {
			transactionResult = resources.Transaction(*transaction, controller.decimals(httpContext))
		}
	} else {
		cardInput.Reference = middleware.BusinessReference(httpContext, "CWD")
		transaction, withdrawError := controller.application.Products.WithdrawCard(httpContext.Request.Context(), middleware.CurrentActor(httpContext), cardInput)
		err = withdrawError
		if transaction != nil {
			transactionResult = resources.Transaction(*transaction, controller.decimals(httpContext))
		}
	}
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Done.", transactionResult)
}

// SetCardFrozen handles POST /cards/:id/freeze (true) and POST /cards/:id/unfreeze (false).
func (controller *ProductsController) SetCardFrozen(frozen bool) gin.HandlerFunc {
	return func(httpContext *gin.Context) {
		card, err := controller.application.Products.UpdateCard(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("id"), products.CardChanges{Frozen: &frozen})
		if err != nil {
			responses.Error(httpContext, err)
			return
		}
		responses.OK(httpContext, "Saved.", resources.Card(*card, money.Zero()))
	}
}

// UpdateCard  PATCH /cards/:id  (design, label, limit, online payments, wallet)
func (controller *ProductsController) UpdateCard(httpContext *gin.Context) {
	var input requests.UpdateCardRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	card, err := controller.application.Products.UpdateCard(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("id"), products.CardChanges{
		Label: input.Label, DesignFinish: input.DesignFinish, DesignShade: input.DesignShade, MonthlyLimit: input.MonthlyLimit,
		OnlinePayments: input.OnlinePayments, InWallet: input.InWallet, Version: input.Version,
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Saved.", resources.Card(*card, money.Zero()))
}

// ChangeCardDesign  PATCH /cards/:id/design  -> only the look (finish + shade)
func (controller *ProductsController) ChangeCardDesign(httpContext *gin.Context) {
	var input requests.CardDesignChangeRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.saveCardChanges(httpContext, products.CardChanges{DesignFinish: &input.DesignFinish, DesignShade: input.DesignShade, Version: input.Version})
}

// ChangeCardLimit  PATCH /cards/:id/limit  -> only the monthly spending limit
func (controller *ProductsController) ChangeCardLimit(httpContext *gin.Context) {
	var input requests.CardLimitChangeRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.saveCardChanges(httpContext, products.CardChanges{MonthlyLimit: &input.MonthlyLimit, Version: input.Version})
}

// saveCardChanges applies card changes and answers with the updated card.
func (controller *ProductsController) saveCardChanges(httpContext *gin.Context, changes products.CardChanges) {
	card, err := controller.application.Products.UpdateCard(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("id"), changes)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Saved.", resources.Card(*card, money.Zero()))
}

// RevealCard  POST /cards/:id/reveal  (needs the PIN; never stored)
func (controller *ProductsController) RevealCard(httpContext *gin.Context) {
	var input requests.PinOnlyRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	details, err := controller.application.Products.RevealCard(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("id"), input.Pin)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	httpContext.Header("Cache-Control", "no-store")
	responses.OK(httpContext, "Card details.", details)
}

// DeleteCard  DELETE /cards/:id  (body: {"pin": "1234"})
func (controller *ProductsController) DeleteCard(httpContext *gin.Context) {
	var input requests.PinOnlyRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	transaction, err := controller.application.Products.DeleteCard(httpContext.Request.Context(), middleware.CurrentActor(httpContext), httpContext.Param("id"), input.Pin, middleware.BusinessReference(httpContext, "CDL"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Card deleted. Its balance is back in your USD wallet.", resources.Transaction(*transaction, controller.decimals(httpContext)))
}

// ─── Cashpoints ──────────────────────────────────────────────────────

// Cashpoints  GET /cashpoints
func (controller *ProductsController) Cashpoints(httpContext *gin.Context) {
	list, err := controller.application.Products.ListCashpoints(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Cashpoints loaded.", list)
}

// CreateCashpoint  POST /cashpoints
func (controller *ProductsController) CreateCashpoint(httpContext *gin.Context) {
	var input requests.CashpointRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	cashpoint, code, err := controller.application.Products.CreateCashpoint(httpContext.Request.Context(), middleware.CurrentActor(httpContext), input.AmountNgn, input.Pin, middleware.BusinessReference(httpContext, "CSH"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	amount, _ := money.ParseUnits(cashpoint.Amount)
	responses.Created(httpContext, "Cashpoint created.", gin.H{"id": cashpoint.ID, "code": code, "amount": amount.Decimal(2), "transaction_id": cashpoint.TransactionID})
}

// RedeemCashpoint  POST /cashpoints/redeem
func (controller *ProductsController) RedeemCashpoint(httpContext *gin.Context) {
	var input requests.RedeemCashpointRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	transaction, err := controller.application.Products.RedeemCashpoint(httpContext.Request.Context(), middleware.CurrentActor(httpContext), input.Code, middleware.BusinessReference(httpContext, "CSR"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Cashpoint redeemed.", resources.Transaction(*transaction, controller.decimals(httpContext)))
}

// CancelCashpoint  POST /cashpoints/:id/cancel
func (controller *ProductsController) CancelCashpoint(httpContext *gin.Context) {
	if err := controller.application.Products.CancelCashpoint(httpContext.Request.Context(), middleware.CurrentActor(httpContext), httpContext.Param("id")); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Cashpoint cancelled. The money is back in your wallet.", nil)
}

// ─── Referrals ───────────────────────────────────────────────────────

// Referrals  GET /referrals
func (controller *ProductsController) Referrals(httpContext *gin.Context) {
	summary, err := controller.application.Products.Referrals(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Referrals loaded.", summary)
}

// WithdrawReferral  POST /referrals/withdraw
func (controller *ProductsController) WithdrawReferral(httpContext *gin.Context) {
	var input requests.ReferralWithdrawRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	transaction, err := controller.application.Products.WithdrawReferral(httpContext.Request.Context(), middleware.CurrentActor(httpContext), input.AmountNgn, input.Pin, middleware.BusinessReference(httpContext, "REF"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Moved to your NGN wallet.", resources.Transaction(*transaction, controller.decimals(httpContext)))
}

// ─── Inbox, support, statements ──────────────────────────────────────

// Inbox  GET /inbox
func (controller *ProductsController) Inbox(httpContext *gin.Context) {
	messages, err := controller.application.Account.Inbox(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Inbox loaded.", messages)
}

// MarkInboxRead  POST /inbox/:id/read   (id "all" marks everything)
func (controller *ProductsController) MarkInboxRead(httpContext *gin.Context) {
	messageID := httpContext.Param("id")
	if messageID == "all" {
		messageID = ""
	}
	if err := controller.application.Account.MarkInboxRead(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, messageID); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Marked as read.", nil)
}

// SupportTickets  GET /support/tickets
func (controller *ProductsController) SupportTickets(httpContext *gin.Context) {
	threads, err := controller.application.Account.SupportTickets(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Tickets loaded.", threads)
}

// SupportTicket  GET /support/tickets/:id
func (controller *ProductsController) SupportTicket(httpContext *gin.Context) {
	thread, messages, err := controller.application.Account.SupportTicket(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("id"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Ticket loaded.", gin.H{"thread": thread, "messages": messages})
}

// OpenTicket  POST /support/tickets
func (controller *ProductsController) OpenTicket(httpContext *gin.Context) {
	var input requests.TicketRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	thread, err := controller.application.Account.OpenTicket(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, input.Subject, input.Body, input.TransactionID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "We got your message.", thread)
}

// ReplyToTicket  POST /support/tickets/:id/messages
func (controller *ProductsController) ReplyToTicket(httpContext *gin.Context) {
	var input requests.MessageRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	if err := controller.application.Account.ReplyToTicket(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, httpContext.Param("id"), input.Body); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Sent.", nil)
}

// RequestStatement  POST /statements  -> QUEUE EXAMPLE: answers at once, the worker builds it.
func (controller *ProductsController) RequestStatement(httpContext *gin.Context) {
	var input requests.StatementRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.Dispatcher.GenerateStatement(httpContext.Request.Context(), jobs.GenerateStatementPayload{
		UserID: middleware.CurrentActor(httpContext).UserID, AssetCode: input.Asset, FromDate: input.FromDate, ToDate: input.ToDate,
	})
	responses.Accepted(httpContext, "We're preparing your statement and will email it to you.", gin.H{"queued": true})
}
