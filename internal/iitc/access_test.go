package iitc

import (
	"testing"

	"github.com/pocketbase/pocketbase/core"
)

func testUser(id string, set map[string]any) *core.Record {
	c := core.NewAuthCollection("users")
	c.Fields.Add(&core.TextField{Name: "verification"}, &core.TextField{Name: "faction"}, &core.BoolField{Name: "supporter"})
	r := core.NewRecord(c)
	r.Id = id
	for k, v := range set {
		r.Set(k, v)
	}
	return r
}

func testPlugin(author, status, current, access string) *core.Record {
	c := core.NewBaseCollection(PluginsCollection)
	c.Fields.Add(&core.TextField{Name: "author"}, &core.TextField{Name: "status"}, &core.TextField{Name: "current_version"}, &core.JSONField{Name: "access"})
	r := core.NewRecord(c)
	r.Set("author", author)
	r.Set("status", status)
	r.Set("current_version", current)
	r.Set("access", access)
	return r
}

func TestValidateAccess(t *testing.T) {
	cases := []struct {
		raw string
		ok  bool
	}{
		{``, true},
		{`{"rules":[]}`, true},
		{`{"rules":[{"type":"all"}]}`, true},
		{`{"rules":[{"type":"supporter"},{"type":"verified"}]}`, true},
		{`{"rules":[{"type":"faction","factions":["resistance"]}]}`, true},
		{`{"rules":[{"type":"faction","factions":[""]}]}`, false},
		{`{"rules":[{"type":"faction","factions":[]}]}`, false},
		{`{"rules":[{"type":"faction","factions":["RESISTANCE"]}]}`, false},
		{`{"rules":[{"type":"user","userIds":["abc"]}]}`, true},
		{`{"rules":[{"type":"user","userIds":[]}]}`, false},
		{`{"rules":[{"type":"everyone"}]}`, false},
		{`not json`, false},
	}
	for _, c := range cases {
		if err := ValidateAccess(c.raw); (err == nil) != c.ok {
			t.Errorf("ValidateAccess(%s) err = %v, want ok=%v", c.raw, err, c.ok)
		}
	}
}

func TestCanAccess(t *testing.T) {
	plain := testUser("u1", nil)
	supporter := testUser("u2", map[string]any{"supporter": true})
	verifiedRes := testUser("u3", map[string]any{"verification": "verified", "faction": "resistance"})
	unverifiedRes := testUser("u4", map[string]any{"faction": "resistance"})

	factionRule := `{"rules":[{"type":"faction","factions":["resistance"]}]}`

	cases := []struct {
		name   string
		user   *core.Record
		plugin *core.Record
		want   bool
	}{
		{"nil user", nil, testPlugin("a", StatusApproved, "v", `{"rules":[{"type":"all"}]}`), false},
		{"all rule", plain, testPlugin("a", StatusApproved, "v", `{"rules":[{"type":"all"}]}`), true},
		{"no rules means author only", plain, testPlugin("a", StatusApproved, "v", ``), false},
		{"author always", plain, testPlugin("u1", StatusDraft, "", ``), true},
		{"author of a disabled plugin", plain, testPlugin("u1", StatusDisabled, "v", ``), true},
		{"not approved", plain, testPlugin("a", StatusPendingReview, "v", `{"rules":[{"type":"all"}]}`), false},
		{"approved without a version", plain, testPlugin("a", StatusApproved, "", `{"rules":[{"type":"all"}]}`), false},
		{"disabled", plain, testPlugin("a", StatusDisabled, "v", `{"rules":[{"type":"all"}]}`), false},
		{"supporter rule, supporter", supporter, testPlugin("a", StatusApproved, "v", `{"rules":[{"type":"supporter"}]}`), true},
		{"supporter rule, plain", plain, testPlugin("a", StatusApproved, "v", `{"rules":[{"type":"supporter"}]}`), false},
		{"verified rule, verified", verifiedRes, testPlugin("a", StatusApproved, "v", `{"rules":[{"type":"verified"}]}`), true},
		{"verified rule, unverified", unverifiedRes, testPlugin("a", StatusApproved, "v", `{"rules":[{"type":"verified"}]}`), false},
		{"faction rule, verified match", verifiedRes, testPlugin("a", StatusApproved, "v", factionRule), true},
		{"faction rule needs verification", unverifiedRes, testPlugin("a", StatusApproved, "v", factionRule), false},
		{"faction rule, other faction", testUser("u5", map[string]any{"verification": "x", "faction": "enlightened"}), testPlugin("a", StatusApproved, "v", factionRule), false},
		{"user rule, listed", plain, testPlugin("a", StatusApproved, "v", `{"rules":[{"type":"user","userIds":["zz","u1"]}]}`), true},
		{"user rule, not listed", plain, testPlugin("a", StatusApproved, "v", `{"rules":[{"type":"user","userIds":["zz"]}]}`), false},
		{"rules are OR'd", plain, testPlugin("a", StatusApproved, "v", `{"rules":[{"type":"supporter"},{"type":"user","userIds":["u1"]}]}`), true},
		{"broken JSON fails closed", plain, testPlugin("a", StatusApproved, "v", `{"rules":`), false},
	}
	for _, c := range cases {
		if got := CanAccess(c.user, c.plugin); got != c.want {
			t.Errorf("%s: CanAccess = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestCanAuthorChangeStatus(t *testing.T) {
	cases := []struct {
		from, to string
		approved bool
		want     bool
	}{
		{StatusDraft, StatusDraft, false, true},
		{StatusApproved, StatusDraft, true, true},
		{StatusDraft, StatusApproved, true, true},
		{StatusDraft, StatusApproved, false, false},
		{StatusDraft, StatusPendingReview, false, false},
		{StatusPendingReview, StatusApproved, false, false},
		{StatusRejected, StatusApproved, false, false},
		{StatusDisabled, StatusApproved, true, false},
		{StatusDisabled, StatusDraft, true, false},
		{StatusApproved, StatusDisabled, true, false},
	}
	for _, c := range cases {
		if got := CanAuthorChangeStatus(c.from, c.to, c.approved); got != c.want {
			t.Errorf("%s -> %s (approved=%v) = %v, want %v", c.from, c.to, c.approved, got, c.want)
		}
	}
}

func TestCanReviewVersion(t *testing.T) {
	cases := []struct {
		from, to string
		want     bool
	}{
		{VersionPending, VersionApproved, true},
		{VersionPending, VersionRejected, true},
		{VersionApproved, VersionRejected, true},
		{VersionApproved, VersionPending, false},
		{VersionRejected, VersionApproved, false},
		{VersionRejected, VersionRejected, true},
	}
	for _, c := range cases {
		if got := CanReviewVersion(c.from, c.to); got != c.want {
			t.Errorf("%s -> %s = %v, want %v", c.from, c.to, got, c.want)
		}
	}
}
