package routes

import (
	"io"
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
	var gotPath, gotBody, gotAgent string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		gotPath, gotBody, gotAgent = r.URL.Path, string(body), r.Header.Get("User-Agent")
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
			Name:            "upstream results are translated in request order",
			Method:          http.MethodPost,
			URL:             "/api/mu-calc",
			Body:            strings.NewReader(valid[:len(valid)-1] + `,{"coordinates":[["1.5","2"],[3,4],[5,6.25]]}]`),
			ExpectedStatus:  http.StatusOK,
			ExpectedContent: []string{`[{"mu":93237,"area_km2":12.5,"name":"A"},{"mu":7}]`},
			BeforeTestFunc: stub(http.StatusOK, "application/json", `{"count":2,"model":"m","results":[`+
				`{"index":0,"ok":true,"mu":93237,"area_km2":12.5,"coordinates":["50.865073,4.360697","50.836025,4.431384","50.940825,4.443193"]},`+
				`{"index":1,"ok":true,"mu":7,"coordinates":["1.5,2","3,4","5,6.25"]}]}`),
			AfterTestFunc: func(t testing.TB, app *tests.TestApp, res *http.Response) {
				if gotPath != "/getMUbulk" {
					t.Errorf("upstream path = %q, want /getMUbulk", gotPath)
				}
				want := `{"fields":[{"coordinates":["50.865073,4.360697","50.836025,4.431384","50.940825,4.443193"]},{"coordinates":["1.5,2","3,4","5,6.25"]}]}`
				if gotBody != want {
					t.Errorf("upstream body = %s, want %s", gotBody, want)
				}
				if !strings.HasPrefix(gotAgent, "IngressPlus-MUCalc/") {
					t.Errorf("upstream User-Agent = %q", gotAgent)
				}
			},
		},
		{
			Name:            "a field the upstream rejects is reported 1-based",
			Method:          http.MethodPost,
			URL:             "/api/mu-calc",
			Body:            strings.NewReader(valid[:len(valid)-1] + `,{"coordinates":[[1,1],[2,2],[3,3]]}]`),
			ExpectedStatus:  http.StatusBadRequest,
			ExpectedContent: []string{`"error":"Field 2: Degenerate triangle."`},
			BeforeTestFunc: stub(http.StatusOK, "application/json", `{"count":2,"model":"m","results":[`+
				`{"index":0,"ok":true,"mu":93237},{"index":1,"ok":false,"error":"Degenerate triangle."}]}`),
		},
		{
			Name:            "upstream time budget becomes a busy message",
			Method:          http.MethodPost,
			URL:             "/api/mu-calc",
			Body:            strings.NewReader(valid),
			ExpectedStatus:  http.StatusGatewayTimeout,
			ExpectedContent: []string{`"error":"The MU calculator is busy right now. Please try again in a moment."`},
			BeforeTestFunc:  stub(http.StatusGatewayTimeout, "application/json", `{"error":"Time budget exceeded.","model":"m"}`),
		},
		{
			Name:            "upstream result count mismatch is rejected",
			Method:          http.MethodPost,
			URL:             "/api/mu-calc",
			Body:            strings.NewReader(valid),
			ExpectedStatus:  http.StatusBadGateway,
			ExpectedContent: []string{`"error":"The MU calculator API returned an invalid response. Please try again."`},
			BeforeTestFunc:  stub(http.StatusOK, "application/json", `{"count":0,"model":"m","results":[]}`),
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
