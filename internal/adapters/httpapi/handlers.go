package httpapi

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strconv"
	"time"

	"github.com/dwijamahardika/recond/internal/adapters/sqlite"
	"github.com/dwijamahardika/recond/internal/app"
	"github.com/dwijamahardika/recond/internal/domain"
)

func (h *handlers) webhook(w http.ResponseWriter, r *http.Request) {
	provider := r.PathValue("provider")
	secret, ok := h.WebhookSecrets[provider]
	if !ok || len(secret) == 0 {
		// Fail closed: a provider without a configured secret cannot deliver.
		h.Metrics.Webhook("unknown", "bad_signature")
		writeProblem(w, r, http.StatusUnauthorized, "Unauthorized", "Webhooks are not enabled for this provider.")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeProblem(w, r, http.StatusRequestEntityTooLarge, "Body too large", "Webhook bodies are limited to 64 KiB.")
			return
		}
		writeProblem(w, r, http.StatusBadRequest, "Unreadable body", "")
		return
	}
	if err := Verify(secret, r.Header.Get(HeaderTimestamp), r.Header.Get(HeaderSignature), body, h.WebhookTolerance, h.Clock.Now()); err != nil {
		result := "bad_signature"
		if errors.Is(err, ErrStale) {
			result = "stale_timestamp"
		}
		h.Metrics.Webhook(provider, result)
		LoggerFrom(r.Context()).Warn("webhook rejected", "provider", provider, "reason", err.Error())
		writeProblem(w, r, http.StatusUnauthorized, "Unauthorized", "Invalid or stale webhook signature.")
		return
	}
	res, err := h.Service.ApplyWebhook(r.Context(), provider, body)
	switch {
	case errors.Is(err, app.ErrValidation):
		writeProblem(w, r, http.StatusBadRequest, "Invalid event", err.Error())
		return
	case errors.Is(err, app.ErrUnknownProvider):
		writeProblem(w, r, http.StatusNotFound, "Unknown provider", "")
		return
	case err != nil:
		internal(w, r, err)
		return
	}
	// 200 for duplicates and rejected transitions too: the event is recorded,
	// and a non-2xx would only make the provider retry it forever.
	writeJSON(w, http.StatusOK, res)
}

func (h *handlers) payment(w http.ResponseWriter, r *http.Request) {
	p, hist, err := h.Service.Payment(r.Context(), r.PathValue("provider"), r.PathValue("ref"))
	if errors.Is(err, sqlite.ErrNotFound) {
		writeProblem(w, r, http.StatusNotFound, "Not found", "No events for this payment.")
		return
	}
	if err != nil {
		internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": p.Status(), "state": p, "events": hist})
}

type runRequest struct {
	AsOf *time.Time `json:"as_of"`
}

func (h *handlers) createRun(w http.ResponseWriter, r *http.Request) {
	var req runRequest
	if r.ContentLength != 0 {
		dec := json.NewDecoder(r.Body)
		dec.DisallowUnknownFields()
		if err := dec.Decode(&req); err != nil && !errors.Is(err, io.EOF) {
			writeProblem(w, r, http.StatusBadRequest, "Malformed JSON", err.Error())
			return
		}
	}
	if !h.runMu.TryLock() {
		w.Header().Set("Retry-After", "10")
		writeProblem(w, r, http.StatusConflict, "Run in progress", "Another reconciliation run is in progress.")
		return
	}
	defer h.runMu.Unlock()
	// A large run outlives the default write timeout.
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Minute))
	opt := app.RunOptions{}
	if req.AsOf != nil {
		opt.AsOf = *req.AsOf
	}
	sum, err := h.Service.Run(r.Context(), opt)
	if err != nil {
		internal(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/runs/"+strconv.FormatInt(sum.RunID, 10))
	writeJSON(w, http.StatusCreated, sum)
}

func (h *handlers) listRuns(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	runs, err := h.Service.Runs(r.Context(), limit)
	if err != nil {
		internal(w, r, err)
		return
	}
	if runs == nil {
		runs = []sqlite.Run{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"data": runs})
}

// runID parses {id}; "latest" means the newest run (0).
func runID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	v := r.PathValue("id")
	if v == "latest" {
		return 0, true
	}
	id, err := strconv.ParseInt(v, 10, 64)
	if err != nil || id <= 0 {
		writeProblem(w, r, http.StatusBadRequest, "Invalid run id", "Use a positive integer or \"latest\".")
		return 0, false
	}
	return id, true
}

func (h *handlers) getRun(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	sum, _, err := h.Service.Summary(r.Context(), id)
	if notFound(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, sum)
}

func (h *handlers) runItems(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	if _, _, err := h.Service.Summary(r.Context(), id); notFound(w, r, err) {
		return
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Minute))
	w.Header().Set("Content-Type", "text/csv; charset=utf-8")
	if _, err := h.Service.WriteItemsCSV(r.Context(), id, w); err != nil {
		LoggerFrom(r.Context()).Error("items csv", "err", err) // headers already sent
	}
}

func (h *handlers) verifyRun(w http.ResponseWriter, r *http.Request) {
	id, ok := runID(w, r)
	if !ok {
		return
	}
	if id == 0 {
		_, run, err := h.Service.Summary(r.Context(), 0)
		if notFound(w, r, err) {
			return
		}
		id = run.ID
	}
	_ = http.NewResponseController(w).SetWriteDeadline(time.Now().Add(15 * time.Minute))
	v, err := h.Service.Verify(r.Context(), id)
	if notFound(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, v)
}

func (h *handlers) listExceptions(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := sqlite.ExceptionFilter{Status: q.Get("status"), Class: q.Get("class"), Provider: q.Get("provider")}
	f.Limit, _ = strconv.Atoi(q.Get("limit"))
	f.AfterID, _ = strconv.ParseInt(q.Get("after"), 10, 64)
	if f.Class != "" && !domain.Class(f.Class).Valid() {
		writeProblem(w, r, http.StatusBadRequest, "Invalid class", "")
		return
	}
	list, err := h.Service.Exceptions(r.Context(), f)
	if err != nil {
		internal(w, r, err)
		return
	}
	out := map[string]any{"data": list}
	if list == nil {
		out["data"] = []sqlite.Exception{}
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100 // the store's default and cap
	}
	if len(list) == limit {
		out["next_after"] = list[len(list)-1].ID
	}
	writeJSON(w, http.StatusOK, out)
}

func exceptionID(w http.ResponseWriter, r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil || id <= 0 {
		writeProblem(w, r, http.StatusBadRequest, "Invalid exception id", "")
		return 0, false
	}
	return id, true
}

func (h *handlers) getException(w http.ResponseWriter, r *http.Request) {
	id, ok := exceptionID(w, r)
	if !ok {
		return
	}
	d, err := h.Service.Exception(r.Context(), id)
	if notFound(w, r, err) {
		return
	}
	writeJSON(w, http.StatusOK, d)
}

type decisionRequest struct {
	Reason string `json:"reason"`
}

func (h *handlers) resolve(w http.ResponseWriter, r *http.Request) { h.decide(w, r, h.Service.Resolve) }
func (h *handlers) reopen(w http.ResponseWriter, r *http.Request)  { h.decide(w, r, h.Service.Reopen) }

// decide records a resolution or reopening. The actor is the authenticated
// principal - never a field the client gets to fill in.
func (h *handlers) decide(w http.ResponseWriter, r *http.Request, fn func(ctx context.Context, id int64, actor, reason string) (sqlite.Exception, error)) {
	id, ok := exceptionID(w, r)
	if !ok {
		return
	}
	var req decisionRequest
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeProblem(w, r, http.StatusBadRequest, "Malformed JSON", "Body must be {\"reason\": \"...\"}.")
		return
	}
	e, err := fn(r.Context(), id, PrincipalFrom(r.Context()), req.Reason)
	switch {
	case errors.Is(err, app.ErrValidation):
		writeProblem(w, r, http.StatusBadRequest, "Validation failed", err.Error())
	case errors.Is(err, sqlite.ErrConflict):
		writeProblem(w, r, http.StatusConflict, "Invalid transition", err.Error())
	case notFound(w, r, err):
	default:
		writeJSON(w, http.StatusOK, e)
	}
}

// notFound writes 404 for ErrNotFound and 500 for other errors; it reports
// whether it wrote a response.
func notFound(w http.ResponseWriter, r *http.Request, err error) bool {
	switch {
	case err == nil:
		return false
	case errors.Is(err, sqlite.ErrNotFound):
		writeProblem(w, r, http.StatusNotFound, "Not found", "")
	default:
		internal(w, r, err)
	}
	return true
}

func internal(w http.ResponseWriter, r *http.Request, err error) {
	LoggerFrom(r.Context()).Error("unhandled error", "err", err)
	writeProblem(w, r, http.StatusInternalServerError, "Internal error", "Quote the instance id when reporting.")
}
