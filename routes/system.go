package routes

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/controllers/system"
	"github.com/tribe-africa/tribe-backend/app/http/middleware"
	"github.com/tribe-africa/tribe-backend/app/http/responses"
)

// registerSystemRoutes adds health checks and provider webhooks.
func registerSystemRoutes(router *gin.Engine, application *bootstrap.App) {
	// Is the process alive?
	router.GET("/health/live", func(httpContext *gin.Context) {
		responses.OK(httpContext, "alive", gin.H{"status": "ok"})
	})
	// Can it reach MySQL and Redis?
	router.GET("/health/ready", func(httpContext *gin.Context) {
		requestContext := httpContext.Request.Context()
		pool, _ := application.Database.DB()
		databaseError := pool.PingContext(requestContext)
		redisError := application.Redis.Ping(requestContext).Err()
		status := http.StatusOK
		if databaseError != nil || redisError != nil {
			status = http.StatusServiceUnavailable
		}
		httpContext.JSON(status, gin.H{
			"success": status == http.StatusOK, "mysql": databaseError == nil, "redis": redisError == nil,
			"rate_stream_listeners":    application.RateHub.SubscriberCount(),
			"support_stream_listeners": application.SupportHub.SubscriberCount(),
		})
	})

	// Providers tell us about payouts here. Signed with HMAC; see app/services/webhooks.
	webhookController := system.NewWebhookController(application)
	router.POST("/webhooks/:provider", middleware.RateLimit(application, "webhooks", 600), webhookController.Receive)
}
