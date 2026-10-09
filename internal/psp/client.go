// Package psp is the invoice service's client for the payment provider.
//
// Its one job beyond HTTP is classification. A call has exactly one of these
// outcomes, and only an explicit, well-formed answer from the provider can be
// Succeeded or Failed. A timeout, a dropped connection, a 5xx, an unexpected
// 4xx or a malformed body is Unknown: we cannot tell whether the customer was
// charged, so callers must leave the payment pending and ask the provider
// again later. Nothing here ever turns uncertainty into "failed".
package psp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"time"

	"github.com/google/uuid"
)

type Status string

const (
	// Succeeded and Failed are the only definitive outcomes.
	Succeeded Status = "succeeded"
	Failed    Status = "failed"
	// Processing means the provider is still working on the charge.
	Processing Status = "processing"
	// NotFound means the provider has no charge for that key (Query only).
	NotFound Status = "not_found"
	// Unknown means we could not learn the outcome.
	Unknown Status = "unknown"
)

// Failure codes the provider may report; they match the API's failure_code values.
const (
	CodeInsufficientFunds = "insufficient_funds"
	CodeCardDeclined      = "card_declined"
)

const maxResponseBytes = 1 << 20

type Outcome struct {
	Status Status
	// PSPRef is the provider's reference, set when Succeeded.
	PSPRef string
	// Code is the decline reason, set when Failed.
	Code string
	// Cause explains an Unknown outcome, for logs. It is never shown to API clients.
	Cause error
}

// Definitive reports whether the outcome may be recorded as the payment's final state.
func (o Outcome) Definitive() bool { return o.Status == Succeeded || o.Status == Failed }

type Client struct {
	baseURL string
	http    *http.Client
}

// NewClient builds a client whose every call is bounded: connecting takes at
// most connectTimeout and the whole call (including reading the response) at
// most totalTimeout.
func NewClient(baseURL string, connectTimeout, totalTimeout time.Duration) *Client {
	transport := &http.Transport{
		DialContext:         (&net.Dialer{Timeout: connectTimeout}).DialContext,
		TLSHandshakeTimeout: connectTimeout,
		MaxIdleConnsPerHost: 20,
		IdleConnTimeout:     60 * time.Second,
	}
	return &Client{
		baseURL: baseURL,
		http:    &http.Client{Transport: transport, Timeout: totalTimeout},
	}
}

type chargeRequest struct {
	Token       string `json:"token"`
	AmountCents int64  `json:"amount_cents"`
}

// wireResponse is the provider's answer; the field names are the provider's own.
type wireResponse struct {
	Status string `json:"status"`
	PSPRef string `json:"psp_ref"`
	Code   string `json:"code"`
}

// Charge asks the provider to charge the card. paymentRef is sent as the
// idempotency key, so repeating the call can never charge twice. The card token
// is sent to the provider and nowhere else: it is not logged or kept.
func (c *Client) Charge(ctx context.Context, paymentRef uuid.UUID, cardToken string, amountCents int64) Outcome {
	body, err := json.Marshal(chargeRequest{Token: cardToken, AmountCents: amountCents})
	if err != nil {
		return unknown(fmt.Errorf("encode charge: %w", err))
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/charges", bytes.NewReader(body))
	if err != nil {
		return unknown(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", paymentRef.String())
	return c.do(ctx, req, paymentRef, false)
}

// Query asks the provider what happened to the charge made with paymentRef.
// The provider is the only source of truth for the outcome.
func (c *Client) Query(ctx context.Context, paymentRef uuid.UUID) Outcome {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/charges/"+url.PathEscape(paymentRef.String()), nil)
	if err != nil {
		return unknown(err)
	}
	return c.do(ctx, req, paymentRef, true)
}

func (c *Client) do(ctx context.Context, req *http.Request, ref uuid.UUID, isQuery bool) Outcome {
	resp, err := c.http.Do(req)
	if err != nil {
		// Timeout, refused or dropped connection, cancelled context: unknown, never failed.
		return unknown(fmt.Errorf("call provider: %w", err))
	}
	defer func() { _ = resp.Body.Close() }()

	switch {
	case resp.StatusCode == http.StatusOK || (!isQuery && resp.StatusCode == http.StatusCreated):
		return parseBody(ctx, resp.Body, ref)
	case isQuery && resp.StatusCode == http.StatusNotFound:
		return Outcome{Status: NotFound}
	case resp.StatusCode >= 400 && resp.StatusCode < 500:
		// Our request was rejected, which means a bug on our side (bad key, bad
		// amount, key reused with other parameters). The charge state is unknown.
		slog.ErrorContext(ctx, "provider rejected our request", "status", resp.StatusCode, "payment_ref_id", ref)
		return unknown(fmt.Errorf("provider returned HTTP %d", resp.StatusCode))
	default:
		// 5xx or anything unexpected: the body is not trusted, whatever it says.
		return unknown(fmt.Errorf("provider returned HTTP %d", resp.StatusCode))
	}
}

func parseBody(ctx context.Context, body io.Reader, ref uuid.UUID) Outcome {
	var w wireResponse
	dec := json.NewDecoder(io.LimitReader(body, maxResponseBytes))
	if err := dec.Decode(&w); err != nil {
		slog.ErrorContext(ctx, "provider sent an unreadable response", "payment_ref_id", ref)
		return unknown(fmt.Errorf("decode provider response: %w", err))
	}
	switch w.Status {
	case string(Succeeded):
		if w.PSPRef == "" {
			slog.ErrorContext(ctx, "provider reported success without a reference", "payment_ref_id", ref)
			return unknown(errors.New("provider reported success without psp_ref"))
		}
		return Outcome{Status: Succeeded, PSPRef: w.PSPRef}
	case string(Failed):
		if w.Code != CodeInsufficientFunds && w.Code != CodeCardDeclined {
			// A decline we can't describe is still not something to guess about.
			slog.ErrorContext(ctx, "provider reported a failure with an unknown code", "payment_ref_id", ref, "code", w.Code)
			return unknown(fmt.Errorf("provider reported failure with unknown code %q", w.Code))
		}
		return Outcome{Status: Failed, Code: w.Code}
	case string(Processing):
		return Outcome{Status: Processing}
	default:
		slog.ErrorContext(ctx, "provider reported an unknown status", "payment_ref_id", ref, "status", w.Status)
		return unknown(fmt.Errorf("provider reported unknown status %q", w.Status))
	}
}

func unknown(cause error) Outcome { return Outcome{Status: Unknown, Cause: cause} }
