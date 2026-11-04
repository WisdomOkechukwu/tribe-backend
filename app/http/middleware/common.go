package middleware

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
	"github.com/tribe-africa/tribe-backend/app/bootstrap"
	"github.com/tribe-africa/tribe-backend/app/http/responses"
	"github.com/tribe-africa/tribe-backend/app/models"
	"github.com/tribe-africa/tribe-backend/pkg/ids"
)

// RequestID gives every request an id (returned in the X-Request-Id header)
// so a support ticket can be matched with the log line.
func RequestID() gin.HandlerFunc {
	return func(httpContext *gin.Context) {
		requestID := ids.New()
		httpContext.Set("request_id", requestID)
		httpContext.Header("X-Request-Id", requestID)
		httpContext.Next()
	}
}

// Cors lets the admin website call the API from the browser.
func Cors(allowedOrigins []string) gin.HandlerFunc {
	allowed := map[string]bool{}
	for _, origin := range allowedOrigins {
		allowed[origin] = true
	}
	return func(httpContext *gin.Context) {
		origin := httpContext.GetHeader("Origin")
		if allowed[origin] {
			httpContext.Header("Access-Control-Allow-Origin", origin)
			httpContext.Header("Vary", "Origin")
			httpContext.Header("Access-Control-Allow-Headers", "Authorization, Content-Type, Idempotency-Key, X-Device-Imei")
			httpContext.Header("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		}
		if httpContext.Request.Method == http.MethodOptions {
			httpContext.AbortWithStatus(http.StatusNoContent)
			return
		}
		httpContext.Next()
	}
}

// RateLimit allows at most `limit` requests per minute per IP for a route group.
// It uses a Redis counter that expires after a minute (a "fixed window").
func RateLimit(application *bootstrap.App, group string, limit int) gin.HandlerFunc {
	return func(httpContext *gin.Context) {
		if application.Settings.App.RateLimitsOff && !application.Settings.IsProduction() {
			httpContext.Next()
			return
		}
		window := time.Now().Unix() / 60
		key := "rate-limit:" + group + ":" + httpContext.ClientIP() + ":" + strconv.FormatInt(window, 10)
		count, err := application.Redis.Incr(httpContext.Request.Context(), key).Result()
		if err == nil && count == 1 {
			application.Redis.Expire(httpContext.Request.Context(), key, time.Minute)
		}
		if err == nil && count > int64(limit) {
			responses.Error(httpContext, apperrors.New(http.StatusTooManyRequests, "TOO_MANY_REQUESTS", "Slow down a little and try again."))
			return
		}
		httpContext.Next()
	}
}

// keyRawBody is where ReadRequestBody keeps the body it read, so the API
// trail and the idempotency check can share ONE copy instead of each
// reading (and copying) the whole body again.
const keyRawBody = "raw_request_body"

// ReadRequestBody reads the request body ONCE, up front, with two limits:
//
//   - size: a body bigger than maximumBytes is refused with 413 before it
//     is held in memory (http.MaxBytesReader stops reading at the limit);
//   - time: the body must arrive within readTimeout, so a client sending
//     one byte every few seconds cannot keep a connection busy forever.
//
// The read deadline is set only while the body is being read and is cleared
// straight after, so slow handlers and the live streams are not affected.
// (A server-wide ReadTimeout would cut the live-rates stream off.)
func ReadRequestBody(maximumBytes int64, readTimeout time.Duration) gin.HandlerFunc {
	return func(httpContext *gin.Context) {
		request := httpContext.Request
		if request.Body == nil || request.Body == http.NoBody || request.Method == http.MethodGet || request.Method == http.MethodHead {
			httpContext.Next()
			return
		}
		if request.ContentLength > maximumBytes {
			responses.Error(httpContext, apperrors.New(http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "This request is too large."))
			return
		}
		controller := http.NewResponseController(httpContext.Writer)
		_ = controller.SetReadDeadline(time.Now().Add(readTimeout)) // not every writer supports it; the size limit still applies
		body, err := io.ReadAll(http.MaxBytesReader(httpContext.Writer, request.Body, maximumBytes))
		_ = controller.SetReadDeadline(time.Time{}) // body done: no deadline for the handler
		if err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				responses.Error(httpContext, apperrors.New(http.StatusRequestEntityTooLarge, "BODY_TOO_LARGE", "This request is too large."))
				return
			}
			responses.Error(httpContext, apperrors.New(http.StatusRequestTimeout, "BODY_INCOMPLETE", "The request did not arrive in full. Please try again."))
			return
		}
		httpContext.Set(keyRawBody, body)
		request.Body = io.NopCloser(bytes.NewReader(body))
		httpContext.Next()
	}
}

// requestBodyOf returns the body read by ReadRequestBody. If that middleware
// did not run (a route outside the main router), it reads the body itself
// and puts it back for the controller.
func requestBodyOf(httpContext *gin.Context) ([]byte, error) {
	if stored, found := httpContext.Get(keyRawBody); found {
		return stored.([]byte), nil
	}
	if httpContext.Request.Body == nil {
		return nil, nil
	}
	body, err := io.ReadAll(httpContext.Request.Body)
	if err != nil {
		return nil, err
	}
	httpContext.Set(keyRawBody, body)
	httpContext.Request.Body = io.NopCloser(bytes.NewReader(body))
	return body, nil
}

// sensitiveKey matches JSON keys whose values are hidden in logged bodies.
// Any key that CONTAINS password, pin, token or secret is hidden (so
// verification_token and temporary_password are caught too), plus codes and
// identity, card and account numbers.
var sensitiveKey = regexp.MustCompile(`(?i)^(?:[a-z_]*(?:password|pin|token|secret)[a-z_]*|code|debug_code|otp|bvn|nin|cvv|number|[a-z_]*_number|details|answers)$`)

// sensitiveFields is the fallback for bodies that are not valid JSON (for
// example cut off): the same keys, matched with a regular expression. The
// last value pattern, "...$, also catches a string cut off before its
// closing quote, so half a secret is never stored.
var sensitiveFields = regexp.MustCompile(`(?i)"([a-z_]*(?:password|pin|token|secret)[a-z_]*|code|debug_code|otp|bvn|nin|cvv|number|[a-z_]*_number|details|answers)"\s*:\s*("(?:[^"\\]|\\.)*"|-?[0-9.]+|\{[^{}]*\}|\[[^\[\]]*\]|"(?:[^"\\]|\\.)*$)`)

// unloggedBodies are paths whose bodies are never stored, because they carry
// sign-in secrets or new passwords (auth, admin sessions, team invites).
var unloggedBodies = regexp.MustCompile(`^/api/mobile/v1/auth/|^/admin-api/sessions|^/admin-api/team-members`)

// loggedBodyLimit is the most characters of a body kept in the API trail.
const loggedBodyLimit = 8000

// capturedResponseLimit is the most response bytes the API trail copies.
// Big admin lists are cut here instead of being copied whole into memory.
const capturedResponseLimit = 64 * 1024

// redact hides passwords, PINs, codes and other secrets in a JSON body, and
// THEN cuts it to loggedBodyLimit. Redacting first means a cut can never
// split a secret and leave part of it visible.
func redact(body []byte) string {
	var parsed any
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber() // keep numbers exactly as sent
	if decoder.Decode(&parsed) == nil {
		if cleaned, err := json.Marshal(redactValue(parsed)); err == nil {
			return truncate(string(cleaned), loggedBodyLimit)
		}
	}
	return truncate(sensitiveFields.ReplaceAllString(string(body), `"$1":"[hidden]"`), loggedBodyLimit)
}

// redactValue walks parsed JSON at any depth and hides sensitive keys,
// including ones inside nested objects and lists.
func redactValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		for key, inner := range typed {
			if sensitiveKey.MatchString(key) {
				typed[key] = "[hidden]"
			} else {
				typed[key] = redactValue(inner)
			}
		}
		return typed
	case []any:
		for index, inner := range typed {
			typed[index] = redactValue(inner)
		}
		return typed
	default:
		return value
	}
}

// RequestLogger writes every API call to api_request_logs (the admin "API
// trail"), with secrets hidden. Streaming routes are skipped.
//
// Memory stays bounded: the request body was already size-limited by
// ReadRequestBody, at most 64 KB of the response is copied, and the row is
// handed to the batched apitrail.Writer instead of a goroutine per request.
func RequestLogger(application *bootstrap.App, actorType string) gin.HandlerFunc {
	return func(httpContext *gin.Context) {
		if strings.HasSuffix(httpContext.Request.URL.Path, "/stream") {
			httpContext.Next()
			return
		}
		started := time.Now()
		bodiesLogged := !unloggedBodies.MatchString(httpContext.Request.URL.Path)
		requestBody, err := requestBodyOf(httpContext)
		if err != nil {
			responses.Error(httpContext, apperrors.New(http.StatusBadRequest, "BODY_UNREADABLE", "The request could not be read."))
			return
		}
		captureLimit := capturedResponseLimit
		if !bodiesLogged {
			captureLimit = 0 // nothing to keep
		}
		recorder := &responseRecorder{ResponseWriter: httpContext.Writer, body: &bytes.Buffer{}, limit: captureLimit}
		httpContext.Writer = recorder
		httpContext.Next()

		log := models.ApiRequestLog{
			RequestID: httpContext.GetString("request_id"), ActorType: actorType,
			Method: httpContext.Request.Method, Path: httpContext.Request.URL.Path, Status: recorder.Status(),
			LatencyMs: int(time.Since(started).Milliseconds()), IPAddress: httpContext.ClientIP(),
			Device: truncate(httpContext.GetHeader("User-Agent"), 191), CreatedAt: time.Now().UTC(),
		}
		if userID := httpContext.GetString(keyUserID); userID != "" {
			log.UserID = &userID
		}
		if !bodiesLogged {
			hidden := `"[not logged: contains sign-in secrets]"`
			log.RequestBody = &hidden
			log.ResponseBody = &hidden
		} else {
			if len(requestBody) > 0 {
				text := redact(requestBody)
				log.RequestBody = &text
			}
			responseText := redact(recorder.body.Bytes())
			log.ResponseBody = &responseText
		}
		if len(httpContext.Errors) > 0 {
			message := truncate(httpContext.Errors.Last().Error(), 255)
			log.ErrorMessage = &message
		}
		// Queued, not written here: the user never waits for the log write.
		application.ApiTrail.Add(log)
	}
}

// truncate cuts text to a maximum length.
func truncate(text string, maximumLength int) string {
	if len(text) <= maximumLength {
		return text
	}
	return text[:maximumLength]
}
