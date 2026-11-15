package routes

import (
	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/controllers/admin"
	"github.com/tribe-africa/tribe-backend/app/http/middleware"
)

// registerAdminRoutes adds every /admin-api route. Every route (except
// sign-in) needs an admin token AND the permission named next to it.
func registerAdminRoutes(router *gin.Engine, application *bootstrap.App) {
	controller := admin.NewController(application)
	can := middleware.RequirePermission
	idempotent := middleware.RequireIdempotencyKey(application, "admin")

	api := router.Group("/admin-api", middleware.RequestLogger(application, "admin"))
	api.POST("/sessions", middleware.RateLimit(application, "admin-auth", 20), controller.Login)

	signedIn := api.Group("", middleware.AuthenticateAdmin(application), middleware.RateLimit(application, "admin", 600))
	signedIn.DELETE("/sessions/current", controller.Logout)
	signedIn.GET("/me", controller.Me)
	signedIn.GET("/permissions", controller.Permissions)

	// Team
	signedIn.GET("/team-members", can("team.view"), controller.TeamMembers)
	signedIn.POST("/team-members", can("team.manage"), idempotent, controller.CreateTeamMember)
	signedIn.PATCH("/team-members/:id", can("team.manage"), idempotent, controller.UpdateTeamMember)
	signedIn.DELETE("/team-members/:id", can("team.manage"), idempotent, controller.DeleteTeamMember)

	// Overview & customers
	signedIn.GET("/overview", can("overview.view"), controller.Overview) // everything below in one map
	signedIn.GET("/overview/stats", can("overview.view"), controller.OverviewStats)
	signedIn.GET("/overview/series", can("overview.view"), controller.OverviewSeries)
	signedIn.GET("/overview/by-category", can("overview.view"), controller.OverviewByCategory)
	signedIn.GET("/overview/failures", can("overview.view"), controller.OverviewFailures)
	signedIn.GET("/customers", can("customers.view"), controller.Customers)
	signedIn.GET("/customers/:id", can("customers.view"), controller.Customer)
	signedIn.GET("/customers/:id/transactions", can("transactions.view"), controller.CustomerTransactions)
	signedIn.GET("/customers/:id/verifications", can("kyc.view"), controller.CustomerVerifications)
	signedIn.GET("/customers/:id/api-requests", can("api.view"), controller.CustomerApiRequests)
	signedIn.PATCH("/customers/:id/status", can("customers.freeze"), idempotent, controller.SetCustomerStatus)
	signedIn.PUT("/customers/:id/labels", can("customers.labels"), idempotent, controller.SetCustomerLabels)
	// Manual credits & debits need two admins too, and only a SUPER ADMIN approves. A credit only posts if the vault's FREE
	// funds cover it; a debit only if the customer's wallet does.
	signedIn.POST("/customers/:id/adjustments", can("adjustments.request"), idempotent, controller.CreateCustomerAdjustment)
	signedIn.GET("/customer-adjustments", can("inventory.view"), controller.CustomerAdjustments)
	signedIn.POST("/customer-adjustments/:id/approve", can("adjustments.approve"), idempotent, controller.ApproveCustomerAdjustment)
	signedIn.POST("/customer-adjustments/:id/reject", can("adjustments.approve"), idempotent, controller.RejectCustomerAdjustment)

	// KYC & transactions
	signedIn.GET("/verifications", can("kyc.view"), controller.Verifications)
	signedIn.GET("/verifications/counts", can("kyc.view"), controller.VerificationCounts) // GET has no /verifications/:id, so nothing swallows it
	signedIn.POST("/verifications/:id/decisions", can("kyc.decide"), idempotent, controller.DecideVerification)
	signedIn.GET("/transactions", can("transactions.view"), controller.Transactions)
	signedIn.GET("/transactions/:id", can("transactions.view"), controller.Transaction)

	// Rates, margins & fees
	signedIn.GET("/currencies", can("currencies.view"), controller.Currencies)
	signedIn.POST("/currencies", can("currencies.manage"), idempotent, controller.CreateCurrency)
	signedIn.PUT("/currencies/:code", can("currencies.manage"), idempotent, controller.UpdateCurrency)
	signedIn.GET("/rates", can("currencies.view"), controller.Rates)
	signedIn.GET("/rates/history", can("currencies.view"), controller.RateHistory)
	signedIn.GET("/rate-margins", can("markups.view"), controller.RateMargins)
	signedIn.PUT("/rate-margins/:asset", can("markups.edit"), idempotent, controller.UpdateRateMargin)
	signedIn.GET("/transaction-fees", can("markups.view"), controller.TransactionFees)
	signedIn.PUT("/transaction-fees/:type", can("markups.edit"), idempotent, controller.UpdateTransactionFee)

	// Inventory & ledger
	signedIn.GET("/inventory", can("inventory.view"), controller.Inventory)
	signedIn.GET("/inventory/entries", can("inventory.view"), controller.InventoryEntries)
	signedIn.GET("/free-funds", can("inventory.view"), controller.FreeFunds)
	// Treasury funding needs two admins: one requests, a DIFFERENT one approves (only then is it posted).
	signedIn.GET("/treasury-fundings", can("inventory.view"), controller.TreasuryFundings)
	signedIn.POST("/treasury-fundings", can("inventory.topup"), idempotent, controller.CreateTreasuryFunding)
	signedIn.POST("/treasury-fundings/:id/approve", can("inventory.approve"), idempotent, controller.ApproveTreasuryFunding)
	signedIn.POST("/treasury-fundings/:id/reject", can("inventory.approve"), idempotent, controller.RejectTreasuryFunding)
	signedIn.GET("/ledger-accounts", can("ledger.view"), controller.LedgerAccounts)
	signedIn.GET("/ledger-accounts/:id/legs", can("ledger.view"), controller.LedgerAccountLegs)
	signedIn.GET("/journal-entries/:id", can("ledger.view"), controller.JournalEntry)
	signedIn.GET("/ledger-snapshots", can("ledger.view"), controller.LedgerSnapshots)
	signedIn.GET("/ledger/verify", can("ledger.view"), controller.VerifyLedger)

	// Providers (NGN rail switch), service switches & card designs
	signedIn.GET("/providers", can("providers.view"), controller.Providers)
	signedIn.PUT("/providers/:id", can("providers.manage"), idempotent, controller.UpdateProvider)
	// Service switches: turn a customer-facing service (swap, airtime, cards ...) off and back on
	signedIn.GET("/services", can("services.view"), controller.Services)
	signedIn.PUT("/services/:code", can("services.manage"), idempotent, controller.UpdateService)
	signedIn.GET("/card-designs", can("card_designs.manage"), controller.CardDesigns)
	signedIn.POST("/card-designs", can("card_designs.manage"), idempotent, controller.CreateCardDesign)
	signedIn.PUT("/card-designs/:id", can("card_designs.manage"), idempotent, controller.SaveCardDesign)
	signedIn.DELETE("/card-designs/:id", can("card_designs.manage"), idempotent, controller.DeleteCardDesign) // ?version=N
	signedIn.POST("/card-designs/:id/enable", can("card_designs.manage"), idempotent, controller.EnableCardDesign)

	// Support
	signedIn.GET("/support-threads", can("support.view"), controller.SupportThreads)
	signedIn.GET("/support-threads/stream", can("support.view"), controller.SupportStream) // live inbox (SSE)
	signedIn.GET("/support-threads/:id", can("support.view"), controller.SupportThread)
	signedIn.POST("/support-threads/:id/messages", can("support.reply"), idempotent, controller.ReplyToThread)
	signedIn.PATCH("/support-threads/:id/status", can("support.resolve"), idempotent, controller.SetThreadStatus)

	// Logs & jobs
	signedIn.GET("/api-requests", can("api.view"), controller.ApiRequests)
	signedIn.GET("/audit-logs", can("audit.view"), controller.AuditLogs)
	signedIn.GET("/jobs", can("jobs.view"), controller.Jobs) // the four lists below in one object
	signedIn.GET("/jobs/runs", can("jobs.view"), controller.JobRuns)
	signedIn.GET("/jobs/queues", can("jobs.view"), controller.JobQueues)
	signedIn.GET("/jobs/failed", can("jobs.view"), controller.FailedJobs)
	signedIn.GET("/jobs/schedule", can("jobs.view"), controller.JobSchedule)
	signedIn.POST("/jobs/:queue/:id/retry", can("jobs.manage"), idempotent, controller.RetryJob)
	signedIn.POST("/jobs/daily-snapshot/run", can("jobs.manage"), idempotent, controller.RunDailySnapshot) // CRON EXAMPLE
}
