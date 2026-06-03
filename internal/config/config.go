package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	defaultConfigPath = "configs/example.yaml"
	defaultEndpoint   = "/mcp"
	defaultHost       = "127.0.0.1"
	defaultPort       = 8080
	defaultMaxRows    = 100
)

type Config struct {
	Server      ServerConfig                `json:"server" yaml:"server"`
	Auth        AuthConfig                  `json:"auth" yaml:"auth"`
	Connections map[string]ConnectionConfig `json:"connections" yaml:"connections"`
	Audit       AuditConfig                 `json:"audit" yaml:"audit"`
}

type ServerConfig struct {
	Host           string `json:"host" yaml:"host"`
	Port           int    `json:"port" yaml:"port"`
	Endpoint       string `json:"endpoint" yaml:"endpoint"`
	QueryTimeout   string `json:"query_timeout" yaml:"query_timeout"`
	MaxRows        int    `json:"max_rows" yaml:"max_rows"`
	JSONResponse   bool   `json:"json_response" yaml:"json_response"`
	Stateless      bool   `json:"stateless" yaml:"stateless"`
	SessionTimeout string `json:"session_timeout" yaml:"session_timeout"`
}

type AuthConfig struct {
	APIKeys []APIKeyConfig `json:"api_keys" yaml:"api_keys"`
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
	DSN          string        `json:"dsn" yaml:"dsn"`
	DSNEnv       string        `json:"dsn_env" yaml:"dsn_env"`
	Schemas      []string      `json:"schemas" yaml:"schemas"`
	MaxRows      int           `json:"max_rows" yaml:"max_rows"`
	QueryTimeout string        `json:"query_timeout" yaml:"query_timeout"`
	Masking      MaskingConfig `json:"masking" yaml:"masking"`
	DMLPolicies  []DMLPolicy   `json:"dml_policies" yaml:"dml_policies"`
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
			err = json.Unmarshal(data, &cfg)
		default:
			err = yaml.Unmarshal(data, &cfg)
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
		Server: ServerConfig{Host: defaultHost, Port: defaultPort, Endpoint: defaultEndpoint},
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
	if cfg.Server.MaxRows <= 0 {
		cfg.Server.MaxRows = defaultMaxRows
	}
	if cfg.Server.QueryTimeout == "" {
		cfg.Server.QueryTimeout = "10s"
	}
	if cfg.Server.SessionTimeout == "" {
		cfg.Server.SessionTimeout = "30m"
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
		if c.QueryTimeout == "" {
			c.QueryTimeout = cfg.Server.QueryTimeout
		}
		cfg.Connections[name] = c
	}
}

func (cfg *Config) Validate() error {
	if len(cfg.Connections) == 0 {
		return errors.New("at least one connection is required")
	}
	if len(cfg.Auth.APIKeys) == 0 {
		return errors.New("at least one auth.api_keys entry or MCP_API_KEY is required")
	}
	for name, c := range cfg.Connections {
		if strings.TrimSpace(name) == "" {
			return errors.New("connection name cannot be empty")
		}
		if strings.TrimSpace(c.DSN) == "" {
			return fmt.Errorf("connection %q has no dsn or resolved dsn_env", name)
		}
		if _, err := time.ParseDuration(c.QueryTimeout); err != nil {
			return fmt.Errorf("connection %q query_timeout: %w", name, err)
		}
	}
	if _, err := time.ParseDuration(cfg.Server.QueryTimeout); err != nil {
		return fmt.Errorf("server query_timeout: %w", err)
	}
	if _, err := time.ParseDuration(cfg.Server.SessionTimeout); err != nil {
		return fmt.Errorf("server session_timeout: %w", err)
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

func (c ConnectionConfig) SchemaAllowed(schema string) bool {
	for _, allowed := range c.Schemas {
		if allowed == "*" || strings.EqualFold(allowed, schema) {
			return true
		}
	}
	return false
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

func matchesName(pattern, value string) bool {
	return pattern == "*" || strings.EqualFold(pattern, value)
}
