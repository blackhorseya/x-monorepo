package paymentv1

import _ "embed"

// OpenAPI is the generated OpenAPI document for this contract (openapi.yaml).
// Apps may serve it in non-production environments via server.Doc.
//
//go:embed openapi.yaml
var OpenAPI []byte

// PublicOpenAPI is the document of payment.v1.PublicPaymentService alone (public.openapi.yaml): what apps/rpc
// publishes. OpenAPI above also covers the internal PaymentService of payment-rpc, which rpc does not serve.
//
//go:embed public.openapi.yaml
var PublicOpenAPI []byte
