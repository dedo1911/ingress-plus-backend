package campaigns

import (
	"encoding/json"
	"fmt"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
)

// TargetRule is one entry in a campaign's targeting.rules array. Which
// fields are set depends on Type:
//   - "all": matches every user, ignoring every other rule
//   - "faction": Factions holds the faction values to match (including ""
//     for "no faction")
//   - "supporter": matches users with supporter = true
//   - "user": UserIds holds specific user record ids to include
type TargetRule struct {
	Type     string   `json:"type"`
	Factions []string `json:"factions,omitempty"`
	UserIds  []string `json:"userIds,omitempty"`
}

// Targeting is the parsed shape of a campaign's "targeting" JSON field.
type Targeting struct {
	RequireOptIn bool         `json:"requireOptIn"`
	Rules        []TargetRule `json:"rules"`
}

// ResolveAudience parses a campaign's targeting JSON and returns every
// matching user record. Rules are combined with OR; RequireOptIn then ANDs
// the result down to users with newsletterOptIn = true.
//
// NB: all rule values are bound as filter params (never string-concatenated
// into the filter itself), per PocketBase's own guidance for untrusted
// filter input.
func ResolveAudience(app core.App, targetingRaw string) ([]*core.Record, error) {
	var targeting Targeting
	if targetingRaw != "" {
		if err := json.Unmarshal([]byte(targetingRaw), &targeting); err != nil {
			return nil, fmt.Errorf("invalid targeting JSON: %w", err)
		}
	}

	matchesAll := false
	var clauses []string
	params := dbx.Params{}

	for i, rule := range targeting.Rules {
		switch rule.Type {
		case "all":
			matchesAll = true
		case "supporter":
			clauses = append(clauses, "supporter = true")
		case "faction":
			var factionClauses []string
			for j, f := range rule.Factions {
				key := fmt.Sprintf("faction%d_%d", i, j)
				factionClauses = append(factionClauses, fmt.Sprintf("faction = {:%s}", key))
				params[key] = f
			}
			if len(factionClauses) > 0 {
				clauses = append(clauses, "("+joinOr(factionClauses)+")")
			}
		case "user":
			var userClauses []string
			for j, id := range rule.UserIds {
				key := fmt.Sprintf("user%d_%d", i, j)
				userClauses = append(userClauses, fmt.Sprintf("id = {:%s}", key))
				params[key] = id
			}
			if len(userClauses) > 0 {
				clauses = append(clauses, "("+joinOr(userClauses)+")")
			}
		}
	}

	filter := ""
	if !matchesAll {
		if len(clauses) == 0 {
			return nil, nil // no rules configured - nobody to send to
		}
		filter = "(" + joinOr(clauses) + ")"
	}

	if targeting.RequireOptIn {
		if filter != "" {
			filter += " && newsletterOptIn = true"
		} else {
			filter = "newsletterOptIn = true"
		}
	}

	return app.FindRecordsByFilter("users", filter, "", 0, 0, params)
}

func joinOr(clauses []string) string {
	result := ""
	for i, c := range clauses {
		if i > 0 {
			result += " || "
		}
		result += c
	}
	return result
}
