// Package webhook is the provider's webhook route: a plain HTTP handler, not a Connect procedure
// (docs/payment.md §7). It has no API key: the credential is the SIGNATURE, checked over the raw body
// by the use case before anything is written. The auth interceptor does not wrap it (see
// server.WithHTTPRoute), so this handler and its use case are the only gate.
package webhook

import (
	"errors"
	"io"
	"log/slog"
	"net/http"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

const (
	// Path is where the provider POSTs. It is not under any Connect prefix.
	Path = "/api/payment/webhook"
	// SignatureHeader carries the signature over the raw body.
	SignatureHeader = "X-Payment-Signature"
)

// ReceiveUC is the webhook use case.
type ReceiveUC = usecasex.UseCase[dto.WebhookInput, *dto.Ack]

// Handler serves the webhook.
type Handler struct {
	receive ReceiveUC
	log     *slog.Logger
}

// NewHandler takes the process's logger: a plain HTTP route has no Connect logging interceptor to put
// a request-scoped logger in the context, so the handler logs through the root logger itself.
func NewHandler(receive ReceiveUC, logger *slog.Logger) *Handler {
	return &Handler{receive: receive, log: logger}
}

// ServeHTTP answers 200 for a notification that is stored (or was already), 400 for one that must
// not be retried (bad signature, not an event, too large), 500 for one the provider should send again
// (our storage failed). Every answer has a fixed body: nothing of the request, of the signature or of an
// error is echoed, logged or put in a span attribute here — only a fixed category.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// The limit is applied before a byte is read: an unauthenticated caller cannot make us buffer more.
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, payment.MaxWebhookBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			h.log.WarnContext(ctx, "webhook rejected", "code", "payment.webhook_too_large")
			reply(w, http.StatusRequestEntityTooLarge, "webhook body too large")

			return
		}
		h.log.WarnContext(ctx, "webhook rejected", "code", "payment.webhook_unreadable")
		reply(w, http.StatusBadRequest, "webhook body could not be read")

		return
	}

	_, err = h.receive.Execute(ctx, dto.WebhookInput{Body: body, Signature: r.Header.Get(SignatureHeader)})
	switch {
	case err == nil:
		reply(w, http.StatusOK, "")
	case errors.Is(err, port.ErrInvalidSignature):
		h.log.WarnContext(ctx, "webhook rejected", "code", "payment.webhook_signature_invalid")
		reply(w, http.StatusBadRequest, "invalid signature")
	case errors.Is(err, port.ErrMalformedWebhook):
		h.log.WarnContext(ctx, "webhook rejected", "code", "payment.webhook_malformed")
		reply(w, http.StatusBadRequest, "malformed webhook")
	default:
		h.log.ErrorContext(ctx, "webhook not stored", "code", "payment.webhook_unavailable")
		reply(w, http.StatusInternalServerError, "webhook could not be stored, retry")
	}
}

func reply(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(status)
	if msg != "" {
		_, _ = io.WriteString(w, msg+"\n")
	}
}
