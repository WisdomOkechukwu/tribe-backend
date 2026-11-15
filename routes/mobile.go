package routes

import (
	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/controllers/mobile"
	"github.com/tribe-africa/tribe-backend/app/http/middleware"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
)

// registerMobileRoutes adds every /api/mobile/v1 route.
//
// Middleware groups, from outside to inside:
//
//	public      : anyone (sign-up, sign-in)                   + rate limit
//	signedIn    : valid access token + EnsureSingleDevice     (this phone is the active one)
//	idempotent  : + Idempotency-Key header                     (every change)
//	serviceOn   : + the admin has not switched this service off (503 SERVICE_DISABLED)
//
// serviceOn sits on the money routes where the route alone says which
// service it is. Bills, sends and quotes pick their service from the body,
// so their service methods check it instead (shared.RequireService).
// Ways OUT stay open on purpose: cancelling a cashpoint and deleting a card.
func registerMobileRoutes(router *gin.Engine, application *bootstrap.App) {
	authController := mobile.NewAuthController(application)
	profileController := mobile.NewProfileController(application)
	moneyController := mobile.NewMoneyController(application)
	productsController := mobile.NewProductsController(application)
	supportSocket := mobile.NewSupportSocketController(application)

	api := router.Group("/api/mobile/v1", middleware.RequestLogger(application, "user"))
	idempotent := middleware.RequireIdempotencyKey(application, "user")
	serviceOn := func(serviceCode string) gin.HandlerFunc {
		return middleware.ServiceEnabled(application, serviceCode)
	}

	// ── Public: no token needed. These are the "new entry" routes that decide
	//    which phone becomes active, so EnsureSingleDevice does NOT run here.
	public := api.Group("/auth", middleware.RateLimit(application, "mobile-auth", 30))
	public.POST("/email/check", authController.CheckEmail)
	public.POST("/email/send-code", authController.SendEmailCode) // QUEUE EXAMPLE: email is sent by the worker
	public.POST("/email/verify", authController.VerifyEmail)
	public.GET("/tags/:tag/available", authController.TagAvailable)
	public.POST("/register", authController.Register)
	public.POST("/login", authController.Login)
	public.POST("/device/verify", authController.VerifyDevice)
	public.POST("/refresh", authController.Refresh)
	public.POST("/password/forgot", authController.ForgotPassword)
	public.POST("/password/reset", authController.ResetPassword)

	// ── Signed in on the ACTIVE phone.
	signedIn := api.Group("", middleware.AuthenticateUser(application), middleware.EnsureSingleDevice(application), middleware.RateLimit(application, "mobile", 240))
	signedIn.POST("/auth/logout", authController.Logout)

	// Profile & security
	signedIn.GET("/profile", profileController.Show)
	signedIn.PATCH("/profile", idempotent, profileController.Update)
	signedIn.PATCH("/profile/tag", idempotent, profileController.ChangeTag)
	signedIn.POST("/onboarding/complete", idempotent, profileController.CompleteOnboarding)
	signedIn.POST("/pin", idempotent, profileController.CreatePin)
	signedIn.POST("/pin/change", idempotent, profileController.ChangePin)
	signedIn.POST("/password/change", idempotent, profileController.ChangePassword)
	signedIn.POST("/phone/send-otp", idempotent, profileController.SendPhoneCode)
	signedIn.POST("/phone/verify", idempotent, profileController.VerifyPhone)
	signedIn.GET("/devices", profileController.Devices)
	signedIn.POST("/push-tokens", profileController.SavePushToken)

	// KYC
	signedIn.GET("/kyc", profileController.Kyc)
	signedIn.POST("/kyc/tier-1", idempotent, profileController.SubmitKyc(1))
	signedIn.POST("/kyc/tier-2", idempotent, profileController.SubmitKyc(2))
	signedIn.POST("/kyc/tier-3", idempotent, profileController.SubmitKyc(3))
	signedIn.POST("/global-accounts/apply", idempotent, profileController.ApplyGlobalAccounts)

	// Wallets
	signedIn.GET("/assets", moneyController.Assets)
	signedIn.GET("/wallets", moneyController.Wallets)
	signedIn.POST("/wallets/:code/open", idempotent, serviceOn(shared.ServiceWalletOpen), moneyController.OpenWallet)
	signedIn.GET("/wallets/:code/receive", serviceOn(shared.ServiceReceive), moneyController.ReceiveDetails)

	// Which services an admin has switched off (so the app can grey them out)
	signedIn.GET("/services", moneyController.Services)

	// Rates & quotes (rates/stream is the live SSE connection)
	signedIn.GET("/rates", moneyController.Rates)
	signedIn.GET("/rates/stream", moneyController.RatesStream)
	signedIn.POST("/quotes", moneyController.CreateQuote) // refused for a switched-off swap or send (checked in rates.CreateQuote)

	// Transactions
	signedIn.GET("/transactions", moneyController.Transactions)
	signedIn.GET("/transactions/:id", moneyController.Transaction)

	// Swap & send
	signedIn.POST("/swaps", idempotent, serviceOn(shared.ServiceSwap), moneyController.Swap)
	signedIn.GET("/recipients/lookup", moneyController.LookupRecipient)
	signedIn.GET("/channels", moneyController.Channels)
	// QUEUE EXAMPLE: bank/wallet payouts finish in the worker. The service (send_tribe, send_bank,
	// send_global or send_crypto) depends on the recipient, so transfers.Send checks it.
	signedIn.POST("/sends", idempotent, moneyController.Send)
	signedIn.GET("/beneficiaries", moneyController.Beneficiaries)
	signedIn.POST("/beneficiaries", idempotent, moneyController.SaveBeneficiary)
	signedIn.DELETE("/beneficiaries/:id", idempotent, moneyController.DeleteBeneficiary)

	// Bills, vault, recurring
	signedIn.GET("/billers", productsController.Billers)
	signedIn.POST("/bills/verify-customer", productsController.VerifyBillCustomer)
	signedIn.POST("/bills", idempotent, productsController.PayBill) // the service is the biller's category, checked in products.PayBill
	signedIn.GET("/vault/tokens", productsController.VaultTokens)
	signedIn.PATCH("/vault/tokens/:id", idempotent, productsController.UpdateVaultToken)
	signedIn.GET("/recurring-bills", productsController.RecurringBills)
	signedIn.POST("/recurring-bills", idempotent, serviceOn(shared.ServiceRecurringBills), productsController.CreateRecurringBill)
	signedIn.PATCH("/recurring-bills/:id", idempotent, productsController.UpdateRecurringBill) // pausing always works; new consent is checked in products.UpdateRecurring
	signedIn.DELETE("/recurring-bills/:id", idempotent, productsController.DeleteRecurringBill)

	// Gift cards
	signedIn.GET("/gift-cards/brands", productsController.GiftCardBrands)
	signedIn.POST("/gift-cards/buy", idempotent, serviceOn(shared.ServiceGiftCardBuy), productsController.BuyGiftCard)
	signedIn.POST("/gift-cards/sell", idempotent, serviceOn(shared.ServiceGiftCardSell), productsController.SellGiftCard)
	signedIn.POST("/gift-cards/:id/reveal", productsController.RevealGiftCard) // reads only (needs the PIN), like card reveal

	// Cashpoints
	signedIn.GET("/cashpoints", productsController.Cashpoints)
	signedIn.POST("/cashpoints", idempotent, serviceOn(shared.ServiceCashpointCreate), productsController.CreateCashpoint)
	signedIn.POST("/cashpoints/redeem", idempotent, serviceOn(shared.ServiceCashpointRedeem), middleware.RateLimit(application, "cashpoint-redeem", 10), productsController.RedeemCashpoint)
	signedIn.POST("/cashpoints/:id/cancel", idempotent, productsController.CancelCashpoint) // never switched off: it gives the creator their Naira back

	// Virtual cards (designs are stored in card_designs; each card keeps its own finish)
	signedIn.GET("/card-designs", productsController.CardDesigns)
	signedIn.GET("/cards", productsController.Cards)
	signedIn.POST("/cards", idempotent, serviceOn(shared.ServiceVirtualCards), productsController.CreateCard)
	signedIn.POST("/cards/:id/fund", idempotent, serviceOn(shared.ServiceVirtualCards), productsController.FundCard)
	signedIn.POST("/cards/:id/withdraw", idempotent, serviceOn(shared.ServiceVirtualCards), productsController.WithdrawCard)
	signedIn.POST("/cards/:id/freeze", idempotent, productsController.SetCardFrozen(true))
	signedIn.POST("/cards/:id/unfreeze", idempotent, productsController.SetCardFrozen(false))
	signedIn.PATCH("/cards/:id", idempotent, productsController.UpdateCard) // label, online payments, wallet
	signedIn.PATCH("/cards/:id/design", idempotent, productsController.ChangeCardDesign)
	signedIn.PATCH("/cards/:id/limit", idempotent, productsController.ChangeCardLimit)
	signedIn.POST("/cards/:id/reveal", productsController.RevealCard)
	signedIn.DELETE("/cards/:id", idempotent, productsController.DeleteCard) // never switched off: it returns the card's balance to the wallet

	// Referrals, inbox, support, statements
	signedIn.GET("/referrals", productsController.Referrals)
	signedIn.POST("/referrals/withdraw", idempotent, serviceOn(shared.ServiceReferralWithdraw), productsController.WithdrawReferral)
	signedIn.GET("/inbox", productsController.Inbox)
	signedIn.POST("/inbox/:id/read", productsController.MarkInboxRead)
	signedIn.GET("/support/tickets", productsController.SupportTickets)
	signedIn.GET("/support/tickets/:id", productsController.SupportTicket)
	signedIn.POST("/support/tickets", idempotent, productsController.OpenTicket)
	signedIn.POST("/support/tickets/:id/messages", idempotent, productsController.ReplyToTicket)
	// live: a WebSocket with this customer's own support events (agent replies, status changes)
	signedIn.GET("/support/stream", supportSocket.SupportSocket)
	signedIn.POST("/statements", idempotent, productsController.RequestStatement) // QUEUE EXAMPLE
}
