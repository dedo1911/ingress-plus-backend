package routes

import (
	"bytes"
	"encoding/json"
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
)

type muCalcField struct {
	Name        string              `json:"name,omitempty"`
	Coordinates [][]json.RawMessage `json:"coordinates"`
}

// MuCalc proxies the documented Declassified bulk MU operation. Keeping this
// call in the API backend means the browser uses one stable Ingress Plus route;
// MU_CALC_API_BASE_URL can switch its upstream when we host the calculation.
func MuCalc(e *core.RequestEvent) error {
	body, err := io.ReadAll(io.LimitReader(e.Request.Body, muCalcMaxRequestBytes+1))
	if err != nil {
		return e.JSON(http.StatusBadRequest, map[string]string{"error": "Could not read the request body."})
	}
	if len(body) > muCalcMaxRequestBytes {
		return e.JSON(http.StatusRequestEntityTooLarge, map[string]string{"error": "Request body is too large."})
	}

	var fields []muCalcField
	if err := json.Unmarshal(body, &fields); err != nil {
		return e.JSON(http.StatusBadRequest, map[string]string{"error": "Request body must be a JSON array of fields."})
	}
	if len(fields) == 0 || len(fields) > muCalcMaxFields {
		return e.JSON(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Submit between 1 and %d fields.", muCalcMaxFields)})
	}
	for fieldIndex, field := range fields {
		if len(field.Coordinates) != 3 {
			return e.JSON(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Field %d: supply exactly three coordinates.", fieldIndex+1)})
		}
		if len(field.Name) > 200 {
			return e.JSON(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Field %d: name must be 200 characters or fewer.", fieldIndex+1)})
		}
		for _, coordinate := range field.Coordinates {
			if len(coordinate) != 2 {
				return e.JSON(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Field %d: coordinates must be [latitude, longitude] pairs.", fieldIndex+1)})
			}
			latitude, latErr := parseCoordinate(coordinate[0])
			longitude, lonErr := parseCoordinate(coordinate[1])
			if latErr != nil || lonErr != nil || latitude < -90 || latitude > 90 || longitude < -180 || longitude > 180 {
				return e.JSON(http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("Field %d: enter valid latitude and longitude values.", fieldIndex+1)})
			}
		}
	}

	apiBaseURL := strings.TrimRight(os.Getenv("MU_CALC_API_BASE_URL"), "/")
	if apiBaseURL == "" {
		apiBaseURL = muCalcDefaultAPIBaseURL
	}
	request, err := http.NewRequestWithContext(e.Request.Context(), http.MethodPost, apiBaseURL+"/getMUbulk", bytes.NewReader(body))
	if err != nil {
		return e.JSON(http.StatusInternalServerError, map[string]string{"error": "MU calculator API is not configured correctly."})
	}
	request.Header.Set("Content-Type", "application/json")
	response, err := (&http.Client{Timeout: 15 * time.Second}).Do(request)
	if err != nil {
		return e.JSON(http.StatusBadGateway, map[string]string{"error": "The MU calculator API could not be reached. Please try again."})
	}
	defer response.Body.Close()
	responseBody, err := io.ReadAll(io.LimitReader(response.Body, muCalcMaxRequestBytes+1))
	if err != nil || len(responseBody) > muCalcMaxRequestBytes {
		return e.JSON(http.StatusBadGateway, map[string]string{"error": "The MU calculator API returned an invalid response."})
	}
	contentType := response.Header.Get("Content-Type")
	if contentType == "" {
		contentType = "application/json"
	}
	return e.Blob(response.StatusCode, contentType, responseBody)
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
