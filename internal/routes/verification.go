package routes

import (
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/mail"

	"github.com/dedo1911/ingress-plus-backend/internal/notify"
	"github.com/dedo1911/ingress-plus-backend/internal/players"
	"github.com/dedo1911/ingress-plus-backend/internal/verification"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/mailer"
	"github.com/pocketbase/pocketbase/tools/types"
)

// verifyURL is where every user-facing message sends an agent to fix things.
const verifyURL = "https://ingress.plus/verify"

type mintVerificationRequest struct {
	Tier            string `json:"tier"`
	Contested       bool   `json:"contested"`
	ClaimedUsername string `json:"claimedUsername"`
}

// MintVerification hands the signed-in agent a code to paste into the plugin.
// Route should be registered with apis.RequireAuth("users").
func MintVerification(e *core.RequestEvent) error {
	if !verification.Enabled(e.App) {
		return newErrorResponse(e, errors.New("verification is disabled"), http.StatusForbidden, "Agent verification is currently closed.")
	}

	var data mintVerificationRequest
	if err := decodeBody(e, &data); err != nil {
		return err
	}

	tier, err := verification.ParseTier(data.Tier)
	if err != nil {
		return newErrorResponse(e, err, http.StatusBadRequest, "Unknown verification level")
	}

	record, err := verification.Mint(e.App, e.Auth, tier, data.Contested, data.ClaimedUsername)
	if err != nil {
		return verificationFailure(e, err)
	}

	return e.JSON(http.StatusOK, map[string]any{
		"code":      record.GetString("code"),
		"tier":      record.GetString("tier"),
		"expiresAt": record.GetDateTime("expires_at"),
	})
}

type claimVerificationRequest struct {
	Code     string `json:"code"`
	Nickname string `json:"nickname"`
	Faction  string `json:"faction"`
	PlayerID string `json:"playerId"`
}

// ClaimVerification is the plugin calling home, once, for every tier.
//
// Unauthenticated: the plugin runs on intel.ingress.com with no Ingress Plus
// session, exactly like the upload route. The code is the only thing tying the
// call to an account, which is why it is single-use and short-lived - and why
// the tier is read off the stored record rather than the body. Letting the
// caller pick would let anyone holding a code redeem it as strong with a
// fabricated hash.
func ClaimVerification(telegram *notify.Telegram, hasher *players.Hasher) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		if !verification.Enabled(e.App) {
			return newErrorResponse(e, errors.New("verification is disabled"), http.StatusForbidden, "Agent verification is currently closed.")
		}

		var data claimVerificationRequest
		if err := decodeBody(e, &data); err != nil {
			return err
		}

		record, err := verification.FindUsableCode(e.App, data.Code)
		if err != nil {
			return verificationFailure(e, err)
		}

		user, err := e.App.FindRecordById("users", record.GetString("user"))
		if err != nil {
			return newErrorResponse(e, err, http.StatusInternalServerError, "Could not load the account this code belongs to")
		}

		// The gate, not the proof - see verification.CheckIdentity. A failure
		// leaves the code pending on purpose, so an agent who corrects their
		// profile can retry with the same one.
		if err := verification.CheckIdentity(user, record, data.Nickname, data.Faction); err != nil {
			return verificationFailure(e, err)
		}

		tier, err := verification.ParseTier(record.GetString("tier"))
		if err != nil {
			return newErrorResponse(e, err, http.StatusInternalServerError, "Verification record carries an unknown level")
		}

		hash := ""
		if tier == verification.TierStrong {
			if !players.IsPlayerID(data.PlayerID) {
				return newErrorResponse(e, errors.New("missing or malformed player ID"), http.StatusBadRequest,
					"Strong verification needs your Ingress player ID, which comes from your C.O.R.E. inventory. Without a subscription, verify at the advanced level instead.")
			}
			if hash, err = hasher.Hash(data.PlayerID); err != nil {
				return newErrorResponse(e, err, http.StatusBadRequest, "That is not an Ingress player ID")
			}
		}

		// Consumed before anything is written to it: the conditional UPDATE is
		// what stops two callers redeeming one code, and a record that ends up
		// claimed without a nickname simply fails the check in Apply.
		claimed, err := verification.Consume(e.App, data.Code, verification.StatusPending, verification.StatusClaimed)
		if err != nil {
			return verificationFailure(e, err)
		}

		claimed.Set("nickname", data.Nickname)
		claimed.Set("faction", data.Faction)
		claimed.Set("player_hash", hash)
		claimed.Set("claimed_at", types.NowDateTime())
		if err := e.App.Save(claimed); err != nil {
			return newErrorResponse(e, err, http.StatusInternalServerError, "Could not record the verification attempt")
		}

		if tier.Proves() {
			comm := verification.CommTarget(data.Code)

			telegram.SendAsync(e.App.Logger(), telegram.Topics.Verification, notify.VerificationMessage(
				"Verification waiting for COMM", data.Nickname,
				"Level: "+string(tier), "Code: "+data.Code,
			))

			return e.JSON(http.StatusOK, map[string]any{
				"status":      "post_to_comm",
				"tier":        string(tier),
				"commMessage": comm.Message,
				"commLatE6":   comm.LatE6,
				"commLngE6":   comm.LngE6,
			})
		}

		result, err := applyVerification(e, telegram, claimed.Id)
		if err != nil {
			return verificationFailure(e, err)
		}

		return e.JSON(http.StatusOK, map[string]any{
			"status": "applied",
			"tier":   string(result.Tier),
		})
	}
}

type confirmVerificationRequest struct {
	Nickname string `json:"nickname"`
	Faction  string `json:"faction"`
}

// ConfirmVerification is an admin saying they read the code in COMM, with the
// nickname and faction Niantic put on the plext.
//
// Those two values overwrite whatever the plugin reported at claim time. That
// is the entire point of this step: the claim-time pair came from a page the
// claimant controls, this pair comes from the game.
// Route should be registered with apis.RequireSuperuserAuth().
func ConfirmVerification(telegram *notify.Telegram) func(*core.RequestEvent) error {
	return func(e *core.RequestEvent) error {
		var data confirmVerificationRequest
		if err := decodeBody(e, &data); err != nil {
			return err
		}

		if data.Nickname == "" || data.Faction == "" {
			return newErrorResponse(e, errors.New("missing nickname or faction"), http.StatusBadRequest,
				"Both the nickname and the faction from the COMM message are required")
		}

		// No feature-flag check here: an admin has to be able to finish a
		// verification that is already sitting in COMM after the flag goes off.
		record, err := verification.Confirm(e.App, e.Request.PathValue("code"))
		if err != nil {
			return verificationFailure(e, err)
		}

		record.Set("nickname", data.Nickname)
		record.Set("faction", data.Faction)
		record.Set("confirmed_by", e.Auth.Id)
		record.Set("confirmed_at", types.NowDateTime())
		if err := e.App.Save(record); err != nil {
			return newErrorResponse(e, err, http.StatusInternalServerError, "Could not record the confirmation")
		}

		result, err := applyVerification(e, telegram, record.Id)
		if err != nil {
			// The COMM-attested pair disagreeing with the profile is the one
			// failure the agent cannot see - they are not the one making this
			// request - so it is also the one that has to reach them by email.
			var mismatch *verification.MismatchError
			if errors.As(err, &mismatch) {
				notifyMismatch(e, record, mismatch)
			}
			return verificationFailure(e, err)
		}

		return e.JSON(http.StatusOK, map[string]any{
			"status":   "applied",
			"tier":     string(result.Tier),
			"username": result.Username,
			"trumped":  len(result.Trumped),
		})
	}
}

// applyVerification writes the tier onto the account, then hangs Mediagress
// attribution and the notifications off the result.
//
// Only the first step can fail the request. Attribution is a consumer of the
// verification rather than part of it, and an identity that has been proven
// stays proven whether or not there are uploads to hang off it.
func applyVerification(e *core.RequestEvent, telegram *notify.Telegram, verificationID string) (verification.Result, error) {
	result, err := verification.Apply(e.App, verificationID)
	if err != nil {
		return result, err
	}

	if err := players.ApplyVerification(e.App, result); err != nil {
		e.App.Logger().WarnContext(e.Request.Context(), "Verified an agent but could not attribute their uploads",
			slog.String("user", result.UserID), slog.String("agent", result.Username), slog.Any("error", err))
	}

	telegram.SendAsync(e.App.Logger(), telegram.Topics.Verification, notify.VerificationMessage(
		"Verification applied", result.Username, "Level: "+string(result.Tier),
	))

	// Trumping is destructive and irreversible for someone who is not part of
	// this request, so they are told what happened and what to do about it.
	for _, trumped := range result.Trumped {
		telegram.SendAsync(e.App.Logger(), telegram.Topics.Verification, notify.VerificationMessage(
			"Username taken from another account", result.Username,
			"Previous holder: "+trumped.OldUsername, "Renamed to: "+trumped.FreedUsername,
		))

		sendMail(e, trumped.Email, "Your Ingress Plus username has changed",
			"<p>Hi,</p><p>Another agent has verified <b>"+trumped.OldUsername+"</b> as their in-game "+
				"name, so your account has been renamed to <b>"+trumped.FreedUsername+"</b> and its "+
				"verification level cleared.</p><p>You can pick a new name any time on your "+
				`<a href="https://ingress.plus/agent/settings">settings page</a>. If you believe this `+
				"is a mistake, reply to this email and we will look into it.</p>")
	}

	return result, nil
}

// notifyMismatch tells an agent that the COMM message did not match the profile
// they verified with, since the confirm step happens without them.
func notifyMismatch(e *core.RequestEvent, record *core.Record, mismatch *verification.MismatchError) {
	user, err := e.App.FindRecordById("users", record.GetString("user"))
	if err != nil {
		e.App.Logger().ErrorContext(e.Request.Context(), "Could not load the account to warn about a verification mismatch",
			slog.String("verification", record.Id), slog.Any("error", err))
		return
	}

	sendMail(e, user.Email(), "Your Ingress Plus verification could not be completed",
		"<p>Hi,</p><p>"+mismatch.Error()+"</p><p>Nothing has changed on your account.</p>")
}

// sendMail is advisory: a failed notification must not fail the request that
// caused it, since the thing being notified about has already happened.
func sendMail(e *core.RequestEvent, to, subject, html string) {
	if to == "" {
		return
	}

	message := &mailer.Message{
		From: mail.Address{
			Name:    e.App.Settings().Meta.SenderName,
			Address: e.App.Settings().Meta.SenderAddress,
		},
		To:      []mail.Address{{Address: to}},
		Subject: subject,
		HTML:    html,
	}

	if err := e.App.NewMailClient().Send(message); err != nil {
		e.App.Logger().ErrorContext(e.Request.Context(), "Failed to send a verification email",
			slog.String("subject", subject), slog.Any("error", err))
	}
}

func decodeBody(e *core.RequestEvent, into any) error {
	defer e.Request.Body.Close()
	body, err := io.ReadAll(e.Request.Body)
	if err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "Failed to read request body")
	}
	if err := json.Unmarshal(body, into); err != nil {
		return newErrorResponse(e, err, http.StatusBadRequest, "Invalid JSON format")
	}
	return nil
}

// verificationFailure turns a verification error into a status and a message
// the agent can act on. The plugin puts response bodies straight into an alert
// box, so these are written for them rather than for a log.
func verificationFailure(e *core.RequestEvent, err error) error {
	var mismatch *verification.MismatchError
	if errors.As(err, &mismatch) {
		return newErrorResponse(e, err, http.StatusBadRequest, mismatch.Error())
	}

	switch {
	case errors.Is(err, verification.ErrCodeUnusable):
		// Unknown, expired and already used give one answer on purpose:
		// telling them apart turns this route into an oracle for guessing
		// codes.
		return newErrorResponse(e, err, http.StatusNotFound,
			"That verification code is not usable. Get a new one at "+verifyURL)

	case errors.Is(err, verification.ErrContestedNeedsProof):
		return newErrorResponse(e, err, http.StatusBadRequest, err.Error())

	case errors.Is(err, verification.ErrNoDowngrade),
		errors.Is(err, verification.ErrContested),
		errors.Is(err, verification.ErrWrongStatus):
		return newErrorResponse(e, err, http.StatusConflict, err.Error())
	}

	return newErrorResponse(e, err, http.StatusInternalServerError, "Verification failed")
}
