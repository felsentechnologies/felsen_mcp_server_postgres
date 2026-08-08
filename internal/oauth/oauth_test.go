package oauth

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/authn"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

func testProvider(t *testing.T) *Provider {
	t.Helper()
	manager, err := authn.NewManager(config.AuthConfig{APIKeys: []config.APIKeyConfig{{
		Name: "reader", Token: "api-token", Scopes: []string{"read"}, Connections: []string{"default"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	provider, err := New(config.OAuthConfig{
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
	return provider
}

func TestMetadataAdvertisesOAuthEndpoints(t *testing.T) {
	provider := testProvider(t)
	server := httptest.NewServer(provider)
	t.Cleanup(server.Close)

	req, err := http.NewRequest(http.MethodGet, server.URL+"/.well-known/oauth-authorization-server", nil)
	if err != nil {
		t.Fatal(err)
	}
	response, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("metadata status = %d", response.StatusCode)
	}
	var metadata map[string]any
	if err := json.NewDecoder(response.Body).Decode(&metadata); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"authorization_endpoint", "token_endpoint", "registration_endpoint"} {
		if metadata[field] == nil {
			t.Fatalf("metadata missing %q: %#v", field, metadata)
		}
	}
	if got := metadata["client_id_metadata_document_supported"]; got != false {
		t.Fatalf("CIMD should be explicitly disabled, got %#v", got)
	}

	resourceReq := httptest.NewRequest(http.MethodGet, "/.well-known/oauth-protected-resource", nil)
	resourceRecorder := httptest.NewRecorder()
	provider.ServeHTTP(resourceRecorder, resourceReq)
	if resourceRecorder.Code != http.StatusOK || !strings.Contains(resourceRecorder.Body.String(), "authorization_servers") {
		t.Fatalf("unexpected protected resource metadata: %d %s", resourceRecorder.Code, resourceRecorder.Body.String())
	}
}

func TestDCRAuthorizationPKCEAndTokenExchange(t *testing.T) {
	provider := testProvider(t)
	clientID := registerTestClient(t, provider, "https://client.example/callback")
	verifier := "a-long-pkce-verifier-for-the-mcp-connector-1234567890"
	challenge := pkceChallenge(verifier)
	state := "state-123"
	values := url.Values{
		"client_id":             {clientID},
		"redirect_uri":          {"https://client.example/callback"},
		"response_type":         {"code"},
		"scope":                 {"read"},
		"resource":              {"https://mcp.example.com"},
		"state":                 {state},
		"code_challenge":        {challenge},
		"code_challenge_method": {"S256"},
	}

	loginRequest := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+values.Encode(), nil)
	loginRecorder := httptest.NewRecorder()
	provider.ServeHTTP(loginRecorder, loginRequest)
	if loginRecorder.Code != http.StatusOK || !strings.Contains(loginRecorder.Body.String(), "name=\"username\"") {
		t.Fatalf("expected login form, got %d %s", loginRecorder.Code, loginRecorder.Body.String())
	}

	form := url.Values{}
	for key, items := range values {
		for _, item := range items {
			form.Add(key, item)
		}
	}
	form.Set("username", "chatgpt")
	form.Set("password", "correct-horse-battery-staple")
	form.Set("consent", "allow")
	authorizeRequest := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(form.Encode()))
	authorizeRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	authorizeRecorder := httptest.NewRecorder()
	provider.ServeHTTP(authorizeRecorder, authorizeRequest)
	if authorizeRecorder.Code != http.StatusFound {
		t.Fatalf("authorization status = %d, body = %s", authorizeRecorder.Code, authorizeRecorder.Body.String())
	}
	location, err := url.Parse(authorizeRecorder.Header().Get("Location"))
	if err != nil {
		t.Fatal(err)
	}
	code := location.Query().Get("code")
	if code == "" || location.Query().Get("state") != state {
		t.Fatalf("authorization redirect did not contain code/state: %s", location)
	}

	tokenForm := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"authorization_code"},
		"code":          {code},
		"redirect_uri":  {"https://client.example/callback"},
		"code_verifier": {verifier},
	}
	tokenRequest := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
	tokenRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	tokenRecorder := httptest.NewRecorder()
	provider.ServeHTTP(tokenRecorder, tokenRequest)
	if tokenRecorder.Code != http.StatusOK {
		t.Fatalf("token status = %d, body = %s", tokenRecorder.Code, tokenRecorder.Body.String())
	}
	var tokenResponse map[string]any
	if err := json.Unmarshal(tokenRecorder.Body.Bytes(), &tokenResponse); err != nil {
		t.Fatal(err)
	}
	access, _ := tokenResponse["access_token"].(string)
	refresh, _ := tokenResponse["refresh_token"].(string)
	if access == "" || refresh == "" {
		t.Fatalf("token response missing access/refresh token: %#v", tokenResponse)
	}
	principal, ok := provider.AuthenticateHeader("Bearer " + access)
	if !ok || principal.Name != "reader" || !principal.HasScope("read") || !principal.CanUseConnection("default") {
		t.Fatalf("access token did not produce expected principal: %#v %v", principal, ok)
	}

	refreshForm := url.Values{
		"client_id":     {clientID},
		"grant_type":    {"refresh_token"},
		"refresh_token": {refresh},
	}
	refreshRequest := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(refreshForm.Encode()))
	refreshRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	refreshRecorder := httptest.NewRecorder()
	provider.ServeHTTP(refreshRecorder, refreshRequest)
	if refreshRecorder.Code != http.StatusOK {
		t.Fatalf("refresh status = %d, body = %s", refreshRecorder.Code, refreshRecorder.Body.String())
	}

	// Authorization codes are single-use and PKCE is mandatory.
	reuseRequest := httptest.NewRequest(http.MethodPost, "/oauth/token", strings.NewReader(tokenForm.Encode()))
	reuseRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reuseRecorder := httptest.NewRecorder()
	provider.ServeHTTP(reuseRecorder, reuseRequest)
	if reuseRecorder.Code != http.StatusBadRequest || !strings.Contains(reuseRecorder.Body.String(), "invalid_grant") {
		t.Fatalf("expected single-use code rejection, got %d %s", reuseRecorder.Code, reuseRecorder.Body.String())
	}
}

func TestStaticChatGPTClientAcceptsOnlyOfficialCallbackFamily(t *testing.T) {
	provider := testProvider(t)
	valid := url.Values{
		"client_id":             {"felsen-chatgpt"},
		"redirect_uri":          {"https://chatgpt.com/connector/oauth/connector-123"},
		"response_type":         {"code"},
		"scope":                 {"read"},
		"code_challenge":        {"challenge"},
		"code_challenge_method": {"S256"},
	}
	request := httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+valid.Encode(), nil)
	recorder := httptest.NewRecorder()
	provider.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("expected official ChatGPT callback to be accepted, got %d", recorder.Code)
	}
	valid.Set("redirect_uri", "https://attacker.example/callback")
	request = httptest.NewRequest(http.MethodGet, "/oauth/authorize?"+valid.Encode(), nil)
	recorder = httptest.NewRecorder()
	provider.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("expected unregistered callback to be rejected, got %d", recorder.Code)
	}
}

func TestDCRClientStoreSurvivesProviderRestart(t *testing.T) {
	manager, err := authn.NewManager(config.AuthConfig{APIKeys: []config.APIKeyConfig{{
		Name: "reader", Token: "api-token", Scopes: []string{"read"}, Connections: []string{"default"},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	baseConfig := config.OAuthConfig{
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
		ClientStorePath:      filepath.Join(t.TempDir(), "oauth-clients.json"),
	}
	first, err := New(baseConfig, manager)
	if err != nil {
		t.Fatal(err)
	}
	clientID := registerTestClient(t, first, "https://client.example/callback")
	secondClientID := registerTestClient(t, first, "https://client.example/second-callback")
	second, err := New(baseConfig, manager)
	if err != nil {
		t.Fatal(err)
	}
	if client, ok := second.client(clientID); !ok || client.RedirectURIs[0] != "https://client.example/callback" {
		t.Fatalf("DCR client was not restored: %#v %v", client, ok)
	}
	if client, ok := second.client(secondClientID); !ok || client.RedirectURIs[0] != "https://client.example/second-callback" {
		t.Fatalf("second DCR client was not restored: %#v %v", client, ok)
	}
}

func registerTestClient(t *testing.T, provider *Provider, redirectURI string) string {
	t.Helper()
	body := `{"redirect_uris":["` + redirectURI + `"],"client_name":"test","token_endpoint_auth_method":"none"}`
	request := httptest.NewRequest(http.MethodPost, "/oauth/register", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	provider.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusCreated {
		t.Fatalf("DCR status = %d, body = %s", recorder.Code, recorder.Body.String())
	}
	var response struct {
		ClientID string `json:"client_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.ClientID == "" {
		t.Fatal("DCR response did not return a client ID")
	}
	return response.ClientID
}

func pkceChallenge(verifier string) string {
	sum := sha256.Sum256([]byte(verifier))
	return base64.RawURLEncoding.EncodeToString(sum[:])
}

func TestOAuthErrorsDoNotLeakCredentials(t *testing.T) {
	provider := testProvider(t)
	request := httptest.NewRequest(http.MethodPost, "/oauth/authorize", strings.NewReader(url.Values{
		"client_id":             {"felsen-chatgpt"},
		"redirect_uri":          {"https://chatgpt.com/connector/oauth/connector-123"},
		"response_type":         {"code"},
		"code_challenge":        {"challenge"},
		"code_challenge_method": {"S256"},
		"username":              {"wrong"},
		"password":              {"secret-password"},
	}.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	recorder := httptest.NewRecorder()
	provider.ServeHTTP(recorder, request)
	body, _ := io.ReadAll(recorder.Result().Body)
	if strings.Contains(string(body), "secret-password") {
		t.Fatal("OAuth error response leaked the submitted password")
	}
}
