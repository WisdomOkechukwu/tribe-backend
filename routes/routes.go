// Package routes lists every URL, like Laravel's routes/api.php.
//
// The two apps use DIFFERENT URL styles on purpose:
//
//	Mobile app:   /api/mobile/v1/...   versioned (old app versions keep working), kebab-case
//	Admin panel:  /admin-api/...       unversioned REST resources (always deployed together)
//
// Read a route like a sentence:
//
//	money.POST("/swaps", idempotent, moneyController.Swap)
//	"a POST to /swaps must carry an Idempotency-Key, then MoneyController.Swap runs"
package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/middleware"
	"github.com/tribe-africa/tribe-backend/app/http/responses"
)

// Build creates the router with every route.
func Build(application *bootstrap.App) *gin.Engine {
	if application.Settings.IsProduction() {
		gin.SetMode(gin.ReleaseMode)
	}
	router := gin.New()
	// Only trust X-Forwarded-For from our own load balancer, otherwise anyone
	// could fake their IP address and dodge the rate limits.
	if err := router.SetTrustedProxies(application.Settings.App.TrustedProxies); err != nil {
		panic("TRUSTED_PROXIES is not a valid list of addresses: " + err.Error())
	}
	router.Use(gin.Recovery(), middleware.RequestID(), middleware.Cors(application.Settings.App.AllowedOrigins),
		// Read every body once, with a size and time limit, BEFORE the API
		// trail, rate limits or authentication touch it.
		middleware.ReadRequestBody(application.Settings.App.RequestBodyMaxBytes, application.Settings.App.RequestBodyReadTimeout))

	registerSystemRoutes(router, application)
	registerMobileRoutes(router, application)
	registerAdminRoutes(router, application)

	router.NoRoute(func(httpContext *gin.Context) {
		httpContext.JSON(http.StatusNotFound, responses.ErrorBody{Success: false, Message: "This endpoint does not exist.", Code: "NOT_FOUND"})
	})
	return router
}
