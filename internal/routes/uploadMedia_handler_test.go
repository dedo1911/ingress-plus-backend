package routes

import (
	"net/http"
	"strings"
	"testing"

	"github.com/dedo1911/ingress-plus-backend/internal/notify"
	"github.com/dedo1911/ingress-plus-backend/internal/players"
	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// TestUploadMediaHandlerGuards covers the request-level checks on the
// unauthenticated upload route - the ones that sit before any record is
// touched and so aren't reachable through ensureMedia/ensureUpload.
func TestUploadMediaHandlerGuards(t *testing.T) {
	hasher, err := players.NewHasher("test-pepper")
	if err != nil {
		t.Fatal(err)
	}
	register := func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
		// An empty Telegram is disabled, so notifications are no-ops.
		e.Router.POST("/api/mediagress/v2/upload-media", UploadMediaV2(&notify.Telegram{}, hasher))
	}

	valid := `{"player":{"nickname":"oscarc1","team":"RESISTANCE"},"medias":[]}`

	scenarios := []tests.ApiScenario{
		{
			Name:            "oversized body is rejected before parsing",
			Method:          http.MethodPost,
			URL:             "/api/mediagress/v2/upload-media",
			Body:            strings.NewReader(`{"pad":"` + strings.Repeat("x", maxUploadBodyBytes) + `"}`),
			ExpectedStatus:  http.StatusRequestEntityTooLarge,
			ExpectedContent: []string{`"error":"Request body too large"`},
			TestAppFactory:  func(t testing.TB) *tests.TestApp { return newTestApp(t) },
			BeforeTestFunc:  register,
		},
		{
			Name:            "missing nickname is a client error",
			Method:          http.MethodPost,
			URL:             "/api/mediagress/v2/upload-media",
			Body:            strings.NewReader(`{"player":{"team":"RESISTANCE"},"medias":[]}`),
			ExpectedStatus:  http.StatusBadRequest,
			ExpectedContent: []string{`"error":"Missing player nickname"`},
			TestAppFactory:  func(t testing.TB) *tests.TestApp { return newTestApp(t) },
			BeforeTestFunc:  register,
		},
		{
			Name:            "valid empty batch succeeds",
			Method:          http.MethodPost,
			URL:             "/api/mediagress/v2/upload-media",
			Body:            strings.NewReader(valid),
			ExpectedStatus:  http.StatusOK,
			ExpectedContent: []string{`"previouslyUnknownMediaCount":0`},
			TestAppFactory:  func(t testing.TB) *tests.TestApp { return newTestApp(t) },
			BeforeTestFunc:  register,
		},
		{
			// No medias collection at all, so ensureMedia fails: the 500 must
			// not echo the driver's message to an anonymous caller.
			Name:               "server errors carry no details",
			Method:             http.MethodPost,
			URL:                "/api/mediagress/v2/upload-media",
			Body:               strings.NewReader(`{"player":{"nickname":"oscarc1","team":"RESISTANCE"},"medias":[{"storyItem":{"mediaId":"1","releaseDate":"1782748800000"}}]}`),
			ExpectedStatus:     http.StatusInternalServerError,
			ExpectedContent:    []string{`"error":"Error saving media record"`},
			NotExpectedContent: []string{`"details"`},
			BeforeTestFunc:     register,
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}
