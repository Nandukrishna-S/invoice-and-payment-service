// Package webhookreceiver is demo tooling, not part of the product: a tiny HTTP
// receiver that logs every webhook delivery and whether its signature verified,
// the way a business's own endpoint should. It has no business logic.
//
// Routes:
//
//	POST /hook               verify; 2xx if the signature is good, 401 if not
//	POST /hook/status/{code} verify and log, then answer with {code} (to demo retries)
//	POST /hook/slow          verify and log, then answer after 10s (to demo timeouts)
//	POST /secrets            add a signing secret to verify with: {"secret":"whsec_..."}
//	GET  /healthz
package webhookreceiver

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/go-chi/chi/v5"

	"invoice-and-payment-service/internal/webhooks"
)

const (
	maxBodyBytes = 1 << 20
	maxSeen      = 10000
)

// slowDelay is how long /hook/slow takes to answer; a variable so tests can shorten it.
var slowDelay = 10 * time.Second

type Receiver struct {
	mu      sync.Mutex
	secrets []string
	seen    map[string]bool // webhook-ids already received, to flag redeliveries
	order   []string

	now func() time.Time
}

// New returns a receiver verifying with the given secrets (more can be added later).
func New(secrets ...string) *Receiver {
	r := &Receiver{seen: map[string]bool{}, now: time.Now}
	for _, s := range secrets {
		if s != "" {
			r.secrets = append(r.secrets, s)
		}
	}
	return r
}

func (r *Receiver) Routes() http.Handler {
	mux := chi.NewRouter()
	mux.Post("/hook", r.hook(http.StatusOK))
	mux.Post("/hook/status/{code}", r.hookWithStatus)
	mux.Post("/hook/slow", r.slow)
	mux.Post("/secrets", r.addSecret)
	mux.Get("/healthz", func(w http.ResponseWriter, _ *http.Request) { _, _ = io.WriteString(w, `{"status":"ok"}`) })
	return mux
}

// verified is the result of checking one delivery.
type verified struct {
	ok     bool
	reason string
}

// check logs the delivery and reports whether its signature verified. The body is
// read raw, because the signature covers exactly those bytes.
func (r *Receiver) check(w http.ResponseWriter, req *http.Request) (verified, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, req.Body, maxBodyBytes))
	if err != nil {
		http.Error(w, "unreadable body", http.StatusBadRequest)
		return verified{}, false
	}
	id := req.Header.Get(webhooks.HeaderID)

	r.mu.Lock()
	secrets := append([]string(nil), r.secrets...)
	duplicate := id != "" && r.seen[id]
	if id != "" && !duplicate {
		r.seen[id] = true
		r.order = append(r.order, id)
		if len(r.order) > maxSeen {
			delete(r.seen, r.order[0])
			r.order = r.order[1:]
		}
	}
	r.mu.Unlock()

	v := verified{ok: true}
	switch {
	case len(secrets) == 0:
		v = verified{reason: "no signing secret configured; add one with POST /secrets"}
	default:
		if err := webhooks.Verify(secrets, id, req.Header.Get(webhooks.HeaderTimestamp), req.Header.Get(webhooks.HeaderSignature), body, r.now()); err != nil {
			v = verified{reason: err.Error()}
		}
	}

	// What is logged: identifiers and the verdict. Never the secret, the signature or
	// the body itself, which belongs to the business.
	var evt struct {
		Type string `json:"type"`
		Data struct {
			Object struct {
				ID     string `json:"id"`
				Status string `json:"status"`
			} `json:"object"`
		} `json:"data"`
	}
	_ = json.Unmarshal(body, &evt)
	attrs := []any{
		"webhook_id", id,
		"event_type", evt.Type,
		"invoice_id", evt.Data.Object.ID,
		"invoice_status", evt.Data.Object.Status,
		"signature_verified", v.ok,
		"redelivery", duplicate,
		"path", req.URL.Path,
	}
	if !v.ok {
		attrs = append(attrs, "reason", v.reason)
		slog.WarnContext(req.Context(), "webhook received, signature NOT verified", attrs...)
	} else {
		slog.InfoContext(req.Context(), "webhook received, signature verified", attrs...)
	}
	return v, true
}

// hook answers okStatus when the signature verified, and 401 when it did not, which
// is what a real endpoint does so the sender keeps retrying.
func (r *Receiver) hook(okStatus int) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		v, ok := r.check(w, req)
		if !ok {
			return
		}
		if !v.ok {
			http.Error(w, "invalid signature", http.StatusUnauthorized)
			return
		}
		w.WriteHeader(okStatus)
	}
}

func (r *Receiver) hookWithStatus(w http.ResponseWriter, req *http.Request) {
	code, err := strconv.Atoi(chi.URLParam(req, "code"))
	if err != nil || code < 200 || code > 599 {
		http.Error(w, "status must be a number between 200 and 599", http.StatusBadRequest)
		return
	}
	if _, ok := r.check(w, req); !ok {
		return
	}
	w.WriteHeader(code)
}

func (r *Receiver) slow(w http.ResponseWriter, req *http.Request) {
	if _, ok := r.check(w, req); !ok {
		return
	}
	select {
	case <-time.After(slowDelay):
		w.WriteHeader(http.StatusOK)
	case <-req.Context().Done():
	}
}

// addSecret registers a signing secret at runtime, so the demo does not need a
// restart after an endpoint is created. It is deliberately unauthenticated: this is
// demo tooling and must never be exposed beyond localhost.
func (r *Receiver) addSecret(w http.ResponseWriter, req *http.Request) {
	var in struct {
		Secret string `json:"secret"`
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, req.Body, 4096))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&in); err != nil {
		http.Error(w, "body must be {\"secret\":\"whsec_...\"}", http.StatusBadRequest)
		return
	}
	if _, err := webhooks.SecretKey(in.Secret); err != nil {
		http.Error(w, err.Error(), http.StatusUnprocessableEntity)
		return
	}
	r.mu.Lock()
	for _, s := range r.secrets {
		if s == in.Secret {
			r.mu.Unlock()
			w.WriteHeader(http.StatusOK)
			return
		}
	}
	r.secrets = append(r.secrets, in.Secret)
	n := len(r.secrets)
	r.mu.Unlock()
	slog.InfoContext(req.Context(), "signing secret added", "secrets_known", n)
	w.WriteHeader(http.StatusCreated)
}
