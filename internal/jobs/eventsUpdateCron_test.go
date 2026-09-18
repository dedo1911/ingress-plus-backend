package jobs

import (
	"testing"
	"time"
)

const firstSaturday = "0 0 0 ? 1/1 SAT#1 *"

func date(s string) time.Time {
	t, err := time.Parse(time.RFC3339, s)
	if err != nil {
		panic(err)
	}
	return t.UTC()
}

func TestNextOccurrence(t *testing.T) {
	// First Saturday of September 2026 is the 5th; of October, the 3rd.
	start := date("2026-09-05T00:00:00Z")
	end := date("2026-09-05T23:59:59Z")

	cases := []struct {
		name      string
		now       time.Time
		local     bool
		wantMove  bool
		wantStart time.Time
	}{
		{"upcoming is left alone", date("2026-09-01T12:00:00Z"), false, false, start},
		{"running is left alone", date("2026-09-05T01:00:00Z"), false, false, start},
		{"ended global advances", date("2026-09-06T00:30:00Z"), false, true, date("2026-10-03T00:00:00Z")},
		{"ended local waits for the slack", date("2026-09-06T00:30:00Z"), true, false, start},
		{"ended local advances after it", date("2026-09-06T12:30:00Z"), true, true, date("2026-10-03T00:00:00Z")},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotStart, gotEnd, moved, err := nextOccurrence(c.now, start, end, c.local, firstSaturday)
			if err != nil {
				t.Fatal(err)
			}
			if moved != c.wantMove || !gotStart.Equal(c.wantStart) {
				t.Fatalf("got moved=%v start=%s, want moved=%v start=%s", moved, gotStart, c.wantMove, c.wantStart)
			}
			if gotEnd.Sub(gotStart) != end.Sub(start) {
				t.Fatalf("duration changed: %s -> %s", end.Sub(start), gotEnd.Sub(gotStart))
			}
		})
	}
}

// A 22:00-02:00 event used to get an end time twenty hours before its start
// once rebuilt from the new date and the old clock.
func TestNextOccurrenceCrossesMidnight(t *testing.T) {
	start := date("2026-09-05T22:00:00Z")
	end := date("2026-09-06T02:00:00Z")

	gotStart, gotEnd, moved, err := nextOccurrence(date("2026-09-06T03:00:00Z"), start, end, false, firstSaturday)
	if err != nil || !moved {
		t.Fatalf("expected an advance, got moved=%v err=%v", moved, err)
	}
	if want := date("2026-10-03T22:00:00Z"); !gotStart.Equal(want) {
		t.Fatalf("start %s, want %s", gotStart, want)
	}
	if want := date("2026-10-04T02:00:00Z"); !gotEnd.Equal(want) {
		t.Fatalf("end %s, want %s", gotEnd, want)
	}
}
