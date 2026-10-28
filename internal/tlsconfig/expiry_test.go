package tlsconfig

import (
	"crypto/tls"
	"crypto/x509"
	"net/url"
	"testing"
	"time"
)

func TestPeerExpiryIsCheckedOnExistingConnectionState(t *testing.T) {
	id, e := url.Parse("spiffe://endlessnet.ru/service/relay/r")
	if e != nil {
		t.Fatal(e)
	}
	cert := &x509.Certificate{URIs: []*url.URL{id}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour)}
	state := tls.ConnectionState{PeerCertificates: []*x509.Certificate{cert}}
	if _, e = PeerID(state); e != nil {
		t.Fatal(e)
	}
	cert.NotAfter = time.Now().Add(-time.Second)
	if _, e = PeerID(state); e == nil {
		t.Fatal("expired peer accepted on existing stream")
	}
	cert.NotAfter = time.Now().Add(time.Hour)
	cert.NotBefore = time.Now().Add(time.Minute)
	if _, e = PeerID(state); e == nil {
		t.Fatal("future peer accepted")
	}
}
