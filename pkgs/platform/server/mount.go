package server

// Mount is what one bounded context hands apps/rpc: its services, its OpenAPI
// documents, and the Postgres schema whose poison table the admin service
// serves ("" when the BC consumes no events and has none). A BC's provider.go
// provides it as its own named type, so wire can tell the BCs apart; apps/rpc
// lists them in one place.
type Mount struct {
	Routes       []Route
	Docs         []Doc
	PoisonSchema string
}
