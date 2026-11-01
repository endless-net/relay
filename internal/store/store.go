package store

import (
	"context"
	"errors"
	"math"
	"sort"
	"strings"
	"sync"
	"time"
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
}

type Memory struct {
	boots     map[string]map[string]bool
	mu        sync.Mutex
	instances map[string]Instance
	sessions  map[string]Session
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
	if m.boots == nil {
		m.boots = map[string]map[string]bool{}
	}
	if m.boots[instance.RelayID] == nil {
		m.boots[instance.RelayID] = map[string]bool{}
	}
	if prior, ok := m.instances[instance.RelayID]; ok {
		if prior.BootID != instance.BootID && m.boots[instance.RelayID][instance.BootID] {
			return nil, time.Time{}, ErrFenced
		}
		if prior.BootID != instance.BootID && now.Before(prior.LeaseExpires) {
			return nil, time.Time{}, ErrFenced
		}
		if prior.BootID == instance.BootID && !now.Before(prior.LeaseExpires) {
			return nil, time.Time{}, ErrFenced
		}
	}
	instance.LeaseExpires = now.Add(ttl)
	m.instances[instance.RelayID] = instance
	m.boots[instance.RelayID][instance.BootID] = true
	return m.peersLocked(instance.RelayID, now), instance.LeaseExpires, nil
}

func (m *Memory) HeartbeatInstance(_ context.Context, relayID, bootID string, ttl time.Duration) ([]Instance, time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	instance, ok := m.instances[relayID]
	if !ok || instance.BootID != bootID || !time.Now().Before(instance.LeaseExpires) {
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
	if current, exists := m.sessions[key]; exists && time.Now().Before(current.LeaseExpires) {
		return Session{}, ErrSessionFenced
	}
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

func sessionKey(networkID, nodeID string) string {
	return networkID + "\x00" + nodeID
}

func canonicalRequired(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}
