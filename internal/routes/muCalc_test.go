package routes

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/pocketbase/pocketbase/core"
	"github.com/pocketbase/pocketbase/tests"
)

// TestMuCalcHandler covers the proxy's own validation and what it relays from
// the upstream, which is stubbed via MU_CALC_API_BASE_URL.
func TestMuCalcHandler(t *testing.T) {
	var upstreamStatus int
	var upstreamBody, upstreamType string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", upstreamType)
		w.WriteHeader(upstreamStatus)
		w.Write([]byte(upstreamBody))
	}))
	defer upstream.Close()
	t.Setenv("MU_CALC_API_BASE_URL", upstream.URL)

	register := func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
		e.Router.POST("/api/mu-calc", MuCalc)
	}
	stub := func(status int, contentType, body string) func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
		return func(t testing.TB, app *tests.TestApp, e *core.ServeEvent) {
			upstreamStatus, upstreamType, upstreamBody = status, contentType, body
			register(t, app, e)
		}
	}
	valid := `[{"name":"A","coordinates":[[50.865073,4.360697],[50.836025,4.431384],[50.940825,4.443193]]}]`

	scenarios := []tests.ApiScenario{
		{
			Name:            "repeated portal is reported 1-based",
			Method:          http.MethodPost,
			URL:             "/api/mu-calc",
			Body:            strings.NewReader(valid[:len(valid)-1] + `,{"coordinates":[[1,2],[1.0000001,2],[3,4]]}]`),
			ExpectedStatus:  http.StatusBadRequest,
			ExpectedContent: []string{`"error":"Field 2: the three portals must be different."`},
			BeforeTestFunc:  register,
		},
		{
			Name:            "out-of-range longitude is rejected",
			Method:          http.MethodPost,
			URL:             "/api/mu-calc",
			Body:            strings.NewReader(`[{"coordinates":[[1,2],[3,190],[5,6]]}]`),
			ExpectedStatus:  http.StatusBadRequest,
			ExpectedContent: []string{`"error":"Field 1: enter valid latitude and longitude values."`},
			BeforeTestFunc:  register,
		},
		{
			Name:            "upstream JSON is relayed",
			Method:          http.MethodPost,
			URL:             "/api/mu-calc",
			Body:            strings.NewReader(valid),
			ExpectedStatus:  http.StatusOK,
			ExpectedContent: []string{`[{"id":0,"mu":93237,"name":"A"}]`},
			BeforeTestFunc:  stub(http.StatusOK, "application/json", `[{"id":0,"mu":93237,"name":"A"}]`),
		},
		{
			Name:               "upstream HTML error page is not relayed",
			Method:             http.MethodPost,
			URL:                "/api/mu-calc",
			Body:               strings.NewReader(valid),
			ExpectedStatus:     http.StatusBadGateway,
			ExpectedContent:    []string{`"error":"The MU calculator API returned an invalid response. Please try again."`},
			NotExpectedContent: []string{`<html>`, `"details"`},
			BeforeTestFunc:     stub(http.StatusInternalServerError, "text/html", `<html>oops</html>`),
		},
	}

	for _, scenario := range scenarios {
		scenario.Test(t)
	}
}
