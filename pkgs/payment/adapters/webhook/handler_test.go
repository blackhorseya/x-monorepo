package webhook_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/blackhorseya/x-monorepo/pkgs/payment/adapters/webhook"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/dto"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/application/port"
	"github.com/blackhorseya/x-monorepo/pkgs/payment/domain/payment"
	"github.com/blackhorseya/x-monorepo/pkgs/platform/usecasex"
)

type call struct {
	in dto.WebhookInput
}

func uc(calls *[]call, err error) webhook.ReceiveUC {
	return usecasex.Func[dto.WebhookInput, *dto.Ack](func(_ context.Context, in dto.WebhookInput) (*dto.Ack, error) {
		*calls = append(*calls, call{in})

		return &dto.Ack{}, err
	})
}

func post(h http.Handler, body io.Reader, sig string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodPost, webhook.Path, body)
	if sig != "" {
		req.Header.Set(webhook.SignatureHeader, sig)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)

	return rec
}

// The use case gets the RAW body and the signature header, byte for byte.
func TestHandler_PassesTheRawBodyAndTheSignature(t *testing.T) {
	var calls []call
	h := webhook.NewHandler(uc(&calls, nil), slog.New(slog.DiscardHandler))
	body := "{ \"id\" :\"evt-1\",\n \"amount\":  1000 }" // odd spacing on purpose: the signature covers these bytes
	rec := post(h, strings.NewReader(body), "sig-1")
	if rec.Code != http.StatusOK || rec.Body.Len() != 0 {
		t.Fatalf("answer = %d %q", rec.Code, rec.Body.String())
	}
	if len(calls) != 1 || string(calls[0].in.Body) != body || calls[0].in.Signature != "sig-1" {
		t.Fatalf("use case input = %+v", calls)
	}
}

func TestHandler_Answers(t *testing.T) {
	const marker = "SECRET-HANDLER-MARKER-31bd"
	for name, tc := range map[string]struct {
		err  error
		want int
	}{
		"bad signature: do not retry": {port.ErrInvalidSignature, http.StatusBadRequest},
		"malformed: do not retry":     {port.ErrMalformedWebhook, http.StatusBadRequest},
		// our storage failed: the provider should send it again; the body must not echo the cause
		"storage failed: retry": {errors.New("connection reset " + marker), http.StatusInternalServerError},
		"a domain error":        {payment.ErrInvalidWebhook, http.StatusInternalServerError},
	} {
		t.Run(name, func(t *testing.T) {
			var calls []call
			rec := post(webhook.NewHandler(uc(&calls, tc.err), slog.New(slog.DiscardHandler)), strings.NewReader("x"), "s")
			if rec.Code != tc.want {
				t.Fatalf("status = %d, want %d", rec.Code, tc.want)
			}
			if strings.Contains(rec.Body.String(), marker) || strings.Contains(rec.Body.String(), "connection") {
				t.Fatalf("the cause is in the answer: %q", rec.Body.String())
			}
			if rec.Body.Len() == 0 {
				t.Fatal("a refusal with an empty body leaves the provider guessing")
			}
		})
	}
}

// The size limit is applied while reading: a body over it is refused as too large and never reaches the
// use case; one at the limit goes through.
func TestHandler_BodyLimit(t *testing.T) {
	var calls []call
	h := webhook.NewHandler(uc(&calls, nil), slog.New(slog.DiscardHandler))
	if rec := post(h, bytes.NewReader(make([]byte, payment.MaxWebhookBody+1)), "s"); rec.Code != http.StatusRequestEntityTooLarge || len(calls) != 0 {
		t.Fatalf("over the limit: %d, %d calls", rec.Code, len(calls))
	}
	if rec := post(h, bytes.NewReader(make([]byte, payment.MaxWebhookBody)), "s"); rec.Code != http.StatusOK || len(calls) != 1 {
		t.Fatalf("at the limit: %d, %d calls", rec.Code, len(calls))
	}
}

type brokenBody struct{}

func (brokenBody) Read([]byte) (int, error) { return 0, errors.New("connection reset by peer") }

func TestHandler_UnreadableBody(t *testing.T) {
	var calls []call
	if rec := post(webhook.NewHandler(uc(&calls, nil), slog.New(slog.DiscardHandler)), brokenBody{}, "s"); rec.Code != http.StatusBadRequest || len(calls) != 0 {
		t.Fatalf("unreadable body: %d, %d calls", rec.Code, len(calls))
	}
}
