CREATE TABLE IF NOT EXISTS relay_instances (
    relay_id text,
    boot_id text,
    mesh_addr text,
    lease_expires_at timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS relay_instances_id_idx
    ON relay_instances (relay_id);

CREATE TABLE IF NOT EXISTS node_session_leases (
    network_id text,
    node_id text,
    relay_id text,
    boot_id text,
    epoch bigint,
    lease_expires_at timestamptz
);

CREATE UNIQUE INDEX IF NOT EXISTS node_session_identity_idx
    ON node_session_leases (network_id, node_id);

CREATE TABLE IF NOT EXISTS platform_relay_snapshot (
    snapshot_key text,
    version bigint
);

CREATE UNIQUE INDEX IF NOT EXISTS platform_relay_snapshot_key_idx
    ON platform_relay_snapshot (snapshot_key);

CREATE TABLE IF NOT EXISTS platform_relay_endpoints (
    endpoint_id text,
    addr text,
    protocol text,
    priority integer,
    region text
);

CREATE UNIQUE INDEX IF NOT EXISTS platform_relay_endpoints_id_idx
    ON platform_relay_endpoints (endpoint_id);
