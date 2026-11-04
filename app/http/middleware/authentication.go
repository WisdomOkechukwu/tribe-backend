// Package middleware holds code that runs BEFORE (and sometimes after) a
// controller, like Laravel middleware. A route lists its middleware in
// order; each one either calls httpContext.Next() to continue, or stops the
// request with an error response.
package middleware

import (
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/responses"
	"github.com/tribe-africa/tribe-backend/app/services/adminauth"
	"github.com/tribe-africa/tribe-backend/app/services/security"
	"github.com/tribe-africa/tribe-backend/app/services/shared"
)

// Keys used to store values on the request (httpContext.Set / Get).
const (
	keyUserID    = "user_id"
	keyDeviceID  = "device_id"
	keySessionID = "session_id"
	keyAdmin     = "admin"
)

// HeaderDeviceImei is the header every mobile request sends with the phone's ID.
const HeaderDeviceImei = "X-Device-Imei"

// AuthenticateUser checks the mobile access token ("Authorization: Bearer ...").
// A token made for the admin panel is refused here (different audience).
func AuthenticateUser(application *bootstrap.App) gin.HandlerFunc {
	return func(httpContext *gin.Context) {
		token := bearerToken(httpContext)
		claims, err := application.Toolkit.ParseAccessToken(token, security.AudienceMobile)
		if token == "" || err != nil {
			responses.Error(httpContext, apperrors.ErrUnauthenticated)
			return
		}
		httpContext.Set(keyUserID, claims.Subject)
		httpContext.Set(keyDeviceID, claims.DeviceID)
		httpContext.Set(keySessionID, claims.SessionID)
		httpContext.Next()
	}
}

// EnsureSingleDevice is the SINGLE-DEVICE guard. It runs after
// AuthenticateUser on every signed-in mobile route and checks:
//
//  1. the session still exists and was not revoked,
//  2. the phone of that session is still the ACTIVE phone (not replaced
//     by a sign-in on another phone),
//  3. the X-Device-Imei header matches that phone.
//
// If any check fails the app gets 401 DEVICE_NOT_ACTIVE and shows
// "You signed in on another phone".
//
// Sign-up and sign-in routes do NOT use this middleware: they are the
// "new entry" that decides which phone is active in the first place.
func EnsureSingleDevice(application *bootstrap.App) gin.HandlerFunc {
	return func(httpContext *gin.Context) {
		session, err := application.Auth.LoadSession(httpContext.Request.Context(), httpContext.GetString(keySessionID))
		if err != nil {
			responses.Error(httpContext, apperrors.ErrUnauthenticated)
			return
		}
		deviceImei := strings.TrimSpace(httpContext.GetHeader(HeaderDeviceImei))
		if !session.IsActive || deviceImei == "" || deviceImei != session.DeviceImei || session.DeviceID != httpContext.GetString(keyDeviceID) {
			responses.Error(httpContext, apperrors.ErrDeviceNotActive)
			return
		}
		if session.UserStatus == "closed" {
			responses.Error(httpContext, apperrors.ErrUnauthenticated)
			return
		}
		httpContext.Next()
	}
}

// CurrentActor returns the signed-in user and their phone.
func CurrentActor(httpContext *gin.Context) shared.Actor {
	return shared.Actor{UserID: httpContext.GetString(keyUserID), DeviceID: httpContext.GetString(keyDeviceID)}
}

// CurrentSessionID returns the session id of the signed-in user.
func CurrentSessionID(httpContext *gin.Context) string {
	return httpContext.GetString(keySessionID)
}

// AuthenticateAdmin checks the admin access token and loads the admin.
func AuthenticateAdmin(application *bootstrap.App) gin.HandlerFunc {
	return func(httpContext *gin.Context) {
		token := bearerToken(httpContext)
		claims, err := application.Toolkit.ParseAccessToken(token, security.AudienceAdmin)
		if token == "" || err != nil {
			responses.Error(httpContext, apperrors.ErrUnauthenticated)
			return
		}
		admin, err := application.AdminAuth.LoadAdmin(httpContext.Request.Context(), claims.Subject, claims.SessionID)
		if err != nil {
			responses.Error(httpContext, err)
			return
		}
		httpContext.Set(keyAdmin, admin)
		httpContext.Next()
	}
}

// RequirePermission stops the request unless the admin has the permission.
func RequirePermission(permission string) gin.HandlerFunc {
	return func(httpContext *gin.Context) {
		admin := CurrentAdmin(httpContext)
		if admin == nil || !admin.Can(permission) {
			responses.Error(httpContext, apperrors.ErrForbidden)
			return
		}
		httpContext.Next()
	}
}

// CurrentAdmin returns the signed-in admin (nil if none).
func CurrentAdmin(httpContext *gin.Context) *adminauth.Admin {
	value, exists := httpContext.Get(keyAdmin)
	if !exists {
		return nil
	}
	admin, _ := value.(*adminauth.Admin)
	return admin
}

// bearerToken reads "Authorization: Bearer <token>". Server-Sent Event
// connections from browsers can't set headers, so ?access_token= also works.
func bearerToken(httpContext *gin.Context) string {
	header := httpContext.GetHeader("Authorization")
	if token, found := strings.CutPrefix(header, "Bearer "); found {
		return strings.TrimSpace(token)
	}
	return httpContext.Query("access_token")
}
