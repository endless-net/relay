-- +goose Up
CREATE SEQUENCE relay_epoch_seq AS bigint MINVALUE 1 NO CYCLE;
CREATE TABLE relay_instances (
    relay_id text PRIMARY KEY,
    boot_id text NOT NULL,
    mesh_addr text NOT NULL,
    lease_expires_at timestamptz NOT NULL
);
CREATE TABLE relay_instance_boots (
    relay_id text NOT NULL,
    boot_id text NOT NULL,
    PRIMARY KEY (relay_id,boot_id)
);
CREATE TABLE node_session_leases (
    network_id text NOT NULL,
    node_id text NOT NULL,
    relay_id text NOT NULL REFERENCES relay_instances(relay_id),
    boot_id text NOT NULL,
    epoch bigint NOT NULL CHECK (epoch > 0),
    lease_expires_at timestamptz NOT NULL,
    PRIMARY KEY (network_id,node_id)
);
-- +goose Down
DROP TABLE node_session_leases;
DROP TABLE relay_instances;
DROP TABLE relay_instance_boots;
DROP SEQUENCE relay_epoch_seq;
