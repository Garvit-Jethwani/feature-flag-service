package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/flag"
)

// apiError is a request-level failure raised by the HTTP layer itself, such
// as a malformed body.
type apiError struct {
	status  int
	code    string
	message string
}

func (e *apiError) Error() string { return e.message }

func badRequest(message string) error {
	return &apiError{status: http.StatusBadRequest, code: "invalid_request", message: message}
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// classify is the single place where errors become HTTP status codes. Any
// error it does not recognise is a 500 with a generic message, so internal
// details never reach the client.
func classify(err error) (status int, code, message string) {
	var apiErr *apiError
	var validation *flag.ValidationError
	switch {
	case errors.As(err, &apiErr):
		return apiErr.status, apiErr.code, apiErr.message
	case errors.As(err, &validation):
		return http.StatusBadRequest, "invalid_argument", validation.Error()
	case errors.Is(err, flag.ErrNotFound):
		return http.StatusNotFound, "flag_not_found", "flag not found"
	case errors.Is(err, flag.ErrOverrideNotFound):
		return http.StatusNotFound, "override_not_found", "the flag has no override for this user"
	case errors.Is(err, flag.ErrAlreadyExists):
		return http.StatusConflict, "flag_exists", "a flag with this name already exists"
	case errors.Is(err, flag.ErrHasDependents):
		return http.StatusConflict, "flag_has_dependents", "other flags require this flag; remove those dependencies first"
	case errors.Is(err, flag.ErrDependencyCycle):
		return http.StatusConflict, "dependency_cycle", "this change would make a flag require itself, directly or through other flags"
	case errors.Is(err, flag.ErrDependencyTooDeep), errors.Is(err, flag.ErrPrerequisiteDepth):
		// The first is raised when a change is rejected. The second is the
		// evaluator's own guard; it should be unreachable, but if it ever
		// fires it is a state problem, not a server crash.
		return http.StatusConflict, "dependency_too_deep", "the chain of prerequisite flags is longer than 10 levels"
	case errors.Is(err, context.DeadlineExceeded):
		return http.StatusServiceUnavailable, "timeout", "the request took too long; try again"
	default:
		return http.StatusInternalServerError, "internal_error", "internal server error"
	}
}

// fail writes the response for err and logs server-side failures with the
// request ID, so a client report can be matched to a log entry.
func (a *api) fail(w http.ResponseWriter, r *http.Request, err error) {
	status, code, message := classify(err)
	if status >= http.StatusInternalServerError {
		a.log.ErrorContext(r.Context(), "request failed",
			"request_id", requestID(r.Context()),
			"status", status,
			"error", err.Error(),
		)
	}
	writeError(w, status, code, message)
}

func writeError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: message}})
}

func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	// The status line is already sent, so an encoding or write failure here
	// (normally a client that went away) cannot be reported to the client.
	_ = json.NewEncoder(w).Encode(body)
}
