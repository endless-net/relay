package mesh

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	relayv1 "github.com/unng-lab/endlessnet-relay/api/relay/v1"
	"github.com/unng-lab/endlessnet-relay/internal/relay"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func TestManagersForwardOneHopAcrossRelays(t *testing.T) {
	ca, caKey := testCA(t)
	certA := testRelayCertificate(t, ca, caKey, "relay-a")
	certB := testRelayCertificate(t, ca, caKey, "relay-b")
	roots := x509.NewCertPool()
	roots.AddCert(ca)
	serverConfig := func(certificate tls.Certificate) *tls.Config {
		return &tls.Config{Certificates: []tls.Certificate{certificate}, ClientCAs: roots, ClientAuth: tls.RequireAndVerifyClientCert, MinVersion: tls.VersionTLS13}
	}
	clientConfig := func(certificate tls.Certificate) *tls.Config {
		return &tls.Config{Certificates: []tls.Certificate{certificate}, RootCAs: roots, ServerName: "localhost", MinVersion: tls.VersionTLS13}
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	type deliveredFrame struct {
		networkID, fromNodeID, toNodeID string
		epoch                           int64
		payload                         []byte
	}
	delivered := make(chan deliveredFrame, 1)
	managerA := NewManager(ctx, "relay-a", "boot-a", clientConfig(certA), nil)
	managerB := NewManager(ctx, "relay-b", "boot-b", clientConfig(certB), func(networkID, fromNodeID, toNodeID string, epoch int64, payload []byte) error {
		delivered <- deliveredFrame{networkID: networkID, fromNodeID: fromNodeID, toNodeID: toNodeID, epoch: epoch, payload: payload}
		return nil
	})
	defer managerA.Close()
	defer managerB.Close()
	listenerB, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverB := grpc.NewServer(grpc.Creds(credentials.NewTLS(serverConfig(certB))))
	relayv1.RegisterRelayMeshServer(serverB, managerB)
	go serverB.Serve(listenerB)
	defer serverB.Stop()

	managerA.UpdatePeers([]*relayv1.RelayInstance{{RelayId: "relay-b", BootId: "boot-b", MeshAddr: listenerB.Addr().String()}})
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		err := managerA.Forward(ctx, relay.PeerRoute{RelayID: "relay-b", BootID: "boot-b", Epoch: 9}, "network", "node-a", "node-b", []byte("opaque"))
		if err == nil {
			break
		}
		select {
		case <-deadline.C:
			t.Fatalf("mesh peer did not become ready: %v", err)
		case <-ticker.C:
		}
	}
	select {
	case frame := <-delivered:
		if frame.networkID != "network" || frame.fromNodeID != "node-a" || frame.toNodeID != "node-b" || frame.epoch != 9 || string(frame.payload) != "opaque" {
			t.Fatalf("delivered frame = %#v", frame)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("cross-relay frame was not delivered")
	}
}

func TestForwardRejectsStalePeerBoot(t *testing.T) {
	manager := NewManager(context.Background(), "relay-a", "boot-a", &tls.Config{MinVersion: tls.VersionTLS13}, nil)
	defer manager.Close()
	manager.UpdatePeers([]*relayv1.RelayInstance{{RelayId: "relay-b", BootId: "new-boot", MeshAddr: "127.0.0.1:1"}})
	err := manager.Forward(context.Background(), relay.PeerRoute{RelayID: "relay-b", BootID: "old-boot", Epoch: 1}, "network", "a", "b", []byte("frame"))
	if err != relay.ErrDestinationFenced {
		t.Fatalf("stale boot forwarding error = %v", err)
	}
}

func TestForwardRejectsUnavailablePeer(t *testing.T) {
	manager := NewManager(context.Background(), "relay-a", "boot-a", &tls.Config{MinVersion: tls.VersionTLS13}, nil)
	defer manager.Close()
	manager.UpdatePeers([]*relayv1.RelayInstance{{RelayId: "relay-b", BootId: "boot-b", MeshAddr: "127.0.0.1:1"}})
	err := manager.Forward(context.Background(), relay.PeerRoute{RelayID: "relay-b", BootID: "boot-b", Epoch: 1}, "network", "a", "b", []byte("frame"))
	if err == nil || err.Error() != "relay mesh peer is unavailable" {
		t.Fatalf("unavailable peer forwarding error = %v", err)
	}
}

func TestValidateMeshMessageRejectsVersionBodyAndUnknownFields(t *testing.T) {
	unknown := []byte{0x98, 0x06, 0x01}
	for _, message := range []*relayv1.MeshMessage{
		{Body: &relayv1.MeshMessage_Ping{Ping: &relayv1.MeshPing{}}},
		{ProtocolVersion: relayv1.MeshProtocolVersion},
		{ProtocolVersion: relayv1.MeshProtocolVersion, Body: &relayv1.MeshMessage_Hello{Hello: &relayv1.MeshHello{RelayId: " relay", BootId: "boot"}}},
	} {
		if err := validateMeshMessage(message); err == nil {
			t.Fatalf("invalid mesh message was accepted: %#v", message)
		}
	}
	message := &relayv1.MeshMessage{ProtocolVersion: relayv1.MeshProtocolVersion, Body: &relayv1.MeshMessage_Ping{Ping: &relayv1.MeshPing{}}}
	message.ProtoReflect().SetUnknown(unknown)
	if err := validateMeshMessage(message); err == nil {
		t.Fatal("mesh message with unknown protobuf fields was accepted")
	}
}

func testCA(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	raw, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(raw)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

func testRelayCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, relayID string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	identity, err := url.Parse("spiffe://endlessnet.ru/relay/" + relayID)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	template := &x509.Certificate{SerialNumber: big.NewInt(int64(len(relayID) + 10)), Subject: pkix.Name{CommonName: relayID}, DNSNames: []string{"localhost"}, URIs: []*url.URL{identity}, NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth}}
	raw, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{raw, ca.Raw}, PrivateKey: key}
}
