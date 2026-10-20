package httpapi

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"log/slog"
	"net/http"
	"runtime/debug"
	"time"
)

type ctxKey int

const (
	ctxRequestID ctxKey = iota
	ctxLogger
	ctxPrincipal
)

// Request headers recond reads.
const (
	HeaderRequestID = "X-Request-ID"
	HeaderAPIKey    = "X-API-Key" //nolint:gosec // a header name, not a credential
)

// RequestIDFrom returns the request ID stored in ctx.
func RequestIDFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxRequestID).(string)
	return v
}

// LoggerFrom returns the request-scoped logger, or the default.
func LoggerFrom(ctx context.Context) *slog.Logger {
	if l, ok := ctx.Value(ctxLogger).(*slog.Logger); ok {
		return l
	}
	return slog.Default()
}

// PrincipalFrom returns the authenticated API principal, if any.
func PrincipalFrom(ctx context.Context) string {
	v, _ := ctx.Value(ctxPrincipal).(string)
	return v
}

// Middleware is the decorator shape; chain applies them so the first listed
// runs outermost.
type Middleware func(http.Handler) http.Handler

func chain(h http.Handler, mws ...Middleware) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

func newID() string {
	b := make([]byte, 12)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// requestID honours a sane inbound ID or mints one, and echoes it back.
func requestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get(HeaderRequestID)
		if id == "" || len(id) > 64 {
			id = newID()
		}
		w.Header().Set(HeaderRequestID, id)
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxRequestID, id)))
	})
}

func recoverer(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() { //nolint:contextcheck // the request context travels in r
			if p := recover(); p != nil {
				if p == http.ErrAbortHandler { //nolint:errorlint // sentinel compared by identity per net/http docs
					panic(p)
				}
				LoggerFrom(r.Context()).Error("panic recovered", "panic", p, "stack", string(debug.Stack()))
				writeProblem(w, r, http.StatusInternalServerError, "Internal error", "Unexpected failure.")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (s *statusWriter) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusWriter) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	n, err := s.ResponseWriter.Write(b)
	s.bytes += n
	return n, err
}

func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func logging(base *slog.Logger, m *Metrics) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			log := base.With("request_id", RequestIDFrom(r.Context()))
			ctx := context.WithValue(r.Context(), ctxLogger, log)
			sw := &statusWriter{ResponseWriter: w}
			next.ServeHTTP(sw, r.WithContext(ctx))
			if sw.status == 0 {
				sw.status = http.StatusOK
			}
			dur := time.Since(start)
			route := r.Pattern
			if route == "" {
				route = "unmatched"
			}
			m.Observe(route, r.Method, sw.status, dur)
			lvl := slog.LevelInfo
			if sw.status >= 500 {
				lvl = slog.LevelError
			}
			log.Log(ctx, lvl, "http request", "method", r.Method, "route", route, "status", sw.status, "bytes", sw.bytes,
				"duration_ms", dur.Milliseconds(), "principal", PrincipalFrom(ctx))
		})
	}
}

// apiKeyAuth compares the presented key against every configured key in
// constant time. With no keys configured the API is closed, not open.
func apiKeyAuth(keys map[string]string) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			presented := []byte(r.Header.Get(HeaderAPIKey))
			principal := ""
			for key, name := range keys {
				if subtle.ConstantTimeCompare([]byte(key), presented) == 1 {
					principal = name
				}
			}
			if principal == "" {
				w.Header().Set("WWW-Authenticate", `ApiKey realm="recond"`)
				writeProblem(w, r, http.StatusUnauthorized, "Unauthorized", "Missing or invalid X-API-Key.")
				return
			}
			next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxPrincipal, principal)))
		})
	}
}

func maxBody(n int64) Middleware {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, n)
			next.ServeHTTP(w, r)
		})
	}
}

// Problem is an RFC 9457 problem details body.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"` // request ID
}

func writeProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{Type: "about:blank", Title: title, Status: status, Detail: detail, Instance: RequestIDFrom(r.Context())})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}
