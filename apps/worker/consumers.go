package main

import "github.com/blackhorseya/x-monorepo/pkgs/platform/eventbus"

// Consumer is what one event BC hands the worker: its Postgres schema (its poison topic, outbox and inbox
// tables) and the subscriptions its handlers serve. The worker registers each on the router.
type Consumer struct {
	schema string
	subs   []eventbus.Subscription
}

// NewConsumers lists every BC that consumes events: a parameter (its Subscriptions, a named type so wire can
// tell BCs apart) and an element. `task new-bc -- <name> --events` inserts both at the anchors below.
// Adding such a BC by hand: do the same, add its ProviderSet to wire.go and its Schema to poison.go's schemas.
func NewConsumers(
// new-bc:consumer-param
) []Consumer {
	return []Consumer{
		// new-bc:consumer-elem
	}
}
