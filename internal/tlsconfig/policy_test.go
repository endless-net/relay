package tlsconfig

import "testing"

func TestIdentityPolicyOperatorDomain(t *testing.T) {
	p, err := NewIdentityPolicy("operator.example", "spiffe://operator.example/control", "spiffe://operator.example/authz")
	if err != nil {
		t.Fatal(err)
	}
	id, err := p.RelayIdentity("east")
	if err != nil || id.String() != "spiffe://operator.example/relay/east" {
		t.Fatalf("identity: %v %v", id, err)
	}
	for _, invalid := range []string{"", "a/b", " a", "a?b", "a%20b"} {
		if _, err := p.RelayIdentity(invalid); err == nil {
			t.Errorf("accepted relay ID %q", invalid)
		}
	}
}
func TestIdentityPolicyRejectsContradictions(t *testing.T) {
	for _, v := range [][3]string{
		{"spiffe://operator.example", "", ""},
		{"operator.example", "spiffe://endlessnet.ru/service/relay-coordinator", ""},
		{"operator.example", "spiffe://operator.example/relay/r", ""},
		{"operator.example", "spiffe://operator.example/same", "spiffe://operator.example/same"},
		{" operator.example", "", ""},
	} {
		if _, err := NewIdentityPolicy(v[0], v[1], v[2]); err == nil {
			t.Errorf("accepted contradictory policy %v", v)
		}
	}
}
