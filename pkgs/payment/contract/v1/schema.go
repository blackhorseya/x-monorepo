package paymentv1

// Schema is the Postgres schema of the payment bounded context. It is here, in the contract, so
// that the composition roots which must name the schema (apps/worker's and apps/rpc's poison / prune
// lists) can do so without importing any payment domain code. pkgs/payment/infra/postgres.Schema is
// the same value; a test pins them together.
const Schema = "payment"
