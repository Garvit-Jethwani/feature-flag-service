package httpapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/Garvit-Jethwani/feature-flag-service/internal/flag"
	"github.com/Garvit-Jethwani/feature-flag-service/internal/service"
)

// maxBodyBytes is the largest request body accepted.
const maxBodyBytes = 1 << 20 // 1 MB

// Response shapes.

type flagSummary struct {
	Name              string    `json:"name"`
	Description       string    `json:"description"`
	Enabled           bool      `json:"enabled"`
	RolloutPercentage int       `json:"rollout_percentage"`
	Prerequisites     []string  `json:"prerequisites"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type flagDetail struct {
	flagSummary
	Overrides []overrideBody `json:"overrides"`
}

type overrideBody struct {
	UserID    string    `json:"user_id"`
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
}

type flagList struct {
	Flags []flagSummary `json:"flags"`
	// NextAfter is set when there may be more flags; pass it as ?after=.
	NextAfter string `json:"next_after,omitempty"`
}

type evaluationBody struct {
	Flag    string `json:"flag"`
	UserID  string `json:"user_id"`
	Enabled bool   `json:"enabled"`
	Reason  string `json:"reason"`
}

func toSummary(f flag.Flag) flagSummary {
	prerequisites := f.Prerequisites
	if prerequisites == nil {
		prerequisites = []string{} // render as [] rather than null
	}
	return flagSummary{
		Name:              f.Name,
		Description:       f.Description,
		Enabled:           f.Enabled,
		RolloutPercentage: f.RolloutPercentage,
		Prerequisites:     prerequisites,
		CreatedAt:         f.CreatedAt,
		UpdatedAt:         f.UpdatedAt,
	}
}

func toOverride(o flag.Override) overrideBody {
	return overrideBody{UserID: o.UserID, Enabled: o.Enabled, UpdatedAt: o.UpdatedAt}
}

func toDetail(f flag.Flag) flagDetail {
	overrides := make([]overrideBody, 0, len(f.Overrides))
	for _, o := range f.Overrides {
		overrides = append(overrides, toOverride(o))
	}
	return flagDetail{flagSummary: toSummary(f), Overrides: overrides}
}

// decodeJSON reads a single JSON object from the request body into dst. It
// enforces the content type and the size limit, and rejects unknown fields
// and trailing data.
func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || mediaType != "application/json" {
		return &apiError{
			status:  http.StatusUnsupportedMediaType,
			code:    "unsupported_media_type",
			message: "Content-Type must be application/json",
		}
	}

	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)
	decoder := json.NewDecoder(r.Body)
	decoder.DisallowUnknownFields()

	if err := decoder.Decode(dst); err != nil {
		return decodeError(err)
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			return decodeError(err)
		}
		return badRequest("the body must contain a single JSON object")
	}
	return nil
}

func decodeError(err error) error {
	var tooLarge *http.MaxBytesError
	var syntax *json.SyntaxError
	var wrongType *json.UnmarshalTypeError
	switch {
	case errors.As(err, &tooLarge):
		return &apiError{
			status:  http.StatusRequestEntityTooLarge,
			code:    "body_too_large",
			message: "the body must be at most 1 MB",
		}
	case errors.Is(err, io.EOF):
		return badRequest("the body must not be empty")
	case errors.As(err, &syntax), errors.Is(err, io.ErrUnexpectedEOF):
		return badRequest("the body is not valid JSON")
	case errors.As(err, &wrongType):
		if wrongType.Field == "" {
			return badRequest("the body must be a JSON object")
		}
		return badRequest("field " + strconv.Quote(wrongType.Field) + " has the wrong type")
	case strings.HasPrefix(err.Error(), "json: unknown field "):
		// encoding/json has no typed error for this case.
		return badRequest("unknown field " + strings.TrimPrefix(err.Error(), "json: unknown field "))
	default:
		return badRequest("the body could not be read as JSON")
	}
}

func (a *api) createFlag(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name        string `json:"name"`
		Description string `json:"description"`
		Enabled     bool   `json:"enabled"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		a.fail(w, r, err)
		return
	}
	f, err := a.svc.CreateFlag(r.Context(), principalFrom(r.Context()).Actor, service.CreateInput{
		Name:        body.Name,
		Description: body.Description,
		Enabled:     body.Enabled,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/flags/"+f.Name)
	writeJSON(w, http.StatusCreated, toDetail(f))
}

func (a *api) listFlags(w http.ResponseWriter, r *http.Request) {
	query := r.URL.Query()
	limit := service.DefaultListLimit
	if raw := query.Get("limit"); raw != "" {
		parsed, err := strconv.Atoi(raw)
		if err != nil || parsed < 1 {
			a.fail(w, r, &flag.ValidationError{Field: "limit", Message: "must be from 1 to 500"})
			return
		}
		limit = parsed
	}

	flags, err := a.svc.ListFlags(r.Context(), query.Get("after"), limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}

	out := flagList{Flags: make([]flagSummary, 0, len(flags))}
	for _, f := range flags {
		out.Flags = append(out.Flags, toSummary(f))
	}
	if len(flags) == limit {
		out.NextAfter = flags[len(flags)-1].Name
	}
	writeJSON(w, http.StatusOK, out)
}

func (a *api) getFlag(w http.ResponseWriter, r *http.Request) {
	f, err := a.svc.GetFlag(r.Context(), r.PathValue("name"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDetail(f))
}

func (a *api) updateFlag(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Description *string `json:"description"`
		Enabled     *bool   `json:"enabled"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		a.fail(w, r, err)
		return
	}
	f, err := a.svc.UpdateFlag(r.Context(), principalFrom(r.Context()).Actor, r.PathValue("name"), flag.Patch{
		Description: body.Description,
		Enabled:     body.Enabled,
	})
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDetail(f))
}

func (a *api) deleteFlag(w http.ResponseWriter, r *http.Request) {
	if err := a.svc.DeleteFlag(r.Context(), principalFrom(r.Context()).Actor, r.PathValue("name")); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) setOverride(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled"`
	}
	if err := decodeJSON(w, r, &body); err != nil {
		a.fail(w, r, err)
		return
	}
	if body.Enabled == nil {
		a.fail(w, r, &flag.ValidationError{Field: "enabled", Message: "is required"})
		return
	}
	o, err := a.svc.SetOverride(r.Context(), principalFrom(r.Context()).Actor,
		r.PathValue("name"), r.PathValue("userID"), *body.Enabled)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toOverride(o))
}

func (a *api) deleteOverride(w http.ResponseWriter, r *http.Request) {
	err := a.svc.DeleteOverride(r.Context(), principalFrom(r.Context()).Actor,
		r.PathValue("name"), r.PathValue("userID"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (a *api) evaluate(w http.ResponseWriter, r *http.Request) {
	evaluation, err := a.svc.Evaluate(r.Context(), r.PathValue("name"), r.URL.Query().Get("user_id"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, evaluationBody{
		Flag:    evaluation.Flag,
		UserID:  evaluation.UserID,
		Enabled: evaluation.Enabled,
		Reason:  string(evaluation.Reason),
	})
}
