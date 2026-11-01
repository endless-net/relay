-- name: Register :one
INSERT INTO relay_instances(relay_id,boot_id,mesh_addr,lease_expires_at)
VALUES(sqlc.arg(relay_id),sqlc.arg(boot_id),sqlc.arg(mesh_addr),sqlc.arg(expires))
ON CONFLICT(relay_id) DO UPDATE SET boot_id=EXCLUDED.boot_id,
mesh_addr=EXCLUDED.mesh_addr,lease_expires_at=EXCLUDED.lease_expires_at
WHERE (relay_instances.boot_id=EXCLUDED.boot_id AND relay_instances.lease_expires_at>sqlc.arg(now_at))
OR (relay_instances.boot_id<>EXCLUDED.boot_id AND relay_instances.lease_expires_at<=sqlc.arg(now_at))
RETURNING *;

-- name: LockRegistry :exec
SELECT pg_advisory_xact_lock(901009);

-- name: KnownBoot :one
SELECT boot_id FROM relay_instance_boots WHERE relay_id=sqlc.arg(relay_id) AND boot_id=sqlc.arg(boot_id);

-- name: RecordBoot :exec
INSERT INTO relay_instance_boots(relay_id,boot_id) VALUES(sqlc.arg(relay_id),sqlc.arg(boot_id)) ON CONFLICT DO NOTHING;

-- name: InstanceCount :one
SELECT count(*) FROM relay_instances;

-- name: ActiveSessionCount :one
SELECT count(*) FROM node_session_leases WHERE relay_id=sqlc.arg(relay_id) AND lease_expires_at>sqlc.arg(now_at);

-- name: Clock :one
SELECT clock_timestamp()::timestamptz;

-- name: Heartbeat :one
UPDATE relay_instances SET lease_expires_at=sqlc.arg(expires)
WHERE relay_id=sqlc.arg(relay_id) AND boot_id=sqlc.arg(boot_id)
AND lease_expires_at>sqlc.arg(now_at) RETURNING *;

-- name: Peers :many
SELECT * FROM relay_instances WHERE relay_id<>sqlc.arg(relay_id)
AND lease_expires_at>sqlc.arg(now_at) ORDER BY relay_id LIMIT 64;

-- name: LockInstance :one
SELECT * FROM relay_instances WHERE relay_id=sqlc.arg(relay_id) FOR UPDATE;

-- name: Acquire :one
INSERT INTO node_session_leases(network_id,node_id,relay_id,boot_id,epoch,lease_expires_at)
VALUES(sqlc.arg(network_id),sqlc.arg(node_id),sqlc.arg(relay_id),sqlc.arg(boot_id),nextval('relay_epoch_seq'),sqlc.arg(expires))
ON CONFLICT(network_id,node_id) DO UPDATE SET relay_id=EXCLUDED.relay_id,
boot_id=EXCLUDED.boot_id,epoch=EXCLUDED.epoch,lease_expires_at=EXCLUDED.lease_expires_at
WHERE node_session_leases.lease_expires_at<=sqlc.arg(now_at) RETURNING *;

-- name: Renew :one
UPDATE node_session_leases AS s SET lease_expires_at=sqlc.arg(expires)
WHERE s.network_id=sqlc.arg(network_id) AND s.node_id=sqlc.arg(node_id)
AND s.relay_id=sqlc.arg(relay_id) AND s.boot_id=sqlc.arg(boot_id) AND s.epoch=sqlc.arg(epoch)
AND s.lease_expires_at>sqlc.arg(now_at)
AND EXISTS (SELECT 1 FROM relay_instances i WHERE i.relay_id=sqlc.arg(relay_id)
AND i.boot_id=sqlc.arg(boot_id) AND i.lease_expires_at>sqlc.arg(now_at)) RETURNING *;

-- name: Release :exec
UPDATE node_session_leases SET lease_expires_at=TIMESTAMPTZ '1970-01-01 00:00:00+00'
WHERE network_id=sqlc.arg(network_id) AND node_id=sqlc.arg(node_id)
AND relay_id=sqlc.arg(relay_id) AND boot_id=sqlc.arg(boot_id) AND epoch=sqlc.arg(epoch);

-- name: Resolve :one
SELECT s.* FROM node_session_leases s JOIN relay_instances i ON i.relay_id=s.relay_id
AND i.boot_id=s.boot_id WHERE s.network_id=sqlc.arg(network_id) AND s.node_id=sqlc.arg(node_id)
AND s.lease_expires_at>sqlc.arg(now_at) AND i.lease_expires_at>sqlc.arg(now_at);
