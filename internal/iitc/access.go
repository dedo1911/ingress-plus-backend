// Package iitc implements the Ingress Plus IITC plugin store: who may see and
// load which plugin, the long-lived tokens the sideloader authenticates with,
// and the per-request encryption and watermarking of the code it serves.
package iitc

import (
	"encoding/json"
	"errors"
	"fmt"

	"github.com/dedo1911/ingress-plus-backend/internal/campaigns"
	"github.com/pocketbase/pocketbase/core"
)

// Collection names. The schemas live in the database (see docs/iitc-plugins.md
// and CreateCollections), these are the only names the Go code relies on.
const (
	PluginsCollection  = "iitc_plugins"
	VersionsCollection = "iitc_plugin_versions"
	TokensCollection   = "iitc_tokens"
)

// Plugin statuses.
const (
	StatusDraft         = "draft"
	StatusPendingReview = "pending_review"
	StatusApproved      = "approved"
	StatusRejected      = "rejected"
	StatusDisabled      = "disabled"
)

// Version statuses.
const (
	VersionPending  = "pending"
	VersionApproved = "approved"
	VersionRejected = "rejected"
)

// Access rule types. Rules are OR'd together, the same way campaign targeting
// rules are, and they reuse its JSON shape (campaigns.TargetRule) so the
// frontend can share one rule editor between the two.
const (
	RuleAll       = "all"
	RuleUser      = "user"
	RuleSupporter = "supporter"
	RuleVerified  = "verified"
	RuleFaction   = "faction"
)

const (
	maxRules          = 20
	maxUsersPerRule   = 500
	maxFactionsInRule = 3
)

// validFactions are the users.faction values a faction rule may name. "No
// faction" is deliberately not one of them: a faction rule only counts for
// verified agents, and a verified agent always has a faction.
var validFactions = map[string]bool{
	"enlightened": true,
	"resistance":  true,
	"machina":     true,
}

// Access is the parsed shape of a plugin's "access" JSON field.
type Access struct {
	Rules []campaigns.TargetRule `json:"rules"`
}

// ParseAccess decodes the access JSON. An empty value means no rules, which
// gives access to the author alone.
func ParseAccess(raw string) (Access, error) {
	var access Access
	if raw == "" || raw == "null" {
		return access, nil
	}
	if err := json.Unmarshal([]byte(raw), &access); err != nil {
		return access, fmt.Errorf("invalid access JSON: %w", err)
	}
	return access, nil
}

// ValidateAccess rejects rule sets the store doesn't understand, so a typo in
// a rule type can't quietly turn into "nobody but the author" (or worse, be
// read differently by a later version of CanAccess).
func ValidateAccess(raw string) error {
	access, err := ParseAccess(raw)
	if err != nil {
		return err
	}
	if len(access.Rules) > maxRules {
		return fmt.Errorf("too many access rules (max %d)", maxRules)
	}
	for i, rule := range access.Rules {
		switch rule.Type {
		case RuleAll, RuleSupporter, RuleVerified:
		case RuleUser:
			if len(rule.UserIds) == 0 {
				return fmt.Errorf("rule %d: pick at least one user", i+1)
			}
			if len(rule.UserIds) > maxUsersPerRule {
				return fmt.Errorf("rule %d: too many users (max %d)", i+1, maxUsersPerRule)
			}
		case RuleFaction:
			if len(rule.Factions) == 0 || len(rule.Factions) > maxFactionsInRule {
				return fmt.Errorf("rule %d: pick between 1 and %d factions", i+1, maxFactionsInRule)
			}
			for _, f := range rule.Factions {
				if !validFactions[f] {
					return fmt.Errorf("rule %d: unknown faction %q", i+1, f)
				}
			}
		default:
			return fmt.Errorf("rule %d: unknown rule type %q", i+1, rule.Type)
		}
	}
	return nil
}

// IsVerified reports whether the agent went through the verification flow.
// While verified, users can't change their own faction, which is what makes a
// faction rule meaningful.
func IsVerified(user *core.Record) bool {
	return user != nil && user.GetString("verification") != ""
}

// IsAuthor reports whether user owns plugin.
func IsAuthor(user, plugin *core.Record) bool {
	return user != nil && plugin != nil && plugin.GetString("author") == user.Id
}

// IsAdmin reports whether user is an Ingress Plus admin: a verified agent
// whose email also belongs to a superuser. Superusers can't sign in with
// Google, so this is how an admin's agent account is recognised. The email
// has to be verified, otherwise anyone could sign up with an admin's address.
func IsAdmin(app core.App, user *core.Record) bool {
	if user == nil || !user.Verified() || user.Email() == "" {
		return false
	}
	_, err := app.FindAuthRecordByEmail(core.CollectionNameSuperusers, user.Email())
	return err == nil
}

// MatchesRules reports whether user is granted access by plugin's rules. The
// author is not special-cased here, see CanAccess.
func MatchesRules(user *core.Record, access Access) bool {
	if user == nil {
		return false
	}
	for _, rule := range access.Rules {
		switch rule.Type {
		case RuleAll:
			return true
		case RuleSupporter:
			if user.GetBool("supporter") {
				return true
			}
		case RuleVerified:
			if IsVerified(user) {
				return true
			}
		case RuleFaction:
			if !IsVerified(user) {
				continue
			}
			faction := user.GetString("faction")
			for _, f := range rule.Factions {
				if f != "" && f == faction {
					return true
				}
			}
		case RuleUser:
			for _, id := range rule.UserIds {
				if id == user.Id {
					return true
				}
			}
		}
	}
	return false
}

// CanAccess reports whether user may see and load plugin's published version.
// Authors always see their own plugins; everybody else needs an approved
// plugin with an approved version, and a matching access rule. Unparseable
// rules fail closed.
//
// This is the single gate for every route that lists or serves plugins: a user
// who fails it must not learn the plugin exists, so callers answer 404, never
// 403.
func CanAccess(user, plugin *core.Record) bool {
	if user == nil || plugin == nil {
		return false
	}
	if IsAuthor(user, plugin) {
		return true
	}
	if plugin.GetString("status") != StatusApproved || plugin.GetString("current_version") == "" {
		return false
	}
	access, err := ParseAccess(plugin.GetString("access"))
	if err != nil {
		return false
	}
	return MatchesRules(user, access)
}

// ErrNotFound is what every lookup returns when the plugin doesn't exist or
// the caller can't access it - the two cases are deliberately indistinguishable.
var ErrNotFound = errors.New("plugin not found")
