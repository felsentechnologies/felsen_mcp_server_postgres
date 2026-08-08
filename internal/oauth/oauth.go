package oauth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/authn"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

const (
	accessTokenType  = "access_token"
	refreshTokenType = "refresh_token"
)

var supportedScopes = map[string]bool{
	"read":  true,
	"write": true,
	"ddl":   true,
	"admin": true,
}

// Provider is a small OAuth 2.1 authorization server for the MCP endpoint.
// It is deliberately isolated from the MCP protocol so its discovery and
// token endpoints can remain public while the MCP endpoint stays protected.
type Provider struct {
	cfg        config.OAuthConfig
	principals *authn.Manager
	mu         sync.RWMutex
	clients    map[string]registeredClient
	codes      map[string]authorizationCode
	accessTTL  time.Duration
	refreshTTL time.Duration
	codeTTL    time.Duration
}

type registeredClient struct {
	ClientID                string   `json:"client_id"`
	ClientName              string   `json:"client_name,omitempty"`
	RedirectURIs            []string `json:"redirect_uris"`
	TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
	GrantTypes              []string `json:"grant_types"`
	ResponseTypes           []string `json:"response_types"`
	Scopes                  []string `json:"scopes,omitempty"`
	IssuedAt                int64    `json:"client_id_issued_at,omitempty"`
}

type authorizationCode struct {
	ClientID      string
	RedirectURI   string
	CodeChallenge string
	Scopes        []string
	Principal     string
	ExpiresAt     time.Time
}

type tokenClaims struct {
	Issuer      string   `json:"iss"`
	Subject     string   `json:"sub"`
	ClientID    string   `json:"client_id"`
	Audience    string   `json:"aud"`
	Scope       string   `json:"scope"`
	Connections []string `json:"connections,omitempty"`
	IssuedAt    int64    `json:"iat"`
	ExpiresAt   int64    `json:"exp"`
	Type        string   `json:"typ"`
	ID          string   `json:"jti"`
}

// New creates an enabled OAuth provider. Configuration validation is normally
// performed by config.Load, but the constructor repeats the essential checks
// so direct callers cannot accidentally start an unusable provider.
func New(cfg config.OAuthConfig, principals *authn.Manager) (*Provider, error) {
	if !cfg.Enabled {
		return nil, nil
	}
	if principals == nil {
		return nil, errors.New("OAuth requires an API-key principal manager")
	}
	if strings.TrimSpace(cfg.SigningKey) == "" || len(cfg.SigningKey) < 32 {
		return nil, errors.New("OAuth signing key must contain at least 32 characters")
	}
	if strings.TrimSpace(cfg.Issuer) == "" || strings.TrimSpace(cfg.Resource) == "" {
		return nil, errors.New("OAuth issuer and resource are required")
	}
	accessTTL, err := time.ParseDuration(cfg.AccessTokenTTL)
	if err != nil || accessTTL <= 0 {
		return nil, fmt.Errorf("OAuth access token TTL: %w", durationError(err))
	}
	refreshTTL, err := time.ParseDuration(cfg.RefreshTokenTTL)
	if err != nil || refreshTTL <= 0 {
		return nil, fmt.Errorf("OAuth refresh token TTL: %w", durationError(err))
	}
	codeTTL, err := time.ParseDuration(cfg.AuthorizationCodeTTL)
	if err != nil || codeTTL <= 0 {
		return nil, fmt.Errorf("OAuth authorization code TTL: %w", durationError(err))
	}
	principalName := strings.TrimSpace(cfg.Principal)
	if _, ok := principals.Principal(principalName); !ok {
		return nil, fmt.Errorf("OAuth principal %q is not configured", principalName)
	}
	if strings.TrimSpace(cfg.Username) == "" || cfg.Password == "" {
		return nil, errors.New("OAuth username and password are required")
	}
	if strings.TrimSpace(cfg.ClientID) == "" {
		return nil, errors.New("OAuth client ID is required")
	}
	if len(cfg.RedirectURIs) == 0 {
		return nil, errors.New("OAuth callback URL is required in configuration")
	}

	p := &Provider{
		cfg:        cfg,
		principals: principals,
		clients:    map[string]registeredClient{},
		codes:      map[string]authorizationCode{},
		accessTTL:  accessTTL,
		refreshTTL: refreshTTL,
		codeTTL:    codeTTL,
	}
	if err := p.loadClients(); err != nil {
		return nil, err
	}
	// The configured client ID supports the ChatGPT "User-Defined OAuth
	// Client" option. DCR-created clients are kept alongside it.
	p.clients[cfg.ClientID] = registeredClient{
		ClientID:                cfg.ClientID,
		ClientName:              "Configured ChatGPT client",
		RedirectURIs:            append([]string(nil), cfg.RedirectURIs...),
		TokenEndpointAuthMethod: "none",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		ResponseTypes:           []string{"code"},
	}
	return p, nil
}

// ServeHTTP serves OAuth metadata and protocol endpoints. Mount the listed
// paths publicly in the application mux; the MCP endpoint itself remains
// behind the normal bearer middleware.
func (p *Provider) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if p == nil {
		http.NotFound(w, r)
		return
	}
	switch strings.TrimRight(r.URL.Path, "/") {
	case "/.well-known/oauth-protected-resource":
		p.protectedResourceMetadata(w, r)
	case "/.well-known/oauth-protected-resource/mcp", "/mcp/.well-known/oauth-protected-resource":
		p.protectedResourceMetadata(w, r)
	case "/.well-known/oauth-authorization-server":
		p.authorizationServerMetadata(w, r)
	case "/oauth/register":
		p.register(w, r)
	case "/oauth/authorize":
		p.authorize(w, r)
	case "/oauth/token":
		p.token(w, r)
	default:
		http.NotFound(w, r)
	}
}

// Paths returns every public endpoint that must be mounted in net/http.
func (p *Provider) Paths() []string {
	return []string{
		"/.well-known/oauth-protected-resource",
		"/.well-known/oauth-protected-resource/",
		"/.well-known/oauth-protected-resource/mcp",
		"/mcp/.well-known/oauth-protected-resource",
		"/mcp/.well-known/oauth-protected-resource/",
		"/.well-known/oauth-authorization-server",
		"/.well-known/oauth-authorization-server/",
		"/oauth/register",
		"/oauth/authorize",
		"/oauth/token",
	}
}

// Challenge returns the RFC 9728 resource metadata challenge used on 401
// responses from the MCP endpoint.
func (p *Provider) Challenge() string {
	return fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, strings.TrimRight(p.cfg.Issuer, "/"))
}

func (p *Provider) protectedResourceMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, _ := p.principals.Principal(p.cfg.Principal)
	writeJSON(w, http.StatusOK, map[string]any{
		"resource":               p.cfg.Resource,
		"authorization_servers":  []string{p.cfg.Issuer},
		"scopes_supported":       p.scopesSupported(principal),
		"resource_documentation": "MCP Postgres tools protected by OAuth 2.1 bearer tokens.",
	})
}

func (p *Provider) authorizationServerMetadata(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	principal, _ := p.principals.Principal(p.cfg.Principal)
	issuer := strings.TrimRight(p.cfg.Issuer, "/")
	writeJSON(w, http.StatusOK, map[string]any{
		"issuer":                                p.cfg.Issuer,
		"authorization_endpoint":                issuer + "/oauth/authorize",
		"token_endpoint":                        issuer + "/oauth/token",
		"registration_endpoint":                 issuer + "/oauth/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"token_endpoint_auth_methods_supported": []string{"none"},
		"code_challenge_methods_supported":      []string{"S256"},
		"scopes_supported":                      p.scopesSupported(principal),
		"client_id_metadata_document_supported": false,
	})
}

func (p *Provider) register(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	var input struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		Scope                   string   `json:"scope"`
	}
	decoder := json.NewDecoder(r.Body)
	if err := decoder.Decode(&input); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "registration body must be valid JSON")
		return
	}
	if len(input.RedirectURIs) == 0 {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "redirect_uris is required")
		return
	}
	for _, redirectURI := range input.RedirectURIs {
		if err := validateRedirectURI(redirectURI); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
			return
		}
	}
	method := strings.TrimSpace(input.TokenEndpointAuthMethod)
	if method == "" {
		method = "none"
	}
	if method != "none" {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "only token_endpoint_auth_method=none is supported")
		return
	}
	grantTypes := normalize(input.GrantTypes)
	if len(grantTypes) == 0 {
		grantTypes = []string{"authorization_code", "refresh_token"}
	}
	for _, grantType := range grantTypes {
		if grantType != "authorization_code" && grantType != "refresh_token" {
			oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "only authorization_code and refresh_token grants are supported")
			return
		}
	}
	if !contains(grantTypes, "authorization_code") {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "authorization_code grant is required")
		return
	}
	responseTypes := normalize(input.ResponseTypes)
	if len(responseTypes) == 0 {
		responseTypes = []string{"code"}
	}
	if len(responseTypes) != 1 || responseTypes[0] != "code" {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", "only response_type=code is supported")
		return
	}
	principal, _ := p.principals.Principal(p.cfg.Principal)
	scopes := parseScopes(input.Scope)
	if len(scopes) == 0 {
		scopes = p.defaultScopes()
	}
	if err := p.validateScopes(scopes, principal); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_client_metadata", err.Error())
		return
	}
	clientID, err := randomToken(24)
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not create client ID")
		return
	}
	client := registeredClient{
		ClientID:                "mcp_" + clientID,
		ClientName:              strings.TrimSpace(input.ClientName),
		RedirectURIs:            append([]string(nil), input.RedirectURIs...),
		TokenEndpointAuthMethod: method,
		GrantTypes:              grantTypes,
		ResponseTypes:           responseTypes,
		Scopes:                  scopes,
		IssuedAt:                time.Now().Unix(),
	}
	p.mu.Lock()
	p.clients[client.ClientID] = client
	err = p.saveClientsLocked()
	p.mu.Unlock()
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not persist client registration")
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{
		"client_id":                  client.ClientID,
		"client_id_issued_at":        client.IssuedAt,
		"redirect_uris":              client.RedirectURIs,
		"client_name":                client.ClientName,
		"token_endpoint_auth_method": client.TokenEndpointAuthMethod,
		"grant_types":                client.GrantTypes,
		"response_types":             client.ResponseTypes,
		"scope":                      strings.Join(client.Scopes, " "),
	})
}

type authorizationRequest struct {
	ClientID      string
	RedirectURI   string
	State         string
	Scopes        []string
	CodeChallenge string
}

func (p *Provider) authorize(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		w.Header().Set("Allow", "GET, POST")
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method == http.MethodPost {
		r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
		if err := r.ParseForm(); err != nil {
			oauthError(w, http.StatusBadRequest, "invalid_request", "authorization form is invalid")
			return
		}
	}
	values := r.URL.Query()
	if r.Method == http.MethodPost {
		values = r.Form
	}
	request, err := p.validateAuthorizationRequest(values)
	if err != nil {
		if request.ClientID == "" || request.RedirectURI == "" || !p.redirectAllowed(request.ClientID, request.RedirectURI) {
			oauthError(w, http.StatusBadRequest, "invalid_request", err.Error())
			return
		}
		redirectOAuthError(w, request.RedirectURI, request.State, "invalid_request", err.Error())
		return
	}
	if r.Method == http.MethodGet {
		p.renderLogin(w, request, "")
		return
	}
	username := r.FormValue("username")
	password := r.FormValue("password")
	if subtle.ConstantTimeCompare([]byte(username), []byte(p.cfg.Username)) != 1 ||
		subtle.ConstantTimeCompare([]byte(password), []byte(p.cfg.Password)) != 1 {
		p.renderLogin(w, request, "Invalid username or password.")
		return
	}
	if !isApproved(r.FormValue("consent")) {
		redirectOAuthError(w, request.RedirectURI, request.State, "access_denied", "user denied the authorization request")
		return
	}
	code, err := randomToken(32)
	if err != nil {
		http.Error(w, "could not create authorization code", http.StatusInternalServerError)
		return
	}
	p.mu.Lock()
	p.cleanupCodesLocked(time.Now())
	p.codes[code] = authorizationCode{
		ClientID:      request.ClientID,
		RedirectURI:   request.RedirectURI,
		CodeChallenge: request.CodeChallenge,
		Scopes:        append([]string(nil), request.Scopes...),
		Principal:     p.cfg.Principal,
		ExpiresAt:     time.Now().Add(p.codeTTL),
	}
	p.mu.Unlock()
	parsed, _ := url.Parse(request.RedirectURI)
	query := parsed.Query()
	query.Set("code", code)
	if request.State != "" {
		query.Set("state", request.State)
	}
	parsed.RawQuery = query.Encode()
	http.Redirect(w, r, parsed.String(), http.StatusFound)
}

func (p *Provider) validateAuthorizationRequest(values url.Values) (authorizationRequest, error) {
	request := authorizationRequest{
		ClientID:      strings.TrimSpace(values.Get("client_id")),
		RedirectURI:   strings.TrimSpace(values.Get("redirect_uri")),
		State:         values.Get("state"),
		CodeChallenge: strings.TrimSpace(values.Get("code_challenge")),
	}
	if request.ClientID == "" {
		return request, errors.New("client_id is required")
	}
	client, ok := p.client(request.ClientID)
	if !ok {
		return request, errors.New("unknown client_id")
	}
	if request.RedirectURI == "" {
		return request, errors.New("redirect_uri is required")
	}
	if err := validateRedirectURI(request.RedirectURI); err != nil {
		return request, err
	}
	if !p.redirectAllowedForClient(client, request.RedirectURI) {
		return request, errors.New("redirect_uri is not registered for this client")
	}
	if values.Get("response_type") != "code" {
		return request, errors.New("response_type=code is required")
	}
	if request.CodeChallenge == "" || values.Get("code_challenge_method") != "S256" {
		return request, errors.New("PKCE S256 code_challenge is required")
	}
	if resource := strings.TrimSpace(values.Get("resource")); resource != "" && resource != p.cfg.Resource {
		return request, errors.New("resource is not this MCP server")
	}
	request.Scopes = parseScopes(values.Get("scope"))
	if len(request.Scopes) == 0 {
		request.Scopes = p.defaultScopes()
	}
	principal, ok := p.principals.Principal(p.cfg.Principal)
	if !ok {
		return request, errors.New("OAuth principal is no longer configured")
	}
	if len(client.Scopes) > 0 {
		for _, scope := range request.Scopes {
			if !contains(client.Scopes, scope) {
				return request, fmt.Errorf("scope %q was not registered for this client", scope)
			}
		}
	}
	if err := p.validateScopes(request.Scopes, principal); err != nil {
		return request, err
	}
	return request, nil
}

func (p *Provider) renderLogin(w http.ResponseWriter, request authorizationRequest, message string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	form := []string{
		`<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>Authorize Postgres MCP</title>`,
		`<style>body{font-family:system-ui,sans-serif;max-width:30rem;margin:4rem auto;padding:0 1rem;color:#202124}label{display:block;margin:.8rem 0 .25rem}input{box-sizing:border-box;width:100%;padding:.65rem;border:1px solid #999;border-radius:.35rem}button{margin-top:1.2rem;padding:.7rem 1rem;border:0;border-radius:.35rem;background:#0b57d0;color:white;font-weight:600}.error{color:#b3261e}.scopes{background:#f1f3f4;border-radius:.35rem;padding:.7rem}</style></head><body>`,
		`<h1>Authorize Postgres MCP</h1><p>Sign in to allow ChatGPT to use the selected database tools.</p>`,
	}
	if message != "" {
		form = append(form, `<p class="error">`+template.HTMLEscapeString(message)+`</p>`)
	}
	form = append(form,
		`<div class="scopes"><strong>Requested scopes:</strong> `+template.HTMLEscapeString(strings.Join(request.Scopes, ", "))+`</div>`,
		`<form method="post" action="/oauth/authorize">`,
		hiddenInput("client_id", request.ClientID),
		hiddenInput("redirect_uri", request.RedirectURI),
		hiddenInput("response_type", "code"),
		hiddenInput("scope", strings.Join(request.Scopes, " ")),
		hiddenInput("state", request.State),
		hiddenInput("code_challenge", request.CodeChallenge),
		hiddenInput("code_challenge_method", "S256"),
		`<label for="username">Username</label><input id="username" name="username" autocomplete="username" required>`,
		`<label for="password">Password</label><input id="password" name="password" type="password" autocomplete="current-password" required>`,
		`<button type="submit" name="consent" value="allow">Authorize</button></form></body></html>`,
	)
	_, _ = w.Write([]byte(strings.Join(form, "")))
}

func hiddenInput(name, value string) string {
	return `<input type="hidden" name="` + template.HTMLEscapeString(name) + `" value="` + template.HTMLEscapeString(value) + `">`
}

func (p *Provider) token(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32*1024)
	if err := r.ParseForm(); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_request", "token form is invalid")
		return
	}
	if r.Header.Get("Authorization") != "" {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "this server requires token_endpoint_auth_method=none")
		return
	}
	clientID := strings.TrimSpace(r.FormValue("client_id"))
	client, ok := p.client(clientID)
	if !ok || client.TokenEndpointAuthMethod != "none" {
		oauthError(w, http.StatusUnauthorized, "invalid_client", "unknown or unsupported client")
		return
	}
	if resource := strings.TrimSpace(r.FormValue("resource")); resource != "" && resource != p.cfg.Resource {
		oauthError(w, http.StatusBadRequest, "invalid_target", "resource is not this MCP server")
		return
	}
	switch r.FormValue("grant_type") {
	case "authorization_code":
		p.exchangeAuthorizationCode(w, r, client)
	case "refresh_token":
		p.exchangeRefreshToken(w, r, client)
	default:
		oauthError(w, http.StatusBadRequest, "unsupported_grant_type", "only authorization_code and refresh_token are supported")
	}
}

func (p *Provider) exchangeAuthorizationCode(w http.ResponseWriter, r *http.Request, client registeredClient) {
	codeValue := strings.TrimSpace(r.FormValue("code"))
	if codeValue == "" {
		oauthError(w, http.StatusBadRequest, "invalid_request", "code is required")
		return
	}
	p.mu.Lock()
	code, ok := p.codes[codeValue]
	if ok {
		delete(p.codes, codeValue)
	}
	p.mu.Unlock()
	if !ok || code.ExpiresAt.Before(time.Now()) || code.ClientID != client.ClientID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "authorization code is invalid or expired")
		return
	}
	if redirectURI := strings.TrimSpace(r.FormValue("redirect_uri")); redirectURI != "" && redirectURI != code.RedirectURI {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "redirect_uri does not match the authorization request")
		return
	}
	verifier := strings.TrimSpace(r.FormValue("code_verifier"))
	if !verifyPKCE(verifier, code.CodeChallenge) {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "code_verifier does not satisfy PKCE")
		return
	}
	p.issueTokenResponse(w, client.ClientID, code.Principal, code.Scopes)
}

func (p *Provider) exchangeRefreshToken(w http.ResponseWriter, r *http.Request, client registeredClient) {
	claims, ok := p.verifyToken(strings.TrimSpace(r.FormValue("refresh_token")), refreshTokenType)
	if !ok || claims.Subject == "" || claims.Audience != p.cfg.Resource {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token is invalid or expired")
		return
	}
	if claims.Subject != p.cfg.Principal {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token principal is invalid")
		return
	}
	if claims.ClientID != client.ClientID {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "refresh token was issued to a different client")
		return
	}
	p.issueTokenResponse(w, client.ClientID, claims.Subject, parseScopes(claims.Scope))
}

func (p *Provider) issueTokenResponse(w http.ResponseWriter, clientID, principalName string, scopes []string) {
	principal, ok := p.principals.Principal(principalName)
	if !ok {
		oauthError(w, http.StatusBadRequest, "invalid_grant", "OAuth principal is no longer configured")
		return
	}
	if err := p.validateScopes(scopes, principal); err != nil {
		oauthError(w, http.StatusBadRequest, "invalid_grant", err.Error())
		return
	}
	now := time.Now()
	access, err := p.signToken(tokenClaims{
		Issuer:      p.cfg.Issuer,
		Subject:     principalName,
		ClientID:    clientID,
		Audience:    p.cfg.Resource,
		Scope:       strings.Join(scopes, " "),
		Connections: principalConnectionNames(principal),
		IssuedAt:    now.Unix(),
		ExpiresAt:   now.Add(p.accessTTL).Unix(),
		Type:        accessTokenType,
		ID:          mustRandomToken(16),
	})
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not issue access token")
		return
	}
	refresh, err := p.signToken(tokenClaims{
		Issuer:      p.cfg.Issuer,
		Subject:     principalName,
		ClientID:    clientID,
		Audience:    p.cfg.Resource,
		Scope:       strings.Join(scopes, " "),
		Connections: principalConnectionNames(principal),
		IssuedAt:    now.Unix(),
		ExpiresAt:   now.Add(p.refreshTTL).Unix(),
		Type:        refreshTokenType,
		ID:          mustRandomToken(16),
	})
	if err != nil {
		oauthError(w, http.StatusInternalServerError, "server_error", "could not issue refresh token")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int64(p.accessTTL / time.Second),
		"refresh_token": refresh,
		"scope":         strings.Join(scopes, " "),
	})
}

// AuthenticateHeader lets the main server compose OAuth access tokens with
// the existing API-key authenticator.
func (p *Provider) AuthenticateHeader(header string) (authn.Principal, bool) {
	token := strings.TrimSpace(header)
	if len(token) < len("Bearer ") || !strings.EqualFold(token[:len("Bearer ")], "Bearer ") {
		return authn.Principal{}, false
	}
	claims, ok := p.verifyToken(strings.TrimSpace(token[len("Bearer "):]), accessTokenType)
	if !ok || claims.Audience != p.cfg.Resource {
		return authn.Principal{}, false
	}
	base, ok := p.principals.Principal(claims.Subject)
	if !ok {
		return authn.Principal{}, false
	}
	scopes := parseScopes(claims.Scope)
	if p.validateScopes(scopes, base) != nil {
		return authn.Principal{}, false
	}
	connections := map[string]bool{}
	for _, connection := range claims.Connections {
		if connection == "*" || base.CanUseConnection(connection) {
			connections[connection] = true
		}
	}
	if len(connections) == 0 && base.Connections["*"] {
		connections["*"] = true
	}
	return authn.Principal{Name: base.Name, Scopes: scopeMap(scopes), Connections: connections}, true
}

func (p *Provider) client(clientID string) (registeredClient, bool) {
	p.mu.RLock()
	defer p.mu.RUnlock()
	client, ok := p.clients[clientID]
	return client, ok
}

func (p *Provider) redirectAllowed(clientID, redirectURI string) bool {
	client, ok := p.client(clientID)
	return ok && p.redirectAllowedForClient(client, redirectURI)
}

func (p *Provider) redirectAllowedForClient(client registeredClient, redirectURI string) bool {
	for _, allowed := range client.RedirectURIs {
		if allowed == redirectURI {
			return true
		}
	}
	return false
}

func (p *Provider) scopesSupported(principal authn.Principal) []string {
	scopes := []string{}
	for scope := range supportedScopes {
		if principal.HasScope(scope) {
			scopes = append(scopes, scope)
		}
	}
	sort.Strings(scopes)
	return scopes
}

func (p *Provider) defaultScopes() []string {
	return normalize(append(append([]string(nil), p.cfg.BaseScopes...), p.cfg.DefaultScopes...))
}

func (p *Provider) validateScopes(scopes []string, principal authn.Principal) error {
	if len(scopes) == 0 {
		return errors.New("at least one OAuth scope is required")
	}
	for _, scope := range scopes {
		if !supportedScopes[scope] {
			return fmt.Errorf("unsupported OAuth scope %q", scope)
		}
		if !principal.HasScope(scope) {
			return fmt.Errorf("OAuth principal is not allowed the %q scope", scope)
		}
	}
	return nil
}

func principalConnectionNames(principal authn.Principal) []string {
	connections := make([]string, 0, len(principal.Connections))
	for connection, allowed := range principal.Connections {
		if allowed {
			connections = append(connections, connection)
		}
	}
	sort.Strings(connections)
	return connections
}

func (p *Provider) signToken(claims tokenClaims) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	encodedHeader := base64.RawURLEncoding.EncodeToString(header)
	encodedPayload := base64.RawURLEncoding.EncodeToString(payload)
	message := encodedHeader + "." + encodedPayload
	mac := hmac.New(sha256.New, []byte(p.cfg.SigningKey))
	_, _ = mac.Write([]byte(message))
	return message + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil)), nil
}

func (p *Provider) verifyToken(value, expectedType string) (tokenClaims, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 3 {
		return tokenClaims{}, false
	}
	mac := hmac.New(sha256.New, []byte(p.cfg.SigningKey))
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil || subtle.ConstantTimeCompare(signature, mac.Sum(nil)) != 1 {
		return tokenClaims{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return tokenClaims{}, false
	}
	var claims tokenClaims
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Type != expectedType ||
		claims.Issuer != p.cfg.Issuer || claims.ExpiresAt <= time.Now().Unix() {
		return tokenClaims{}, false
	}
	return claims, true
}

func verifyPKCE(verifier, challenge string) bool {
	if verifier == "" || challenge == "" {
		return false
	}
	sum := sha256.Sum256([]byte(verifier))
	expected := base64.RawURLEncoding.EncodeToString(sum[:])
	return subtle.ConstantTimeCompare([]byte(expected), []byte(challenge)) == 1
}

func (p *Provider) cleanupCodesLocked(now time.Time) {
	for value, code := range p.codes {
		if code.ExpiresAt.Before(now) {
			delete(p.codes, value)
		}
	}
}

func (p *Provider) loadClients() error {
	path := strings.TrimSpace(p.cfg.ClientStorePath)
	if path == "" {
		return nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read OAuth client store: %w", err)
	}
	var clients []registeredClient
	if err := json.Unmarshal(data, &clients); err != nil {
		return fmt.Errorf("decode OAuth client store: %w", err)
	}
	for _, client := range clients {
		if client.ClientID == "" || client.TokenEndpointAuthMethod != "none" {
			return errors.New("OAuth client store contains an invalid client")
		}
		p.clients[client.ClientID] = client
	}
	return nil
}

func (p *Provider) saveClientsLocked() error {
	path := strings.TrimSpace(p.cfg.ClientStorePath)
	if path == "" {
		return nil
	}
	clients := make([]registeredClient, 0, len(p.clients))
	for _, client := range p.clients {
		if client.ClientID == p.cfg.ClientID {
			continue
		}
		clients = append(clients, client)
	}
	sort.Slice(clients, func(i, j int) bool { return clients[i].ClientID < clients[j].ClientID })
	data, err := json.MarshalIndent(clients, "", "  ")
	if err != nil {
		return err
	}
	directory := filepath.Dir(path)
	if directory != "." {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return err
		}
	}
	tempPath := path + ".tmp"
	if err := os.WriteFile(tempPath, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func randomToken(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func mustRandomToken(size int) string {
	value, err := randomToken(size)
	if err != nil {
		return "unavailable"
	}
	return value
}

func validateRedirectURI(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" || parsed.User != nil ||
		parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLocalhost(parsed.Hostname()))) {
		return errors.New("redirect URI must be an absolute HTTPS URL (HTTP is allowed only for localhost) without credentials, query, or fragment")
	}
	return nil
}

func isLocalhost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "127.")
}

func parseScopes(value string) []string {
	return normalize(strings.FieldsFunc(value, func(r rune) bool { return r == ' ' || r == ',' || r == '\n' || r == '\r' || r == '\t' }))
}

func normalize(values []string) []string {
	result := make([]string, 0, len(values))
	seen := map[string]bool{}
	for _, value := range values {
		value = strings.ToLower(strings.TrimSpace(value))
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		result = append(result, value)
	}
	return result
}

func contains(values []string, wanted string) bool {
	for _, value := range values {
		if value == wanted {
			return true
		}
	}
	return false
}

func scopeMap(scopes []string) map[string]bool {
	result := map[string]bool{}
	for _, scope := range scopes {
		result[scope] = true
	}
	return result
}

func isApproved(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "allow", "approve", "approved", "yes", "true", "1":
		return true
	default:
		return false
	}
}

func redirectOAuthError(w http.ResponseWriter, redirectURI, state, code, description string) {
	parsed, err := url.Parse(redirectURI)
	if err != nil {
		oauthError(w, http.StatusBadRequest, code, description)
		return
	}
	query := parsed.Query()
	query.Set("error", code)
	query.Set("error_description", description)
	if state != "" {
		query.Set("state", state)
	}
	parsed.RawQuery = query.Encode()
	w.Header().Set("Location", parsed.String())
	w.WriteHeader(http.StatusFound)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func oauthError(w http.ResponseWriter, status int, code, description string) {
	writeJSON(w, status, map[string]string{"error": code, "error_description": description})
}

func durationError(err error) error {
	if err != nil {
		return err
	}
	return errors.New("must be positive")
}
