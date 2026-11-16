package routes

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/pocketbase/pocketbase/core"
)

const (
	muCalcDefaultAPIBaseURL = "https://api.investigatexm.com"
	muCalcMaxRequestBytes   = 64 << 10
	muCalcMaxFields         = 50
	muCalcMaxNameLength     = 200
	// The upstream gives each request 20s including queue time, then answers
	// 504 (or "deadline exceeded" per bulk field), so wait a little longer.
	muCalcUpstreamTimeout = 25 * time.Second
	muCalcUserAgent       = "IngressPlus-MUCalc/1.0 (+https://ingress.plus)"
)

type muCalcField struct {
	Name        string              `json:"name,omitempty"`
	Coordinates [][]json.RawMessage `json:"coordinates"`
}

// Request and response shapes of the upstream bulk operation
// (https://mu.investigatexm.com/api.html).
type muCalcUpstreamField struct {
	Coordinates []string `json:"coordinates"`
}

type muCalcUpstreamResult struct {
	OK      bool     `json:"ok"`
	MU      *float64 `json:"mu"`
	AreaKm2 *float64 `json:"area_km2"`
	Error   string   `json:"error"`
}

type muCalcUpstreamResponse struct {
	Count   int                    `json:"count"`
	Results []muCalcUpstreamResult `json:"results"`
	Error   string                 `json:"error"`
}

// muCalcResult is what the page receives per field, in request order.
type muCalcResult struct {
	MU      float64  `json:"mu"`
	AreaKm2 *float64 `json:"area_km2,omitempty"`
	Name    string   `json:"name,omitempty"`
}

// MuCalc proxies the InvestigateXM bulk MU operation (POST /getMUbulk). Keeping this
// call in the API backend means the browser uses one stable Ingress Plus route;
// MU_CALC_API_BASE_URL can switch its upstream when we host the calculation.
// The upstream sees this server's IP, not the user's, so per-user throttling
// has to happen here: a PocketBase rate-limit rule on "POST /api/mu-calc".
func MuCalc(e *core.RequestEvent) error {
	badRequest := func(msg string) error {
		return newErrorResponse(e, errors.New(msg), http.StatusBadRequest, msg)
	}

	body, err := io.ReadAll(io.LimitReader(e.Request.Body, muCalcMaxRequestBytes+1))
	if err != nil {
		return badRequest("Could not read the request body.")
	}
	if len(body) > muCalcMaxRequestBytes {
		return newErrorResponse(e, errors.New("body over limit"), http.StatusRequestEntityTooLarge, "Request body is too large.")
	}

	var fields []muCalcField
	if err := json.Unmarshal(body, &fields); err != nil {
		return badRequest("Request body must be a JSON array of fields.")
	}
	if len(fields) == 0 || len(fields) > muCalcMaxFields {
		return badRequest(fmt.Sprintf("Submit between 1 and %d fields.", muCalcMaxFields))
	}
	upstreamFields := make([]muCalcUpstreamField, len(fields))
	for fieldIndex, field := range fields {
		if len(field.Coordinates) != 3 {
			return badRequest(fmt.Sprintf("Field %d: supply exactly three coordinates.", fieldIndex+1))
		}
		if len(field.Name) > muCalcMaxNameLength {
			return badRequest(fmt.Sprintf("Field %d: name must be %d characters or fewer.", fieldIndex+1, muCalcMaxNameLength))
		}
		// The upstream rejects repeated points too, but only per field and in its
		// own words, so catch the common case here with the site's wording.
		seen := map[string]bool{}
		for _, coordinate := range field.Coordinates {
			if len(coordinate) != 2 {
				return badRequest(fmt.Sprintf("Field %d: coordinates must be [latitude, longitude] pairs.", fieldIndex+1))
			}
			latitude, latErr := parseCoordinate(coordinate[0])
			longitude, lonErr := parseCoordinate(coordinate[1])
			if latErr != nil || lonErr != nil || latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180 {
				return badRequest(fmt.Sprintf("Field %d: enter valid latitude and longitude values.", fieldIndex+1))
			}
			key := fmt.Sprintf("%.6f,%.6f", latitude, longitude)
			if seen[key] {
				return badRequest(fmt.Sprintf("Field %d: the three portals must be different.", fieldIndex+1))
			}
			seen[key] = true
			// The upstream takes "lat,lng" strings and rounds them half up to six
			// decimals itself, so send the shortest exact form, unrounded.
			upstreamFields[fieldIndex].Coordinates = append(upstreamFields[fieldIndex].Coordinates,
				strconv.FormatFloat(latitude, 'f', -1, 64)+","+strconv.FormatFloat(longitude, 'f', -1, 64))
		}
	}
	upstreamBody, err := json.Marshal(map[string]any{"fields": upstreamFields})
	if err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "Could not prepare the MU calculation.")
	}

	apiBaseURL := strings.TrimRight(os.Getenv("MU_CALC_API_BASE_URL"), "/")
	if apiBaseURL == "" {
		apiBaseURL = muCalcDefaultAPIBaseURL
	}
	request, err := http.NewRequestWithContext(e.Request.Context(), http.MethodPost, apiBaseURL+"/getMUbulk", bytes.NewReader(upstreamBody))
	if err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "MU calculator API is not configured correctly.")
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	// The upstream asks for a descriptive User-Agent; Go's default is blocked at its edge.
	request.Header.Set("User-Agent", muCalcUserAgent)
	response, err := (&http.Client{Timeout: muCalcUpstreamTimeout}).Do(request)
	if err != nil {
		return newErrorResponse(e, err, http.StatusBadGateway, "The MU calculator API could not be reached. Please try again.")
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, muCalcMaxRequestBytes+1))
	// Never relay the upstream body as is: an error page (HTML from a proxy or
	// crash) must not be served from our origin, and the page only understands
	// its own shapes, so everything is decoded and rebuilt here.
	var upstream muCalcUpstreamResponse
	if err != nil || len(responseBody) > muCalcMaxRequestBytes || json.Unmarshal(responseBody, &upstream) != nil {
		if err == nil {
			err = fmt.Errorf("upstream returned HTTP %d with a non-JSON body", response.StatusCode)
		}
		return newErrorResponse(e, err, http.StatusBadGateway, "The MU calculator API returned an invalid response. Please try again.")
	}

	if response.StatusCode != http.StatusOK {
		upstreamErr := fmt.Errorf("upstream returned HTTP %d: %s", response.StatusCode, upstream.Error)
		switch {
		case response.StatusCode == http.StatusGatewayTimeout:
			return newErrorResponse(e, upstreamErr, http.StatusGatewayTimeout, "The MU calculator is busy right now. Please try again in a moment.")
		case response.StatusCode == http.StatusTooManyRequests:
			return newErrorResponse(e, upstreamErr, http.StatusServiceUnavailable, "The MU calculator is busy right now. Please try again in a moment.")
		case response.StatusCode >= 400 && response.StatusCode < 500 && upstream.Error != "":
			// Our validation mirrors the upstream's, so this is a case it didn't
			// cover (e.g. a degenerate triangle); the message describes the input.
			return badRequest(upstream.Error)
		default:
			return newErrorResponse(e, upstreamErr, http.StatusBadGateway, "The MU calculation failed. Please try again.")
		}
	}

	if len(upstream.Results) != len(fields) {
		return newErrorResponse(e, fmt.Errorf("upstream returned %d results for %d fields", len(upstream.Results), len(fields)), http.StatusBadGateway, "The MU calculator API returned an invalid response. Please try again.")
	}
	// Results come back one per field, in request order.
	results := make([]muCalcResult, len(fields))
	for index, result := range upstream.Results {
		if !result.OK {
			if result.Error == "deadline exceeded" {
				return newErrorResponse(e, fmt.Errorf("field %d: %s", index+1, result.Error), http.StatusGatewayTimeout, "The MU calculator is busy right now. Please try again in a moment.")
			}
			// One rejected field fails the whole request, as before: the page shows
			// a single error and keeps every field uncalculated.
			return badRequest(fmt.Sprintf("Field %d: %s", index+1, result.Error))
		}
		if result.MU == nil || math.IsNaN(*result.MU) || math.IsInf(*result.MU, 0) {
			return newErrorResponse(e, fmt.Errorf("field %d: missing mu", index+1), http.StatusBadGateway, "The MU calculator API returned an invalid response. Please try again.")
		}
		results[index] = muCalcResult{MU: *result.MU, AreaKm2: result.AreaKm2, Name: fields[index].Name}
	}
	return e.JSON(http.StatusOK, results)
}

func parseCoordinate(raw json.RawMessage) (float64, error) {
	var number float64
	if err := json.Unmarshal(raw, &number); err == nil {
		if math.IsNaN(number) || math.IsInf(number, 0) {
			return 0, fmt.Errorf("coordinate must be finite")
		}
		return number, nil
	}
	var text string
	if err := json.Unmarshal(raw, &text); err != nil {
		return 0, fmt.Errorf("coordinate must be a number")
	}
	return strconv.ParseFloat(strings.TrimSpace(text), 64)
}
