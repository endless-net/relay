package app

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestRegistryGateDoesNotInvokeMutationWhileUnavailable(t *testing.T) {
	r := &Runtime{}
	calls := 0
	handler := func(context.Context, any) (any, error) {
		calls++
		return nil, nil
	}
	for _, available := range []bool{false, true, false} {
		r.registryHealthy.Store(available)
		_, err := r.requireRegistry(t.Context(), nil, nil, handler)
		if available && err != nil || !available && status.Code(err) != codes.Unavailable {
			t.Fatalf("registry available=%v: %v", available, err)
		}
	}
	if calls != 1 {
		t.Fatalf("handler invoked %d times", calls)
	}
}
