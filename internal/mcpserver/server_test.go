package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/authn"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/oauth"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/postgres"
)

type bearerRoundTripper struct {
	next  http.RoundTripper
	token string
}

func (t bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	clone := req.Clone(req.Context())
	token := t.token
	if token == "" {
		token = "test-token"
	}
	clone.Header.Set("Authorization", "Bearer "+token)
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
	}, Auth: config.AuthConfig{APIKeys: []config.APIKeyConfig{{
		Name: "reader", Scopes: []string{"read"}, Connections: []string{"default"},
	}}}, OAuth: config.OAuthConfig{Enabled: true, Principal: "reader"}}
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
	if _, ok := seen["search"].Meta["securitySchemes"]; !ok {
		t.Fatalf("search must advertise OAuth security schemes in _meta: %#v", seen["search"].Meta)
	}
	if _, ok := seen["execute_dml"].Meta["securitySchemes"]; ok {
		t.Fatal("read-only OAuth principal must not advertise an unavailable write scope")
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

func TestTopLevelToolSecuritySchemesMirrorMeta(t *testing.T) {
	body := []byte(`{"jsonrpc":"2.0","id":1,"result":{"tools":[{"name":"search","_meta":{"securitySchemes":[{"type":"oauth2","scopes":["read"]}]}}]}}`)
	transformed := addTopLevelToolSecuritySchemes(body, "application/json")
	if transformed == nil {
		t.Fatal("expected tool descriptor response to be transformed")
	}
	var envelope map[string]any
	if err := json.Unmarshal(transformed, &envelope); err != nil {
		t.Fatal(err)
	}
	result := envelope["result"].(map[string]any)
	tool := result["tools"].([]any)[0].(map[string]any)
	if _, ok := tool["securitySchemes"]; !ok {
		t.Fatalf("top-level securitySchemes missing: %#v", tool)
	}
	if addTopLevelToolSecuritySchemes(body, "text/event-stream") != nil {
		t.Fatal("SSE responses must not be buffered or transformed")
	}
}

func TestOAuthAccessTokenCanListMCPTools(t *testing.T) {
	manager, err := authn.NewManager(config.AuthConfig{APIKeys: []config.APIKeyConfig{{
		Name: "reader", Token: "api-token", Scopes: []string{"read"}, Connections: []string{"default"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := oauth.New(config.OAuthConfig{
		Enabled:              true,
		Issuer:               "https://mcp.example.com",
		Resource:             "https://mcp.example.com",
		SigningKey:           "test-oauth-signing-key-with-at-least-32-chars",
		Username:             "chatgpt",
		Password:             "correct-horse-battery-staple",
		Principal:            "reader",
		ClientID:             "felsen-chatgpt",
		DefaultScopes:        []string{"read"},
		BaseScopes:           []string{"read"},
		AccessTokenTTL:       time.Hour.String(),
		RefreshTokenTTL:      (24 * time.Hour).String(),
		AuthorizationCodeTTL: (5 * time.Minute).String(),
	}, manager)
	if err != nil {
		t.Fatal(err)
	}
	verifier := "mcp-integration-pkce-verifier-1234567890"
	sum := sha256.Sum256([]byte(verifier))
	challenge := base64.RawURLEncoding.EncodeToString(sum[:])
	authorizeValues := url.Values{
		"client_id":             {"felsen-chatgpt"},
		"redirect_uri":          {"https://chatgpt.com/connector/oauth/test"},
		"response_type":         {"code"},
		"scope":                 {"read"},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}
	authorizeValues.Set("username", "chatgpt")
	authorizeValues.Set("password", "correct-horse-battery-staple")
	authorizeValues.Set("consent", "allow")
	authorizeRequest := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(authorizeValues.Encode()))
	authorizeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	authorizeRecorder := httptest.NewRecorder()
	provider.ServeHTTP(authorizeRecorder, authorizeRequest)
	if authorizeRecorder.Code != http.StatusFound {
		t.Fatalf("authorize status = %d, body = %s", authorizeRecorder.Code, authorizeRecorder.Body.String())
	}
	redirect, err := url.Parse(authorizeRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	tokenForm := url.Values{
		"client_id":     {"felsen-chatgpt"},
		"grant_type":    {"authorization_code"},
		"code":          {redirect.Query().Get("code")},
		"redirect_uri":  {"https://chatgpt.com/connector/oauth/test"},
		"code_verifier": {verifier},
	}
	tokenRequest := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRecorder := httptest.NewRecorder()
	provider.ServeHTTP(tokenRecorder, tokenRequest)
	if tokenRecorder.Code != http.StatusOK {
		t.Fatalf("token status = %d, body = %s", tokenRecorder.Code, tokenRecorder.Body.String())
	}
	var tokenResponse struct {
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(tokenRecorder.Body.Bytes(), &tokenResponse); err != nil {
		t.Fatal(err)
	}
	if tokenResponse.AccessToken == "" {
		t.Fatal("OAuth token response did not contain access_token")
	}

	cfg := &config.Config{
		Server: config.ServerConfig{Endpoint: "/custom-mcp", PublicBaseURL: "https://mcp.example.com", JSONResponse: true, Stateless: true, SessionTimeout: time.Minute.String()},
		Auth:   config.AuthConfig{APIKeys: []config.APIKeyConfig{{Name: "reader", Scopes: []string{"read"}, Connections: []string{"default"}}}},
		OAuth:  config.OAuthConfig{Enabled: true, Principal: "reader"},
	}
	authenticator := authn.NewComposite(manager, provider)
	handler := New(cfg, &postgres.Store{}, authenticator, nil, nil)
	httpServer := httptest.NewServer(authn.HTTPMiddlewareWithLoggerAndChallenge(authenticator, handler, nil, provider.Challenge()))
	t.Cleanup(httpServer.Close)
	client := mcp.NewClient(&mcp.Implementation{Name: "oauth-contract-test", Version: "test"}, nil)
	transport := &mcp.StreamableClientTransport{
		Endpoint:             httpServer.URL + "/custom-mcp",
		HTTPClient:           &http.Client{Transport: bearerRoundTripper{next: http.DefaultTransport, token: tokenResponse.AccessToken}},
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
	for _, tool := range tools.Tools {
		if tool.Name == "search" {
			if _, ok := tool.Meta["securitySchemes"]; !ok {
				t.Fatalf("OAuth-authenticated tools/list did not return search security metadata: %#v", tool.Meta)
			}
			return
		}
	}
	t.Fatal("OAuth-authenticated tools/list did not return search")
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
