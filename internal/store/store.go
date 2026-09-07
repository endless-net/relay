package store

import (
	"context"
	"errors"
	"math"
	"reflect"
	"sort"
	"strings"
	"sync"
	"time"

	protocolv1 "github.com/endless-net/relay/protocol/v1"
)

var (
	ErrFenced         = errors.New("relay instance is fenced")
	ErrSessionFenced  = errors.New("relay session is fenced")
	ErrSessionMissing = errors.New("relay session is missing")
)

type Instance struct {
	RelayID      string
	BootID       string
	MeshAddr     string
	LeaseExpires time.Time
}

type Session struct {
	NetworkID    string
	NodeID       string
	RelayID      string
	BootID       string
	Epoch        int64
	LeaseExpires time.Time
}

type Store interface {
	RegisterInstance(context.Context, Instance, time.Duration) ([]Instance, time.Time, error)
	HeartbeatInstance(context.Context, string, string, time.Duration) ([]Instance, time.Time, error)
	AcquireSession(context.Context, Session, time.Duration) (Session, error)
	RenewSession(context.Context, Session, time.Duration) (Session, error)
	ReleaseSession(context.Context, Session) error
	ResolveSession(context.Context, string, string, time.Time) (Session, error)
	EndpointSnapshot(context.Context) (protocolv1.EndpointSnapshot, error)
	ReplaceEndpoints(context.Context, protocolv1.EndpointSnapshot, time.Time) error
}

type Memory struct {
	mu        sync.Mutex
	instances map[string]Instance
	sessions  map[string]Session
	snapshot  protocolv1.EndpointSnapshot
}

func NewMemory() *Memory {
	return &Memory{instances: map[string]Instance{}, sessions: map[string]Session{}}
}

func (m *Memory) RegisterInstance(_ context.Context, instance Instance, ttl time.Duration) ([]Instance, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateInstance(instance); err != nil {
		return nil, time.Time{}, err
	}
	now := time.Now().UTC()
	instance.LeaseExpires = now.Add(ttl)
	m.instances[instance.RelayID] = instance
	return m.peersLocked(instance.RelayID, now), instance.LeaseExpires, nil
}

func (m *Memory) HeartbeatInstance(_ context.Context, relayID, bootID string, ttl time.Duration) ([]Instance, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	instance, ok := m.instances[relayID]
	if !ok || instance.BootID != bootID {
		return nil, time.Time{}, ErrFenced
	}
	now := time.Now().UTC()
	instance.LeaseExpires = now.Add(ttl)
	m.instances[instance.RelayID] = instance
	return m.peersLocked(instance.RelayID, now), instance.LeaseExpires, nil
}

func (m *Memory) AcquireSession(_ context.Context, session Session, ttl time.Duration) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := validateSession(session); err != nil {
		return Session{}, err
	}
	instance, ok := m.instances[session.RelayID]
	if !ok || instance.BootID != session.BootID || !time.Now().UTC().Before(instance.LeaseExpires) {
		return Session{}, ErrFenced
	}
	key := sessionKey(session.NetworkID, session.NodeID)
	if m.sessions[key].Epoch == math.MaxInt64 {
		return Session{}, errors.New("session epoch exhausted")
	}
	session.Epoch = m.sessions[key].Epoch + 1
	session.LeaseExpires = time.Now().UTC().Add(ttl)
	m.sessions[key] = session
	return session, nil
}

func (m *Memory) RenewSession(_ context.Context, session Session, ttl time.Duration) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	current, ok := m.sessions[sessionKey(session.NetworkID, session.NodeID)]
	if !ok {
		return Session{}, ErrSessionMissing
	}
	if !time.Now().UTC().Before(current.LeaseExpires) || current.RelayID != session.RelayID || current.BootID != session.BootID || current.Epoch != session.Epoch {
		return Session{}, ErrSessionFenced
	}
	instance, ok := m.instances[session.RelayID]
	if !ok || instance.BootID != session.BootID || !time.Now().UTC().Before(instance.LeaseExpires) {
		return Session{}, ErrFenced
	}
	current.LeaseExpires = time.Now().UTC().Add(ttl)
	m.sessions[sessionKey(session.NetworkID, session.NodeID)] = current
	return current, nil
}

func (m *Memory) ReleaseSession(_ context.Context, session Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	key := sessionKey(session.NetworkID, session.NodeID)
	current, ok := m.sessions[key]
	if ok && current.RelayID == session.RelayID && current.BootID == session.BootID && current.Epoch == session.Epoch {
		current.LeaseExpires = time.Unix(0, 0).UTC()
		m.sessions[key] = current
	}
	return nil
}

func (m *Memory) ResolveSession(_ context.Context, networkID, nodeID string, now time.Time) (Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	session, ok := m.sessions[sessionKey(networkID, nodeID)]
	if !ok || !now.UTC().Before(session.LeaseExpires) {
		return Session{}, ErrSessionMissing
	}
	instance, exists := m.instances[session.RelayID]
	if !exists || instance.BootID != session.BootID || !now.Before(instance.LeaseExpires) {
		return Session{}, ErrSessionFenced
	}
	return session, nil
}

func (m *Memory) EndpointSnapshot(context.Context) (protocolv1.EndpointSnapshot, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return cloneSnapshot(m.snapshot), nil
}

func (m *Memory) ReplaceEndpoints(_ context.Context, snapshot protocolv1.EndpointSnapshot, _ time.Time) error {
	if err := validateSnapshot(snapshot); err != nil {
		return err
	}
	snapshot = canonicalSnapshot(snapshot)
	m.mu.Lock()
	defer m.mu.Unlock()
	if snapshot.Version < m.snapshot.Version {
		return errors.New("relay endpoint snapshot version moved backwards")
	}
	if snapshot.Version == m.snapshot.Version && m.snapshot.Version != 0 {
		if !reflect.DeepEqual(snapshot, m.snapshot) {
			return errors.New("relay endpoint snapshot content changed without a version change")
		}
		return nil
	}
	m.snapshot = cloneSnapshot(snapshot)
	return nil
}

func (m *Memory) peersLocked(relayID string, now time.Time) []Instance {
	peers := make([]Instance, 0, len(m.instances))
	for _, peer := range m.instances {
		if peer.RelayID != relayID && now.Before(peer.LeaseExpires) {
			peers = append(peers, peer)
		}
	}
	sort.Slice(peers, func(i, j int) bool { return peers[i].RelayID < peers[j].RelayID })
	return peers
}

func validateInstance(instance Instance) error {
	if !canonicalRequired(instance.RelayID) || !canonicalRequired(instance.BootID) || !canonicalRequired(instance.MeshAddr) {
		return errors.New("relay id, boot id, and mesh address are required")
	}
	return nil
}

func validateSession(session Session) error {
	if !canonicalRequired(session.NetworkID) || !canonicalRequired(session.NodeID) || !canonicalRequired(session.RelayID) || !canonicalRequired(session.BootID) {
		return errors.New("relay session identity is required")
	}
	return nil
}

func validateSnapshot(snapshot protocolv1.EndpointSnapshot) error {
	return snapshot.Validate()
}

func sessionKey(networkID, nodeID string) string {
	return networkID + "\x00" + nodeID
}

func canonicalRequired(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}

func cloneSnapshot(snapshot protocolv1.EndpointSnapshot) protocolv1.EndpointSnapshot {
	copy := snapshot
	copy.Endpoints = append([]protocolv1.Endpoint(nil), snapshot.Endpoints...)
	return copy
}

func canonicalSnapshot(snapshot protocolv1.EndpointSnapshot) protocolv1.EndpointSnapshot {
	snapshot = cloneSnapshot(snapshot)
	sort.Slice(snapshot.Endpoints, func(i, j int) bool {
		if snapshot.Endpoints[i].Priority != snapshot.Endpoints[j].Priority {
			return snapshot.Endpoints[i].Priority < snapshot.Endpoints[j].Priority
		}
		return snapshot.Endpoints[i].ID < snapshot.Endpoints[j].ID
	})
	return snapshot
}
