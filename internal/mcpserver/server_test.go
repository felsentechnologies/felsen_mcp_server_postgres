package mcpserver

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/authn"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/postgres"
)

type bearerRoundTripper struct {
	next http.RoundTripper
}

func (t bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	clone.Header.Set("Authorization", "Bearer test-token")
	return t.next.RoundTrip(clone)
}

func TestStreamableHTTPContract(t *testing.T) {
	manager, err := authn.NewManager(config.AuthConfig{APIKeys: []config.APIKeyConfig{{
		Name: "reader", Token: "test-token", Scopes: []string{"read"}, Connections: []string{"default"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Server: config.ServerConfig{
		Endpoint:       "/custom-mcp",
		PublicBaseURL:  "https://mcp.example.com",
		JSONResponse:   true,
		Stateless:      true,
		SessionTimeout: time.Minute.String(),
	}}
	handler := New(cfg, &postgres.Store{}, manager, nil, nil)
	httpServer := httptest.NewServer(authn.HTTPMiddleware(manager, handler))
	t.Cleanup(httpServer.Close)

	unauthorized, err := http.Get(httpServer.URL + "/custom-mcp")
	if err != nil {
		t.Fatal(err)
	}
	_ = unauthorized.Body.Close()
	if unauthorized.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected unauthorized request to fail, got %d", unauthorized.StatusCode)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "contract-test", Version: "test"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL + "/custom-mcp",
		HTTPClient:           &http.Client{Transport: bearerRoundTripper{next: http.DefaultTransport}},
		DisableStandaloneSSE: true,
		MaxRetries:           -1,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	session, err := client.Connect(ctx, transport, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = session.Close() })

	tools, err := session.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]*mcp.Tool{}
	for _, tool := range tools.Tools {
		seen[tool.Name] = tool
	}
	for _, name := range []string{"search", "fetch", "execute_dml", "execute_ddl"} {
		tool, ok := seen[name]
		if !ok {
			t.Fatalf("missing tool %q", name)
		}
		if tool.OutputSchema == nil {
			t.Fatalf("tool %q has no output schema", name)
		}
	}
	if seen["search"].Annotations == nil || !seen["search"].Annotations.ReadOnlyHint {
		t.Fatal("search must advertise readOnlyHint=true")
	}
	if seen["execute_dml"].Annotations == nil || seen["execute_dml"].Annotations.ReadOnlyHint ||
		seen["execute_dml"].Annotations.DestructiveHint == nil || !*seen["execute_dml"].Annotations.DestructiveHint {
		t.Fatal("execute_dml must advertise a consequential destructive operation")
	}

	for name, field := range map[string]string{"search": "query", "fetch": "id"} {
		var schema map[string]any
		data, err := json.Marshal(seen[name].InputSchema)
		if err != nil {
			t.Fatalf("marshal %s input schema: %v", name, err)
		}
		if err := json.Unmarshal(data, &schema); err != nil {
			t.Fatalf("decode %s input schema: %v", name, err)
		}
		properties, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Fatalf("%s input schema has no properties: %s", name, data)
		}
		if _, ok := properties[field]; !ok {
			t.Fatalf("%s input schema must expose %q: %s", name, field, data)
		}
		if _, ok := properties["connection_name"]; ok {
			t.Fatalf("%s input schema must follow the standard single-argument contract: %s", name, data)
		}
	}
}

func TestObjectIDsAreConnectionScoped(t *testing.T) {
	first := objectReference{Connection: "analytics", Schema: "public", Table: "customers"}
	second := objectReference{Connection: "billing", Schema: "public", Table: "customers"}
	firstID := encodeObjectID(first)
	secondID := encodeObjectID(second)
	if firstID == secondID {
		t.Fatal("object IDs from different connections must be unique")
	}
	decoded, err := decodeObjectID(firstID)
	if err != nil {
		t.Fatalf("decode object ID: %v", err)
	}
	if decoded != first {
		t.Fatalf("decoded object ID differs: got %+v, want %+v", decoded, first)
	}
	if _, err := decodeObjectID("public.customers"); err == nil {
		t.Fatal("legacy ambiguous IDs must not be accepted by standard fetch")
	}
}

func TestCitationURLIsSignedAndScoped(t *testing.T) {
	app := &App{cfg: &config.Config{Server: config.ServerConfig{
		PublicBaseURL:      "https://mcp.example.com",
		CitationSigningKey: "test-citation-signing-key-with-32-characters",
		CitationTTL:        "15m",
	}}}

	citation, err := url.Parse(app.sourceURL("default", "public", "customers", "email"))
	if err != nil {
		t.Fatal(err)
	}
	query := citation.Query()
	if query.Get("signature") == "" || query.Get("expires") == "" {
		t.Fatalf("citation URL is not signed: %s", citation)
	}
	if !app.validCitationURL(query) {
		t.Fatal("expected freshly generated citation URL to validate")
	}

	tampered := url.Values{}
	for key, values := range query {
		tampered[key] = append([]string(nil), values...)
	}
	tampered.Set("table", "payments")
	if app.validCitationURL(tampered) {
		t.Fatal("expected changing the signed object to invalidate the citation")
	}
}

func TestSignedSourceRequestReachesSourceAuthorization(t *testing.T) {
	manager, err := authn.NewManager(config.AuthConfig{APIKeys: []config.APIKeyConfig{{
		Name: "reader", Token: "test-token", Scopes: []string{"read"}, Connections: []string{"default"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	cfg := &config.Config{Server: config.ServerConfig{
		Endpoint:           "/mcp",
		PublicBaseURL:      "https://mcp.example.com",
		CitationSigningKey: "test-citation-signing-key-with-32-characters",
		CitationTTL:        "15m",
		SessionTimeout:     time.Minute.String(),
	}}
	app := &App{cfg: cfg, store: &postgres.Store{}, auth: manager}
	request := httptest.NewRequest(http.MethodGet, app.sourceURL("default", "public", "customers", ""), nil)
	recorder := httptest.NewRecorder()
	app.sourceHTTPHandler(recorder, request)
	if recorder.Code == http.StatusUnauthorized {
		t.Fatal("signed source request must not require a Bearer header")
	}
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("expected empty test store to return not found after signature validation, got %d", recorder.Code)
	}
}
