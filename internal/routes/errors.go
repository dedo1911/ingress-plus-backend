package routes

import (
	"log/slog"

	"github.com/pocketbase/pocketbase/core"
)

func newErrorResponse(e *core.RequestEvent, err error, status int, msg string) error {
	e.App.Logger().ErrorContext(e.Request.Context(), msg, slog.Any("error", err))
	return e.JSON(status, map[string]any{
		"error":   msg,
		"details": err.Error(),
	})
}
