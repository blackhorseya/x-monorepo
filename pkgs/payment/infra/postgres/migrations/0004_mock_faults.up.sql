-- Slice 6 of the payment BC (docs/payment.md §9 S6): the faults `payment-rpc mock fail` sets on the mock
-- provider, so that a kind cluster can make the gateway unreachable for `serve`, `consume` and `expire` — three
-- processes — and bring it back. One row = "this operation fails". A provider reads the table only where
-- payment.mock_control is on, and only the mock does; nothing here is read or written otherwise.
-- Demo / kind data, like 0003. Only a new table, no ALTER of an existing one: the file takes no lock a running
-- payment-rpc waits on.

CREATE TABLE payment.mock_faults (
    -- the provider operations the mock can fail (pkgs/payment/infra/provider/mock: Op*)
    op         text        PRIMARY KEY CHECK (op IN ('create_checkout', 'get_payment', 'cancel_checkout', 'refund')),
    created_at timestamptz NOT NULL DEFAULT now()
);
