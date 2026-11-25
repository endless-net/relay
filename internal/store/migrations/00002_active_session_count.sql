-- +goose Up
CREATE INDEX node_session_leases_relay_expiry
    ON node_session_leases (relay_id, lease_expires_at);

-- +goose Down
DROP INDEX node_session_leases_relay_expiry;
