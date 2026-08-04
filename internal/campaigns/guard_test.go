package campaigns_test

import (
	"testing"

	"github.com/dedo1911/ingress-plus-backend/internal/campaigns"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
	"github.com/pocketbase/pocketbase/tools/router"
)

func TestCanTransitionStatus(t *testing.T) {
	scenarios := []struct {
		oldStatus string
		newStatus string
		allowed   bool
	}{
		// normal composer lifecycle
		{"draft", "draft", true},
		{"draft", "queued", true},
		{"failed", "queued", true},
		{"failed", "draft", true},
		{"queued", "queued", true}, // retry of a failed dispatch call re-targets the same record
		{"queued", "draft", true},  // cancel before the cron picks it up

		// the duplicate-send regressions this guard exists for
		{"sent", "queued", false},
		{"sent", "draft", false},
		{"sent", "failed", false},
		{"sending", "queued", false},
		{"sending", "draft", false},

		// no-op and recovery
		{"sent", "sent", true},
		{"sending", "sending", true},
		{"sending", "failed", true}, // manual recovery of a campaign stuck mid-send
	}

	for _, s := range scenarios {
		if got := campaigns.CanTransitionStatus(s.oldStatus, s.newStatus); got != s.allowed {
			t.Errorf("CanTransitionStatus(%q, %q) = %v, want %v", s.oldStatus, s.newStatus, got, s.allowed)
		}
	}
}

func TestGuardCampaignUpdateRequest(t *testing.T) {
	app, err := tests.NewTestApp()
	if err != nil {
		t.Fatal(err)
	}
	defer app.Cleanup()

	collection := core.NewBaseCollection("email_campaigns")
	collection.Fields.Add(&core.TextField{Name: "status"})
	if err := app.Save(collection); err != nil {
		t.Fatalf("failed to create test collection: %v", err)
	}

	setupRecord := func(t *testing.T, oldStatus, newStatus string) *core.Record {
		record := core.NewRecord(collection)
		record.Set("status", oldStatus)
		if err := app.Save(record); err != nil {
			t.Fatalf("failed to save test record: %v", err)
		}
		// reload so Original() reflects the persisted state, mirroring how
		// the record update endpoint loads the record before applying the
		// submitted data
		loaded, err := app.FindRecordById("email_campaigns", record.Id)
		if err != nil {
			t.Fatalf("failed to reload test record: %v", err)
		}
		loaded.Set("status", newStatus)
		return loaded
	}

	t.Run("blocks re-queueing a sent campaign", func(t *testing.T) {
		event := new(core.RecordRequestEvent)
		event.Record = setupRecord(t, "sent", "queued")

		err := campaigns.GuardCampaignUpdateRequest(event)
		if err == nil {
			t.Fatal("expected an error, got nil")
		}
		apiErr, ok := err.(*router.ApiError)
		if !ok {
			t.Fatalf("expected *router.ApiError, got %T (%v)", err, err)
		}
		if apiErr.Status != 409 {
			t.Fatalf("expected status 409, got %d", apiErr.Status)
		}
	})

	t.Run("allows queueing a draft", func(t *testing.T) {
		event := new(core.RecordRequestEvent)
		event.Record = setupRecord(t, "draft", "queued")

		if err := campaigns.GuardCampaignUpdateRequest(event); err != nil {
			t.Fatalf("expected no error, got %v", err)
		}
	})
}
