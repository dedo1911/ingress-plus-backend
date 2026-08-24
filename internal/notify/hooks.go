package notify

import (
	"github.com/pocketbase/pocketbase/core"
)

// RegisterHooks wires the record notifications that used to live in
// pb_hooks/telegram.pb.js. The JS used the model-level
// onModelAfterCreateSuccess/onModelAfterDeleteSuccess hooks filtered by
// collection name; the record-level equivalents below fire for the same
// changes but hand us a *core.Record directly.
//
// Registering the hooks is unconditional even when the client is not
// configured - Send/SendAsync no-op in that case - so that a missing token
// can't silently change which hooks exist.
func RegisterHooks(app core.App, t *Telegram) {
	app.OnRecordAfterCreateSuccess("badges").BindFunc(func(e *core.RecordEvent) error {
		t.SendAsync(e.App.Logger(), t.Topics.Badges, Message("Badge created", e.Record.GetString("title")))
		return e.Next()
	})

	app.OnRecordAfterUpdateSuccess("badges").BindFunc(func(e *core.RecordEvent) error {
		t.SendAsync(e.App.Logger(), t.Topics.Badges, Message("Badge updated", e.Record.GetString("title")))
		return e.Next()
	})

	app.OnRecordAfterDeleteSuccess("badges").BindFunc(func(e *core.RecordEvent) error {
		t.SendAsync(e.App.Logger(), t.Topics.Badges, Message("Badge deleted", e.Record.GetString("title")))
		return e.Next()
	})

	app.OnRecordAfterCreateSuccess("bug_reports").BindFunc(func(e *core.RecordEvent) error {
		t.SendAsync(e.App.Logger(), t.Topics.BugReports, Message("Bug report created", e.Record.GetString("title")))
		return e.Next()
	})
}
