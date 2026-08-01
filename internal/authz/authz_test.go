package authz

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	protocolv1 "github.com/endless-net/relay/protocol/v1"
)

type stubAuthorizer struct {
	err    error
	calls  int
	bundle protocolv1.SigningTrustBundle
}

func (s *stubAuthorizer) AuthorizeCredential(context.Context, protocolv1.Credential) error {
	s.calls++
	return s.err
}

func (s *stubAuthorizer) AuthorizePeer(context.Context, protocolv1.Credential, string) error {
	s.calls++
	return s.err
}

func (s *stubAuthorizer) RelayTrustBundle(context.Context) (protocolv1.SigningTrustBundle, error) {
	return s.bundle, s.err
}

func TestCacheUsesPositiveStaleWindowOnlyOnUpstreamFailure(t *testing.T) {
	now := time.Now().UTC()
	upstream := &stubAuthorizer{}
	cache := NewCache(upstream)
	cache.Now = func() time.Time { return now }
	credential := protocolv1.Credential{NetworkID: "network", NodeID: "node"}
	if err := cache.AuthorizeCredential(context.Background(), credential); err != nil {
		t.Fatal(err)
	}
	now = now.Add(6 * time.Second)
	upstream.err = errors.New("unavailable")
	if err := cache.AuthorizeCredential(context.Background(), credential); err != nil {
		t.Fatalf("stale authorization failed: %v", err)
	}
	now = now.Add(25 * time.Second)
	if err := cache.AuthorizeCredential(context.Background(), credential); err == nil {
		t.Fatal("authorization survived beyond stale window")
	}
}

func TestCacheDoesNotUseNegativeStaleWindow(t *testing.T) {
	now := time.Now().UTC()
	upstream := &stubAuthorizer{err: ErrDenied}
	cache := NewCache(upstream)
	cache.Now = func() time.Time { return now }
	credential := protocolv1.Credential{NetworkID: "network", NodeID: "node"}
	if err := cache.AuthorizeCredential(context.Background(), credential); !errors.Is(err, ErrDenied) {
		t.Fatalf("negative authorization error = %v", err)
	}
	now = now.Add(2 * time.Second)
	upstream.err = errors.New("unavailable")
	if err := cache.AuthorizeCredential(context.Background(), credential); err == nil {
		t.Fatal("negative decision gained a stale window")
	}
}

func TestHTTPAuthorizerRequiresExactNoContentSuccess(t *testing.T) {
	for _, test := range []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{name: "no content", statusCode: http.StatusNoContent},
		{name: "legacy JSON success", statusCode: http.StatusOK, wantErr: true},
		{name: "denied", statusCode: http.StatusForbidden, wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(response http.ResponseWriter, request *http.Request) {
				if request.Header.Get("X-EndlessNet-Service-Token") != "" {
					t.Error("retired shared service token header was sent")
				}
				response.WriteHeader(test.statusCode)
			}))
			defer server.Close()
			authorizer := HTTPAuthorizer{BaseURL: server.URL, HTTPClient: server.Client()}
			err := authorizer.AuthorizeCredential(context.Background(), protocolv1.Credential{})
			if (err != nil) != test.wantErr {
				t.Fatalf("error = %v, want error %v", err, test.wantErr)
			}
		})
	}
}
