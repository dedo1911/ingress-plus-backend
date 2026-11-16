package iitc

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/pocketbase/dbx"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tools/hook"
	"github.com/pocketbase/pocketbase/tools/router"
	"github.com/pocketbase/pocketbase/tools/types"
)

const (
	// TokenPrefix makes the tokens easy to recognise in logs and secret
	// scanners, and keeps them from ever parsing as a PocketBase JWT.
	TokenPrefix = "ipx_"
	tokenBytes  = 32

	// MaxTokensPerUser caps how many devices one agent can connect. Old
	// ones are revoked from Agent Settings.
	MaxTokensPerUser = 20
	maxLabelLength   = 60

	// lastUsedResolution throttles the last_used_at write: the sideloader
	// makes several calls on every IITC page load, and a write per call
	// would be pure churn for a value only shown as "last used 3 days ago".
	lastUsedResolution = 10 * time.Minute

	// UserContextKey and TokenContextKey are where RequireToken stores the
	// authenticated user and their token record.
	UserContextKey  = "iitcUser"
	TokenContextKey = "iitcToken"
)

var ErrTooManyTokens = fmt.Errorf("too many connected devices (max %d), revoke one from Agent Settings first", MaxTokensPerUser)

// HashToken returns the stored form of a token. A plain SHA-256 is enough
// here, unlike for player IDs: the token is 256 bits of randomness, so there
// is nothing to brute-force.
func HashToken(plain string) string {
	sum := sha256.Sum256([]byte(plain))
	return hex.EncodeToString(sum[:])
}

// NewToken generates a fresh token and its hash.
func NewToken() (plain, hash string, err error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", "", err
	}
	plain = TokenPrefix + base64.RawURLEncoding.EncodeToString(buf)
	return plain, HashToken(plain), nil
}

// NormalizeLabel trims a device label to something displayable.
func NormalizeLabel(label string) string {
	label = strings.TrimSpace(label)
	if label == "" {
		return "IITC"
	}
	if r := []rune(label); len(r) > maxLabelLength {
		label = string(r[:maxLabelLength])
	}
	return label
}

// CreateToken stores a new token for user and returns the plaintext. The
// plaintext is never stored, so this is the only time it exists server-side.
func CreateToken(app core.App, user *core.Record, label string) (string, *core.Record, error) {
	collection, err := app.FindCollectionByNameOrId(TokensCollection)
	if err != nil {
		return "", nil, err
	}

	count, err := app.CountRecords(TokensCollection, dbx.HashExp{"user": user.Id})
	if err != nil {
		return "", nil, err
	}
	if count >= MaxTokensPerUser {
		return "", nil, ErrTooManyTokens
	}

	plain, hash, err := NewToken()
	if err != nil {
		return "", nil, err
	}

	record := core.NewRecord(collection)
	record.Set("user", user.Id)
	record.Set("label", NormalizeLabel(label))
	record.Set("token_hash", hash)
	record.Set("last_used_at", types.NowDateTime())
	if err := app.Save(record); err != nil {
		return "", nil, err
	}
	return plain, record, nil
}

// ErrInvalidToken covers a missing, unknown or orphaned token alike. Revoking
// a device deletes its token record, so a revoked token is simply unknown.
var ErrInvalidToken = errors.New("invalid or revoked IITC token")

// AuthenticateToken resolves a plaintext token to its (still existing) user.
func AuthenticateToken(app core.App, plain string) (*core.Record, *core.Record, error) {
	if !strings.HasPrefix(plain, TokenPrefix) {
		return nil, nil, ErrInvalidToken
	}

	token, err := app.FindFirstRecordByData(TokensCollection, "token_hash", HashToken(plain))
	if err != nil {
		return nil, nil, ErrInvalidToken
	}

	user, err := app.FindRecordById("users", token.GetString("user"))
	if err != nil {
		return nil, nil, ErrInvalidToken
	}

	if time.Since(token.GetDateTime("last_used_at").Time()) > lastUsedResolution {
		token.Set("last_used_at", types.NowDateTime())
		if err := app.Save(token); err != nil {
			// Not worth failing the request over a bookkeeping field.
			app.Logger().Warn("could not update IITC token last_used_at", "error", err)
		}
	}

	return user, token, nil
}

// tokenFromRequest reads "Authorization: Bearer ipx_...". PocketBase's own
// loadAuthToken middleware sees the same header first; it fails to parse it
// as a JWT, logs at debug level and moves on, leaving e.Auth unset.
func tokenFromRequest(e *core.RequestEvent) string {
	header := strings.TrimSpace(e.Request.Header.Get("Authorization"))
	if after, ok := strings.CutPrefix(header, "Bearer "); ok {
		header = strings.TrimSpace(after)
	}
	return header
}

// RequireToken is the middleware for the routes the sideloader calls. On
// success the user record is available as e.Get(UserContextKey).
func RequireToken() *hook.Handler[*core.RequestEvent] {
	return &hook.Handler[*core.RequestEvent]{
		Id: "iitcRequireToken",
		Func: func(e *core.RequestEvent) error {
			user, token, err := AuthenticateToken(e.App, tokenFromRequest(e))
			if err != nil {
				return router.NewApiError(http.StatusUnauthorized, "Invalid or revoked IITC token. Reconnect Ingress Plus from the IITC toolbox.", nil)
			}
			e.Set(UserContextKey, user)
			e.Set(TokenContextKey, token)
			return e.Next()
		},
	}
}

// TokenUser returns the user RequireToken authenticated.
func TokenUser(e *core.RequestEvent) *core.Record {
	user, _ := e.Get(UserContextKey).(*core.Record)
	return user
}

// CurrentToken returns the token record RequireToken authenticated.
func CurrentToken(e *core.RequestEvent) *core.Record {
	token, _ := e.Get(TokenContextKey).(*core.Record)
	return token
}
