package authn

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"net/http"
	"strings"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

type Principal struct {
	Name        string
	Scopes      map[string]bool
	Connections map[string]bool
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
	if strings.HasPrefix(strings.ToLower(token), "bearer ") {
		token = strings.TrimSpace(token[7:])
	}
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

func (p Principal) HasScope(scope string) bool {
	return p.Scopes["admin"] || p.Scopes[strings.ToLower(scope)]
}

func (p Principal) CanUseConnection(name string) bool {
	return p.HasScope("admin") || p.Connections["*"] || p.Connections[name]
}

func HTTPMiddleware(manager *Manager, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}
		if _, ok := manager.AuthenticateHeader(r.Header.Get("Authorization")); !ok {
			w.Header().Set("WWW-Authenticate", `Bearer realm="postgres-mcp"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
