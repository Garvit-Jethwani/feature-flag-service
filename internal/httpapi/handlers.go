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

// toSummary converts a domain flag to its JSON shape without overrides.
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

// toOverride converts a domain override to its JSON shape.
func toOverride(o flag.Override) overrideBody {
	return overrideBody{UserID: o.UserID, Enabled: o.Enabled, UpdatedAt: o.UpdatedAt}
}

// toDetail converts a domain flag to its full JSON shape, with overrides.
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

// decodeError turns a JSON decoding failure into a client-facing error with
// a message that says what was wrong without echoing internals.
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

// createFlag handles POST /v1/flags. A new flag is disabled unless the body
// sets "enabled": true. The actor is taken from the API key, not the body.
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

// listFlags handles GET /v1/flags: one page of flags in name order. When the
// page is full, next_after tells the client where the next page starts.
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

// getFlag handles GET /v1/flags/{name}: the flag with its per-user overrides
// and prerequisites.
func (a *api) getFlag(w http.ResponseWriter, r *http.Request) {
	f, err := a.svc.GetFlag(r.Context(), r.PathValue("name"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toDetail(f))
}

// updateFlag handles PATCH /v1/flags/{name}. Only the fields present in the
// body change; this is how a flag is toggled globally.
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

// deleteFlag handles DELETE /v1/flags/{name}. The flag's overrides are
// removed with it.
func (a *api) deleteFlag(w http.ResponseWriter, r *http.Request) {
	if err := a.svc.DeleteFlag(r.Context(), principalFrom(r.Context()).Actor, r.PathValue("name")); err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// setOverride handles PUT /v1/flags/{name}/overrides/{userID}: forces the
// flag on or off for one user, whatever the global state is.
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

// deleteOverride handles DELETE /v1/flags/{name}/overrides/{userID}: the
// user goes back to the flag's global state.
func (a *api) deleteOverride(w http.ResponseWriter, r *http.Request) {
	err := a.svc.DeleteOverride(r.Context(), principalFrom(r.Context()).Actor,
		r.PathValue("name"), r.PathValue("userID"))
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// evaluate handles GET /v1/flags/{name}/evaluate?user_id=U: is this flag on
// for this user? The response includes the reason code for the decision.
//
// Results are cached. The X-Cache response header says how this request was
// answered: HIT (from the cache), MISS (computed and then cached), or BYPASS
// (the client sent "Cache-Control: no-cache" to force a fresh result).
func (a *api) evaluate(w http.ResponseWriter, r *http.Request) {
	bypass := wantsFreshResult(r.Header.Values("Cache-Control"))
	evaluation, err := a.svc.Evaluate(r.Context(), r.PathValue("name"), r.URL.Query().Get("user_id"), bypass)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	w.Header().Set("X-Cache", string(evaluation.Cache))
	writeJSON(w, http.StatusOK, evaluationBody{
		Flag:    evaluation.Flag,
		UserID:  evaluation.UserID,
		Enabled: evaluation.Enabled,
		Reason:  string(evaluation.Reason),
	})
}

// wantsFreshResult reports whether a request's Cache-Control header contains
// the no-cache directive, which asks the service to skip its cache.
func wantsFreshResult(cacheControl []string) bool {
	for _, value := range cacheControl {
		for _, directive := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(directive), "no-cache") {
				return true
			}
		}
	}
	return false
}

// auditEntryBody is the JSON shape of one audit entry. Previous and New are
// passed through as stored; each is null when there was no value.
type auditEntryBody struct {
	ID         int64           `json:"id"`
	Flag       string          `json:"flag"`
	Action     string          `json:"action"`
	Actor      string          `json:"actor"`
	Previous   json.RawMessage `json:"previous"`
	New        json.RawMessage `json:"new"`
	OccurredAt time.Time       `json:"occurred_at"`
}

type auditList struct {
	Entries []auditEntryBody `json:"entries"`
	// NextBefore is set when there may be older entries; pass it as ?before=.
	NextBefore int64 `json:"next_before,omitempty"`
}

// auditPage reads the limit and before query parameters shared by the audit
// endpoints. A missing limit is returned as zero, meaning the default.
func auditPage(r *http.Request) (before int64, limit int, err error) {
	query := r.URL.Query()
	if raw := query.Get("limit"); raw != "" {
		limit, err = strconv.Atoi(raw)
		if err != nil || limit < 1 {
			return 0, 0, &flag.ValidationError{Field: "limit", Message: "must be from 1 to 200"}
		}
	}
	if raw := query.Get("before"); raw != "" {
		before, err = strconv.ParseInt(raw, 10, 64)
		if err != nil || before < 1 {
			return 0, 0, &flag.ValidationError{Field: "before", Message: "must be a positive number"}
		}
	}
	return before, limit, nil
}

// writeAudit writes one page of audit entries. limit is the page size that
// was asked for (zero meaning the default); a full page means there may be
// more, so the id to continue from is included.
func writeAudit(w http.ResponseWriter, entries []flag.AuditEntry, limit int) {
	if limit == 0 {
		limit = service.DefaultAuditLimit
	}
	out := auditList{Entries: make([]auditEntryBody, 0, len(entries))}
	for _, e := range entries {
		out.Entries = append(out.Entries, auditEntryBody{
			ID:         e.ID,
			Flag:       e.FlagName,
			Action:     string(e.Action),
			Actor:      e.Actor,
			Previous:   e.Previous,
			New:        e.New,
			OccurredAt: e.OccurredAt,
		})
	}
	if len(entries) == limit {
		out.NextBefore = entries[len(entries)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

// listAudit handles GET /v1/audit: every flag's changes, newest first.
// Admin keys only, because entries name actors and overridden users.
func (a *api) listAudit(w http.ResponseWriter, r *http.Request) {
	before, limit, err := auditPage(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	entries, err := a.svc.Audit(r.Context(), before, limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeAudit(w, entries, limit)
}

// flagAudit handles GET /v1/flags/{name}/audit: one flag's changes, newest
// first. It also works after the flag has been deleted.
func (a *api) flagAudit(w http.ResponseWriter, r *http.Request) {
	before, limit, err := auditPage(r)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	entries, err := a.svc.FlagAudit(r.Context(), r.PathValue("name"), before, limit)
	if err != nil {
		a.fail(w, r, err)
		return
	}
	writeAudit(w, entries, limit)
}
