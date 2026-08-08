package mcpserver

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/audit"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/authn"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/postgres"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/sqlguard"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/version"
)

type App struct {
	cfg     *config.Config
	store   *postgres.Store
	auth    authn.Authenticator
	auditor *audit.Auditor
	logger  *slog.Logger
	server  *mcp.Server
}

func New(cfg *config.Config, store *postgres.Store, auth authn.Authenticator, auditor *audit.Auditor, logger *slog.Logger) http.Handler {
	app := &App{cfg: cfg, store: store, auth: auth, auditor: auditor, logger: logger}
	app.server = mcp.NewServer(&mcp.Implementation{Name: "postgres-mcp", Version: version.String()}, &mcp.ServerOptions{
		Instructions: "Use search to discover database objects and fetch to retrieve their details. Read-only tools may be called without confirmation; execute_dml and execute_ddl are consequential and require explicit user approval.",
		Logger:       logger,
	})
	app.registerResources()
	app.registerTools()

	mux := http.NewServeMux()

	var streamableHandler http.Handler = mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return app.server
	}, &mcp.StreamableHTTPOptions{
		JSONResponse:   cfg.Server.JSONResponse,
		Stateless:      cfg.Server.Stateless,
		SessionTimeout: cfg.Server.SessionTimeoutDuration(),
		Logger:         logger,
	})
	if cfg.Server.JSONResponse {
		streamableHandler = withTopLevelToolSecuritySchemes(streamableHandler)
	}

	sseHandler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server {
		return app.server
	}, nil)

	endpoint := strings.TrimRight(cfg.Server.Endpoint, "/")
	mux.Handle(endpoint, streamableHandler)
	if endpoint != "/" {
		mux.Handle(endpoint+"/", streamableHandler)
	}
	mux.Handle("/sse", sseHandler)
	mux.Handle("/sse/", sseHandler)
	mux.Handle("/sources", http.HandlerFunc(app.sourceHTTPHandler))
	mux.Handle("/sources/", http.HandlerFunc(app.sourceHTTPHandler))

	return mux
}

func (a *App) registerResources() {
	a.server.AddResource(&mcp.Resource{
		URI:         "postgres://connections",
		Name:        "postgres_connections",
		Title:       "Postgres connections",
		Description: "Named Postgres connections available to the authenticated token.",
		MIMEType:    "application/json",
	}, a.readResource)
	for _, tmpl := range []struct {
		uri, name, title, desc string
	}{
		{"postgres://{connection}/schema", "postgres_schema_summary", "Postgres schema summary", "Summary of schemas and table counts."},
		{"postgres://{connection}/schemas", "postgres_schemas", "Postgres schemas", "Allowed schemas for a connection."},
		{"postgres://{connection}/tables", "postgres_tables", "Postgres tables", "Tables and views for allowed schemas."},
		{"postgres://{connection}/table/{schema}/{table}", "postgres_table", "Postgres table", "Detailed table description."},
	} {
		a.server.AddResourceTemplate(&mcp.ResourceTemplate{
			URITemplate: tmpl.uri,
			Name:        tmpl.name,
			Title:       tmpl.title,
			Description: tmpl.desc,
			MIMEType:    "application/json",
		}, a.readResource)
	}
}

func (a *App) registerTools() {
	readOnly := true
	openWorld := true
	destructive := true
	mcp.AddTool(a.server, &mcp.Tool{Name: "list_connections", Title: "List database connections", Description: "List named Postgres connections allowed for this token.", Meta: a.toolSecurityMeta("read"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.listConnections)
	mcp.AddTool(a.server, &mcp.Tool{Name: "list_schemas", Title: "List database schemas", Description: "List allowed schemas in a Postgres connection.", Meta: a.toolSecurityMeta("read"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.listSchemas)
	mcp.AddTool(a.server, &mcp.Tool{Name: "list_tables", Title: "List database tables", Description: "List tables and views for a schema.", Meta: a.toolSecurityMeta("read"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.listTables)
	mcp.AddTool(a.server, &mcp.Tool{Name: "describe_table", Title: "Describe a database table", Description: "Describe columns, primary key, foreign keys and indexes for a table.", Meta: a.toolSecurityMeta("read"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.describeTable)
	mcp.AddTool(a.server, &mcp.Tool{Name: "sample_rows", Title: "Sample database rows", Description: "Return a limited, masked sample of table rows.", Meta: a.toolSecurityMeta("read"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.sampleRows)
	mcp.AddTool(a.server, &mcp.Tool{Name: "validate_sql", Title: "Validate SQL", Description: "Validate SQL against read-only or DML policies without executing it.", Meta: a.toolSecurityMeta("read", "write", "ddl"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.validateSQL)
	mcp.AddTool(a.server, &mcp.Tool{Name: "execute_sql", Title: "Execute read-only SQL", Description: "Execute a validated SELECT query with configured row limits and masking.", Meta: a.toolSecurityMeta("read"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, OpenWorldHint: &openWorld}}, a.executeSQL)
	mcp.AddTool(a.server, &mcp.Tool{Name: "execute_dml", Title: "Execute a database write", Description: "Execute INSERT, UPDATE or DELETE only when explicitly allowlisted and approved by the user.", Meta: a.toolSecurityMeta("write"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &destructive, OpenWorldHint: &openWorld}}, a.executeDML)
	mcp.AddTool(a.server, &mcp.Tool{Name: "explain_sql", Title: "Explain read-only SQL", Description: "Run EXPLAIN (FORMAT JSON) for a validated SELECT query.", Meta: a.toolSecurityMeta("read"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.explainSQL)
	mcp.AddTool(a.server, &mcp.Tool{Name: "execute_ddl", Title: "Execute database DDL", Description: "Execute allowlisted CREATE, ALTER, DROP or TRUNCATE statements only when DDL is enabled and explicitly approved.", Meta: a.toolSecurityMeta("ddl"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &destructive, OpenWorldHint: &openWorld}}, a.executeDDL)
	mcp.AddTool(a.server, &mcp.Tool{Name: "refresh_schema_cache", Title: "Refresh schema cache", Description: "Clear cached schema descriptions for a connection.", Meta: a.toolSecurityMeta("read"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.refreshSchemaCache)
	mcp.AddTool(a.server, &mcp.Tool{Name: "search", Title: "Search database objects", Description: "Search all database connections allowed for this token for tables, columns, and database objects matching a query. Each result ID is an opaque connection-scoped ID for fetch.", Meta: a.toolSecurityMeta("read"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.search)
	mcp.AddTool(a.server, &mcp.Tool{Name: "fetch", Title: "Fetch a database object", Description: "Fetch detailed information about a database object by the opaque connection-scoped ID returned by search.", Meta: a.toolSecurityMeta("read"), Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.fetch)
}

func (a *App) toolSecurityMeta(scopes ...string) mcp.Meta {
	if a.cfg == nil || !a.cfg.OAuth.Enabled {
		return nil
	}
	// Do not advertise action-level scopes that the configured OAuth
	// principal cannot ever receive. ChatGPT then falls back to the default
	// scopes instead of requesting write/DDL access from a read-only connector.
	if len(a.cfg.Auth.APIKeys) > 0 {
		principalName := a.cfg.OAuth.Principal
		if principalName == "" {
			principalName = a.cfg.Auth.APIKeys[0].Name
		}
		var principal *config.APIKeyConfig
		for i := range a.cfg.Auth.APIKeys {
			if a.cfg.Auth.APIKeys[i].Name == principalName {
				principal = &a.cfg.Auth.APIKeys[i]
				break
			}
		}
		if principal == nil {
			return nil
		}
		for _, scope := range scopes {
			if !containsScope(principal.Scopes, scope) && !containsScope(principal.Scopes, "admin") {
				return nil
			}
		}
	}
	return mcp.Meta{
		"securitySchemes": []map[string]any{{
			"type":   "oauth2",
			"scopes": scopes,
		}},
	}
}

func containsScope(scopes []string, wanted string) bool {
	for _, scope := range scopes {
		if strings.EqualFold(strings.TrimSpace(scope), wanted) {
			return true
		}
	}
	return false
}

type bufferedResponseWriter struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (w *bufferedResponseWriter) Header() http.Header { return w.header }

func (w *bufferedResponseWriter) WriteHeader(status int) {
	if w.status == 0 {
		w.status = status
	}
}

func (w *bufferedResponseWriter) Write(data []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.body.Write(data)
}

func withTopLevelToolSecuritySchemes(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		buffered := &bufferedResponseWriter{header: make(http.Header)}
		next.ServeHTTP(buffered, r)
		body := buffered.body.Bytes()
		transformed := addTopLevelToolSecuritySchemes(body, buffered.header.Get("Content-Type"))
		for key, values := range buffered.header {
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		if transformed != nil {
			w.Header().Del("Content-Length")
			body = transformed
		}
		status := buffered.status
		if status == 0 {
			status = http.StatusOK
		}
		w.WriteHeader(status)
		_, _ = w.Write(body)
	})
}

func addTopLevelToolSecuritySchemes(body []byte, contentType string) []byte {
	if !strings.Contains(strings.ToLower(contentType), "application/json") {
		return nil
	}
	var envelope map[string]any
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil
	}
	result, ok := envelope["result"].(map[string]any)
	if !ok {
		return nil
	}
	tools, ok := result["tools"].([]any)
	if !ok {
		return nil
	}
	changed := false
	for _, rawTool := range tools {
		tool, ok := rawTool.(map[string]any)
		if !ok {
			continue
		}
		if _, exists := tool["securitySchemes"]; exists {
			continue
		}
		meta, ok := tool["_meta"].(map[string]any)
		if !ok {
			continue
		}
		security, ok := meta["securitySchemes"]
		if !ok {
			continue
		}
		tool["securitySchemes"] = security
		changed = true
	}
	if !changed {
		return nil
	}
	transformed, err := json.Marshal(envelope)
	if err != nil {
		return nil
	}
	return append(transformed, '\n')
}

type connectionInput struct {
	ConnectionName string `json:"connection_name,omitempty" jsonschema:"named Postgres connection; defaults when only one connection is configured"`
}

type listConnectionsOutput struct {
	Connections []string `json:"connections"`
}

type listSchemasOutput struct {
	ConnectionName string   `json:"connection_name"`
	Schemas        []string `json:"schemas"`
}

type listTablesInput struct {
	ConnectionName string `json:"connection_name,omitempty"`
	Schema         string `json:"schema" jsonschema:"schema name"`
}

type listTablesOutput struct {
	ConnectionName string               `json:"connection_name"`
	Schema         string               `json:"schema"`
	Tables         []postgres.TableInfo `json:"tables"`
}

type describeTableInput struct {
	ConnectionName string `json:"connection_name,omitempty"`
	Schema         string `json:"schema"`
	Table          string `json:"table"`
}

type sampleRowsInput struct {
	ConnectionName string `json:"connection_name,omitempty"`
	Schema         string `json:"schema"`
	Table          string `json:"table"`
	Limit          int    `json:"limit,omitempty"`
}

type sqlInput struct {
	ConnectionName string `json:"connection_name,omitempty"`
	SQL            string `json:"sql"`
}

type validateSQLInput struct {
	ConnectionName string `json:"connection_name,omitempty"`
	SQL            string `json:"sql"`
	Mode           string `json:"mode,omitempty" jsonschema:"read, dml or explain; defaults to read"`
}

type refreshCacheOutput struct {
	ConnectionName string `json:"connection_name"`
	Refreshed      bool   `json:"refreshed"`
}

type searchInput struct {
	Query string `json:"query" jsonschema:"search query to find tables, columns, or database objects"`
}

type searchResultItem struct {
	ID    string `json:"id"`
	Title string `json:"title"`
	URL   string `json:"url"`
}

type searchOutput struct {
	Results []searchResultItem `json:"results"`
}

type fetchInput struct {
	ID string `json:"id" jsonschema:"opaque object ID returned by search"`
}

type fetchOutput struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	Text     string            `json:"text"`
	URL      string            `json:"url"`
	Metadata map[string]string `json:"metadata,omitempty"`
}

const objectIDPrefix = "pg:v1:"

type objectReference struct {
	Connection string `json:"connection"`
	Schema     string `json:"schema"`
	Table      string `json:"table"`
	Column     string `json:"column,omitempty"`
}

func encodeObjectID(ref objectReference) string {
	payload, _ := json.Marshal(ref)
	return objectIDPrefix + base64.RawURLEncoding.EncodeToString(payload)
}

func decodeObjectID(id string) (objectReference, error) {
	if !strings.HasPrefix(id, objectIDPrefix) {
		return objectReference{}, fmt.Errorf("invalid ID format: expected a connection-scoped ID returned by search")
	}
	payload, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(id, objectIDPrefix))
	if err != nil {
		return objectReference{}, fmt.Errorf("invalid object ID encoding: %w", err)
	}
	var ref objectReference
	if err := json.Unmarshal(payload, &ref); err != nil {
		return objectReference{}, fmt.Errorf("invalid object ID payload: %w", err)
	}
	if strings.TrimSpace(ref.Connection) == "" || strings.TrimSpace(ref.Schema) == "" || strings.TrimSpace(ref.Table) == "" {
		return objectReference{}, errors.New("invalid object ID: connection, schema, and table are required")
	}
	return ref, nil
}

func decodeLegacyObjectID(id, connection string) (objectReference, error) {
	parts := strings.Split(id, ".")
	if len(parts) < 2 || len(parts) > 3 || parts[0] == "" || parts[1] == "" {
		return objectReference{}, fmt.Errorf("invalid legacy ID format: expected schema.table or schema.table.column, got: %s", id)
	}
	ref := objectReference{Connection: connection, Schema: parts[0], Table: parts[1]}
	if len(parts) == 3 {
		if parts[2] == "" {
			return objectReference{}, fmt.Errorf("invalid legacy ID format: empty column in %s", id)
		}
		ref.Column = parts[2]
	}
	return ref, nil
}

func objectTitle(ref objectReference) string {
	title := ref.Connection + ":" + ref.Schema + "." + ref.Table
	if ref.Column != "" {
		title += "." + ref.Column
	}
	return title
}

func (a *App) listConnections(ctx context.Context, req *mcp.CallToolRequest, _ struct{}) (*mcp.CallToolResult, listConnectionsOutput, error) {
	p, err := a.principal(req)
	if err != nil {
		return nil, listConnectionsOutput{}, err
	}
	names := []string{}
	for _, name := range a.store.ConnectionNames() {
		if p.CanUseConnection(name) {
			names = append(names, name)
		}
	}
	return nil, listConnectionsOutput{Connections: names}, nil
}

func (a *App) listSchemas(ctx context.Context, req *mcp.CallToolRequest, in connectionInput) (*mcp.CallToolResult, listSchemasOutput, error) {
	start := time.Now()
	p, connection, err := a.authorize(req, in.ConnectionName, "read")
	if err != nil {
		return nil, listSchemasOutput{}, err
	}
	schemas, err := a.store.ListSchemas(ctx, connection)
	a.audit(p, connection, "list_schemas", "", nil, err == nil, 0, start, err)
	return nil, listSchemasOutput{ConnectionName: connection, Schemas: schemas}, err
}

func (a *App) listTables(ctx context.Context, req *mcp.CallToolRequest, in listTablesInput) (*mcp.CallToolResult, listTablesOutput, error) {
	start := time.Now()
	p, connection, err := a.authorize(req, in.ConnectionName, "read")
	if err != nil {
		return nil, listTablesOutput{}, err
	}
	tables, err := a.store.ListTables(ctx, connection, in.Schema)
	a.audit(p, connection, "list_tables", "", []string{in.Schema + ".*"}, err == nil, 0, start, err)
	return nil, listTablesOutput{ConnectionName: connection, Schema: in.Schema, Tables: tables}, err
}

func (a *App) describeTable(ctx context.Context, req *mcp.CallToolRequest, in describeTableInput) (*mcp.CallToolResult, postgres.TableDescription, error) {
	start := time.Now()
	p, connection, err := a.authorize(req, in.ConnectionName, "read")
	if err != nil {
		return nil, postgres.TableDescription{}, err
	}
	desc, err := a.store.DescribeTable(ctx, connection, in.Schema, in.Table)
	a.audit(p, connection, "describe_table", "", []string{in.Schema + "." + in.Table}, err == nil, 0, start, err)
	return nil, desc, err
}

func (a *App) sampleRows(ctx context.Context, req *mcp.CallToolRequest, in sampleRowsInput) (*mcp.CallToolResult, postgres.QueryResult, error) {
	start := time.Now()
	p, connection, err := a.authorize(req, in.ConnectionName, "read")
	if err != nil {
		return nil, postgres.QueryResult{}, err
	}
	result, err := a.store.SampleRows(ctx, connection, in.Schema, in.Table, in.Limit)
	a.audit(p, connection, "sample_rows", "", []string{in.Schema + "." + in.Table}, err == nil, int64(result.RowCount), start, err)
	return nil, result, err
}

func (a *App) validateSQL(ctx context.Context, req *mcp.CallToolRequest, in validateSQLInput) (*mcp.CallToolResult, sqlguard.ValidationResult, error) {
	p, connection, err := a.authorize(req, in.ConnectionName, "read")
	if err != nil {
		return nil, sqlguard.ValidationResult{}, err
	}
	cfg, ok := a.store.ConnectionConfig(connection)
	if !ok {
		return nil, sqlguard.ValidationResult{}, fmt.Errorf("unknown connection: %s", connection)
	}
	mode := sqlguard.Mode(strings.ToLower(in.Mode))
	if mode == "" {
		mode = sqlguard.ModeRead
	}
	if mode == sqlguard.ModeDML && !p.HasScope("write") {
		return nil, sqlguard.ValidationResult{Valid: false, Reason: "write scope is required"}, nil
	}
	if mode == sqlguard.ModeDDL && !p.HasScope("ddl") {
		return nil, sqlguard.ValidationResult{Valid: false, Reason: "ddl scope is required"}, nil
	}
	result := sqlguard.Validate(in.SQL, cfg, mode)
	return nil, result, nil
}

func (a *App) executeSQL(ctx context.Context, req *mcp.CallToolRequest, in sqlInput) (*mcp.CallToolResult, postgres.QueryResult, error) {
	start := time.Now()
	p, connection, err := a.authorize(req, in.ConnectionName, "read")
	if err != nil {
		return nil, postgres.QueryResult{}, err
	}
	result, validation, err := a.store.ExecuteSQL(ctx, connection, in.SQL)
	a.audit(p, connection, "execute_sql", fingerprint(in.SQL), validation.TablesDetected, err == nil, int64(result.RowCount), start, err)
	return nil, result, err
}

func (a *App) executeDML(ctx context.Context, req *mcp.CallToolRequest, in sqlInput) (*mcp.CallToolResult, postgres.DMLResult, error) {
	start := time.Now()
	p, connection, err := a.authorize(req, in.ConnectionName, "write")
	if err != nil {
		return nil, postgres.DMLResult{}, err
	}
	result, validation, err := a.store.ExecuteDML(ctx, connection, in.SQL)
	a.audit(p, connection, "execute_dml", fingerprint(in.SQL), validation.TablesDetected, err == nil, result.RowsAffected, start, err)
	return nil, result, err
}

func (a *App) explainSQL(ctx context.Context, req *mcp.CallToolRequest, in sqlInput) (*mcp.CallToolResult, postgres.QueryResult, error) {
	start := time.Now()
	p, connection, err := a.authorize(req, in.ConnectionName, "read")
	if err != nil {
		return nil, postgres.QueryResult{}, err
	}
	result, validation, err := a.store.ExplainSQL(ctx, connection, in.SQL)
	a.audit(p, connection, "explain_sql", fingerprint(in.SQL), validation.TablesDetected, err == nil, int64(result.RowCount), start, err)
	return nil, result, err
}

func (a *App) executeDDL(ctx context.Context, req *mcp.CallToolRequest, in sqlInput) (*mcp.CallToolResult, postgres.DDLResult, error) {
	start := time.Now()
	p, connection, err := a.authorize(req, in.ConnectionName, "ddl")
	if err != nil {
		return nil, postgres.DDLResult{}, err
	}
	result, validation, err := a.store.ExecuteDDL(ctx, connection, in.SQL)
	a.audit(p, connection, "execute_ddl", fingerprint(in.SQL), validation.TablesDetected, err == nil, 0, start, err)
	return nil, result, err
}

func (a *App) refreshSchemaCache(ctx context.Context, req *mcp.CallToolRequest, in connectionInput) (*mcp.CallToolResult, refreshCacheOutput, error) {
	start := time.Now()
	p, connection, err := a.authorize(req, in.ConnectionName, "read")
	if err != nil {
		return nil, refreshCacheOutput{}, err
	}
	err = a.store.RefreshSchemaCache(connection)
	a.audit(p, connection, "refresh_schema_cache", "", nil, err == nil, 0, start, err)
	return nil, refreshCacheOutput{ConnectionName: connection, Refreshed: err == nil}, err
}

func (a *App) search(ctx context.Context, req *mcp.CallToolRequest, in searchInput) (*mcp.CallToolResult, searchOutput, error) {
	start := time.Now()
	p, err := a.principal(req)
	if err != nil {
		return nil, searchOutput{}, err
	}
	if !p.HasScope("read") {
		return nil, searchOutput{}, errors.New("read scope is required")
	}

	query := strings.ToLower(strings.TrimSpace(in.Query))
	if query == "" {
		return nil, searchOutput{Results: []searchResultItem{}}, nil
	}

	results := make([]searchResultItem, 0, a.cfg.Server.MaxSearchResults)
	seen := map[string]bool{}
	for _, connection := range a.store.ConnectionNames() {
		if !p.CanUseConnection(connection) {
			continue
		}
		cfg, ok := a.store.ConnectionConfig(connection)
		if !ok {
			err := fmt.Errorf("unknown connection: %s", connection)
			a.audit(p, connection, "search", "", nil, false, int64(len(results)), start, err)
			return nil, searchOutput{}, err
		}

		schemas := cfg.Schemas
		for _, schema := range schemas {
			if schema == "*" {
				schemas, err = a.store.ListSchemas(ctx, connection)
				if err != nil {
					a.audit(p, connection, "search", "", nil, false, int64(len(results)), start, err)
					return nil, searchOutput{}, err
				}
				break
			}
		}
		for _, schema := range schemas {
			if len(results) >= a.cfg.Server.MaxSearchResults {
				break
			}
			if err := a.searchInSchema(ctx, connection, schema, query, seen, &results, a.cfg.Server.MaxSearchResults); err != nil {
				a.audit(p, connection, "search", "", nil, false, int64(len(results)), start, err)
				return nil, searchOutput{}, err
			}
		}
		a.audit(p, connection, "search", "", nil, true, int64(len(results)), start, nil)
	}

	return nil, searchOutput{Results: results}, nil
}

func (a *App) searchInSchema(ctx context.Context, connection, schema, query string, seen map[string]bool, results *[]searchResultItem, maxResults int) error {
	tables, err := a.store.ListTables(ctx, connection, schema)
	if err != nil {
		return err
	}

	for _, table := range tables {
		if len(*results) >= maxResults {
			return nil
		}
		tableRef := objectReference{Connection: connection, Schema: schema, Table: table.Name}
		tableID := encodeObjectID(tableRef)

		if strings.Contains(strings.ToLower(table.Name), query) || strings.Contains(strings.ToLower(schema), query) {
			if !seen[tableID] {
				seen[tableID] = true
				*results = append(*results, searchResultItem{
					ID:    tableID,
					Title: objectTitle(tableRef),
					URL:   a.sourceURL(connection, schema, table.Name, ""),
				})
			}
		}
		if len(*results) >= maxResults {
			return nil
		}

		desc, err := a.store.DescribeTable(ctx, connection, schema, table.Name)
		if err != nil {
			return err
		}

		for _, col := range desc.Columns {
			if len(*results) >= maxResults {
				return nil
			}
			colRef := objectReference{Connection: connection, Schema: schema, Table: table.Name, Column: col.Name}
			colID := encodeObjectID(colRef)
			if strings.Contains(strings.ToLower(col.Name), query) {
				if !seen[colID] {
					seen[colID] = true
					*results = append(*results, searchResultItem{
						ID:    colID,
						Title: objectTitle(colRef),
						URL:   a.sourceURL(connection, schema, table.Name, col.Name),
					})
				}
			}
		}
	}
	return nil
}

func (a *App) fetch(ctx context.Context, req *mcp.CallToolRequest, in fetchInput) (*mcp.CallToolResult, fetchOutput, error) {
	start := time.Now()
	p, err := a.principal(req)
	if err != nil {
		return nil, fetchOutput{}, err
	}
	if !p.HasScope("read") {
		return nil, fetchOutput{}, errors.New("read scope is required")
	}

	ref, err := decodeObjectID(in.ID)
	if err != nil && !strings.HasPrefix(in.ID, objectIDPrefix) {
		allowed := make([]string, 0, len(a.store.ConnectionNames()))
		for _, name := range a.store.ConnectionNames() {
			if p.CanUseConnection(name) {
				allowed = append(allowed, name)
			}
		}
		switch len(allowed) {
		case 1:
			ref, err = decodeLegacyObjectID(in.ID, allowed[0])
		case 0:
			// Preserve the original invalid-ID error when the store has no connections.
		default:
			err = errors.New("legacy object ID is ambiguous with multiple connections; call search to obtain a connection-scoped ID")
		}
	}
	if err != nil {
		return nil, fetchOutput{}, err
	}
	var connection string
	_, connection, err = a.authorize(req, ref.Connection, "read")
	if err != nil {
		return nil, fetchOutput{}, err
	}

	schema := ref.Schema
	table := ref.Table

	cfg, ok := a.store.ConnectionConfig(connection)
	if !ok {
		return nil, fetchOutput{}, fmt.Errorf("unknown connection: %s", connection)
	}

	if !cfg.SchemaAllowed(schema) {
		return nil, fetchOutput{}, fmt.Errorf("schema not allowed: %s", schema)
	}

	desc, err := a.store.DescribeTable(ctx, connection, schema, table)
	if err != nil {
		return nil, fetchOutput{}, err
	}

	var text strings.Builder
	text.WriteString(fmt.Sprintf("Table: %s.%s\n\n", schema, table))

	text.WriteString("Columns:\n")
	for _, col := range desc.Columns {
		pk := ""
		if col.IsPrimaryKey {
			pk = " [PRIMARY KEY]"
		}
		nullable := ""
		if !col.Nullable {
			nullable = " NOT NULL"
		}
		defaultVal := ""
		if col.Default != nil {
			defaultVal = " DEFAULT " + *col.Default
		}
		text.WriteString(fmt.Sprintf("  - %s %s%s%s%s\n", col.Name, col.Type, nullable, defaultVal, pk))
	}

	if len(desc.PrimaryKey) > 0 {
		text.WriteString(fmt.Sprintf("\nPrimary Key: %s\n", strings.Join(desc.PrimaryKey, ", ")))
	}

	if len(desc.ForeignKeys) > 0 {
		text.WriteString("\nForeign Keys:\n")
		for _, fk := range desc.ForeignKeys {
			text.WriteString(fmt.Sprintf("  - %s -> %s.%s.%s\n", fk.Column, fk.ForeignSchema, fk.ForeignTable, fk.ForeignColumn))
		}
	}

	if len(desc.Indexes) > 0 {
		text.WriteString("\nIndexes:\n")
		for _, idx := range desc.Indexes {
			unique := ""
			if idx.Unique {
				unique = " [UNIQUE]"
			}
			text.WriteString(fmt.Sprintf("  - %s%s: %s\n", idx.Name, unique, strings.Join(idx.Columns, ", ")))
		}
	}

	output := fetchOutput{
		ID:    encodeObjectID(ref),
		Title: objectTitle(ref),
		Text:  text.String(),
		URL:   a.sourceURL(connection, schema, table, ""),
		Metadata: map[string]string{
			"schema":     schema,
			"table":      table,
			"connection": connection,
		},
	}

	if ref.Column != "" {
		colName := ref.Column
		found := false
		for _, col := range desc.Columns {
			if col.Name == colName {
				output.ID = encodeObjectID(ref)
				output.Title = objectTitle(ref)
				output.Text = fmt.Sprintf("Column: %s\nType: %s\nNullable: %v\nPrimary Key: %v\n",
					col.Name, col.Type, col.Nullable, col.IsPrimaryKey)
				if col.Default != nil {
					output.Text += fmt.Sprintf("Default: %s\n", *col.Default)
				}
				if col.Comment != nil {
					output.Text += fmt.Sprintf("Comment: %s\n", *col.Comment)
				}
				output.URL = a.sourceURL(connection, schema, table, colName)
				output.Metadata["column"] = colName
				found = true
				break
			}
		}
		if !found {
			return nil, fetchOutput{}, fmt.Errorf("column not found: %s", colName)
		}
	}

	a.audit(p, connection, "fetch", "", []string{schema + "." + table}, true, 0, start, nil)
	return nil, output, nil
}

func (a *App) sourceHTTPHandler(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/sources" && r.URL.Path != "/sources/" {
		http.NotFound(w, r)
		return
	}
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	query := r.URL.Query()
	signed := a.validCitationURL(query)
	var p authn.Principal
	var authenticated bool
	if a.auth != nil {
		p, authenticated = a.auth.AuthenticateHeader(r.Header.Get("Authorization"))
	}
	if !signed && (!authenticated || !p.HasScope("read")) {
		w.Header().Set("WWW-Authenticate", a.authChallenge())
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	connection := query.Get("connection")
	schema := query.Get("schema")
	table := query.Get("table")
	column := query.Get("column")
	if connection == "" || schema == "" || table == "" || (!signed && !p.CanUseConnection(connection)) {
		http.Error(w, "invalid or unauthorized source", http.StatusNotFound)
		return
	}
	cfg, ok := a.store.ConnectionConfig(connection)
	if !ok || !cfg.SchemaAllowed(schema) {
		http.Error(w, "source not found", http.StatusNotFound)
		return
	}
	desc, err := a.store.DescribeTable(r.Context(), connection, schema, table)
	if err != nil {
		http.Error(w, "source not found", http.StatusNotFound)
		return
	}
	if column != "" {
		found := false
		for _, item := range desc.Columns {
			if item.Name == column {
				found = true
				break
			}
		}
		if !found {
			http.Error(w, "source not found", http.StatusNotFound)
			return
		}
	}

	ref := objectReference{Connection: connection, Schema: schema, Table: table, Column: column}
	id := encodeObjectID(ref)
	title := objectTitle(ref)
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "private, no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(map[string]any{
		`id`:          id,
		`title`:       title,
		`url`:         a.sourceURL(connection, schema, table, column),
		`connection`:  connection,
		`schema`:      schema,
		`table`:       table,
		`column`:      column,
		`description`: desc,
	})
}

func (a *App) readResource(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
	p, err := a.resourcePrincipal(req)
	if err != nil {
		return nil, err
	}
	uri := req.Params.URI
	if uri == "postgres://connections" {
		names := []string{}
		for _, name := range a.store.ConnectionNames() {
			if p.CanUseConnection(name) {
				names = append(names, name)
			}
		}
		return resourceJSON(uri, listConnectionsOutput{Connections: names})
	}
	parsed, err := url.Parse(uri)
	if err != nil {
		return nil, mcp.ResourceNotFoundError(uri)
	}
	connection := parsed.Host
	if !p.HasScope("read") || !p.CanUseConnection(connection) {
		return nil, errors.New("read scope and connection access are required")
	}
	parts := strings.Split(strings.Trim(parsed.Path, "/"), "/")
	switch {
	case len(parts) == 1 && parts[0] == "schema":
		out, err := a.store.SchemaSummary(ctx, connection)
		if err != nil {
			return nil, err
		}
		return resourceJSON(uri, out)
	case len(parts) == 1 && parts[0] == "schemas":
		out, err := a.store.ListSchemas(ctx, connection)
		if err != nil {
			return nil, err
		}
		return resourceJSON(uri, map[string]any{"connection_name": connection, "schemas": out})
	case len(parts) == 1 && parts[0] == "tables":
		all := []postgres.TableInfo{}
		cfg, ok := a.store.ConnectionConfig(connection)
		if !ok {
			return nil, mcp.ResourceNotFoundError(uri)
		}
		schemas := cfg.Schemas
		for _, schema := range schemas {
			if schema == "*" {
				schemas, err = a.store.ListSchemas(ctx, connection)
				if err != nil {
					return nil, err
				}
				break
			}
		}
		for _, schema := range schemas {
			tables, err := a.store.ListTables(ctx, connection, schema)
			if err != nil {
				return nil, err
			}
			all = append(all, tables...)
		}
		return resourceJSON(uri, map[string]any{"connection_name": connection, "tables": all})
	case len(parts) == 3 && parts[0] == "table":
		out, err := a.store.DescribeTable(ctx, connection, parts[1], parts[2])
		if err != nil {
			return nil, err
		}
		return resourceJSON(uri, out)
	default:
		return nil, mcp.ResourceNotFoundError(uri)
	}
}

func (a *App) authorize(req *mcp.CallToolRequest, requestedConnection, scope string) (authn.Principal, string, error) {
	p, err := a.principal(req)
	if err != nil {
		return authn.Principal{}, "", err
	}
	if !p.HasScope(scope) {
		return authn.Principal{}, "", fmt.Errorf("%s scope is required", scope)
	}
	connection, err := a.resolveConnection(requestedConnection)
	if err != nil {
		return authn.Principal{}, "", err
	}
	if !p.CanUseConnection(connection) {
		return authn.Principal{}, "", fmt.Errorf("token cannot access connection: %s", connection)
	}
	return p, connection, nil
}

func (a *App) principal(req *mcp.CallToolRequest) (authn.Principal, error) {
	if req == nil || req.GetExtra() == nil {
		return authn.Principal{}, errors.New("missing request metadata")
	}
	if a.auth == nil {
		return authn.Principal{}, errors.New("unauthorized")
	}
	p, ok := a.auth.AuthenticateHeader(req.GetExtra().Header.Get("Authorization"))
	if !ok {
		return authn.Principal{}, errors.New("unauthorized")
	}
	return p, nil
}

func (a *App) resourcePrincipal(req *mcp.ReadResourceRequest) (authn.Principal, error) {
	if req == nil || req.GetExtra() == nil {
		return authn.Principal{}, errors.New("missing request metadata")
	}
	if a.auth == nil {
		return authn.Principal{}, errors.New("unauthorized")
	}
	p, ok := a.auth.AuthenticateHeader(req.GetExtra().Header.Get("Authorization"))
	if !ok {
		return authn.Principal{}, errors.New("unauthorized")
	}
	return p, nil
}

func (a *App) authChallenge() string {
	if a.cfg != nil && a.cfg.OAuth.Enabled && strings.TrimSpace(a.cfg.OAuth.Issuer) != "" {
		return fmt.Sprintf(`Bearer resource_metadata="%s/.well-known/oauth-protected-resource"`, strings.TrimRight(a.cfg.OAuth.Issuer, "/"))
	}
	return `Bearer realm="postgres-mcp"`
}

func (a *App) resolveConnection(requested string) (string, error) {
	if requested != "" {
		if _, ok := a.store.ConnectionConfig(requested); !ok {
			return "", fmt.Errorf("unknown connection: %s", requested)
		}
		return requested, nil
	}
	names := a.store.ConnectionNames()
	if len(names) == 1 {
		return names[0], nil
	}
	return "", errors.New("connection_name is required when multiple connections are configured")
}

func (a *App) audit(p authn.Principal, connection, tool, fp string, tables []string, allowed bool, rows int64, start time.Time, err error) {
	msg := ""
	if err != nil {
		msg = err.Error()
	}
	a.auditor.Record(audit.Event{
		Principal:      p.Name,
		Connection:     connection,
		Tool:           tool,
		SQLFingerprint: fp,
		Tables:         tables,
		Allowed:        allowed,
		Rows:           rows,
		DurationMillis: time.Since(start).Milliseconds(),
		Error:          msg,
	})
}

func resourceJSON(uri string, value any) (*mcp.ReadResourceResult, error) {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return nil, err
	}
	return &mcp.ReadResourceResult{Contents: []*mcp.ResourceContents{{
		URI:      uri,
		MIMEType: "application/json",
		Text:     string(data),
	}}}, nil
}

func (a *App) sourceURL(connection, schema, table, column string) string {
	base, err := url.Parse(a.cfg.Server.PublicBaseURL)
	if err != nil {
		return ""
	}
	base.Path = strings.TrimRight(base.Path, "/") + "/sources"
	values := base.Query()
	values.Set("connection", connection)
	values.Set("schema", schema)
	values.Set("table", table)
	if column != "" {
		values.Set("column", column)
	}
	if a.cfg.Server.CitationSigningKey != "" {
		values.Set("expires", fmt.Sprintf("%d", time.Now().Add(a.cfg.Server.CitationTTLDuration()).Unix()))
		values.Set("signature", a.citationSignature(values))
	}
	base.RawQuery = values.Encode()
	return base.String()
}

func (a *App) citationSignature(values url.Values) string {
	canonical := url.Values{}
	for _, key := range []string{"connection", "schema", "table", "column", "expires"} {
		if value := values.Get(key); value != "" {
			canonical.Set(key, value)
		}
	}
	mac := hmac.New(sha256.New, []byte(a.cfg.Server.CitationSigningKey))
	_, _ = mac.Write([]byte(canonical.Encode()))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

func (a *App) validCitationURL(values url.Values) bool {
	if a.cfg == nil || a.cfg.Server.CitationSigningKey == "" || values.Get("signature") == "" {
		return false
	}
	expires, err := strconv.ParseInt(values.Get("expires"), 10, 64)
	if err != nil || expires < time.Now().Unix() {
		return false
	}
	expected := a.citationSignature(values)
	provided, err := base64.RawURLEncoding.DecodeString(values.Get("signature"))
	if err != nil {
		return false
	}
	expectedBytes, err := base64.RawURLEncoding.DecodeString(expected)
	if err != nil {
		return false
	}
	return hmac.Equal(provided, expectedBytes)
}

func fingerprint(sql string) string {
	normalized := strings.Join(strings.Fields(sql), " ")
	sum := sha256.Sum256([]byte(strings.ToLower(normalized)))
	return hex.EncodeToString(sum[:])
}
