package authn

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"log/slog"
	"net/http"
	"strings"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

type Principal struct {
	Name        string
	Scopes      map[string]bool
	Connections map[string]bool
}

// Authenticator validates an HTTP Authorization header and returns the
// principal represented by it. API keys and OAuth access tokens both satisfy
// this interface, which lets the server keep backwards compatibility while
// adding standards-based authentication.
type Authenticator interface {
	AuthenticateHeader(header string) (Principal, bool)
}

type Manager struct {
	keys []apiKey
}

type apiKey struct {
	hash      string
	principal Principal
}

func NewManager(cfg config.AuthConfig) (*Manager, error) {
	m := &Manager{}
	for _, key := range cfg.APIKeys {
		if key.Name == "" {
			return nil, errors.New("api key name is required")
		}
		hash := strings.ToLower(strings.TrimSpace(key.TokenSHA256))
		if hash == "" {
			if key.Token == "" {
				return nil, errors.New("api key token or token_sha256 is required")
			}
			sum := sha256.Sum256([]byte(key.Token))
			hash = hex.EncodeToString(sum[:])
		} else if len(hash) != sha256.Size*2 {
			return nil, errors.New("api key token_sha256 must be 64 hexadecimal characters")
		} else if _, err := hex.DecodeString(hash); err != nil {
			return nil, errors.New("api key token_sha256 must be hexadecimal")
		}
		p := Principal{
			Name:        key.Name,
			Scopes:      map[string]bool{},
			Connections: map[string]bool{},
		}
		for _, scope := range key.Scopes {
			p.Scopes[strings.ToLower(scope)] = true
		}
		for _, conn := range key.Connections {
			p.Connections[conn] = true
		}
		m.keys = append(m.keys, apiKey{hash: hash, principal: p})
	}
	if len(m.keys) == 0 {
		return nil, errors.New("no api keys configured")
	}
	return m, nil
}

func (m *Manager) AuthenticateHeader(header string) (Principal, bool) {
	token := strings.TrimSpace(header)
	if len(token) < len("Bearer ") || !strings.EqualFold(token[:len("Bearer ")], "Bearer ") {
		return Principal{}, false
	}
	token = strings.TrimSpace(token[len("Bearer "):])
	if token == "" {
		return Principal{}, false
	}
	sum := sha256.Sum256([]byte(token))
	hash := hex.EncodeToString(sum[:])
	for _, key := range m.keys {
		if subtle.ConstantTimeCompare([]byte(hash), []byte(key.hash)) == 1 {
			return key.principal, true
		}
	}
	return Principal{}, false
}

// Principal returns a configured API-key principal by name. OAuth uses this
// to inherit the connection allowlist and the maximum scope set of a named
// service identity.
func (m *Manager) Principal(name string) (Principal, bool) {
	if m == nil {
		return Principal{}, false
	}
	for _, key := range m.keys {
		if key.principal.Name == name {
			return clonePrincipal(key.principal), true
		}
	}
	return Principal{}, false
}

// FirstPrincipal returns the first configured API-key principal. It is useful
// for a single-identity OAuth bootstrap configuration.
func (m *Manager) FirstPrincipal() (Principal, bool) {
	if m == nil || len(m.keys) == 0 {
		return Principal{}, false
	}
	return clonePrincipal(m.keys[0].principal), true
}

// NewComposite tries authenticators in order. This keeps direct API-key
// clients working while OAuth tokens are accepted by the same MCP handler.
func NewComposite(authenticators ...Authenticator) Authenticator {
	items := make([]Authenticator, 0, len(authenticators))
	for _, item := range authenticators {
		if item != nil {
			items = append(items, item)
		}
	}
	if len(items) == 1 {
		return items[0]
	}
	return compositeAuthenticator{items: items}
}

type compositeAuthenticator struct {
	items []Authenticator
}

func (c compositeAuthenticator) AuthenticateHeader(header string) (Principal, bool) {
	for _, item := range c.items {
		if principal, ok := item.AuthenticateHeader(header); ok {
			return principal, true
		}
	}
	return Principal{}, false
}

func clonePrincipal(p Principal) Principal {
	clone := Principal{Name: p.Name, Scopes: map[string]bool{}, Connections: map[string]bool{}}
	for scope, allowed := range p.Scopes {
		clone.Scopes[scope] = allowed
	}
	for connection, allowed := range p.Connections {
		clone.Connections[connection] = allowed
	}
	return clone
}

func (p Principal) HasScope(scope string) bool {
	return p.Scopes["admin"] || p.Scopes[strings.ToLower(scope)]
}

func (p Principal) CanUseConnection(name string) bool {
	return p.HasScope("admin") || p.Connections["*"] || p.Connections[name]
}

func HTTPMiddleware(authenticator Authenticator, next http.Handler) http.Handler {
	return HTTPMiddlewareWithLogger(authenticator, next, nil)
}

func HTTPMiddlewareWithLogger(authenticator Authenticator, next http.Handler, logger *slog.Logger) http.Handler {
	return HTTPMiddlewareWithLoggerAndChallenge(authenticator, next, logger, `Bearer realm="postgres-mcp"`)
}

func HTTPMiddlewareWithLoggerAndChallenge(authenticator Authenticator, next http.Handler, logger *slog.Logger, challenge string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if authenticator == nil {
			if logger != nil {
				logger.Error("authentication manager is not configured")
			}
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		if _, ok := authenticator.AuthenticateHeader(r.Header.Get("Authorization")); !ok {
			if logger != nil {
				logger.Warn("unauthorized HTTP request", "method", r.Method, "path", r.URL.Path, "remote_addr", r.RemoteAddr)
			}
			w.Header().Set("WWW-Authenticate", challenge)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
