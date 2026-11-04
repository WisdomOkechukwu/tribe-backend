package mobile

import (
	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/middleware"
	"github.com/tribe-africa/tribe-backend/app/http/requests"
	"github.com/tribe-africa/tribe-backend/app/http/resources"
	"github.com/tribe-africa/tribe-backend/app/http/responses"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/app/services/account"
)

// ProfileController handles the user's own information, PIN, devices and KYC.
type ProfileController struct {
	application *bootstrap.App
}

// NewProfileController creates the controller.
func NewProfileController(application *bootstrap.App) *ProfileController {
	return &ProfileController{application: application}
}

// userResponse sends the user with the has_pin flag and the open KYC request.
func (controller *ProfileController) userResponse(httpContext *gin.Context, message string, user *models.User) {
	hasPin := controller.application.Auth.HasPin(controller.application.Database.WithContext(httpContext.Request.Context()), user.ID)
	controller.application.Account.AttachOpenKyc(httpContext.Request.Context(), user)
	responses.OK(httpContext, message, resources.User(user, hasPin))
}

// Show  GET /profile
func (controller *ProfileController) Show(httpContext *gin.Context) {
	user, err := controller.application.Account.Profile(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.userResponse(httpContext, "Profile loaded.", user)
}

// Update  PATCH /profile
func (controller *ProfileController) Update(httpContext *gin.Context) {
	var input requests.UpdateProfileRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	user, err := controller.application.Account.UpdateProfile(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, account.ProfileChanges{
		FirstName: input.FirstName, LastName: input.LastName, Address: input.Address, DateOfBirth: input.DateOfBirth,
		World: input.World, BiometricsEnabled: input.BiometricsEnabled, TwoFactorEnabled: input.TwoFactorEnabled,
	})
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.userResponse(httpContext, "Profile saved.", user)
}

// ChangeTag  PATCH /profile/tag
func (controller *ProfileController) ChangeTag(httpContext *gin.Context) {
	var input requests.TagRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	user, err := controller.application.Account.ChangeTag(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, input.Tag)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.userResponse(httpContext, "Your @tag was changed.", user)
}

// CompleteOnboarding  POST /onboarding/complete
func (controller *ProfileController) CompleteOnboarding(httpContext *gin.Context) {
	var input requests.OnboardingRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	user, err := controller.application.Account.CompleteOnboarding(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, input.World)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.userResponse(httpContext, "All set.", user)
}

// CreatePin  POST /pin
func (controller *ProfileController) CreatePin(httpContext *gin.Context) {
	var input requests.PinRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	if err := controller.application.Auth.SetPin(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, input.Pin); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.Created(httpContext, "PIN created.", gin.H{"has_pin": true})
}

// ChangePin  POST /pin/change
func (controller *ProfileController) ChangePin(httpContext *gin.Context) {
	var input requests.ChangePinRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	if err := controller.application.Auth.ChangePin(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, input.CurrentPin, input.NewPin); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "PIN changed.", nil)
}

// ChangePassword  POST /password/change
func (controller *ProfileController) ChangePassword(httpContext *gin.Context) {
	var input requests.ChangePasswordRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	if err := controller.application.Auth.ChangePassword(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, input.CurrentPassword, input.NewPassword); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Password changed.", nil)
}

// SendPhoneCode  POST /phone/send-otp
func (controller *ProfileController) SendPhoneCode(httpContext *gin.Context) {
	var input requests.PhoneRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	code, err := controller.application.Account.SendPhoneCode(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, input.Phone)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	data := gin.H{"sent": true}
	if controller.application.Settings.IsLocal() {
		data["debug_code"] = code
	}
	responses.OK(httpContext, "We texted you a code.", data)
}

// VerifyPhone  POST /phone/verify
func (controller *ProfileController) VerifyPhone(httpContext *gin.Context) {
	var input requests.CodeRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	user, err := controller.application.Account.VerifyPhone(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, input.Code)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.userResponse(httpContext, "Phone verified.", user)
}

// Devices  GET /devices  -> the active phone and the history of replaced ones.
func (controller *ProfileController) Devices(httpContext *gin.Context) {
	actor := middleware.CurrentActor(httpContext)
	devices, err := controller.application.Account.Devices(httpContext.Request.Context(), actor.UserID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Devices loaded.", resources.Devices(devices, actor.DeviceID))
}

// SavePushToken  POST /push-tokens
func (controller *ProfileController) SavePushToken(httpContext *gin.Context) {
	var input requests.PushTokenRequest
	if err := requests.Bind(httpContext, &input); err != nil {
		responses.Error(httpContext, err)
		return
	}
	if err := controller.application.Account.SavePushToken(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, input.Token, input.Platform); err != nil {
		responses.Error(httpContext, err)
		return
	}
	responses.OK(httpContext, "Push token saved.", nil)
}

// Kyc  GET /kyc  -> tier, status, the open request, each tier's state and every check.
func (controller *ProfileController) Kyc(httpContext *gin.Context) {
	overview, err := controller.application.Account.KycStatus(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	list := make([]gin.H, 0, len(overview.Checks))
	for _, check := range overview.Checks {
		list = append(list, gin.H{"id": check.ID, "tier": check.Tier, "check": check.CheckType, "status": check.Status,
			"reference": check.Reference, "document_masked": check.DocumentMasked, "submitted_at": check.SubmittedAt, "reason": check.Reason})
	}
	var open any
	if overview.Open != nil {
		open = gin.H{"id": overview.Open.ID, "tier": overview.Open.Tier, "submitted_at": overview.Open.SubmittedAt}
	}
	responses.OK(httpContext, "KYC loaded.", gin.H{
		"tier": overview.User.KycTier, "status": overview.User.KycStatus, "open": open, "tiers": overview.Tiers, "checks": list,
	})
}

// SubmitKyc  POST /kyc/tier-1 | tier-2 | tier-3
func (controller *ProfileController) SubmitKyc(tier int) gin.HandlerFunc {
	return func(httpContext *gin.Context) {
		var input requests.KycRequest
		if err := requests.Bind(httpContext, &input); err != nil {
			responses.Error(httpContext, err)
			return
		}
		user, err := controller.application.Account.SubmitKyc(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, account.KycInput{
			Tier: tier, Bvn: input.Bvn, DateOfBirth: input.DateOfBirth, Nin: input.Nin,
			DocumentType: input.DocumentType, DocumentNumber: input.DocumentNumber, Address: input.Address,
		})
		if err != nil {
			responses.Error(httpContext, err)
			return
		}
		controller.userResponse(httpContext, "Thanks! We're checking your details.", user)
	}
}

// ApplyGlobalAccounts  POST /global-accounts/apply
func (controller *ProfileController) ApplyGlobalAccounts(httpContext *gin.Context) {
	var answers map[string]any
	if err := httpContext.ShouldBindJSON(&answers); err != nil {
		responses.Error(httpContext, err)
		return
	}
	user, err := controller.application.Account.ApplyForGlobalAccounts(httpContext.Request.Context(), middleware.CurrentActor(httpContext).UserID, answers)
	if err != nil {
		responses.Error(httpContext, err)
		return
	}
	controller.userResponse(httpContext, "Application received.", user)
}
