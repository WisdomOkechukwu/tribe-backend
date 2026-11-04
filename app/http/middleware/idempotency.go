package middleware

import (
	"bytes"
	"context"
	"net/http"
	"regexp"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/responses"
	"github.com/tribe-africa/tribe-backend/app/services/idempotency"
	"github.com/tribe-africa/tribe-backend/app/services/security"
)

// HeaderIdempotencyKey is the header the app sends with every change.
const HeaderIdempotencyKey = "Idempotency-Key"

const keyIdempotencyRecordID = "idempotency_record_id"

// frontGuardLifetime is the longest a Redis front guard lives if the server
// dies before deleting it.
const frontGuardLifetime = 30 * time.Second

var validIdempotencyKey = regexp.MustCompile(`^[A-Za-z0-9_-]{8,100}$`)

// RequireIdempotencyKey makes a route safe to retry (see app/services/idempotency).
//
//  1. The request must carry an Idempotency-Key header.
//     A cheap Redis SETNX "front guard" turns away a second copy that
//     arrives while the first is still running (a double tap), before it
//     touches MySQL. If Redis is down we skip the guard: MySQL's UNIQUE
//     key below is still the real protection.
//  2. We hash the raw body and remember (user, key, hash).
//  3. Same key again -> replay the saved response, do NOT run the controller.
//  4. After the controller runs we save its response for future retries.
//     (Server errors are not saved, so the app can safely try again.)
//
// actorType is "user" (mobile) or "admin".
func RequireIdempotencyKey(application *bootstrap.App, actorType string) gin.HandlerFunc {
	return func(httpContext *gin.Context) {
		key := httpContext.GetHeader(HeaderIdempotencyKey)
		if !validIdempotencyKey.MatchString(key) {
			responses.Error(httpContext, apperrors.New(http.StatusBadRequest, "IDEMPOTENCY_KEY_REQUIRED", "Missing or invalid Idempotency-Key header."))
			return
		}
		// The body was already read (and size-limited) once by
		// ReadRequestBody; reuse that copy for the hash.
		body, err := requestBodyOf(httpContext)
		if err != nil {
			responses.Error(httpContext, apperrors.New(http.StatusBadRequest, "BODY_UNREADABLE", "The request could not be read."))
			return
		}

		actorID := httpContext.GetString(keyUserID)
		if actorType == "admin" {
			if admin := CurrentAdmin(httpContext); admin != nil {
				actorID = admin.ID
			}
		}
		guardKey := "idempotency-guard:" + actorType + ":" + actorID + ":" + key
		acquired, guardError := application.Redis.SetNX(httpContext.Request.Context(), guardKey, "1", frontGuardLifetime).Result()
		if guardError == nil && !acquired {
			responses.Error(httpContext, apperrors.New(http.StatusConflict, "REQUEST_IN_PROGRESS", "This request is already being processed."))
			return
		}
		if guardError == nil {
			// Let go of the guard when we finish, so a later retry reaches MySQL and gets the saved response.
			defer application.Redis.Del(context.Background(), guardKey)
		}

		record, stored, err := application.Idempotency.Begin(httpContext.Request.Context(), idempotency.Request{
			ActorType: actorType, ActorID: actorID, Key: key,
			Method: httpContext.Request.Method, Path: httpContext.Request.URL.Path, RequestHash: security.HashBody(body),
		})
		if err != nil {
			responses.Error(httpContext, err)
			return
		}
		if stored != nil {
			// A retry: answer exactly like the first time.
			httpContext.Header("Idempotent-Replay", "true")
			httpContext.Data(stored.Status, "application/json; charset=utf-8", stored.Body)
			httpContext.Abort()
			return
		}

		httpContext.Set(keyIdempotencyRecordID, record.ID)
		recorder := &responseRecorder{ResponseWriter: httpContext.Writer, body: &bytes.Buffer{}, limit: -1}
		httpContext.Writer = recorder
		httpContext.Next()

		status := recorder.Status()
		if status >= 500 {
			application.Idempotency.Release(httpContext.Request.Context(), record.ID)
			return
		}
		application.Idempotency.Complete(httpContext.Request.Context(), record.ID, status, recorder.body.Bytes())
	}
}

// BusinessReference returns a reference for the money operation that is the
// SAME on every retry of this request, e.g. "SWP-01JB...". Services use it
// as the journal reference, so a retry can never post twice.
func BusinessReference(httpContext *gin.Context, prefix string) string {
	return prefix + "-" + httpContext.GetString(keyIdempotencyRecordID)
}

// responseRecorder copies what is written to the response so we can save it.
// limit caps the copy (0 = keep nothing, -1 = keep everything). The
// idempotency check keeps everything, because a retry must get back the
// exact same answer; the API trail only needs the first 64 KB.
type responseRecorder struct {
	gin.ResponseWriter
	body  *bytes.Buffer
	limit int
}

// Write sends bytes to the client AND keeps a copy, up to the limit.
func (recorder *responseRecorder) Write(data []byte) (int, error) {
	if recorder.limit < 0 {
		recorder.body.Write(data)
	} else if room := recorder.limit - recorder.body.Len(); room > 0 {
		recorder.body.Write(data[:min(room, len(data))])
	}
	return recorder.ResponseWriter.Write(data)
}

// WriteString keeps a copy too. Without it gin's WriteString would bypass
// Write above and the copy would be empty.
func (recorder *responseRecorder) WriteString(text string) (int, error) {
	return recorder.Write([]byte(text))
}
