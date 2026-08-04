package campaigns

import (
	"fmt"
	"net/http"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/router"
)

// CanTransitionStatus reports whether a campaign status change is allowed
// through the record REST API. Once a campaign reaches "sending" or "sent",
// flipping it back to "queued"/"draft" would make the cron re-send it to the
// entire audience (there is no per-recipient ledger), so:
//   - "sent" is final: the status can never change again
//   - "sending" may only move to "failed" - the manual recovery step for a
//     campaign stuck mid-send after a crash; re-queueing it is then a
//     deliberate second step (failed -> queued), never a single accident
//
// Everything else (draft/failed/queued and edits that keep the status) is
// allowed. Internal Go-side saves (the dispatch flow itself) don't go
// through this check.
func CanTransitionStatus(oldStatus, newStatus string) bool {
	switch oldStatus {
	case "sent":
		return newStatus == "sent"
	case "sending":
		return newStatus == "sending" || newStatus == "failed"
	default:
		return true
	}
}

// GuardCampaignUpdateRequest blocks REST record updates that would regress a
// campaign's status (see CanTransitionStatus). Superusers bypass collection
// API rules, so without this hook a stale admin tab could re-queue an
// already-sent campaign and double-send it.
// Register on app.OnRecordUpdateRequest("email_campaigns").
func GuardCampaignUpdateRequest(e *core.RecordRequestEvent) error {
	oldStatus := e.Record.Original().GetString("status")
	newStatus := e.Record.GetString("status")

	if !CanTransitionStatus(oldStatus, newStatus) {
		return router.NewApiError(
			http.StatusConflict,
			fmt.Sprintf("Campaign status can't change from %q to %q - it was already sent or is being sent.", oldStatus, newStatus),
			nil,
		)
	}

	return e.Next()
}
