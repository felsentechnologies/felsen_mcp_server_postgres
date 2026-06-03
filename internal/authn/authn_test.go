package authn

import (
	"testing"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

func TestAuthenticateHeaderAndScopes(t *testing.T) {
	manager, err := NewManager(config.AuthConfig{APIKeys: []config.APIKeyConfig{{
		Name:        "reader",
		Token:       "secret-token",
		Scopes:      []string{"read"},
		Connections: []string{"default"},
	}}})
	if err != nil {
		t.Fatal(err)
	}

	principal, ok := manager.AuthenticateHeader("Bearer secret-token")
	if !ok {
		t.Fatal("expected token to authenticate")
	}
	if principal.Name != "reader" || !principal.HasScope("read") || principal.HasScope("write") {
		t.Fatalf("unexpected principal: %#v", principal)
	}
	if !principal.CanUseConnection("default") || principal.CanUseConnection("analytics") {
		t.Fatalf("unexpected connection policy: %#v", principal.Connections)
	}
	if _, ok := manager.AuthenticateHeader("Bearer wrong-token"); ok {
		t.Fatal("invalid token authenticated")
	}
}
