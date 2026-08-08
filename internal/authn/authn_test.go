package authn

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

type testAuthenticator struct {
	token string
}

func (a testAuthenticator) AuthenticateHeader(header string) (Principal, bool) {
	if header == "Bearer "+a.token {
		return Principal{Name: "oauth-user", Scopes: map[string]bool{"read": true}}, true
	}
	return Principal{}, false
}

func TestCompositeAcceptsAPIKeyAndOAuth(t *testing.T) {
	manager, err := NewManager(config.AuthConfig{APIKeys: []config.APIKeyConfig{{
		Name: "reader", Token: "api-token", Scopes: []string{"read"}, Connections: []string{"default"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	composite := NewComposite(manager, testAuthenticator{token: "oauth-token"})
	if principal, ok := composite.AuthenticateHeader("Bearer api-token"); !ok || principal.Name != "reader" {
		t.Fatalf("API key was not accepted: %#v %v", principal, ok)
	}
	if principal, ok := composite.AuthenticateHeader("Bearer oauth-token"); !ok || principal.Name != "oauth-user" {
		t.Fatalf("OAuth token was not accepted: %#v %v", principal, ok)
	}
}

func TestMiddlewareUsesResourceMetadataChallenge(t *testing.T) {
	handler := HTTPMiddlewareWithLoggerAndChallenge(
		testAuthenticator{token: "oauth-token"},
		http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		nil,
		`Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"`,
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/mcp", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d", recorder.Code)
	}
	if got := recorder.Header().Get("WWW-Authenticate"); got != `Bearer resource_metadata="https://mcp.example.com/.well-known/oauth-protected-resource"` {
		t.Fatalf("unexpected challenge: %q", got)
	}

	recorder = httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	request.Header.Set("Authorization", "Bearer oauth-token")
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("authenticated status = %d", recorder.Code)
	}
}
