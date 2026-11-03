package wire

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"
)

const (
	Version                = 1
	MaxFramePayloadBytes   = 65536
	MaxLineBytes           = 147456
	CredentialAlgorithm    = "ed25519-relay-credential-v3"
	MinHeartbeatIntervalMS = 100
	MaxHeartbeatIntervalMS = 30000
	EndpointProtocolTLS    = "relay-v1-tls"
	MessageClientHello     = "client_hello"
	MessageClientFrame     = "client_frame"
	MessageServerFrame     = "server_frame"
	MessageReady           = "ready"
	MessageError           = "error"
	MessageHeartbeat       = "heartbeat"
)

// ErrMessage intentionally contains no untrusted wire or credential contents.
var ErrMessage = errors.New("invalid relay wire message")

// Message is a public wire v1 message, not evidence of trust or authorization.
type Message interface{ wireMessage() }

type ClientHello struct {
	Type                string     `json:"type"`
	ProtocolVersion     int        `json:"protocol_version"`
	Credential          Credential `json:"credential"`
	HeartbeatIntervalMS int        `json:"heartbeat_interval_ms,omitempty"`
}

type ClientFrame struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version"`
	PeerID          string `json:"peer_id"`
	PeerNetworkID   string `json:"peer_network_id"`
	Payload         []byte `json:"payload"`
}

type ServerFrame struct {
	Type            string `json:"type"`
	ProtocolVersion int    `json:"protocol_version"`
	FromNodeID      string `json:"from_node_id"`
	FromNetworkID   string `json:"from_network_id"`
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

func (*ClientHello) wireMessage() {}
func (*ClientFrame) wireMessage() {}
func (*ServerFrame) wireMessage() {}
func (*Ready) wireMessage()       {}
func (*Error) wireMessage()       {}
func (*Heartbeat) wireMessage()   {}

// Decoder preserves buffered data between NDJSON messages and bounds a line
// including its LF delimiter. EOF in an incomplete line never returns a message.
// The stream owner must set deadlines/cancel and authenticate TLS separately.
type Decoder struct {
	reader *bufio.Reader
	failed bool
}

func NewDecoder(reader io.Reader) *Decoder {
	if reader == nil {
		return &Decoder{}
	}
	return &Decoder{reader: bufio.NewReaderSize(reader, MaxLineBytes)}
}

func (d *Decoder) Read() (Message, error) {
	if d == nil || d.reader == nil || d.failed {
		return nil, ErrMessage
	}
	line, err := d.reader.ReadSlice('\n')
	if errors.Is(err, io.EOF) && len(line) == 0 {
		return nil, io.EOF
	}
	if err != nil || len(line) > MaxLineBytes {
		d.failed = true
		return nil, ErrMessage
	}
	message, err := DecodeMessage(line)
	if err != nil {
		d.failed = true
	}
	return message, err
}

// DecodeMessage validates shape and bounds only; it does not verify credential
// signatures, current trust, expiry or directed pair rights. A caller cannot
// treat a decoded Ready as peer payload reachability.
func DecodeMessage(raw []byte) (Message, error) {
	if len(raw) == 0 || len(raw) > MaxLineBytes || !utf8.Valid(raw) {
		return nil, ErrMessage
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if strictValue(decoder, 0) != nil {
		return nil, ErrMessage
	}
	if _, err := decoder.Token(); !errors.Is(err, io.EOF) {
		return nil, ErrMessage
	}
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil {
		return nil, ErrMessage
	}
	var kind string
	if json.Unmarshal(fields["type"], &kind) != nil {
		return nil, ErrMessage
	}
	var target Message
	var required string
	switch kind {
	case MessageClientHello:
		target, required = &ClientHello{}, "type protocol_version credential"
	case MessageClientFrame:
		target, required = &ClientFrame{}, "type protocol_version peer_id peer_network_id payload"
	case MessageServerFrame:
		target, required = &ServerFrame{}, "type protocol_version from_node_id from_network_id payload"
	case MessageReady:
		target, required = &Ready{}, "type protocol_version ready"
	case MessageError:
		target, required = &Error{}, "type protocol_version error"
	case MessageHeartbeat:
		target, required = &Heartbeat{}, "type protocol_version time"
	default:
		return nil, ErrMessage
	}
	optional := ""
	if kind == MessageClientHello {
		optional = "heartbeat_interval_ms"
	}
	if !hasShape(fields, required, optional) {
		return nil, ErrMessage
	}
	decoder = json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || !validMessage(target, fields) {
		return nil, ErrMessage
	}
	return target, nil
}

// MarshalMessage validates a known typed message before producing one LF-ended
// line. No raw encoder or arbitrary DTO is accepted at this boundary.
func MarshalMessage(message Message) ([]byte, error) {
	if !boundedMessage(message) {
		return nil, ErrMessage
	}
	raw, err := json.Marshal(message)
	if err != nil || len(raw)+1 > MaxLineBytes {
		return nil, ErrMessage
	}
	if _, err := DecodeMessage(raw); err != nil {
		return nil, ErrMessage
	}
	return append(raw, '\n'), nil
}

func boundedMessage(message Message) bool {
	var values []string
	switch m := message.(type) {
	case *ClientHello:
		if m == nil {
			return false
		}
		values = []string{m.Type, m.Credential.Algorithm, m.Credential.KeyID, m.Credential.NetworkID, m.Credential.NodeID, m.Credential.Signature}
	case *ClientFrame:
		if m == nil || len(m.Payload) == 0 || len(m.Payload) > MaxFramePayloadBytes {
			return false
		}
		values = []string{m.Type, m.PeerID, m.PeerNetworkID}
	case *ServerFrame:
		if m == nil || len(m.Payload) == 0 || len(m.Payload) > MaxFramePayloadBytes {
			return false
		}
		values = []string{m.Type, m.FromNodeID, m.FromNetworkID}
	case *Ready:
		if m == nil {
			return false
		}
		values = []string{m.Type}
	case *Error:
		if m == nil {
			return false
		}
		values = []string{m.Type, m.Error}
	case *Heartbeat:
		if m == nil {
			return false
		}
		values = []string{m.Type, m.Time}
	default:
		return false
	}
	total := 0
	for _, value := range values {
		if len(value) > MaxLineBytes || !utf8.ValidString(value) {
			return false
		}
		total += len(value)
	}
	return total < MaxLineBytes
}

func strictValue(decoder *json.Decoder, depth int) error {
	if depth > 2 {
		return ErrMessage
	}
	token, err := decoder.Token()
	if err != nil || token == nil {
		return ErrMessage
	}
	if delimiter, ok := token.(json.Delim); ok {
		if delimiter != '{' {
			return ErrMessage
		}
		seen := make(map[string]bool)
		for decoder.More() {
			key, err := decoder.Token()
			name, ok := key.(string)
			if err != nil || !ok || seen[name] {
				return ErrMessage
			}
			seen[name] = true
			if strictValue(decoder, depth+1) != nil {
				return ErrMessage
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return ErrMessage
		}
	} else if depth == 0 {
		return ErrMessage
	}
	return nil
}

func hasShape(fields map[string]json.RawMessage, required, optional string) bool {
	for _, name := range strings.Fields(required) {
		if _, ok := fields[name]; !ok {
			return false
		}
	}
	for name := range fields {
		if !strings.Contains(" "+required+" "+optional+" ", " "+name+" ") {
			return false
		}
	}
	return true
}

func canonicalID(value string) bool {
	return value != "" && strings.TrimSpace(value) == value
}

var wireTime = regexp.MustCompile(`^[0-9]{4}-(0[1-9]|1[0-2])-(0[1-9]|[12][0-9]|3[01])T([01][0-9]|2[0-3]):[0-5][0-9]:[0-5][0-9](\.[0-9]{1,9})?(Z|[+-]([01][0-9]|2[0-3]):[0-5][0-9])$`)

func validTime(raw json.RawMessage) bool {
	var value string
	if json.Unmarshal(raw, &value) != nil || !wireTime.MatchString(value) {
		return false
	}
	_, err := time.Parse(time.RFC3339Nano, value)
	return err == nil
}

func validPayload(value []byte, raw json.RawMessage) bool {
	var encoded string
	return len(value) > 0 && len(value) <= MaxFramePayloadBytes &&
		json.Unmarshal(raw, &encoded) == nil && base64.StdEncoding.EncodeToString(value) == encoded
}

func validMessage(message Message, fields map[string]json.RawMessage) bool {
	switch m := message.(type) {
	case *ClientHello:
		var credential map[string]json.RawMessage
		return m.ProtocolVersion == Version && json.Unmarshal(fields["credential"], &credential) == nil &&
			hasShape(credential, "algorithm key_id network_id node_id expires_at signature", "") &&
			validTime(credential["expires_at"]) && validCredential(m.Credential) &&
			(m.HeartbeatIntervalMS == 0 || m.HeartbeatIntervalMS >= MinHeartbeatIntervalMS && m.HeartbeatIntervalMS <= MaxHeartbeatIntervalMS)
	case *ClientFrame:
		return m.ProtocolVersion == Version && canonicalID(m.PeerID) && canonicalID(m.PeerNetworkID) && validPayload(m.Payload, fields["payload"])
	case *ServerFrame:
		return m.ProtocolVersion == Version && canonicalID(m.FromNodeID) && canonicalID(m.FromNetworkID) && validPayload(m.Payload, fields["payload"])
	case *Ready:
		return m.ProtocolVersion == Version && m.Ready
	case *Error:
		return m.ProtocolVersion == Version
	case *Heartbeat:
		return m.ProtocolVersion == Version && validTime(fields["time"])
	default:
		return false
	}
}

func validCredential(value Credential) bool {
	key, ok := strings.CutPrefix(value.KeyID, "ed25519:")
	decodedKey, err := base64.RawURLEncoding.Strict().DecodeString(key)
	if !ok || err != nil || len(decodedKey) != 32 || base64.RawURLEncoding.EncodeToString(decodedKey) != key {
		return false
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(value.Signature)
	return value.Algorithm == CredentialAlgorithm && canonicalID(value.NetworkID) && canonicalID(value.NodeID) &&
		err == nil && len(signature) == 64 && base64.RawURLEncoding.EncodeToString(signature) == value.Signature
}
