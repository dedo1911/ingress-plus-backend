package campaigns_test

import (
	"sync"
	"testing"

	"github.com/dedo1911/ingress-plus-backend/internal/campaigns"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

func TestRenderPlaceholders(t *testing.T) {
	values := map[string]string{
		"username":  "TestAgent",
		"faction":   "resistance",
		"userEmail": "agent@example.com",
	}

	input := "Hi %username%, your faction is %faction% (%userEmail%)."
	want := "Hi TestAgent, your faction is resistance (agent@example.com)."

	if got := campaigns.RenderPlaceholders(input, values); got != want {
		t.Fatalf("RenderPlaceholders() = %q, want %q", got, want)
	}
}

// TestClaimCampaign_ConcurrentRace empirically verifies the fix for the race
// condition PocketBase's overlapping cron ticks can trigger: many goroutines
// calling ClaimCampaign on the same "queued" campaign at once must result in
// exactly one of them winning, mirroring two overlapping cron ticks (or the
// cron and a manual "send now" click) reaching the same not-yet-claimed
// campaign at roughly the same time.
func TestClaimCampaign_ConcurrentRace(t *testing.T) {
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

	record := core.NewRecord(collection)
	record.Set("status", "queued")
	if err := app.Save(record); err != nil {
		t.Fatalf("failed to create test campaign record: %v", err)
	}

	const attempts = 25
	results := make([]bool, attempts)

	var wg sync.WaitGroup
	var ready sync.WaitGroup
	start := make(chan struct{})

	wg.Add(attempts)
	ready.Add(attempts)
	for i := 0; i < attempts; i++ {
		go func(i int) {
			defer wg.Done()
			ready.Done()
			<-start // line every goroutine up so they hit ClaimCampaign together

			claimed, err := campaigns.ClaimCampaign(app, record.Id)
			if err != nil {
				t.Errorf("ClaimCampaign() error = %v", err)
				return
			}
			results[i] = claimed
		}(i)
	}

	ready.Wait()
	close(start)
	wg.Wait()

	claimedCount := 0
	for _, claimed := range results {
		if claimed {
			claimedCount++
		}
	}

	if claimedCount != 1 {
		t.Fatalf("expected exactly 1 of %d concurrent ClaimCampaign calls to succeed, got %d", attempts, claimedCount)
	}

	final, err := app.FindRecordById("email_campaigns", record.Id)
	if err != nil {
		t.Fatalf("failed to reload campaign: %v", err)
	}
	if got := final.GetString("status"); got != "sending" {
		t.Fatalf("expected final status %q, got %q", "sending", got)
	}
}
