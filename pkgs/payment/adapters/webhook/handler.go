// Package webhook is the provider's webhook route: a plain HTTP handler, not a Connect procedure
// (docs/payment.md §7). It has no API key: the credential is the SIGNATURE, checked over the raw body
// by the use case before anything is written. The auth interceptor does not wrap it (see
// server.WithHTTPRoute), so this handler and its use case are the only gate.
package webhook

import (
	"log/slog"
	"net/http"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
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

// ServeHTTP answers 200 to everything. SPEC STAGE: nothing is checked, nothing is stored.
func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	_, _ = h.receive, h.log
	w.WriteHeader(http.StatusOK)
}
