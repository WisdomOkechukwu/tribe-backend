// Package admin holds the controllers for the admin panel routes
// (/admin-api/...). Same recipe as the mobile controllers: bind input,
// call one service, send the response. Permissions are checked by the
// RequirePermission middleware on each route (see routes/admin.go).
package admin

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/hibiken/asynq"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/console"
	"github.com/tribe-africa/tribe-backend/app/http/middleware"
	"github.com/tribe-africa/tribe-backend/app/http/requests"
	"github.com/tribe-africa/tribe-backend/app/http/responses"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/adminauth"
	"github.com/tribe-africa/tribe-backend/app/services/backoffice"
	"github.com/tribe-africa/tribe-backend/app/services/ledger"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
)

// Controller handles every admin route.
type Controller struct {
	application *bootstrap.App
}

// NewController creates the admin controller.
func NewController(application *bootstrap.App) *Controller {
	return &Controller{application: application}
}

// apiRequestFilters reads ?status=&method=&path=&min_latency=&page= for the API trail.
func apiRequestFilters(httpContext *gin.Context, customerID string) backoffice.ApiRequestFilters {
	minimumLatency, _ := strconv.Atoi(httpContext.Query("min_latency"))
	return backoffice.ApiRequestFilters{
		CustomerID: customerID, Status: httpContext.Query("status"), Method: httpContext.Query("method"),
		Path: httpContext.Query("path"), MinLatencyMs: minimumLatency, Page: pageNumber(httpContext),
	}
}

// pageNumber reads ?page= (default 1).
func pageNumber(httpContext *gin.Context) int {
	page, err := strconv.Atoi(httpContext.DefaultQuery("page", "1"))
	if err != nil || page < 1 {
		return 1
	}
	return page
}

// ─── Sessions and team ───────────────────────────────────────────────

// Login  POST /admin-api/sessions
func (controller *Controller) Login(httpContext *gin.Context) {
	var input requests.AdminLoginRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	result, err := controller.application.AdminAuth.Login(httpContext.Request.Context(), input.Email, input.Password, httpContext.ClientIP(), httpContext.GetHeader("User-Agent"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Signed in.", result)
}

// Logout  DELETE /admin-api/sessions/current
func (controller *Controller) Logout(httpContext *gin.Context) {
	admin := middleware.CurrentAdmin(httpContext)
	if err := controller.application.AdminAuth.Logout(httpContext.Request.Context(), admin); err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), admin, "auth.logout", admin.Email, "Signed out", nil, nil)
	responses.OK(httpContext, "Signed out.", nil)
}

// Me  GET /admin-api/me
func (controller *Controller) Me(httpContext *gin.Context) {
	responses.OK(httpContext, "Signed in admin.", middleware.CurrentAdmin(httpContext))
}

// Permissions  GET /admin-api/permissions  -> the permission catalog.
func (controller *Controller) Permissions(httpContext *gin.Context) {
	responses.OK(httpContext, "Permissions.", adminauth.Catalog)
}

// TeamMembers  GET /admin-api/team-members
func (controller *Controller) TeamMembers(httpContext *gin.Context) {
	team, err := controller.application.AdminAuth.ListTeam(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Team loaded.", team)
}

// CreateTeamMember  POST /admin-api/team-members
func (controller *Controller) CreateTeamMember(httpContext *gin.Context) {
	var input requests.TeamMemberRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	admin := middleware.CurrentAdmin(httpContext)
	member, err := controller.application.AdminAuth.CreateAdmin(httpContext.Request.Context(), adminauth.NewAdminInput{
		Email: input.Email, Name: input.Name, Color: input.Color, Permissions: input.Permissions, CreatedBy: &admin.ID,
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), admin, "team.add", member.Email, "Added teammate", nil, member.Permissions)
	responses.Created(httpContext, "Teammate added.", member)
}

// UpdateTeamMember  PATCH /admin-api/team-members/:id
func (controller *Controller) UpdateTeamMember(httpContext *gin.Context) {
	var input requests.UpdateTeamMemberRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	admin := middleware.CurrentAdmin(httpContext)
	member, err := controller.application.AdminAuth.UpdateTeammate(httpContext.Request.Context(), admin, httpContext.Param("id"), adminauth.TeamChanges{
		Name: input.Name, Color: input.Color, Status: input.Status, Permissions: input.Permissions, ResetPassword: input.ResetPassword, Version: input.Version,
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), admin, "team.update", member.Email, "Changed teammate", nil, input)
	responses.OK(httpContext, "Saved.", member)
}

// DeleteTeamMember  DELETE /admin-api/team-members/:id
func (controller *Controller) DeleteTeamMember(httpContext *gin.Context) {
	admin := middleware.CurrentAdmin(httpContext)
	if err := controller.application.AdminAuth.RemoveTeammate(httpContext.Request.Context(), admin, httpContext.Param("id")); err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), admin, "team.remove", httpContext.Param("id"), "Removed teammate", nil, nil)
	responses.OK(httpContext, "Removed.", nil)
}

// ─── Customers, KYC, transactions ────────────────────────────────────

// Overview  GET /admin-api/overview
func (controller *Controller) Overview(httpContext *gin.Context) {
	overview, err := controller.application.Backoffice.Overview(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext).Can("customers.view"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Overview loaded.", overview)
}

// OverviewStats  GET /admin-api/overview/stats  -> the headline numbers only
func (controller *Controller) OverviewStats(httpContext *gin.Context) {
	stats, err := controller.application.Backoffice.OverviewStats(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Overview stats loaded.", stats)
}

// OverviewSeries  GET /admin-api/overview/series  -> volume per day, last 30 days
func (controller *Controller) OverviewSeries(httpContext *gin.Context) {
	series, err := controller.application.Backoffice.OverviewSeries(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Overview series loaded.", series)
}

// OverviewByCategory  GET /admin-api/overview/by-category  -> volume per category, last 7 days
func (controller *Controller) OverviewByCategory(httpContext *gin.Context) {
	byCategory, err := controller.application.Backoffice.OverviewByCategory(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Overview categories loaded.", byCategory)
}

// OverviewFailures  GET /admin-api/overview/failures  -> the newest failed transactions
// (customer names only with customers.view).
func (controller *Controller) OverviewFailures(httpContext *gin.Context) {
	failures, err := controller.application.Backoffice.OverviewFailures(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext).Can("customers.view"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Recent failures loaded.", failures)
}

// Customers  GET /admin-api/customers?search=&status=&kyc=&label=&page=
func (controller *Controller) Customers(httpContext *gin.Context) {
	page, err := controller.application.Backoffice.ListCustomers(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), backoffice.CustomerFilters{
		Search: httpContext.Query("search"), Status: httpContext.Query("status"), Kyc: httpContext.Query("kyc"),
		Label: httpContext.Query("label"), Page: pageNumber(httpContext),
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Customers loaded.", page)
}

// Customer  GET /admin-api/customers/:id
func (controller *Controller) Customer(httpContext *gin.Context) {
	detail, err := controller.application.Backoffice.GetCustomer(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), httpContext.Param("id"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Customer loaded.", detail)
}

// CustomerTransactions  GET /admin-api/customers/:id/transactions
func (controller *Controller) CustomerTransactions(httpContext *gin.Context) {
	page, err := controller.application.Backoffice.ListTransactions(httpContext.Request.Context(), backoffice.TransactionFilters{
		CustomerID: httpContext.Param("id"), Category: httpContext.Query("category"), Page: pageNumber(httpContext),
		ShowCustomers: middleware.CurrentAdmin(httpContext).Can("customers.view"),
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Transactions loaded.", page)
}

// CustomerVerifications  GET /admin-api/customers/:id/verifications
func (controller *Controller) CustomerVerifications(httpContext *gin.Context) {
	page, err := controller.application.Backoffice.ListVerifications(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), "", httpContext.Param("id"), pageNumber(httpContext))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Verifications loaded.", page)
}

// CustomerApiRequests  GET /admin-api/customers/:id/api-requests
func (controller *Controller) CustomerApiRequests(httpContext *gin.Context) {
	page, err := controller.application.Backoffice.ListApiRequests(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), apiRequestFilters(httpContext, httpContext.Param("id")))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "API requests loaded.", page)
}

// SetCustomerStatus  PATCH /admin-api/customers/:id/status
func (controller *Controller) SetCustomerStatus(httpContext *gin.Context) {
	var input requests.CustomerStatusRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	customer, err := controller.application.Backoffice.SetCustomerStatus(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), httpContext.Param("id"), input.Status, input.Reason, input.Version)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Customer is now "+input.Status+".", customer)
}

// SetCustomerLabels  PUT /admin-api/customers/:id/labels
func (controller *Controller) SetCustomerLabels(httpContext *gin.Context) {
	var input requests.CustomerLabelsRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	customer, err := controller.application.Backoffice.SetCustomerLabels(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), httpContext.Param("id"), input.Labels)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Labels saved.", customer)
}

// Verifications  GET /admin-api/verifications?status=pending
func (controller *Controller) Verifications(httpContext *gin.Context) {
	page, err := controller.application.Backoffice.ListVerifications(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), httpContext.DefaultQuery("status", "pending"), "", pageNumber(httpContext))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Verifications loaded.", page)
}

// VerificationCounts  GET /admin-api/verifications/counts  -> {pending, approved, rejected, all}
func (controller *Controller) VerificationCounts(httpContext *gin.Context) {
	counts, err := controller.application.Backoffice.CountVerificationsByStatus(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Verification counts loaded.", counts)
}

// DecideVerification  POST /admin-api/verifications/:id/decisions  (:id is a verification REQUEST id)
func (controller *Controller) DecideVerification(httpContext *gin.Context) {
	var input requests.VerificationDecisionRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	if err := controller.application.Backoffice.DecideSubmission(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), httpContext.Param("id"), input.Decision, input.Reason); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Decision saved.", nil)
}

// Transactions  GET /admin-api/transactions?category=&type=&status=&asset=&search=&from=&to=&page=
func (controller *Controller) Transactions(httpContext *gin.Context) {
	page, err := controller.application.Backoffice.ListTransactions(httpContext.Request.Context(), backoffice.TransactionFilters{
		Category: httpContext.Query("category"), Type: httpContext.Query("type"), Status: httpContext.Query("status"),
		Asset: httpContext.Query("asset"), Search: httpContext.Query("search"), From: httpContext.Query("from"), To: httpContext.Query("to"),
		ShowCustomers: middleware.CurrentAdmin(httpContext).Can("customers.view"),
		Page:          pageNumber(httpContext),
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Transactions loaded.", page)
}

// Transaction  GET /admin-api/transactions/:id  -> with journal entries and legs.
func (controller *Controller) Transaction(httpContext *gin.Context) {
	detail, err := controller.application.Backoffice.GetTransaction(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), httpContext.Param("id"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Transaction loaded.", detail)
}

// ─── Rates, fees, inventory, ledger ──────────────────────────────────

// Currencies  GET /admin-api/currencies
func (controller *Controller) Currencies(httpContext *gin.Context) {
	var assets []models.Asset
	controller.application.Database.WithContext(httpContext.Request.Context()).Order("display_order").Find(&assets)
	responses.OK(httpContext, "Currencies loaded.", assets)
}

// CreateCurrency  POST /admin-api/currencies  -> a new currency, switched off
// (409 CURRENCY_EXISTS when the code is taken).
func (controller *Controller) CreateCurrency(httpContext *gin.Context) {
	var input requests.CurrencyCreateRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	asset, err := controller.application.Backoffice.CreateCurrency(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), backoffice.CurrencyInput{
		Code: input.Code, Name: input.Name, Kind: input.Kind, Symbol: input.Symbol, DecimalPlaces: *input.DecimalPlaces,
		ConversionProviderID: input.ConversionProviderID, PriceSource: input.PriceSource,
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Currency added. Switch it on when you are ready.", asset)
}

// UpdateCurrency  PUT /admin-api/currencies/:code  -> rename, reorder, or switch a currency on or off
func (controller *Controller) UpdateCurrency(httpContext *gin.Context) {
	var input requests.CurrencyUpdateRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	asset, err := controller.application.Backoffice.UpdateCurrency(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), httpContext.Param("code"), backoffice.CurrencyChanges{
		Name: input.Name, Symbol: input.Symbol, DisplayOrder: input.DisplayOrder, Enabled: input.Enabled, Version: input.Version,
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Currency saved.", asset)
}

// Rates  GET /admin-api/rates
func (controller *Controller) Rates(httpContext *gin.Context) {
	event, err := controller.application.Rates.LatestEvent(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Rates loaded.", event)
}

// RateHistory  GET /admin-api/rates/history?asset=BTC&hours=24
func (controller *Controller) RateHistory(httpContext *gin.Context) {
	hours, _ := strconv.Atoi(httpContext.DefaultQuery("hours", "24"))
	history, err := controller.application.Backoffice.RateHistory(httpContext.Request.Context(), httpContext.DefaultQuery("asset", "BTC"), hours)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Rate history loaded.", history)
}

// RateMargins  GET /admin-api/rate-margins
func (controller *Controller) RateMargins(httpContext *gin.Context) {
	margins, err := controller.application.Backoffice.ListMargins(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Margins loaded.", margins)
}

// UpdateRateMargin  PUT /admin-api/rate-margins/:asset
func (controller *Controller) UpdateRateMargin(httpContext *gin.Context) {
	var input requests.RateMarginRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	margin, err := controller.application.Backoffice.SaveMargin(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), models.RateMargin{
		AssetCode: httpContext.Param("asset"), BuyMarginPercent: input.BuyMarginPercent, SellMarginPercent: input.SellMarginPercent, Version: input.Version,
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Margins saved.", margin)
}

// TransactionFees  GET /admin-api/transaction-fees
func (controller *Controller) TransactionFees(httpContext *gin.Context) {
	fees, err := controller.application.Backoffice.ListFees(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Fees loaded.", fees)
}

// UpdateTransactionFee  PUT /admin-api/transaction-fees/:type
func (controller *Controller) UpdateTransactionFee(httpContext *gin.Context) {
	var input requests.TransactionFeeRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	fee, err := controller.application.Backoffice.SaveFee(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), models.TransactionFee{
		TransactionType: httpContext.Param("type"), FeePercent: input.FeePercent, FlatFee: input.FlatFee, FlatFeeAsset: input.FlatFeeAsset,
		MaximumFee: input.MaximumFee, Active: input.Active, Version: input.Version,
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Fee saved.", fee)
}

// Inventory  GET /admin-api/inventory  -> company vaults with balances.
func (controller *Controller) Inventory(httpContext *gin.Context) {
	inventory, err := controller.application.Backoffice.Inventory(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Inventory loaded.", inventory)
}

// TreasuryFundings  GET /admin-api/treasury-fundings?status=pending|approved|rejected
func (controller *Controller) TreasuryFundings(httpContext *gin.Context) {
	page, err := controller.application.Backoffice.ListTreasuryFundings(httpContext.Request.Context(), httpContext.Query("status"), pageNumber(httpContext))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Fundings loaded.", page)
}

// InventoryEntries  GET /admin-api/inventory/entries?asset=  -> movements on the company vaults
func (controller *Controller) InventoryEntries(httpContext *gin.Context) {
	page, err := controller.application.Backoffice.InventoryEntries(httpContext.Request.Context(), httpContext.Query("asset"), pageNumber(httpContext))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Inventory entries loaded.", page)
}

// CreateTreasuryFunding  POST /admin-api/treasury-fundings  -> a PENDING request (nothing posted yet)
func (controller *Controller) CreateTreasuryFunding(httpContext *gin.Context) {
	var input requests.TreasuryFundingRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	admin := middleware.CurrentAdmin(httpContext)
	funding, err := controller.application.Wallets.RequestTreasuryFunding(httpContext.Request.Context(), admin.ID, admin.Name, input.Asset, input.Amount, input.ExternalReference, input.Note)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), admin, "inventory.topup", funding.ID, "Requested treasury funding "+input.Amount+" "+input.Asset, nil, funding)
	responses.Created(httpContext, "Funding requested. Another admin must approve it.", controller.fundingView(httpContext, *funding))
}

// ApproveTreasuryFunding  POST /admin-api/treasury-fundings/:id/approve
// A DIFFERENT admin approves; only then is the money posted (Dr Vault / Cr Treasury Funding).
func (controller *Controller) ApproveTreasuryFunding(httpContext *gin.Context) {
	admin := middleware.CurrentAdmin(httpContext)
	funding, err := controller.application.Wallets.ApproveTreasuryFunding(httpContext.Request.Context(), admin.ID, admin.Name, httpContext.Param("id"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), admin, "inventory.approve", funding.ID, "Approved treasury funding "+funding.AssetCode, nil, funding)
	responses.OK(httpContext, "Funding approved and posted.", controller.fundingView(httpContext, *funding))
}

// RejectTreasuryFunding  POST /admin-api/treasury-fundings/:id/reject
func (controller *Controller) RejectTreasuryFunding(httpContext *gin.Context) {
	var input requests.RejectFundingRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	admin := middleware.CurrentAdmin(httpContext)
	funding, err := controller.application.Wallets.RejectTreasuryFunding(httpContext.Request.Context(), admin.ID, admin.Name, httpContext.Param("id"), input.Reason)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), admin, "inventory.reject", funding.ID, "Rejected treasury funding: "+input.Reason, nil, funding)
	responses.OK(httpContext, "Funding rejected.", controller.fundingView(httpContext, *funding))
}

// FreeFunds  GET /admin-api/free-funds  -> per currency: vault, owed to customers, free to credit
func (controller *Controller) FreeFunds(httpContext *gin.Context) {
	funds, err := controller.application.Backoffice.FreeFunds(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Free funds loaded.", funds)
}

// CustomerAdjustments  GET /admin-api/customer-adjustments?status=pending|approved|rejected&kind=credit|debit&customer=<id>
func (controller *Controller) CustomerAdjustments(httpContext *gin.Context) {
	page, err := controller.application.Backoffice.ListCustomerAdjustments(httpContext.Request.Context(), httpContext.Query("status"), httpContext.Query("kind"), httpContext.Query("customer"), pageNumber(httpContext))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Adjustments loaded.", page)
}

// CreateCustomerAdjustment  POST /admin-api/customers/:id/adjustments  -> a PENDING credit or debit (nothing posted yet)
func (controller *Controller) CreateCustomerAdjustment(httpContext *gin.Context) {
	var input requests.CustomerAdjustmentRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	admin := middleware.CurrentAdmin(httpContext)
	adjustment, err := controller.application.Wallets.RequestCustomerAdjustment(httpContext.Request.Context(), admin.ID, admin.Name, httpContext.Param("id"), input.Kind, input.Asset, input.Amount, input.Reason)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), admin, "adjustments.request", adjustment.ID, "Requested customer "+input.Kind+" "+input.Amount+" "+input.Asset+": "+input.Reason, nil, adjustment)
	responses.Created(httpContext, "Request saved. Another admin must approve it.", controller.adjustmentView(httpContext, *adjustment))
}

// ApproveCustomerAdjustment  POST /admin-api/customer-adjustments/:id/approve
// A DIFFERENT admin, who must be a super admin, approves; only then is it posted. A credit must fit the vault's
// free funds; a debit must fit the customer's wallet.
func (controller *Controller) ApproveCustomerAdjustment(httpContext *gin.Context) {
	admin := middleware.CurrentAdmin(httpContext)
	adjustment, err := controller.application.Wallets.ApproveCustomerAdjustment(httpContext.Request.Context(), admin.ID, admin.Name, admin.IsSuperAdmin(), httpContext.Param("id"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), admin, "adjustments.approve", adjustment.ID, "Approved customer "+adjustment.Kind+" "+adjustment.AssetCode, nil, adjustment)
	responses.OK(httpContext, "Approved and posted.", controller.adjustmentView(httpContext, *adjustment))
}

// RejectCustomerAdjustment  POST /admin-api/customer-adjustments/:id/reject
func (controller *Controller) RejectCustomerAdjustment(httpContext *gin.Context) {
	var input requests.RejectFundingRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	admin := middleware.CurrentAdmin(httpContext)
	adjustment, err := controller.application.Wallets.RejectCustomerAdjustment(httpContext.Request.Context(), admin.ID, admin.Name, httpContext.Param("id"), input.Reason)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), admin, "adjustments.reject", adjustment.ID, "Rejected customer "+adjustment.Kind+": "+input.Reason, nil, adjustment)
	responses.OK(httpContext, "Rejected.", controller.adjustmentView(httpContext, *adjustment))
}

// adjustmentView adds the readable amount and customer name to an adjustment row.
func (controller *Controller) adjustmentView(httpContext *gin.Context, adjustment models.CustomerAdjustment) backoffice.CustomerAdjustmentView {
	query := controller.application.Database.WithContext(httpContext.Request.Context())
	var user models.User
	query.Where("id = ?", adjustment.UserID).Take(&user)
	return backoffice.NewCustomerAdjustmentView(adjustment, user, shared.AssetDecimals(query)[adjustment.AssetCode])
}

// fundingView adds the readable amount to a funding row.
func (controller *Controller) fundingView(httpContext *gin.Context, funding models.TreasuryFunding) backoffice.TreasuryFundingView {
	decimals := shared.AssetDecimals(controller.application.Database.WithContext(httpContext.Request.Context()))
	return backoffice.NewTreasuryFundingView(funding, decimals[funding.AssetCode])
}

// LedgerAccounts  GET /admin-api/ledger-accounts?owner=company|<user id>&asset=
func (controller *Controller) LedgerAccounts(httpContext *gin.Context) {
	accounts, err := controller.application.Backoffice.ListLedgerAccounts(httpContext.Request.Context(), httpContext.Query("owner"), httpContext.Query("asset"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Ledger accounts loaded.", accounts)
}

// LedgerAccountLegs  GET /admin-api/ledger-accounts/:id/legs
func (controller *Controller) LedgerAccountLegs(httpContext *gin.Context) {
	accountID, _ := strconv.ParseUint(httpContext.Param("id"), 10, 64)
	page, err := controller.application.Backoffice.AccountLegs(httpContext.Request.Context(), accountID, pageNumber(httpContext))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Legs loaded.", page)
}

// JournalEntry  GET /admin-api/journal-entries/:id
func (controller *Controller) JournalEntry(httpContext *gin.Context) {
	entry, err := controller.application.Backoffice.GetJournalEntry(httpContext.Request.Context(), httpContext.Param("id"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Journal entry loaded.", entry)
}

// LedgerSnapshots  GET /admin-api/ledger-snapshots?date=YYYY-MM-DD
func (controller *Controller) LedgerSnapshots(httpContext *gin.Context) {
	snapshots, date, err := controller.application.Backoffice.ListSnapshots(httpContext.Request.Context(), httpContext.Query("date"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Snapshots loaded.", gin.H{"date": date, "snapshots": snapshots})
}

// VerifyLedger  GET /admin-api/ledger/verify[?full=true]  -> checks the books.
// By default it checks only what changed since the last clean run (fast);
// ?full=true re-checks everything, like `artisan ledger:verify`.
func (controller *Controller) VerifyLedger(httpContext *gin.Context) {
	mode := ledger.VerifyIncremental
	if httpContext.Query("full") == "true" {
		mode = ledger.VerifyFull
	}
	report, err := controller.application.Ledger.RunVerify(httpContext.Request.Context(), mode)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Ledger checked.", gin.H{
		"ok": report.ProblemCount == 0, "problems": report.Problems, "problem_count": report.ProblemCount,
		"mode": report.Mode, "entries_checked": report.EntriesChecked, "snapshots_checked": report.SnapshotsChecked,
	})
}

// ─── Providers, services and card designs ────────────────────────────

// Providers  GET /admin-api/providers
func (controller *Controller) Providers(httpContext *gin.Context) {
	list, err := controller.application.Backoffice.ListProviders(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Providers loaded.", list)
}

// UpdateProvider  PUT /admin-api/providers/:id  (NGN rail switch lives here)
func (controller *Controller) UpdateProvider(httpContext *gin.Context) {
	var input requests.ProviderRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	provider, err := controller.application.Backoffice.UpdateProvider(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), httpContext.Param("id"), backoffice.ProviderChanges{
		UsesNgnRail: input.UsesNgnRail, SupportsDirectConversion: input.SupportsDirectConversion, Enabled: input.Enabled, Version: input.Version,
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Provider saved.", provider)
}

// Services  GET /admin-api/services  -> every customer-facing service with its on/off switch
func (controller *Controller) Services(httpContext *gin.Context) {
	list, err := controller.application.Backoffice.ListServices(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Services loaded.", list)
}

// UpdateService  PUT /admin-api/services/:code  -> switch a service on or off, or change its message
func (controller *Controller) UpdateService(httpContext *gin.Context) {
	var input requests.ServiceRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	serviceSwitch, err := controller.application.Backoffice.UpdateService(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), httpContext.Param("code"), backoffice.ServiceChanges{
		Enabled: input.Enabled, DisabledMessage: input.DisabledMessage, Version: input.Version,
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Service saved.", serviceSwitch)
}

// CardDesigns  GET /admin-api/card-designs
func (controller *Controller) CardDesigns(httpContext *gin.Context) {
	catalog, err := controller.application.Products.ListCardDesigns(httpContext.Request.Context(), true)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Card designs loaded.", catalog)
}

// CreateCardDesign  POST /admin-api/card-designs  -> a new design under the id in the body
// (409 DESIGN_EXISTS when the id or the same look is taken).
func (controller *Controller) CreateCardDesign(httpContext *gin.Context) {
	var input requests.CardDesignRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	design, err := controller.application.Products.CreateCardDesign(httpContext.Request.Context(), cardDesignFromRequest(input.ID, input))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), "card_designs.create", design.ID, "Created card design", nil, design)
	responses.Created(httpContext, "Card design created.", design)
}

// SaveCardDesign  PUT /admin-api/card-designs/:id  -> changes an existing design (needs its current version)
func (controller *Controller) SaveCardDesign(httpContext *gin.Context) {
	var input requests.CardDesignRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	design, err := controller.application.Products.UpdateCardDesign(httpContext.Request.Context(), cardDesignFromRequest(httpContext.Param("id"), input), input.Version)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), "card_designs.save", design.ID, "Saved card design", nil, design)
	responses.OK(httpContext, "Card design saved.", design)
}

// cardDesignFromRequest copies the request fields into a design row with the given id.
func cardDesignFromRequest(designID string, input requests.CardDesignRequest) models.CardDesign {
	return models.CardDesign{
		ID: designID, Kind: input.Kind, Name: input.Name, Finish: input.Finish, HexColor: input.HexColor,
		Pattern: input.Pattern, Enabled: input.Enabled, SortOrder: input.SortOrder,
	}
}

// DeleteCardDesign  DELETE /admin-api/card-designs/:id?version=N  (turns it off; cards using it keep their look)
func (controller *Controller) DeleteCardDesign(httpContext *gin.Context) {
	version, err := strconv.Atoi(httpContext.Query("version"))
	if err != nil {
		responses.Error(httpContext, apperrors.Validation(map[string][]string{"version": {"Send ?version= with the version you are turning off."}}))
		return
	}
	design, err := controller.application.Products.SetCardDesignEnabled(httpContext.Request.Context(), httpContext.Param("id"), false, version)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), "card_designs.disable", design.ID, "Disabled card design", nil, design)
	responses.OK(httpContext, "Card design turned off.", design)
}

// EnableCardDesign  POST /admin-api/card-designs/:id/enable  {version}  (turns it back on)
func (controller *Controller) EnableCardDesign(httpContext *gin.Context) {
	var input requests.CardDesignVersionRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	design, err := controller.application.Products.SetCardDesignEnabled(httpContext.Request.Context(), httpContext.Param("id"), true, input.Version)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), "card_designs.enable", design.ID, "Enabled card design", nil, design)
	responses.OK(httpContext, "Card design turned on.", design)
}

// ─── Support ─────────────────────────────────────────────────────────

// SupportThreads  GET /admin-api/support-threads?status=&channel=
func (controller *Controller) SupportThreads(httpContext *gin.Context) {
	threads, err := controller.application.Backoffice.ListThreads(httpContext.Request.Context(), httpContext.Query("status"), httpContext.Query("channel"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	counts, err := controller.application.Backoffice.CountThreadsByStatus(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OKWithMeta(httpContext, "Threads loaded.", threads, gin.H{"counts": counts})
}

// SupportStream  GET /admin-api/support-threads/stream  -> live inbox over SSE.
//
// Every time a customer writes, an admin replies or a status changes, the
// API publishes to Redis "support.events" and this stream sends:
//
//	event: support
//	data: {"type":"thread.updated","thread_id":"01J..."}
//
// The admin page then reloads that thread. A heartbeat comment is sent
// every 15 s so proxies keep the connection open.
func (controller *Controller) SupportStream(httpContext *gin.Context) {
	writer := httpContext.Writer
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("Connection", "keep-alive")
	writer.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(writer, ": connected\n\n")
	writer.Flush()

	connection := controller.application.SupportHub.Subscribe()
	defer controller.application.SupportHub.Unsubscribe(connection)
	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-httpContext.Request.Context().Done():
			return
		case payload := <-connection:
			fmt.Fprintf(writer, "event: support\ndata: %s\n\n", payload)
			writer.Flush()
		case <-heartbeat.C:
			fmt.Fprint(writer, ": heartbeat\n\n")
			writer.Flush()
		}
	}
}

// SupportThread  GET /admin-api/support-threads/:id
func (controller *Controller) SupportThread(httpContext *gin.Context) {
	thread, messages, err := controller.application.Backoffice.GetThread(httpContext.Request.Context(), httpContext.Param("id"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Thread loaded.", gin.H{"thread": thread, "messages": messages})
}

// ReplyToThread  POST /admin-api/support-threads/:id/messages
func (controller *Controller) ReplyToThread(httpContext *gin.Context) {
	var input requests.MessageRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	if err := controller.application.Backoffice.ReplyToThread(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), httpContext.Param("id"), input.Body); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "Reply sent.", nil)
}

// SetThreadStatus  PATCH /admin-api/support-threads/:id/status
func (controller *Controller) SetThreadStatus(httpContext *gin.Context) {
	var input requests.SupportStatusRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	if err := controller.application.Backoffice.SetThreadStatus(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), httpContext.Param("id"), input.Status); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Status saved.", nil)
}

// ─── Logs and jobs ───────────────────────────────────────────────────

// ApiRequests  GET /admin-api/api-requests?status=2xx|4xx|5xx&page=
func (controller *Controller) ApiRequests(httpContext *gin.Context) {
	page, err := controller.application.Backoffice.ListApiRequests(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), apiRequestFilters(httpContext, httpContext.Query("customer_id")))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "API requests loaded.", page)
}

// AuditLogs  GET /admin-api/audit-logs?action=&page=
func (controller *Controller) AuditLogs(httpContext *gin.Context) {
	page, err := controller.application.Backoffice.ListAuditLogs(httpContext.Request.Context(), httpContext.Query("action"), pageNumber(httpContext))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Audit log loaded.", page)
}

// Jobs  GET /admin-api/jobs  -> cron history + queue sizes + failed queue jobs + schedule, all at once.
func (controller *Controller) Jobs(httpContext *gin.Context) {
	runs, err := controller.application.Backoffice.ListJobRuns(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Jobs loaded.", gin.H{
		"schedule": controller.jobSchedule(), "cron_runs": runs, "queues": controller.jobQueues(), "failed_jobs": controller.failedJobs(),
	})
}

// JobRuns  GET /admin-api/jobs/runs  -> the latest cron runs
func (controller *Controller) JobRuns(httpContext *gin.Context) {
	runs, err := controller.application.Backoffice.ListJobRuns(httpContext.Request.Context())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Cron runs loaded.", runs)
}

// JobQueues  GET /admin-api/jobs/queues  -> the size of each queue
func (controller *Controller) JobQueues(httpContext *gin.Context) {
	responses.OK(httpContext, "Queues loaded.", controller.jobQueues())
}

// FailedJobs  GET /admin-api/jobs/failed  -> queue jobs that ran out of retries
func (controller *Controller) FailedJobs(httpContext *gin.Context) {
	responses.OK(httpContext, "Failed jobs loaded.", controller.failedJobs())
}

// JobSchedule  GET /admin-api/jobs/schedule  -> every cron job and when it runs
func (controller *Controller) JobSchedule(httpContext *gin.Context) {
	responses.OK(httpContext, "Schedule loaded.", controller.jobSchedule())
}

// jobQueueNames are the asynq queues the worker serves.
var jobQueueNames = []string{"critical", "default", "low"}

// jobQueues reads each queue's sizes from Redis (asynq). A queue that has
// never had a job does not exist yet and is left out. Never nil.
func (controller *Controller) jobQueues() []gin.H {
	queues := []gin.H{}
	for _, queueName := range jobQueueNames {
		info, err := controller.application.Inspector.GetQueueInfo(queueName)
		if err != nil {
			continue // the queue does not exist until its first job
		}
		queues = append(queues, gin.H{"name": queueName, "pending": info.Pending, "active": info.Active, "scheduled": info.Scheduled,
			"retry": info.Retry, "failed": info.Archived, "processed_today": info.Processed})
	}
	return queues
}

// failedJobs lists up to 20 archived (failed for good) jobs per queue. Never nil.
func (controller *Controller) failedJobs() []gin.H {
	failed := []gin.H{}
	for _, queueName := range jobQueueNames {
		archived, err := controller.application.Inspector.ListArchivedTasks(queueName, asynq.PageSize(20))
		if err != nil {
			continue // the queue does not exist until its first job
		}
		for _, task := range archived {
			failed = append(failed, gin.H{"id": task.ID, "queue": queueName, "type": task.Type, "error": task.LastErr, "failed_at": task.LastFailedAt})
		}
	}
	return failed
}

// jobSchedule lists the cron jobs from app/console/schedule.go. Never nil.
func (controller *Controller) jobSchedule() []gin.H {
	schedule := []gin.H{}
	for _, job := range console.Jobs(controller.application) {
		schedule = append(schedule, gin.H{"name": job.Name, "cron": job.Schedule})
	}
	return schedule
}

// RetryJob  POST /admin-api/jobs/:queue/:id/retry  -> puts a failed job back on the queue.
func (controller *Controller) RetryJob(httpContext *gin.Context) {
	if err := controller.application.Inspector.RunTask(httpContext.Param("queue"), httpContext.Param("id")); err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), "jobs.retry", httpContext.Param("id"), "Retried a failed job", nil, nil)
	responses.OK(httpContext, "Job queued again.", nil)
}

// RunDailySnapshot  POST /admin-api/jobs/daily-snapshot/run  -> CRON EXAMPLE you can trigger by hand.
func (controller *Controller) RunDailySnapshot(httpContext *gin.Context) {
	message, err := console.RunDailySnapshot(context.WithoutCancel(httpContext.Request.Context()), controller.application)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.AdminAuth.Audit(httpContext.Request.Context(), middleware.CurrentAdmin(httpContext), "jobs.run", "ledger:daily-snapshot", message, nil, nil)
	responses.OK(httpContext, message, nil)
}
