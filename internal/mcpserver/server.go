package mcpserver

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/audit"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/authn"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/postgres"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/sqlguard"
)

type App struct {
	cfg     *config.Config
	store   *postgres.Store
	auth    *authn.Manager
	auditor *audit.Auditor
	logger  *slog.Logger
	server  *mcp.Server
}

func New(cfg *config.Config, store *postgres.Store, auth *authn.Manager, auditor *audit.Auditor, logger *slog.Logger) http.Handler {
	app := &App{cfg: cfg, store: store, auth: auth, auditor: auditor, logger: logger}
	app.server = mcp.NewServer(&mcp.Implementation{Name: "postgres-mcp", Version: "v0.2.0"}, nil)
	app.registerResources()
	app.registerTools()

	mux := http.NewServeMux()

	streamableHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server {
		return app.server
	}, &mcp.StreamableHTTPOptions{
		JSONResponse:   cfg.Server.JSONResponse,
		Stateless:      cfg.Server.Stateless,
		SessionTimeout: cfg.Server.SessionTimeoutDuration(),
		Logger:         logger,
	})

	sseHandler := mcp.NewSSEHandler(func(*http.Request) *mcp.Server {
		return app.server
	}, nil)

	mux.Handle("/mcp", streamableHandler)
	mux.Handle("/sse", sseHandler)
	mux.Handle("/sse/", sseHandler)

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
	openWorld := false
	destructive := true
	mcp.AddTool(a.server, &mcp.Tool{Name: "list_connections", Description: "List named Postgres connections allowed for this token.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.listConnections)
	mcp.AddTool(a.server, &mcp.Tool{Name: "list_schemas", Description: "List allowed schemas in a Postgres connection.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.listSchemas)
	mcp.AddTool(a.server, &mcp.Tool{Name: "list_tables", Description: "List tables and views for a schema.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.listTables)
	mcp.AddTool(a.server, &mcp.Tool{Name: "describe_table", Description: "Describe columns, primary key, foreign keys and indexes for a table.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.describeTable)
	mcp.AddTool(a.server, &mcp.Tool{Name: "sample_rows", Description: "Return a limited, masked sample of table rows.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.sampleRows)
	mcp.AddTool(a.server, &mcp.Tool{Name: "validate_sql", Description: "Validate SQL against read-only or DML policies without executing it.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.validateSQL)
	mcp.AddTool(a.server, &mcp.Tool{Name: "execute_sql", Description: "Execute a validated SELECT query with configured row limits and masking.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: readOnly, OpenWorldHint: &openWorld}}, a.executeSQL)
	mcp.AddTool(a.server, &mcp.Tool{Name: "execute_dml", Description: "Execute INSERT, UPDATE or DELETE only when explicitly allowlisted.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &destructive, OpenWorldHint: &openWorld}}, a.executeDML)
	mcp.AddTool(a.server, &mcp.Tool{Name: "explain_sql", Description: "Run EXPLAIN (FORMAT JSON) for a validated SELECT query.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.explainSQL)
	mcp.AddTool(a.server, &mcp.Tool{Name: "execute_ddl", Description: "Execute DDL statements (CREATE, ALTER, DROP, TRUNCATE) when DDL is enabled for the connection.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: false, DestructiveHint: &destructive, OpenWorldHint: &openWorld}}, a.executeDDL)
	mcp.AddTool(a.server, &mcp.Tool{Name: "refresh_schema_cache", Description: "Clear cached schema descriptions for a connection.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.refreshSchemaCache)
	mcp.AddTool(a.server, &mcp.Tool{Name: "search", Description: "Search for tables, columns, and database objects matching a query.", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.search)
	mcp.AddTool(a.server, &mcp.Tool{Name: "fetch", Description: "Fetch detailed information about a database object by ID (schema.table or schema.table.column).", Annotations: &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: &openWorld}}, a.fetch)
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
	Query          string `json:"query" jsonschema:"search query to find tables, columns, or database objects"`
	ConnectionName string `json:"connection_name,omitempty"`
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
	ID             string `json:"id" jsonschema:"object ID in format schema.table or schema.table.column"`
	ConnectionName string `json:"connection_name,omitempty"`
}

type fetchOutput struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	Text     string            `json:"text"`
	URL      string            `json:"url"`
	Metadata map[string]string `json:"metadata,omitempty"`
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
	p, connection, err := a.authorize(req, in.ConnectionName, "read")
	if err != nil {
		return nil, searchOutput{}, err
	}

	query := strings.ToLower(strings.TrimSpace(in.Query))
	if query == "" {
		return nil, searchOutput{Results: []searchResultItem{}}, nil
	}

	cfg, ok := a.store.ConnectionConfig(connection)
	if !ok {
		return nil, searchOutput{}, fmt.Errorf("unknown connection: %s", connection)
	}

	var results []searchResultItem
	seen := map[string]bool{}

	for _, schema := range cfg.Schemas {
		if schema == "*" {
			schemas, err := a.store.ListSchemas(ctx, connection)
			if err != nil {
				continue
			}
			for _, s := range schemas {
				a.searchInSchema(ctx, connection, s, query, seen, &results)
			}
		} else {
			a.searchInSchema(ctx, connection, schema, query, seen, &results)
		}
	}

	a.audit(p, connection, "search", "", nil, true, int64(len(results)), start, nil)
	return nil, searchOutput{Results: results}, nil
}

func (a *App) searchInSchema(ctx context.Context, connection, schema, query string, seen map[string]bool, results *[]searchResultItem) {
	tables, err := a.store.ListTables(ctx, connection, schema)
	if err != nil {
		return
	}

	for _, table := range tables {
		tableID := schema + "." + table.Name
		title := table.Name

		if strings.Contains(strings.ToLower(table.Name), query) || strings.Contains(strings.ToLower(schema), query) {
			if !seen[tableID] {
				seen[tableID] = true
				*results = append(*results, searchResultItem{
					ID:    tableID,
					Title: title,
					URL:   fmt.Sprintf("postgres://%s/%s/%s", connection, schema, table.Name),
				})
			}
		}

		desc, err := a.store.DescribeTable(ctx, connection, schema, table.Name)
		if err != nil {
			continue
		}

		for _, col := range desc.Columns {
			colID := tableID + "." + col.Name
			if strings.Contains(strings.ToLower(col.Name), query) {
				if !seen[colID] {
					seen[colID] = true
					*results = append(*results, searchResultItem{
						ID:    colID,
						Title: table.Name + "." + col.Name,
						URL:   fmt.Sprintf("postgres://%s/%s/%s#%s", connection, schema, table.Name, col.Name),
					})
				}
			}
		}
	}
}

func (a *App) fetch(ctx context.Context, req *mcp.CallToolRequest, in fetchInput) (*mcp.CallToolResult, fetchOutput, error) {
	start := time.Now()
	p, connection, err := a.authorize(req, in.ConnectionName, "read")
	if err != nil {
		return nil, fetchOutput{}, err
	}

	parts := strings.Split(in.ID, ".")
	if len(parts) < 2 || len(parts) > 3 {
		return nil, fetchOutput{}, fmt.Errorf("invalid ID format: expected schema.table or schema.table.column, got: %s", in.ID)
	}

	schema := parts[0]
	table := parts[1]

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
		ID:    in.ID,
		Title: schema + "." + table,
		Text:  text.String(),
		URL:   fmt.Sprintf("postgres://%s/%s/%s", connection, schema, table),
		Metadata: map[string]string{
			"schema":    schema,
			"table":     table,
			"connection": connection,
		},
	}

	if len(parts) == 3 {
		colName := parts[2]
		found := false
		for _, col := range desc.Columns {
			if col.Name == colName {
				output.ID = in.ID
				output.Title = schema + "." + table + "." + colName
				output.Text = fmt.Sprintf("Column: %s\nType: %s\nNullable: %v\nPrimary Key: %v\n",
					col.Name, col.Type, col.Nullable, col.IsPrimaryKey)
				if col.Default != nil {
					output.Text += fmt.Sprintf("Default: %s\n", *col.Default)
				}
				if col.Comment != nil {
					output.Text += fmt.Sprintf("Comment: %s\n", *col.Comment)
				}
				output.URL = fmt.Sprintf("postgres://%s/%s/%s#%s", connection, schema, table, colName)
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
		for _, schema := range cfg.Schemas {
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
	p, ok := a.auth.AuthenticateHeader(req.GetExtra().Header.Get("Authorization"))
	if !ok {
		return authn.Principal{}, errors.New("unauthorized")
	}
	return p, nil
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

func fingerprint(sql string) string {
	normalized := strings.Join(strings.Fields(sql), " ")
	sum := sha256.Sum256([]byte(strings.ToLower(normalized)))
	return hex.EncodeToString(sum[:])
}
