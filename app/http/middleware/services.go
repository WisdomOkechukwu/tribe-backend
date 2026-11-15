package middleware

import (
	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/responses"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
)

// ServiceEnabled stops the request with 503 SERVICE_DISABLED (and the
// admin's message) while an admin has the service switched off. serviceCode
// is one of the shared.Service... constants.
//
// Use it on routes where the route alone says which service it is, for
// example POST /swaps. Where the service depends on the request body (which
// biller, which kind of recipient) the service method calls
// shared.RequireService itself.
//
// Put it AFTER the idempotent middleware: a retry of a request that already
// went through then still gets its saved answer, even if the service was
// switched off in between. A 503 is never saved, so the same key works
// again once the service is back on.
//
// The switches come from the shared configuration copy, so this costs no
// query, and an admin's change is seen within about a second.
func ServiceEnabled(application *bootstrap.App, serviceCode string) gin.HandlerFunc {
	return func(httpContext *gin.Context) {
		if err := shared.RequireService(application.Database.WithContext(httpContext.Request.Context()), serviceCode); err != nil {
			responses.Error(httpContext, err)
			return
		}
		httpContext.Next()
	}
}
