// Package mobile holds the controllers for the mobile app routes
// (/api/mobile/v1/...). A controller method should stay SHORT:
//
//  1. read and validate the input   (requests.Bind)
//  2. call ONE service method        (the real work lives in app/services)
//  3. send the response              (responses.OK / responses.Error)
package mobile

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/middleware"
	"github.com/tribe-africa/tribe-backend/app/http/requests"
	"github.com/tribe-africa/tribe-backend/app/http/resources"
	"github.com/tribe-africa/tribe-backend/app/http/responses"
	"github.com/tribe-africa/tribe-backend/app/services/auth"
)

// AuthController handles sign-up, sign-in and the single-device flow.
type AuthController struct {
	application *bootstrap.App
}

// NewAuthController creates the controller.
func NewAuthController(application *bootstrap.App) *AuthController {
	return &AuthController{application: application}
}

// CheckEmail  POST /auth/email/check  -> is this email already registered?
func (controller *AuthController) CheckEmail(httpContext *gin.Context) {
	var input requests.EmailRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	taken, err := controller.application.Auth.EmailIsTaken(httpContext.Request.Context(), input.Email)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Email checked.", gin.H{"available": !taken})
}

// SendEmailCode  POST /auth/email/send-code  -> emails a 6-digit code (queued job).
func (controller *AuthController) SendEmailCode(httpContext *gin.Context) {
	var input struct {
		Email   string `json:"email" binding:"required,email"`
		Purpose string `json:"purpose" binding:"omitempty,oneof=signup password_reset"`
	}
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	if input.Purpose == "" {
		input.Purpose = auth.PurposeSignup
	}
	code, err := controller.application.Auth.SendEmailCode(httpContext.Request.Context(), input.Email, input.Purpose, httpContext.ClientIP())
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	data := gin.H{"sent": true}
	if controller.application.Settings.IsLocal() {
		data["debug_code"] = code // ONLY on a developer machine, so you can test without email
	}
	responses.OK(httpContext, "We sent you a code.", data)
}

// VerifyEmail  POST /auth/email/verify  -> returns a verification_token for the next step.
func (controller *AuthController) VerifyEmail(httpContext *gin.Context) {
	var input requests.VerifyEmailRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	if input.Purpose == "" {
		input.Purpose = auth.PurposeSignup
	}
	token, err := controller.application.Auth.VerifyEmailCode(httpContext.Request.Context(), input.Email, input.Purpose, input.Code)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Email verified.", gin.H{"verification_token": token})
}

// TagAvailable  GET /auth/tags/:tag/available
func (controller *AuthController) TagAvailable(httpContext *gin.Context) {
	available, err := controller.application.Auth.TagIsAvailable(httpContext.Request.Context(), httpContext.Param("tag"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Tag checked.", gin.H{"available": available})
}

// Register  POST /auth/register  -> creates the account; this phone becomes the active device.
func (controller *AuthController) Register(httpContext *gin.Context) {
	var input requests.RegisterRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	user, tokens, err := controller.application.Auth.Register(httpContext.Request.Context(), auth.RegisterInput{
		Email: input.Email, VerificationToken: input.VerificationToken, Password: input.Password,
		FirstName: input.FirstName, LastName: input.LastName, Tag: input.Tag, Phone: input.Phone, Country: input.Country,
		ReferralCode: input.ReferralCode, HeardFrom: input.HeardFrom, Device: deviceInput(input.Device),
		IPAddress: httpContext.ClientIP(), UserAgent: httpContext.GetHeader("User-Agent"),
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.application.Account.AttachOpenKyc(httpContext.Request.Context(), user)
	responses.Created(httpContext, "Welcome to Tribe!", gin.H{"user": resources.User(user, false), "tokens": tokens})
}

// Login  POST /auth/login
// Same phone (or first phone) -> 200 with tokens.
// Different phone             -> 202 with next = "verify_new_device".
func (controller *AuthController) Login(httpContext *gin.Context) {
	var input requests.LoginRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	result, err := controller.application.Auth.Login(httpContext.Request.Context(), auth.LoginInput{
		Email: input.Email, Password: input.Password, Device: deviceInput(input.Device),
		IPAddress: httpContext.ClientIP(), UserAgent: httpContext.GetHeader("User-Agent"),
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	if result.Tokens == nil {
		data := gin.H{"next": "verify_new_device", "verification_id": result.DeviceVerificationID, "email": result.User.Email}
		if result.DebugCode != "" {
			data["debug_code"] = result.DebugCode
		}
		httpContext.JSON(http.StatusAccepted, responses.SuccessBody{Success: true, Message: "We emailed you a code to confirm this new phone.", Data: data})
		return
	}
	hasPin := controller.application.Auth.HasPin(controller.application.Database.WithContext(httpContext.Request.Context()), result.User.ID)
	controller.application.Account.AttachOpenKyc(httpContext.Request.Context(), result.User)
	responses.OK(httpContext, "Signed in.", gin.H{"next": "home", "user": resources.User(result.User, hasPin), "tokens": result.Tokens})
}

// VerifyDevice  POST /auth/device/verify  -> activates this phone, signs the old one out.
func (controller *AuthController) VerifyDevice(httpContext *gin.Context) {
	var input requests.VerifyDeviceRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	user, tokens, err := controller.application.Auth.VerifyNewDevice(httpContext.Request.Context(), input.VerificationID, input.Code,
		httpContext.GetHeader(middleware.HeaderDeviceImei), httpContext.ClientIP(), httpContext.GetHeader("User-Agent"))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	hasPin := controller.application.Auth.HasPin(controller.application.Database.WithContext(httpContext.Request.Context()), user.ID)
	controller.application.Account.AttachOpenKyc(httpContext.Request.Context(), user)
	responses.OK(httpContext, "This phone is now your Tribe phone.", gin.H{"user": resources.User(user, hasPin), "tokens": tokens})
}

// Refresh  POST /auth/refresh
func (controller *AuthController) Refresh(httpContext *gin.Context) {
	var input requests.RefreshRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	tokens, err := controller.application.Auth.Refresh(httpContext.Request.Context(), input.RefreshToken, httpContext.GetHeader(middleware.HeaderDeviceImei))
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Tokens refreshed.", gin.H{"tokens": tokens})
}

// Logout  POST /auth/logout
func (controller *AuthController) Logout(httpContext *gin.Context) {
	if err := controller.application.Auth.RevokeSession(httpContext.Request.Context(), middleware.CurrentSessionID(httpContext), "logout"); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Signed out.", nil)
}

// ForgotPassword  POST /auth/password/forgot  -> emails a reset code.
// Always answers "sent" so nobody can find out which emails exist.
func (controller *AuthController) ForgotPassword(httpContext *gin.Context) {
	var input requests.EmailRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	data := gin.H{"sent": true}
	if taken, _ := controller.application.Auth.EmailIsTaken(httpContext.Request.Context(), input.Email); taken {
		code, err := controller.application.Auth.SendEmailCode(httpContext.Request.Context(), input.Email, auth.PurposePasswordReset, httpContext.ClientIP())
		if err != nil {
			responses.Error(httpContext, err)
			return
		}
		if controller.application.Settings.IsLocal() {
			data["debug_code"] = code
		}
	}
	responses.OK(httpContext, "If that email has an account, we sent a code.", data)
}

// ResetPassword  POST /auth/password/reset
func (controller *AuthController) ResetPassword(httpContext *gin.Context) {
	var input requests.ResetPasswordRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	if err := controller.application.Auth.ResetPassword(httpContext.Request.Context(), input.Email, input.VerificationToken, input.Password); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Password changed. Please sign in.", nil)
}

// deviceInput converts the request device into the service device.
func deviceInput(device requests.DeviceRequest) auth.DeviceInput {
	return auth.DeviceInput{Imei: device.Imei, Name: device.Name, Platform: device.Platform, Model: device.Model, AppVersion: device.AppVersion}
}
