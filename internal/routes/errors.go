package routes

import (
	"log/slog"

	"github.com/pocketbase/pocketbase/core"
)

// newErrorResponse logs err and writes a JSON error body. The underlying
// error text is only echoed back for 4xx responses, where it describes the
// caller's own input; a 5xx carries raw SQL/driver messages, and the upload
// routes are unauthenticated and put the body straight into a window.alert.
func newErrorResponse(e *core.RequestEvent, err error, status int, msg string) error {
	e.App.Logger().ErrorContext(e.Request.Context(), msg, slog.Any("error", err))
	body := map[string]any{"error": msg}
	if status < 500 {
		body["details"] = err.Error()
	}
	return e.JSON(status, body)
}
