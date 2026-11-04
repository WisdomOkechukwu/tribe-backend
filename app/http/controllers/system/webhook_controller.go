// Package system holds controllers for routes that are not for the mobile
// app or the admin panel: provider webhooks.
package system

import (
	"io"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/responses"
)

// HeaderSignature is the header a provider puts its HMAC signature in.
const HeaderSignature = "X-Tribe-Signature"

// maximumWebhookBytes stops anyone from sending us a huge body.
const maximumWebhookBytes = 64 * 1024

// WebhookController receives provider webhooks.
type WebhookController struct {
	application *bootstrap.App
}

// NewWebhookController creates the controller.
func NewWebhookController(application *bootstrap.App) *WebhookController {
	return &WebhookController{application: application}
}

// Receive  POST /webhooks/:provider
//
// The provider sends JSON like {"event_id":"evt_1","type":"payout.succeeded",
// "data":{"transaction_id":"01J..."}} with the header
// X-Tribe-Signature: sha256=<HMAC of the raw body>. See app/services/webhooks.
func (controller *WebhookController) Receive(httpContext *gin.Context) {
	rawBody, err := io.ReadAll(io.LimitReader(httpContext.Request.Body, maximumWebhookBytes+1))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	if len(rawBody) > maximumWebhookBytes {
		responses.Error(httpContext, apperrors.New(http.StatusRequestEntityTooLarge, "TOO_LARGE", "The webhook body is too large."))
		return
	}
	outcome, err := controller.application.Webhooks.Receive(httpContext.Request.Context(), httpContext.Param("provider"), rawBody, httpContext.GetHeader(HeaderSignature))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Webhook received.", outcome)
}
