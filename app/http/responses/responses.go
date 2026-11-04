// Package responses builds the JSON every endpoint returns, so the mobile
// app and admin panel always get the same shape:
//
//	success: {"success": true,  "message": "...", "data": {...}, "meta": {...}}
//	error:   {"success": false, "message": "...", "code": "SOME_CODE", "errors": {"field": ["..."]}}
package responses

import (
	"log/slog"
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/tribe-africa/tribe-backend/app/apperrors"
)

// SuccessBody is the JSON shape of a successful response.
type SuccessBody struct {
	Success bool   `json:"success"`
	Message string `json:"message"`
	Data    any    `json:"data"`
	Meta    any    `json:"meta,omitempty"`
}

// ErrorBody is the JSON shape of a failed response.
type ErrorBody struct {
	Success bool                `json:"success"`
	Message string              `json:"message"`
	Code    string              `json:"code"`
	Errors  map[string][]string `json:"errors,omitempty"`
	Data    any                 `json:"data,omitempty"`
}

// Pagination is put in "meta" for list endpoints.
type Pagination struct {
	Page       int    `json:"page,omitempty"`
	PageSize   int    `json:"page_size,omitempty"`
	Total      int64  `json:"total,omitempty"`
	Pages      int    `json:"pages,omitempty"`
	NextCursor string `json:"next_cursor,omitempty"`
}

// OK sends 200 with data.
func OK(httpContext *gin.Context, message string, data any) {
	httpContext.JSON(http.StatusOK, SuccessBody{Success: true, Message: message, Data: data})
}

// OKWithMeta sends 200 with data and meta (usually pagination).
func OKWithMeta(httpContext *gin.Context, message string, data any, meta any) {
	httpContext.JSON(http.StatusOK, SuccessBody{Success: true, Message: message, Data: data, Meta: meta})
}

// Created sends 201 with data.
func Created(httpContext *gin.Context, message string, data any) {
	httpContext.JSON(http.StatusCreated, SuccessBody{Success: true, Message: message, Data: data})
}

// Accepted sends 202 (we started something that finishes later).
func Accepted(httpContext *gin.Context, message string, data any) {
	httpContext.JSON(http.StatusAccepted, SuccessBody{Success: true, Message: message, Data: data})
}

// Error sends the right error response for any error.
// AppErrors keep their status and message; anything else becomes a 500
// with a generic message (the real error is only written to the log).
func Error(httpContext *gin.Context, err error) {
	if appError := apperrors.As(err); appError != nil {
		httpContext.AbortWithStatusJSON(appError.Status, ErrorBody{
			Success: false,
			Message: appError.Message,
			Code:    appError.Code,
			Errors:  appError.FieldErrors,
		})
		return
	}
	slog.Error("unexpected error", "path", httpContext.FullPath(), "error", err)
	httpContext.Error(err)
	httpContext.AbortWithStatusJSON(http.StatusInternalServerError, ErrorBody{
		Success: false,
		Message: "Something went wrong on our side. Please try again.",
		Code:    "SERVER_ERROR",
	})
}
