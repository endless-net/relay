// Package wire содержит JSON DTO публичного Relay wire v1.
// Эти типы не проверяют подпись, authority или срок права.
package wire

import "time"

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
