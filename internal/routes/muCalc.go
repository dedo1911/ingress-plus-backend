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
	muCalcDefaultAPIBaseURL = "https://api.declassified.rocks"
	muCalcMaxRequestBytes   = 64 << 10
	muCalcMaxFields         = 50
	muCalcMaxNameLength     = 200
)

type muCalcField struct {
	Name        string              `json:"name,omitempty"`
	Coordinates [][]json.RawMessage `json:"coordinates"`
}

// MuCalc proxies the documented Declassified bulk MU operation. Keeping this
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
	for fieldIndex, field := range fields {
		if len(field.Coordinates) != 3 {
			return badRequest(fmt.Sprintf("Field %d: supply exactly three coordinates.", fieldIndex+1))
		}
		if len(field.Name) > muCalcMaxNameLength {
			return badRequest(fmt.Sprintf("Field %d: name must be %d characters or fewer.", fieldIndex+1, muCalcMaxNameLength))
		}
		// The upstream rejects repeated points too, but numbers its fields from 0
		// ("Field 0: ..."), so catch the common case here with the site's wording.
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
		}
	}

	apiBaseURL := strings.TrimRight(os.Getenv("MU_CALC_API_BASE_URL"), "/")
	if apiBaseURL == "" {
		apiBaseURL = muCalcDefaultAPIBaseURL
	}
	request, err := http.NewRequestWithContext(e.Request.Context(), http.MethodPost, apiBaseURL+"/getMUbulk", bytes.NewReader(body))
	if err != nil {
		return newErrorResponse(e, err, http.StatusInternalServerError, "MU calculator API is not configured correctly.")
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return newErrorResponse(e, err, http.StatusBadGateway, "The MU calculator API could not be reached. Please try again.")
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, muCalcMaxRequestBytes+1))
	// Only ever relay JSON, and always label it as such: an upstream error page
	// (HTML from a proxy or crash) must not be served from our origin, and the
	// page can only show a sensible message for a JSON body.
	if err != nil || len(responseBody) > muCalcMaxRequestBytes || !json.Valid(responseBody) {
		if err == nil {
			err = fmt.Errorf("upstream returned HTTP %d with a non-JSON body", response.StatusCode)
		}
		return newErrorResponse(e, err, http.StatusBadGateway, "The MU calculator API returned an invalid response. Please try again.")
	}
	return e.Blob(response.StatusCode, "application/json", responseBody)
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
