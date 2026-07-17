package pricing

import (
	"fmt"
	"net/http"
	"strings"
)

// AppError is a structured error carrying an HTTP status and a code.
type AppError struct {
	Status  int
	Code    string
	Message string
}

func (e *AppError) Error() string { return e.Code + ": " + e.Message }

func (e *AppError) Unwrap() error { return e }

var (
	errNotFound         = &AppError{Status: http.StatusNotFound, Code: "not_found", Message: "quote not found"}
	errExpired          = &AppError{Status: http.StatusGone, Code: "EXPIRED", Message: "quote expired"}
	errBadCurrency      = &AppError{Status: http.StatusBadRequest, Code: "invalid_currency", Message: "currency code must be 3-letter uppercase or a crypto symbol"}
	errBadAmount        = &AppError{Status: http.StatusBadRequest, Code: "invalid_amount", Message: "amount must be > 0"}
	errBadTier          = &AppError{Status: http.StatusBadRequest, Code: "invalid_tier", Message: "unsupported user_tier"}
	errBadSide          = &AppError{Status: http.StatusBadRequest, Code: "invalid_side", Message: "side must be buy or sell"}
	errSpotUnavailable  = &AppError{Status: http.StatusServiceUnavailable, Code: "spot_unavailable", Message: "spot rate unavailable"}
)

var supportedTiers = map[string]bool{"TIER_1": true, "TIER_2": true, "TIER_3": true}

// validateCurrency accepts 3-letter uppercase fiat codes or known crypto
// symbols (length 3-5 uppercase).
func validateCurrency(code string) error {
	if len(code) < 3 || len(code) > 5 {
		return errBadCurrency
	}
	for _, r := range code {
		if r < 'A' || r > 'Z' {
			return errBadCurrency
		}
	}
	return nil
}

func validateTier(t string) error {
	if !supportedTiers[t] {
		return errBadTier
	}
	return nil
}

func validateSide(s string) error {
	if s != "BUY" && s != "SELL" {
		return errBadSide
	}
	return nil
}

func parseAmount(s string) (float64, error) {
	if s == "" {
		return 0, errBadAmount
	}
	var f float64
	_, err := fmt.Sscanf(s, "%f", &f)
	if err != nil {
		return 0, errBadAmount
	}
	if f <= 0 {
		return 0, errBadAmount
	}
	return f, nil
}

// quoteRequest is the POST /v1/quotes single payload.
type quoteRequest struct {
	From     string `json:"from"`
	To       string `json:"to"`
	Amount   string `json:"amount"`
	UserTier string `json:"user_tier"`
	Side     string `json:"side"`
}

// bulkRequest is the POST /v1/quotes bulk payload.
type bulkRequest struct {
	Items []quoteRequest `json:"items"`
}

func validateRequest(req quoteRequest) error {
	if err := validateCurrency(req.From); err != nil {
		return err
	}
	if err := validateCurrency(req.To); err != nil {
		return err
	}
	if _, err := parseAmount(req.Amount); err != nil {
		return err
	}
	if err := validateTier(req.UserTier); err != nil {
		return err
	}
	if err := validateSide(req.Side); err != nil {
		return err
	}
	return nil
}

// requestIDFromContext retrieves the request id set by the middleware.
func requestIDFromContext(r *http.Request) string {
	if v := r.Header.Get("X-Request-ID"); v != "" {
		return v
	}
	return strings.TrimSpace(r.URL.Query().Get("request_id"))
}