package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"reflect"
	"strings"
	"time"

	protocolv1 "github.com/endless-net/relay/protocol/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var migrations embed.FS

type Postgres struct {
	pool *pgxpool.Pool
}

func OpenPostgres(ctx context.Context, dsn string) (*Postgres, error) {
	if strings.TrimSpace(dsn) == "" {
		return nil, errors.New("relay coordinator PostgreSQL DSN is required")
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	store := &Postgres{pool: pool}
	if err := store.migrate(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return store, nil
}

func (p *Postgres) Close() { p.pool.Close() }

func (p *Postgres) migrate(ctx context.Context) error {
	entries, err := fs.ReadDir(migrations, "migrations")
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".sql") {
			continue
		}
		raw, err := migrations.ReadFile("migrations/" + entry.Name())
		if err != nil {
			return err
		}
		if _, err := p.pool.Exec(ctx, string(raw)); err != nil {
			return fmt.Errorf("apply relay migration %s: %w", entry.Name(), err)
		}
	}
	return nil
}

func (p *Postgres) RegisterInstance(ctx context.Context, instance Instance, ttl time.Duration) ([]Instance, time.Time, error) {
	if err := validateInstance(instance); err != nil {
		return nil, time.Time{}, err
	}
	now := time.Now().UTC()
	expires := now.Add(ttl)
	_, err := p.pool.Exec(ctx, `INSERT INTO relay_instances (relay_id, boot_id, mesh_addr, lease_expires_at)
		VALUES ($1,$2,$3,$4)
		ON CONFLICT (relay_id) DO UPDATE SET boot_id=EXCLUDED.boot_id, mesh_addr=EXCLUDED.mesh_addr, lease_expires_at=EXCLUDED.lease_expires_at`,
		instance.RelayID, instance.BootID, instance.MeshAddr, expires)
	if err != nil {
		return nil, time.Time{}, err
	}
	peers, err := p.activePeers(ctx, instance.RelayID, now)
	return peers, expires, err
}

func (p *Postgres) HeartbeatInstance(ctx context.Context, relayID, bootID string, ttl time.Duration) ([]Instance, time.Time, error) {
	now := time.Now().UTC()
	expires := now.Add(ttl)
	if !canonicalRequired(relayID) || !canonicalRequired(bootID) {
		return nil, time.Time{}, errors.New("relay id and boot id are required")
	}
	result, err := p.pool.Exec(ctx, `UPDATE relay_instances SET lease_expires_at=$1 WHERE relay_id=$2 AND boot_id=$3`, expires, relayID, bootID)
	if err != nil {
		return nil, time.Time{}, err
	}
	if result.RowsAffected() != 1 {
		return nil, time.Time{}, ErrFenced
	}
	peers, err := p.activePeers(ctx, relayID, now)
	return peers, expires, err
}

func (p *Postgres) AcquireSession(ctx context.Context, session Session, ttl time.Duration) (Session, error) {
	if err := validateSession(session); err != nil {
		return Session{}, err
	}
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.ReadCommitted})
	if err != nil {
		return Session{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	now := time.Now().UTC()
	var instanceBoot string
	var instanceExpires time.Time
	if err := tx.QueryRow(ctx, `SELECT boot_id, lease_expires_at FROM relay_instances WHERE relay_id=$1 FOR UPDATE`, session.RelayID).Scan(&instanceBoot, &instanceExpires); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return Session{}, ErrFenced
		}
		return Session{}, err
	}
	if instanceBoot != session.BootID || !now.Before(instanceExpires) {
		return Session{}, ErrFenced
	}
	var epoch int64
	expires := now.Add(ttl)
	// The row conflict serializes concurrent owners, including the first acquire.
	// Preserve the last epoch on release and fail closed instead of wrapping.
	err = tx.QueryRow(ctx, `INSERT INTO node_session_leases (network_id,node_id,relay_id,boot_id,epoch,lease_expires_at)
		VALUES ($1,$2,$3,$4,1,$5)
		ON CONFLICT (network_id,node_id) DO UPDATE SET relay_id=EXCLUDED.relay_id, boot_id=EXCLUDED.boot_id,
		epoch=node_session_leases.epoch+1, lease_expires_at=EXCLUDED.lease_expires_at
		WHERE node_session_leases.epoch < 9223372036854775807
		RETURNING epoch`, session.NetworkID, session.NodeID, session.RelayID, session.BootID, expires).Scan(&epoch)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, errors.New("session epoch exhausted")
	}
	if err != nil {
		return Session{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return Session{}, err
	}
	session.Epoch = epoch
	session.LeaseExpires = expires
	return session, nil
}

func (p *Postgres) RenewSession(ctx context.Context, session Session, ttl time.Duration) (Session, error) {
	now := time.Now().UTC()
	expires := now.Add(ttl)
	result, err := p.pool.Exec(ctx, `UPDATE node_session_leases SET lease_expires_at=$1
		WHERE network_id=$2 AND node_id=$3 AND relay_id=$4 AND boot_id=$5 AND epoch=$6
		AND lease_expires_at>$7
		AND EXISTS (SELECT 1 FROM relay_instances WHERE relay_id=$4 AND boot_id=$5 AND lease_expires_at>$7)`,
		expires, session.NetworkID, session.NodeID, session.RelayID, session.BootID, session.Epoch, now)
	if err != nil {
		return Session{}, err
	}
	if result.RowsAffected() != 1 {
		return Session{}, ErrSessionFenced
	}
	session.LeaseExpires = expires
	return session, nil
}

func (p *Postgres) ReleaseSession(ctx context.Context, session Session) error {
	_, err := p.pool.Exec(ctx, `UPDATE node_session_leases SET lease_expires_at=TIMESTAMPTZ '1970-01-01 00:00:00+00' WHERE network_id=$1 AND node_id=$2 AND relay_id=$3 AND boot_id=$4 AND epoch=$5`, session.NetworkID, session.NodeID, session.RelayID, session.BootID, session.Epoch)
	return err
}

func (p *Postgres) ResolveSession(ctx context.Context, networkID, nodeID string, now time.Time) (Session, error) {
	var session Session
	if !canonicalRequired(networkID) || !canonicalRequired(nodeID) {
		return Session{}, errors.New("relay session identity is required")
	}
	err := p.pool.QueryRow(ctx, `SELECT s.network_id,s.node_id,s.relay_id,s.boot_id,s.epoch,s.lease_expires_at FROM node_session_leases s JOIN relay_instances i ON i.relay_id=s.relay_id AND i.boot_id=s.boot_id WHERE s.network_id=$1 AND s.node_id=$2 AND s.lease_expires_at>$3 AND i.lease_expires_at>$3`, networkID, nodeID, now.UTC()).Scan(&session.NetworkID, &session.NodeID, &session.RelayID, &session.BootID, &session.Epoch, &session.LeaseExpires)
	if errors.Is(err, pgx.ErrNoRows) {
		return Session{}, ErrSessionMissing
	}
	return session, err
}

func (p *Postgres) activePeers(ctx context.Context, relayID string, now time.Time) ([]Instance, error) {
	rows, err := p.pool.Query(ctx, `SELECT relay_id,boot_id,mesh_addr,lease_expires_at FROM relay_instances WHERE relay_id<>$1 AND lease_expires_at>$2 ORDER BY relay_id`, relayID, now.UTC())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var peers []Instance
	for rows.Next() {
		var peer Instance
		if err := rows.Scan(&peer.RelayID, &peer.BootID, &peer.MeshAddr, &peer.LeaseExpires); err != nil {
			return nil, err
		}
		peers = append(peers, peer)
	}
	return peers, rows.Err()
}

func (p *Postgres) EndpointSnapshot(ctx context.Context) (protocolv1.EndpointSnapshot, error) {
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return protocolv1.EndpointSnapshot{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var snapshot protocolv1.EndpointSnapshot
	err = tx.QueryRow(ctx, `SELECT version FROM platform_relay_snapshot WHERE snapshot_key='platform'`).Scan(&snapshot.Version)
	if errors.Is(err, pgx.ErrNoRows) {
		return snapshot, nil
	}
	if err != nil {
		return protocolv1.EndpointSnapshot{}, err
	}
	rows, err := tx.Query(ctx, `SELECT endpoint_id,addr,protocol,priority,region FROM platform_relay_endpoints ORDER BY priority,endpoint_id`)
	if err != nil {
		return protocolv1.EndpointSnapshot{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var endpoint protocolv1.Endpoint
		if err := rows.Scan(&endpoint.ID, &endpoint.Addr, &endpoint.Protocol, &endpoint.Priority, &endpoint.Region); err != nil {
			return protocolv1.EndpointSnapshot{}, err
		}
		snapshot.Endpoints = append(snapshot.Endpoints, endpoint)
	}
	if err := rows.Err(); err != nil {
		return protocolv1.EndpointSnapshot{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return protocolv1.EndpointSnapshot{}, err
	}
	return snapshot, nil
}

func (p *Postgres) ReplaceEndpoints(ctx context.Context, snapshot protocolv1.EndpointSnapshot, at time.Time) error {
	if err := validateSnapshot(snapshot); err != nil {
		return err
	}
	snapshot = canonicalSnapshot(snapshot)
	tx, err := p.pool.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.Serializable})
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback(ctx) }()
	var current int64
	err = tx.QueryRow(ctx, `SELECT version FROM platform_relay_snapshot WHERE snapshot_key='platform' FOR UPDATE`).Scan(&current)
	if err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return err
	}
	if snapshot.Version < current {
		return errors.New("relay endpoint snapshot version moved backwards")
	}
	if snapshot.Version == current && current != 0 {
		currentSnapshot, err := endpointSnapshotTx(ctx, tx, current)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(currentSnapshot, snapshot) {
			return errors.New("relay endpoint snapshot content changed without a version change")
		}
		return tx.Commit(ctx)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM platform_relay_endpoints`); err != nil {
		return err
	}
	for _, endpoint := range snapshot.Endpoints {
		if _, err := tx.Exec(ctx, `INSERT INTO platform_relay_endpoints (endpoint_id,addr,protocol,priority,region) VALUES ($1,$2,$3,$4,$5)`, endpoint.ID, endpoint.Addr, endpoint.Protocol, endpoint.Priority, endpoint.Region); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO platform_relay_snapshot (snapshot_key,version) VALUES ('platform',$1) ON CONFLICT (snapshot_key) DO UPDATE SET version=EXCLUDED.version`, snapshot.Version); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func endpointSnapshotTx(ctx context.Context, tx pgx.Tx, version int64) (protocolv1.EndpointSnapshot, error) {
	snapshot := protocolv1.EndpointSnapshot{Version: version}
	rows, err := tx.Query(ctx, `SELECT endpoint_id,addr,protocol,priority,region FROM platform_relay_endpoints ORDER BY priority,endpoint_id`)
	if err != nil {
		return protocolv1.EndpointSnapshot{}, err
	}
	defer rows.Close()
	for rows.Next() {
		var endpoint protocolv1.Endpoint
		if err := rows.Scan(&endpoint.ID, &endpoint.Addr, &endpoint.Protocol, &endpoint.Priority, &endpoint.Region); err != nil {
			return protocolv1.EndpointSnapshot{}, err
		}
		snapshot.Endpoints = append(snapshot.Endpoints, endpoint)
	}
	return snapshot, rows.Err()
}
