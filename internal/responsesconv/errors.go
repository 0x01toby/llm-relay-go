package responsesconv

import (
	"errors"
	"net/http"
)

// CompatError is the structured error returned by the converter. It mirrors
// the TS ResponsesChatCompatError and carries status (always 400 — the only
// status the converter produces). It implements the error interface so helper
// functions can return it directly, and callers recover it with errors.As.
type CompatError struct {
	Status  int    `json:"status"`
	Message string `json:"message"`
	Code    *string `json:"code"`
	Param   *string `json:"param"`
}

// Error implements the error interface.
func (c CompatError) Error() string { return c.Message }

// newError builds a status-400 CompatError. A non-empty param/code is attached.
func newError(message string, param string, code string) CompatError {
	ce := CompatError{Status: 400, Message: message}
	if param != "" {
		p := param
		ce.Param = &p
	}
	if code != "" {
		c := code
		ce.Code = &c
	}
	return ce
}

func newErrorf(param, format string, args ...interface{}) CompatError {
	return newError(sprintf(format, args...), param, "")
}

// errNotObject is returned when a JSON value that must be an object isn't.
var errNotObject = CompatError{Status: 400, Message: "value is not a JSON object"}

// AsCompatError extracts a CompatError from an error returned by the converter.
// Reports whether it was one of our structured errors; when false (shouldn't
// happen for converter output), a generic 400 error wraps the message.
func AsCompatError(err error) (CompatError, bool) {
	if err == nil {
		return CompatError{}, false
	}
	var ce CompatError
	if errors.As(err, &ce) {
		return ce, true
	}
	return newError(err.Error(), "", ""), true
}

// WriteErrorResponse writes an OpenAI-shaped error JSON to w for the given
// CompatError. Mirrors createResponsesChatCompatErrorResponse.
func WriteErrorResponse(w http.ResponseWriter, ce CompatError) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(ce.Status)
	_, _ = w.Write([]byte(jsonMustEncode(Obj{
		"error": Obj{
			"message": ce.Message,
			"type":    "invalid_request_error",
			"param":   ce.Param,
			"code":    ce.Code,
		},
	})))
}
