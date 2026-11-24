package platform

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
)

// Error is the single error shape used on the wire by every service:
//
//	{"error": {"code": "insufficient_stock", "message": "..."}}
//
// Message is shown to callers, so it must not contain internal details (hosts,
// stack traces, stock levels). Put those in Cause, which is logged but never
// serialised.
type Error struct {
	Status  int    `json:"-"`
	Code    string `json:"code"`
	Message string `json:"message"`
	Cause   error  `json:"-"`
}

func (e *Error) Error() string {
	if e.Cause != nil {
		return fmt.Sprintf("%d %s: %s: %v", e.Status, e.Code, e.Message, e.Cause)
	}
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Cause }

func ErrNotFound(msg string) *Error {
	return &Error{Status: http.StatusNotFound, Code: "not_found", Message: msg}
}
func ErrBadRequest(msg string) *Error {
	return &Error{Status: http.StatusBadRequest, Code: "bad_request", Message: msg}
}
func ErrConflict(code, msg string) *Error {
	return &Error{Status: http.StatusConflict, Code: code, Message: msg}
}
func ErrUnprocessable(code, msg string) *Error {
	return &Error{Status: http.StatusUnprocessableEntity, Code: code, Message: msg}
}
func ErrUnauthorized(msg string) *Error {
	return &Error{Status: http.StatusUnauthorized, Code: "unauthorized", Message: msg}
}
func ErrInternal(msg string) *Error {
	return &Error{Status: http.StatusInternalServerError, Code: "internal", Message: msg}
}
func ErrUpstream(msg string) *Error {
	return &Error{Status: http.StatusBadGateway, Code: "upstream_error", Message: msg}
}
func ErrUnavailable() *Error {
	return &Error{Status: http.StatusServiceUnavailable, Code: "unavailable", Message: "service temporarily unavailable, please retry"}
}
func ErrTooManyRequests(msg string) *Error {
	return &Error{Status: http.StatusTooManyRequests, Code: "rate_limited", Message: msg}
}

// idPattern is the shape of every identifier that crosses a service boundary:
// product, order and reservation ids, customer ids (IdP subjects), idempotency keys.
var idPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_-]{0,63}$`)

// ValidID reports whether s is a well-formed identifier (1-64 chars, [A-Za-z0-9_-],
// starting with a letter or digit). Validate ids before using them in paths,
// storage, logs or traces.
func ValidID(s string) bool { return idPattern.MatchString(s) }

// AsError extracts an *Error from err, or nil.
func AsError(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return nil
}

// IsStatus reports whether err is an *Error with the given HTTP status.
func IsStatus(err error, status int) bool {
	e := AsError(err)
	return e != nil && e.Status == status
}

// WriteJSON writes v as JSON with the given status.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteError writes err in the standard error envelope. Anything that is not an
// *Error becomes a generic 500: its text may hold internal details, so it is
// never sent to the caller (log it before calling WriteError).
func WriteError(w http.ResponseWriter, err error) {
	e := AsError(err)
	if e == nil {
		e = ErrInternal("internal error")
	}
	WriteJSON(w, e.Status, map[string]*Error{"error": e})
}

// DecodeJSON decodes the request body into v, rejecting unknown fields.
func DecodeJSON(r *http.Request, v any) error {
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return ErrBadRequest("invalid JSON body: " + err.Error())
	}
	return nil
}
