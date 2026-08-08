package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath  = "configs/example.yaml"
	defaultEndpoint    = "/mcp"
	defaultHost        = "127.0.0.1"
	defaultPort        = 8080
	defaultMaxRows     = 100
	defaultMaxBody     = 1 << 20
	defaultMaxSearch   = 100
	defaultMaxConns    = 8
	defaultMaxAffected = 100
	defaultCitationTTL = "15m"
)

type Config struct {
	Server      ServerConfig                `json:"server" yaml:"server"`
	Auth        AuthConfig                  `json:"auth" yaml:"auth"`
	OAuth       OAuthConfig                 `json:"oauth" yaml:"oauth"`
	Connections map[string]ConnectionConfig `json:"connections" yaml:"connections"`
	Audit       AuditConfig                 `json:"audit" yaml:"audit"`
}

type ServerConfig struct {
	Host                  string `json:"host" yaml:"host"`
	Port                  int    `json:"port" yaml:"port"`
	Endpoint              string `json:"endpoint" yaml:"endpoint"`
	PublicBaseURL         string `json:"public_base_url" yaml:"public_base_url"`
	CitationSigningKey    string `json:"citation_signing_key" yaml:"citation_signing_key"`
	CitationSigningKeyEnv string `json:"citation_signing_key_env" yaml:"citation_signing_key_env"`
	CitationTTL           string `json:"citation_ttl" yaml:"citation_ttl"`
	QueryTimeout          string `json:"query_timeout" yaml:"query_timeout"`
	MaxRows               int    `json:"max_rows" yaml:"max_rows"`
	MaxSearchResults      int    `json:"max_search_results" yaml:"max_search_results"`
	MaxBodyBytes          int64  `json:"max_body_bytes" yaml:"max_body_bytes"`
	MaxConcurrent         int    `json:"max_concurrent_requests" yaml:"max_concurrent_requests"`
	JSONResponse          bool   `json:"json_response" yaml:"json_response"`
	Stateless             bool   `json:"stateless" yaml:"stateless"`
	SessionTimeout        string `json:"session_timeout" yaml:"session_timeout"`
	ReadHeaderTimeout     string `json:"read_header_timeout" yaml:"read_header_timeout"`
	ReadTimeout           string `json:"read_timeout" yaml:"read_timeout"`
	WriteTimeout          string `json:"write_timeout" yaml:"write_timeout"`
	IdleTimeout           string `json:"idle_timeout" yaml:"idle_timeout"`
}

type AuthConfig struct {
	APIKeys []APIKeyConfig `json:"api_keys" yaml:"api_keys"`
}

// OAuthConfig configures the embedded OAuth 2.1 authorization server used by
// ChatGPT and other MCP clients. The embedded provider is intentionally small
// and is suitable as a bootstrap identity provider; organizations can later
// point the MCP server at an external IdP without changing the MCP tools.
type OAuthConfig struct {
	Enabled              bool     `json:"enabled" yaml:"enabled"`
	Issuer               string   `json:"issuer" yaml:"issuer"`
	Resource             string   `json:"resource" yaml:"resource"`
	SigningKey           string   `json:"signing_key" yaml:"signing_key"`
	SigningKeyEnv        string   `json:"signing_key_env" yaml:"signing_key_env"`
	Username             string   `json:"username" yaml:"username"`
	UsernameEnv          string   `json:"username_env" yaml:"username_env"`
	Password             string   `json:"password" yaml:"password"`
	PasswordEnv          string   `json:"password_env" yaml:"password_env"`
	Principal            string   `json:"principal" yaml:"principal"`
	ClientID             string   `json:"client_id" yaml:"client_id"`
	RedirectURIs         []string `json:"redirect_uris" yaml:"redirect_uris"`
	ClientStorePath      string   `json:"client_store_path" yaml:"client_store_path"`
	ClientStorePathEnv   string   `json:"client_store_path_env" yaml:"client_store_path_env"`
	DefaultScopes        []string `json:"default_scopes" yaml:"default_scopes"`
	BaseScopes           []string `json:"base_scopes" yaml:"base_scopes"`
	AccessTokenTTL       string   `json:"access_token_ttl" yaml:"access_token_ttl"`
	RefreshTokenTTL      string   `json:"refresh_token_ttl" yaml:"refresh_token_ttl"`
	AuthorizationCodeTTL string   `json:"authorization_code_ttl" yaml:"authorization_code_ttl"`
}

type APIKeyConfig struct {
	Name        string   `json:"name" yaml:"name"`
	Token       string   `json:"token" yaml:"token"`
	TokenEnv    string   `json:"token_env" yaml:"token_env"`
	TokenSHA256 string   `json:"token_sha256" yaml:"token_sha256"`
	Scopes      []string `json:"scopes" yaml:"scopes"`
	Connections []string `json:"connections" yaml:"connections"`
}

type ConnectionConfig struct {
	DSN             string        `json:"dsn" yaml:"dsn"`
	DSNEnv          string        `json:"dsn_env" yaml:"dsn_env"`
	Schemas         []string      `json:"schemas" yaml:"schemas"`
	MaxRows         int           `json:"max_rows" yaml:"max_rows"`
	MaxAffectedRows int           `json:"max_affected_rows" yaml:"max_affected_rows"`
	MinConns        int           `json:"min_conns" yaml:"min_conns"`
	MaxConns        int           `json:"max_conns" yaml:"max_conns"`
	QueryTimeout    string        `json:"query_timeout" yaml:"query_timeout"`
	Masking         MaskingConfig `json:"masking" yaml:"masking"`
	DMLPolicies     []DMLPolicy   `json:"dml_policies" yaml:"dml_policies"`
	DDLEnabled      bool          `json:"ddl_enabled" yaml:"ddl_enabled"`
}

type MaskingConfig struct {
	Enabled          *bool    `json:"enabled" yaml:"enabled"`
	SensitiveColumns []string `json:"sensitive_columns" yaml:"sensitive_columns"`
	AllowColumns     []string `json:"allow_columns" yaml:"allow_columns"`
}

type DMLPolicy struct {
	Schema     string   `json:"schema" yaml:"schema"`
	Table      string   `json:"table" yaml:"table"`
	Operations []string `json:"operations" yaml:"operations"`
}

type AuditConfig struct {
	Destination string `json:"destination" yaml:"destination"`
	File        string `json:"file" yaml:"file"`
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = os.Getenv("POSTGRES_MCP_CONFIG")
	}
	if path == "" {
		if _, err := os.Stat(defaultConfigPath); err == nil {
			path = defaultConfigPath
		}
	}
	var cfg Config
	if path == "" {
		cfg = envFallback()
	} else {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		switch strings.ToLower(filepath.Ext(path)) {
		case ".json":
			decoder := json.NewDecoder(bytes.NewReader(data))
			decoder.DisallowUnknownFields()
			err = decoder.Decode(&cfg)
			if err == nil {
				var extra any
				if nextErr := decoder.Decode(&extra); nextErr != io.EOF {
					if nextErr == nil {
						err = errors.New("configuration must contain exactly one JSON document")
					} else {
						err = nextErr
					}
				}
			}
		default:
			decoder := yaml.NewDecoder(bytes.NewReader(data))
			decoder.KnownFields(true)
			err = decoder.Decode(&cfg)
			if err == nil {
				var extra any
				if nextErr := decoder.Decode(&extra); nextErr != io.EOF {
					if nextErr == nil {
						err = errors.New("configuration must contain exactly one YAML document")
					} else {
						err = nextErr
					}
				}
			}
		}
		if err != nil {
			return nil, err
		}
	}
	applyDefaults(&cfg)
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	return &cfg, nil
}

func envFallback() Config {
	dsn := os.Getenv("DATABASE_URL")
	token := os.Getenv("MCP_API_KEY")
	if token == "" {
		token = os.Getenv("POSTGRES_MCP_API_KEY")
	}
	cfg := Config{
		Server: ServerConfig{
			Host:               defaultHost,
			Port:               defaultPort,
			Endpoint:           defaultEndpoint,
			PublicBaseURL:      os.Getenv("MCP_PUBLIC_BASE_URL"),
			CitationSigningKey: os.Getenv("MCP_CITATION_SIGNING_KEY"),
		},
		Connections: map[string]ConnectionConfig{
			"default": {DSN: dsn, Schemas: []string{"public"}},
		},
		Audit: AuditConfig{Destination: "stdout"},
	}
	if token != "" {
		cfg.Auth.APIKeys = []APIKeyConfig{{
			Name:        "default",
			Token:       token,
			Scopes:      []string{"read"},
			Connections: []string{"default"},
		}}
	}
	return cfg
}

func applyDefaults(cfg *Config) {
	if cfg.Server.Host == "" {
		cfg.Server.Host = defaultHost
	}
	if cfg.Server.Port == 0 {
		cfg.Server.Port = defaultPort
	}
	if cfg.Server.Endpoint == "" {
		cfg.Server.Endpoint = defaultEndpoint
	}
	if publicBaseURL := os.Getenv("MCP_PUBLIC_BASE_URL"); publicBaseURL != "" {
		cfg.Server.PublicBaseURL = publicBaseURL
	}
	if cfg.Server.CitationTTL == "" {
		cfg.Server.CitationTTL = defaultCitationTTL
	}
	if cfg.Server.PublicBaseURL == "" && cfg.Server.Host != "0.0.0.0" && cfg.Server.Host != "::" {
		cfg.Server.PublicBaseURL = "http://" + net.JoinHostPort(cfg.Server.Host, fmt.Sprintf("%d", cfg.Server.Port))
	}
	if cfg.Server.MaxRows <= 0 {
		cfg.Server.MaxRows = defaultMaxRows
	}
	if cfg.Server.MaxSearchResults <= 0 {
		cfg.Server.MaxSearchResults = defaultMaxSearch
	}
	if cfg.Server.MaxBodyBytes <= 0 {
		cfg.Server.MaxBodyBytes = defaultMaxBody
	}
	if cfg.Server.MaxConcurrent <= 0 {
		cfg.Server.MaxConcurrent = 64
	}
	if cfg.Server.QueryTimeout == "" {
		cfg.Server.QueryTimeout = "10s"
	}
	if cfg.Server.SessionTimeout == "" {
		cfg.Server.SessionTimeout = "30m"
	}
	if cfg.Server.ReadHeaderTimeout == "" {
		cfg.Server.ReadHeaderTimeout = "10s"
	}
	if cfg.Server.ReadTimeout == "" {
		cfg.Server.ReadTimeout = "60s"
	}
	if cfg.Server.WriteTimeout == "" {
		cfg.Server.WriteTimeout = "60s"
	}
	if cfg.Server.IdleTimeout == "" {
		cfg.Server.IdleTimeout = "120s"
	}
	if cfg.Audit.Destination == "" {
		cfg.Audit.Destination = "stdout"
	}
	for i, key := range cfg.Auth.APIKeys {
		if key.Token == "" && key.TokenEnv != "" {
			key.Token = os.Getenv(key.TokenEnv)
		}
		cfg.Auth.APIKeys[i] = key
	}
	if cfg.Server.CitationSigningKey == "" && cfg.Server.CitationSigningKeyEnv != "" {
		cfg.Server.CitationSigningKey = os.Getenv(cfg.Server.CitationSigningKeyEnv)
	}
	applyOAuthDefaults(cfg)
	for name, c := range cfg.Connections {
		if c.DSN == "" && c.DSNEnv != "" {
			c.DSN = os.Getenv(c.DSNEnv)
		}
		if len(c.Schemas) == 0 {
			c.Schemas = []string{"public"}
		}
		if c.MaxRows <= 0 {
			c.MaxRows = cfg.Server.MaxRows
		}
		if c.MaxAffectedRows <= 0 {
			c.MaxAffectedRows = defaultMaxAffected
		}
		if c.MaxConns <= 0 {
			c.MaxConns = defaultMaxConns
		}
		if c.QueryTimeout == "" {
			c.QueryTimeout = cfg.Server.QueryTimeout
		}
		cfg.Connections[name] = c
	}
}

func applyOAuthDefaults(cfg *Config) {
	oauth := &cfg.OAuth
	if value, ok := os.LookupEnv("MCP_OAUTH_ENABLED"); ok && strings.TrimSpace(value) != "" {
		if parsed, err := strconv.ParseBool(strings.TrimSpace(value)); err == nil {
			oauth.Enabled = parsed
		}
	}
	if value := strings.TrimSpace(os.Getenv("MCP_OAUTH_ISSUER")); value != "" {
		oauth.Issuer = value
	}
	if value := strings.TrimSpace(os.Getenv("MCP_OAUTH_RESOURCE")); value != "" {
		oauth.Resource = value
	}
	if value := strings.TrimSpace(os.Getenv("MCP_OAUTH_SIGNING_KEY")); value != "" {
		oauth.SigningKey = value
	}
	if value := strings.TrimSpace(os.Getenv("MCP_OAUTH_USERNAME")); value != "" {
		oauth.Username = value
	}
	if value := os.Getenv("MCP_OAUTH_PASSWORD"); value != "" {
		oauth.Password = value
	}
	if value := strings.TrimSpace(os.Getenv("MCP_OAUTH_PRINCIPAL")); value != "" {
		oauth.Principal = value
	}
	if value := strings.TrimSpace(os.Getenv("MCP_OAUTH_CLIENT_ID")); value != "" {
		oauth.ClientID = value
	}
	if value := strings.TrimSpace(os.Getenv("MCP_OAUTH_CLIENT_STORE_PATH")); value != "" {
		oauth.ClientStorePath = value
	}
	if value := strings.TrimSpace(os.Getenv("MCP_OAUTH_DEFAULT_SCOPES")); value != "" {
		oauth.DefaultScopes = splitConfigList(value)
	}
	if value := strings.TrimSpace(os.Getenv("MCP_OAUTH_BASE_SCOPES")); value != "" {
		oauth.BaseScopes = splitConfigList(value)
	}

	if oauth.SigningKey == "" && oauth.SigningKeyEnv != "" {
		oauth.SigningKey = os.Getenv(oauth.SigningKeyEnv)
	}
	if oauth.Username == "" && oauth.UsernameEnv != "" {
		oauth.Username = os.Getenv(oauth.UsernameEnv)
	}
	if oauth.Password == "" && oauth.PasswordEnv != "" {
		oauth.Password = os.Getenv(oauth.PasswordEnv)
	}
	if oauth.ClientStorePath == "" && oauth.ClientStorePathEnv != "" {
		oauth.ClientStorePath = os.Getenv(oauth.ClientStorePathEnv)
	}
	if oauth.Issuer == "" {
		oauth.Issuer = cfg.Server.PublicBaseURL
	}
	if oauth.Resource == "" {
		oauth.Resource = cfg.Server.PublicBaseURL
	}
	if oauth.Principal == "" && len(cfg.Auth.APIKeys) > 0 {
		oauth.Principal = cfg.Auth.APIKeys[0].Name
	}
	if oauth.ClientID == "" {
		oauth.ClientID = "felsen-chatgpt"
	}
	if len(oauth.DefaultScopes) == 0 {
		oauth.DefaultScopes = []string{"read"}
	}
	if len(oauth.BaseScopes) == 0 {
		oauth.BaseScopes = []string{"read"}
	}
	if oauth.AccessTokenTTL == "" {
		oauth.AccessTokenTTL = "1h"
	}
	if oauth.RefreshTokenTTL == "" {
		oauth.RefreshTokenTTL = "720h"
	}
	if oauth.AuthorizationCodeTTL == "" {
		oauth.AuthorizationCodeTTL = "5m"
	}
	oauth.DefaultScopes = normalizeConfigList(oauth.DefaultScopes)
	oauth.BaseScopes = normalizeConfigList(oauth.BaseScopes)
}

func (cfg *Config) Validate() error {
	if len(cfg.Connections) == 0 {
		return errors.New("at least one connection is required")
	}
	if strings.TrimSpace(cfg.Server.Host) == "" ||
		strings.ContainsAny(cfg.Server.Host, " \t\r\n/\\") ||
		(strings.Contains(cfg.Server.Host, ":") && net.ParseIP(cfg.Server.Host) == nil) ||
		cfg.Server.Host == "*" {
		return errors.New("server host must be a hostname or IP address without a port")
	}
	if cfg.Server.Port < 1 || cfg.Server.Port > 65535 {
		return fmt.Errorf("server port must be between 1 and 65535")
	}
	if !strings.HasPrefix(cfg.Server.Endpoint, "/") ||
		strings.HasPrefix(cfg.Server.Endpoint, "//") ||
		strings.ContainsAny(cfg.Server.Endpoint, " \t\r\n") ||
		strings.Contains(cfg.Server.Endpoint, "..") ||
		cfg.Server.Endpoint == "/" ||
		cfg.Server.Endpoint == "/healthz" ||
		cfg.Server.Endpoint == "/readyz" ||
		cfg.Server.Endpoint == "/sources" ||
		cfg.Server.Endpoint == "/sse" {
		return fmt.Errorf("server endpoint must be a safe, non-reserved URL path")
	}
	if strings.TrimSpace(cfg.Server.PublicBaseURL) == "" {
		return errors.New("server.public_base_url is required for absolute MCP search/fetch citations")
	}
	publicURL, err := url.Parse(cfg.Server.PublicBaseURL)
	if err != nil || publicURL.Scheme == "" || publicURL.Host == "" ||
		(publicURL.Scheme != "http" && publicURL.Scheme != "https") ||
		publicURL.User != nil || publicURL.Path != "" && publicURL.Path != "/" ||
		publicURL.RawQuery != "" || publicURL.Fragment != "" {
		return errors.New("server.public_base_url must be an absolute http(s) origin without credentials, path, query, or fragment")
	}
	if strings.EqualFold(publicURL.Hostname(), "0.0.0.0") || strings.EqualFold(publicURL.Hostname(), "::") {
		return errors.New("server.public_base_url must be user-openable and cannot point to a wildcard bind address")
	}
	citationTTL, err := time.ParseDuration(cfg.Server.CitationTTL)
	if err != nil || citationTTL <= 0 {
		if err == nil {
			err = errors.New("must be positive")
		}
		return fmt.Errorf("server citation_ttl: %w", err)
	}
	if cfg.Server.CitationSigningKeyEnv != "" && strings.TrimSpace(cfg.Server.CitationSigningKey) == "" {
		return fmt.Errorf("server.citation_signing_key_env %q is not set", cfg.Server.CitationSigningKeyEnv)
	}
	if cfg.Server.CitationSigningKey != "" {
		if insecureSecret(cfg.Server.CitationSigningKey) {
			return errors.New("server.citation_signing_key uses a known insecure placeholder token")
		}
		if len(cfg.Server.CitationSigningKey) < 32 {
			return errors.New("server.citation_signing_key must contain at least 32 characters")
		}
	}
	if cfg.Server.MaxRows <= 0 || cfg.Server.MaxSearchResults <= 0 ||
		cfg.Server.MaxBodyBytes <= 0 || cfg.Server.MaxConcurrent <= 0 {
		return errors.New("server limits must be positive")
	}
	if len(cfg.Auth.APIKeys) == 0 {
		return errors.New("at least one auth.api_keys entry or MCP_API_KEY is required")
	}
	if cfg.OAuth.Enabled {
		if err := validateOAuthOrigin("oauth.issuer", cfg.OAuth.Issuer); err != nil {
			return err
		}
		if err := validateOAuthOrigin("oauth.resource", cfg.OAuth.Resource); err != nil {
			return err
		}
		if !sameOrigin(cfg.Server.PublicBaseURL, cfg.OAuth.Issuer) || !sameOrigin(cfg.Server.PublicBaseURL, cfg.OAuth.Resource) {
			return errors.New("embedded OAuth issuer and resource must use the same public origin as server.public_base_url")
		}
		if strings.TrimSpace(cfg.OAuth.SigningKey) == "" {
			if cfg.OAuth.SigningKeyEnv != "" {
				return fmt.Errorf("oauth.signing_key_env %q is not set", cfg.OAuth.SigningKeyEnv)
			}
			return errors.New("oauth.signing_key is required when OAuth is enabled")
		}
		if insecureSecret(cfg.OAuth.SigningKey) {
			return errors.New("oauth.signing_key uses a known insecure placeholder token")
		}
		if len(cfg.OAuth.SigningKey) < 32 {
			return errors.New("oauth.signing_key must contain at least 32 characters")
		}
		if strings.TrimSpace(cfg.OAuth.Username) == "" {
			return errors.New("oauth.username is required when OAuth is enabled")
		}
		if strings.TrimSpace(cfg.OAuth.Password) == "" {
			return errors.New("oauth.password is required when OAuth is enabled")
		}
		if insecureSecret(cfg.OAuth.Password) {
			return errors.New("oauth.password uses a known insecure placeholder token")
		}
		if strings.TrimSpace(cfg.OAuth.ClientID) == "" || strings.ContainsAny(cfg.OAuth.ClientID, " \t\r\n") {
			return errors.New("oauth.client_id must be non-empty and contain no whitespace")
		}
		principalNames := map[string]APIKeyConfig{}
		for _, key := range cfg.Auth.APIKeys {
			principalNames[key.Name] = key
		}
		principal, ok := principalNames[cfg.OAuth.Principal]
		if !ok {
			return fmt.Errorf("oauth.principal %q does not match an auth api key", cfg.OAuth.Principal)
		}
		for _, redirectURI := range cfg.OAuth.RedirectURIs {
			if err := validateOAuthRedirectURI(redirectURI); err != nil {
				return fmt.Errorf("oauth.redirect_uris: %w", err)
			}
		}
		knownScopes := map[string]bool{"read": true, "write": true, "ddl": true, "admin": true}
		for field, scopes := range map[string][]string{
			"default_scopes": cfg.OAuth.DefaultScopes,
			"base_scopes":    cfg.OAuth.BaseScopes,
		} {
			for _, scope := range scopes {
				if !knownScopes[scope] {
					return fmt.Errorf("oauth.%s contains unsupported scope %q", field, scope)
				}
				if !containsFold(principal.Scopes, scope) && !containsFold(principal.Scopes, "admin") {
					return fmt.Errorf("oauth.%s requests scope %q not granted to principal %q", field, scope, principal.Name)
				}
			}
		}
		for field, value := range map[string]string{
			"access_token_ttl":       cfg.OAuth.AccessTokenTTL,
			"refresh_token_ttl":      cfg.OAuth.RefreshTokenTTL,
			"authorization_code_ttl": cfg.OAuth.AuthorizationCodeTTL,
		} {
			duration, err := time.ParseDuration(value)
			if err != nil || duration <= 0 {
				if err == nil {
					err = errors.New("must be positive")
				}
				return fmt.Errorf("oauth %s: %w", field, err)
			}
		}
	}
	knownScopes := map[string]bool{"read": true, "write": true, "ddl": true, "admin": true}
	keyNames := map[string]bool{}
	keyHashes := map[string]bool{}
	for _, key := range cfg.Auth.APIKeys {
		if strings.TrimSpace(key.Name) == "" {
			return errors.New("auth api key name is required")
		}
		if keyNames[key.Name] {
			return fmt.Errorf("duplicate auth api key name %q", key.Name)
		}
		keyNames[key.Name] = true
		if strings.TrimSpace(key.Token) == "" && strings.TrimSpace(key.TokenSHA256) == "" {
			return fmt.Errorf("auth api key %q has no token or token_sha256", key.Name)
		}
		if key.Token != "" && insecureSecret(key.Token) {
			return fmt.Errorf("auth api key %q uses a known insecure placeholder token", key.Name)
		}
		if key.TokenSHA256 != "" {
			hash := strings.TrimSpace(key.TokenSHA256)
			if len(hash) != 64 || !isHex(hash) {
				return fmt.Errorf("auth api key %q token_sha256 must be 64 hexadecimal characters", key.Name)
			}
			hash = strings.ToLower(hash)
			if keyHashes[hash] {
				return fmt.Errorf("duplicate auth token hash for key %q", key.Name)
			}
			keyHashes[hash] = true
		}
		if len(key.Scopes) == 0 {
			return fmt.Errorf("auth api key %q must declare at least one scope", key.Name)
		}
		for _, scope := range key.Scopes {
			if !knownScopes[strings.ToLower(strings.TrimSpace(scope))] {
				return fmt.Errorf("auth api key %q has unsupported scope %q", key.Name, scope)
			}
		}
		if len(key.Connections) == 0 {
			return fmt.Errorf("auth api key %q must declare allowed connections", key.Name)
		}
		for _, connection := range key.Connections {
			if connection != "*" {
				if _, ok := cfg.Connections[connection]; !ok {
					return fmt.Errorf("auth api key %q references unknown connection %q", key.Name, connection)
				}
			}
		}
	}
	for name, c := range cfg.Connections {
		if strings.TrimSpace(name) == "" || strings.ContainsAny(name, "/\\") || name == "." || name == ".." {
			return errors.New("connection name must be non-empty and path-safe")
		}
		if strings.TrimSpace(c.DSN) == "" {
			return fmt.Errorf("connection %q has no dsn or resolved dsn_env", name)
		}
		timeout, err := time.ParseDuration(c.QueryTimeout)
		if err != nil || timeout <= 0 {
			if err == nil {
				err = errors.New("must be positive")
			}
			return fmt.Errorf("connection %q query_timeout: %w", name, err)
		}
		if c.MaxRows <= 0 || c.MaxAffectedRows <= 0 {
			return fmt.Errorf("connection %q max_rows and max_affected_rows must be positive", name)
		}
		if c.MinConns < 0 || c.MaxConns <= 0 || c.MinConns > c.MaxConns {
			return fmt.Errorf("connection %q min_conns/max_conns are invalid", name)
		}
		if len(c.Schemas) == 0 {
			return fmt.Errorf("connection %q must declare at least one schema", name)
		}
		for _, schema := range c.Schemas {
			if strings.TrimSpace(schema) == "" {
				return fmt.Errorf("connection %q contains an empty schema", name)
			}
		}
		for _, policy := range c.DMLPolicies {
			if strings.TrimSpace(policy.Schema) == "" || strings.TrimSpace(policy.Table) == "" {
				return fmt.Errorf("connection %q contains an incomplete DML policy", name)
			}
			if len(policy.Operations) == 0 {
				return fmt.Errorf("connection %q contains a DML policy without operations", name)
			}
			for _, operation := range policy.Operations {
				switch strings.ToLower(strings.TrimSpace(operation)) {
				case "insert", "update", "delete", "*":
				default:
					return fmt.Errorf("connection %q has unsupported DML operation %q", name, operation)
				}
			}
		}
	}
	for field, value := range map[string]string{
		"query_timeout":       cfg.Server.QueryTimeout,
		"session_timeout":     cfg.Server.SessionTimeout,
		"read_header_timeout": cfg.Server.ReadHeaderTimeout,
		"read_timeout":        cfg.Server.ReadTimeout,
		"write_timeout":       cfg.Server.WriteTimeout,
		"idle_timeout":        cfg.Server.IdleTimeout,
	} {
		duration, err := time.ParseDuration(value)
		if err != nil || duration <= 0 {
			if err == nil {
				err = errors.New("must be positive")
			}
			return fmt.Errorf("server %s: %w", field, err)
		}
	}
	switch strings.ToLower(cfg.Audit.Destination) {
	case "", "stdout":
	case "file":
		if strings.TrimSpace(cfg.Audit.File) == "" {
			return errors.New("audit.file is required when audit.destination is file")
		}
	default:
		return fmt.Errorf("unsupported audit destination %q", cfg.Audit.Destination)
	}
	return nil
}

func (c ConnectionConfig) QueryTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(c.QueryTimeout)
	return d
}

func (s ServerConfig) SessionTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(s.SessionTimeout)
	return d
}

func (s ServerConfig) CitationTTLDuration() time.Duration {
	d, _ := time.ParseDuration(s.CitationTTL)
	return d
}

func (s ServerConfig) ReadHeaderTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(s.ReadHeaderTimeout)
	return d
}

func (s ServerConfig) ReadTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(s.ReadTimeout)
	return d
}

func (s ServerConfig) WriteTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(s.WriteTimeout)
	return d
}

func (s ServerConfig) IdleTimeoutDuration() time.Duration {
	d, _ := time.ParseDuration(s.IdleTimeout)
	return d
}

func (c ConnectionConfig) SchemaAllowed(schema string) bool {
	for _, allowed := range c.Schemas {
		if allowed == "*" || strings.EqualFold(allowed, schema) {
			return true
		}
	}
	return false
}

func (c ConnectionConfig) DDLAllowed() bool {
	return c.DDLEnabled
}

func (c ConnectionConfig) DMLAllowed(schema, table, operation string) bool {
	for _, policy := range c.DMLPolicies {
		if !matchesName(policy.Schema, schema) || !matchesName(policy.Table, table) {
			continue
		}
		for _, op := range policy.Operations {
			if strings.EqualFold(op, operation) || op == "*" {
				return true
			}
		}
	}
	return false
}

func validateOAuthOrigin(field, value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		(parsed.Scheme != "http" && parsed.Scheme != "https") ||
		parsed.User != nil || (parsed.Path != "" && parsed.Path != "/") ||
		parsed.RawQuery != "" || parsed.Fragment != "" {
		return fmt.Errorf("%s must be an absolute http(s) origin without credentials, path, query, or fragment", field)
	}
	if strings.EqualFold(parsed.Hostname(), "0.0.0.0") || strings.EqualFold(parsed.Hostname(), "::") {
		return fmt.Errorf("%s cannot point to a wildcard bind address", field)
	}
	return nil
}

func validateOAuthRedirectURI(value string) error {
	parsed, err := url.Parse(strings.TrimSpace(value))
	if err != nil || parsed.Scheme == "" || parsed.Host == "" ||
		parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" ||
		(parsed.Scheme != "https" && !(parsed.Scheme == "http" && isLocalHost(parsed.Hostname()))) {
		return errors.New("redirect URI must be an absolute HTTPS URL (HTTP is allowed only for localhost) without credentials, query, or fragment")
	}
	return nil
}

func sameOrigin(left, right string) bool {
	a, errA := url.Parse(strings.TrimSpace(left))
	b, errB := url.Parse(strings.TrimSpace(right))
	if errA != nil || errB != nil {
		return false
	}
	return strings.EqualFold(a.Scheme, b.Scheme) && strings.EqualFold(a.Host, b.Host)
}

func isLocalHost(host string) bool {
	host = strings.ToLower(strings.TrimSpace(host))
	return host == "localhost" || host == "127.0.0.1" || host == "::1" || strings.HasPrefix(host, "127.")
}

func containsFold(values []string, wanted string) bool {
	for _, value := range values {
		if strings.EqualFold(strings.TrimSpace(value), wanted) {
			return true
		}
	}
	return false
}

func splitConfigList(value string) []string {
	parts := strings.FieldsFunc(value, func(r rune) bool { return r == ',' || r == '\n' || r == '\r' || r == '\t' || r == ' ' })
	return normalizeConfigList(parts)
}

func normalizeConfigList(values []string) []string {
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

func matchesName(pattern, value string) bool {
	return pattern == "*" || strings.EqualFold(pattern, value)
}

func insecureSecret(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "change-me", "change-me-reader", "change-me-reader-token",
		"change-me-writer", "change-me-writer-token", "change-me-ddl",
		"change-me-ddl-token", "replace-me", "replace-with-a-real-token",
		"your-token-here", "secret", "password":
		return true
	}
	lower := strings.ToLower(strings.TrimSpace(value))
	return strings.HasPrefix(lower, "change-me-") ||
		strings.HasPrefix(lower, "replace-with-") ||
		strings.HasPrefix(lower, "your-")
}

func isHex(value string) bool {
	for _, r := range value {
		if !(r >= '0' && r <= '9') &&
			!(r >= 'a' && r <= 'f') &&
			!(r >= 'A' && r <= 'F') {
			return false
		}
	}
	return true
}
