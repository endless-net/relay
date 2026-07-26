package protocolv1

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"
)

const (
	Version                   = 1
	MaxFramePayloadBytes      = 64 * 1024
	MinHeartbeatIntervalMS    = 100
	MaxHeartbeatIntervalMS    = 30_000
	CredentialAlgorithm       = "ed25519-relay-credential-v3"
	credentialSchemaVersion   = 3
	SigningTrustBundleVersion = 1
	SigningKeyEd25519         = "ed25519"
	EndpointProtocolTLS       = "relay-v1-tls"
)

const (
	MessageClientHello = "client_hello"
	MessageReady       = "ready"
	MessageClientFrame = "client_frame"
	MessageServerFrame = "server_frame"
	MessageError       = "error"
	MessageHeartbeat   = "heartbeat"
)

type Endpoint struct {
	ID       string `json:"id"`
	Addr     string `json:"addr"`
	Protocol string `json:"protocol"`
	Region   string `json:"region,omitempty"`
	Priority int    `json:"priority"`
}

type EndpointSnapshot struct {
	Version   int64      `json:"version"`
	Endpoints []Endpoint `json:"endpoints"`
}

type Credential struct {
	Algorithm string    `json:"algorithm"`
	KeyID     string    `json:"key_id"`
	NetworkID string    `json:"network_id"`
	NodeID    string    `json:"node_id"`
	ExpiresAt time.Time `json:"expires_at"`
	Signature string    `json:"signature"`
}

func (c *Credential) UnmarshalJSON(raw []byte) error {
	type credentialJSON Credential
	var decoded credentialJSON
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&decoded); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("relay credential contains multiple JSON values")
		}
		return err
	}
	*c = Credential(decoded)
	return nil
}

func DecodeStrict(raw []byte, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("json contains multiple values")
		}
		return err
	}
	return nil
}

type credentialPayload struct {
	Schema    int       `json:"schema"`
	KeyID     string    `json:"key_id"`
	NetworkID string    `json:"network_id"`
	NodeID    string    `json:"node_id"`
	ExpiresAt time.Time `json:"expires_at"`
}

func Sign(privateKey ed25519.PrivateKey, networkID, nodeID string, expiresAt time.Time) (*Credential, error) {
	if len(privateKey) != ed25519.PrivateKeySize {
		return nil, errors.New("invalid relay credential private key")
	}
	publicKey, ok := privateKey.Public().(ed25519.PublicKey)
	if !ok {
		return nil, errors.New("invalid relay credential public key")
	}
	payload, err := SigningPayloadForPublicKey(publicKey, networkID, nodeID, expiresAt)
	if err != nil {
		return nil, err
	}
	return CredentialFromSignature(publicKey, networkID, nodeID, expiresAt, ed25519.Sign(privateKey, payload))
}

func CredentialFromSignature(publicKey ed25519.PublicKey, networkID, nodeID string, expiresAt time.Time, signature []byte) (*Credential, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("invalid relay credential public key")
	}
	if len(signature) != ed25519.SignatureSize {
		return nil, errors.New("invalid relay credential signature")
	}
	publicKeyText := base64.RawURLEncoding.EncodeToString(publicKey)
	keyID, err := SigningKeyID(publicKeyText)
	if err != nil {
		return nil, err
	}
	if _, err := SigningPayload(keyID, networkID, nodeID, expiresAt); err != nil {
		return nil, err
	}
	return &Credential{Algorithm: CredentialAlgorithm, KeyID: keyID, NetworkID: networkID, NodeID: nodeID, ExpiresAt: expiresAt.UTC(), Signature: base64.RawURLEncoding.EncodeToString(signature)}, nil
}

func Verify(credential Credential, trustedPublicKey string, now time.Time) error {
	if credential.Algorithm != CredentialAlgorithm {
		return errors.New("unsupported relay credential algorithm")
	}
	if !isCanonicalRequired(credential.NetworkID) || !isCanonicalRequired(credential.NodeID) {
		return errors.New("relay credential is missing identity")
	}
	if !isCanonicalRequired(credential.KeyID) {
		return errors.New("relay credential key id is missing")
	}
	if !now.Before(credential.ExpiresAt) {
		return errors.New("relay credential is expired")
	}
	publicKey := strings.TrimSpace(trustedPublicKey)
	keyID, err := SigningKeyID(publicKey)
	if err != nil {
		return err
	}
	if credential.KeyID != keyID {
		return errors.New("relay credential key id mismatch")
	}
	publicKeyRaw, err := base64.RawURLEncoding.DecodeString(publicKey)
	if err != nil || len(publicKeyRaw) != ed25519.PublicKeySize {
		return errors.New("invalid relay credential public key")
	}
	signatureRaw, err := base64.RawURLEncoding.DecodeString(credential.Signature)
	if err != nil || len(signatureRaw) != ed25519.SignatureSize {
		return errors.New("invalid relay credential signature")
	}
	payload, err := SigningPayload(credential.KeyID, credential.NetworkID, credential.NodeID, credential.ExpiresAt)
	if err != nil {
		return err
	}
	if !ed25519.Verify(ed25519.PublicKey(publicKeyRaw), payload, signatureRaw) {
		return errors.New("relay credential signature verification failed")
	}
	return nil
}

func SigningPayloadForPublicKey(publicKey ed25519.PublicKey, networkID, nodeID string, expiresAt time.Time) ([]byte, error) {
	if len(publicKey) != ed25519.PublicKeySize {
		return nil, errors.New("invalid relay credential public key")
	}
	keyID, err := SigningKeyID(base64.RawURLEncoding.EncodeToString(publicKey))
	if err != nil {
		return nil, err
	}
	return SigningPayload(keyID, networkID, nodeID, expiresAt)
}

func SigningPayload(keyID, networkID, nodeID string, expiresAt time.Time) ([]byte, error) {
	if !isCanonicalRequired(keyID) {
		return nil, errors.New("relay credential key id is required")
	}
	if !isCanonicalRequired(networkID) || !isCanonicalRequired(nodeID) {
		return nil, errors.New("relay credential identity is required")
	}
	return json.Marshal(credentialPayload{Schema: credentialSchemaVersion, KeyID: keyID, NetworkID: networkID, NodeID: nodeID, ExpiresAt: expiresAt.UTC()})
}

func SigningKeyID(publicKey string) (string, error) {
	raw, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(publicKey))
	if err != nil || len(raw) != ed25519.PublicKeySize {
		return "", errors.New("invalid Ed25519 relay signing public key")
	}
	sum := sha256.Sum256(raw)
	return "ed25519:" + base64.RawURLEncoding.EncodeToString(sum[:]), nil
}

type SigningTrustKey struct {
	KeyID     string     `json:"key_id"`
	Algorithm string     `json:"algorithm"`
	PublicKey string     `json:"public_key"`
	NotBefore *time.Time `json:"not_before,omitempty"`
	NotAfter  *time.Time `json:"not_after,omitempty"`
}

type SigningTrustBundle struct {
	Version     int               `json:"version"`
	ActiveKeyID string            `json:"active_key_id"`
	Keys        []SigningTrustKey `json:"keys"`
}

func NewSigningTrustBundle(publicKey string) (SigningTrustBundle, error) {
	keyID, err := SigningKeyID(publicKey)
	if err != nil {
		return SigningTrustBundle{}, err
	}
	return SigningTrustBundle{Version: SigningTrustBundleVersion, ActiveKeyID: keyID, Keys: []SigningTrustKey{{KeyID: keyID, Algorithm: SigningKeyEd25519, PublicKey: strings.TrimSpace(publicKey)}}}, nil
}

func (b SigningTrustBundle) Validate() error {
	if b.Version != SigningTrustBundleVersion || !isCanonicalRequired(b.ActiveKeyID) || len(b.Keys) == 0 {
		return errors.New("invalid relay signing trust bundle")
	}
	seen := make(map[string]struct{}, len(b.Keys))
	activeFound := false
	for _, key := range b.Keys {
		if !isCanonicalRequired(key.KeyID) || key.Algorithm != SigningKeyEd25519 {
			return errors.New("invalid relay signing trust key")
		}
		if _, exists := seen[key.KeyID]; exists {
			return fmt.Errorf("relay signing trust key %s is duplicated", key.KeyID)
		}
		seen[key.KeyID] = struct{}{}
		derived, err := SigningKeyID(key.PublicKey)
		if err != nil || derived != key.KeyID {
			return fmt.Errorf("relay signing trust key %s does not match its public key", key.KeyID)
		}
		if key.NotBefore != nil && key.NotAfter != nil && !key.NotAfter.After(*key.NotBefore) {
			return fmt.Errorf("relay signing trust key %s has an invalid validity window", key.KeyID)
		}
		activeFound = activeFound || key.KeyID == b.ActiveKeyID
	}
	if !activeFound {
		return errors.New("relay signing trust bundle active key is missing")
	}
	return nil
}

func (b SigningTrustBundle) Resolve(keyID string, at time.Time) (SigningTrustKey, error) {
	if err := b.Validate(); err != nil {
		return SigningTrustKey{}, err
	}
	for _, key := range b.Keys {
		if key.KeyID != keyID {
			continue
		}
		if key.NotBefore != nil && at.UTC().Before(key.NotBefore.UTC()) {
			return SigningTrustKey{}, errors.New("relay signing key is not yet trusted")
		}
		if key.NotAfter != nil && !at.UTC().Before(key.NotAfter.UTC()) {
			return SigningTrustKey{}, errors.New("relay signing key trust has expired")
		}
		return key, nil
	}
	return SigningTrustKey{}, fmt.Errorf("relay signing key id %s is not trusted", keyID)
}

type ClientHello struct {
	Type                string     `json:"type"`
	ProtocolVersion     int        `json:"protocol_version"`
	Credential          Credential `json:"credential"`
	HeartbeatIntervalMS int        `json:"heartbeat_interval_ms,omitempty"`
}

func (m ClientHello) Validate() error {
	if m.Type != MessageClientHello || m.ProtocolVersion != Version {
		return errors.New("unsupported relay protocol")
	}
	if m.HeartbeatIntervalMS != 0 && (m.HeartbeatIntervalMS < MinHeartbeatIntervalMS || m.HeartbeatIntervalMS > MaxHeartbeatIntervalMS) {
		return errors.New("relay heartbeat interval is out of range")
	}
	return nil
}

type ClientFrame struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version"`
	PeerID          string `json:"peer_id"`
	Payload         []byte `json:"payload"`
}

func (m ClientFrame) Validate() error {
	if m.Type != MessageClientFrame || m.ProtocolVersion != Version {
		return errors.New("unsupported relay protocol")
	}
	if !isCanonicalRequired(m.PeerID) {
		return errors.New("relay peer_id is required")
	}
	if len(m.Payload) == 0 {
		return errors.New("relay payload is required")
	}
	if len(m.Payload) > MaxFramePayloadBytes {
		return fmt.Errorf("relay payload exceeds %d bytes", MaxFramePayloadBytes)
	}
	return nil
}

type ServerFrame struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version"`
	FromNodeID      string `json:"from_node_id"`
	Payload         []byte `json:"payload"`
}

type Ready struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version"`
	Ready           bool   `json:"ready"`
}

type Error struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version"`
	Error           string `json:"error"`
}

type Heartbeat struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version"`
	Time            string `json:"time"`
}

func (s EndpointSnapshot) Validate() error {
	if s.Version < 1 {
		return errors.New("relay endpoint snapshot version must be positive")
	}
	seen := make(map[string]struct{}, len(s.Endpoints))
	for _, endpoint := range s.Endpoints {
		if !isCanonicalRequired(endpoint.ID) || !isCanonicalRequired(endpoint.Addr) || endpoint.Protocol != EndpointProtocolTLS {
			return errors.New("relay endpoint id, address, and protocol are invalid")
		}
		if endpoint.Region != strings.TrimSpace(endpoint.Region) || endpoint.Priority < 0 {
			return errors.New("relay endpoint region or priority is invalid")
		}
		if _, exists := seen[endpoint.ID]; exists {
			return errors.New("relay endpoint id is duplicated")
		}
		seen[endpoint.ID] = struct{}{}
	}
	return nil
}

func isCanonicalRequired(value string) bool {
	return value != "" && value == strings.TrimSpace(value)
}
