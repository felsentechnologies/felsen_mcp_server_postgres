package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/masking"
	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/sqlguard"
)

type Store struct {
	cfg   *config.Config
	conns map[string]*database
}

type database struct {
	cfg    config.ConnectionConfig
	pool   *pgxpool.Pool
	masker *masking.Masker
	mu     sync.RWMutex
	cache  map[string]TableDescription
}

func NewStore(ctx context.Context, cfg *config.Config) (*Store, error) {
	store := &Store{cfg: cfg, conns: map[string]*database{}}
	for name, connCfg := range cfg.Connections {
		poolCfg, err := pgxpool.ParseConfig(connCfg.DSN)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		pool, err := pgxpool.NewWithConfig(ctx, poolCfg)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		pingCtx, cancel := context.WithTimeout(ctx, connCfg.QueryTimeoutDuration())
		err = pool.Ping(pingCtx)
		cancel()
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("%s ping: %w", name, err)
		}
		masker, err := masking.New(connCfg.Masking)
		if err != nil {
			pool.Close()
			return nil, fmt.Errorf("%s masking: %w", name, err)
		}
		store.conns[name] = &database{cfg: connCfg, pool: pool, masker: masker, cache: map[string]TableDescription{}}
	}
	return store, nil
}

func (s *Store) Close() {
	for _, conn := range s.conns {
		conn.pool.Close()
	}
}

func (s *Store) ConnectionNames() []string {
	names := make([]string, 0, len(s.conns))
	for name := range s.conns {
		names = append(names, name)
	}
	return names
}

func (s *Store) ConnectionConfig(name string) (config.ConnectionConfig, bool) {
	db, ok := s.conns[name]
	if !ok {
		return config.ConnectionConfig{}, false
	}
	return db.cfg, true
}

func (s *Store) ListSchemas(ctx context.Context, connection string) ([]string, error) {
	db, err := s.db(connection)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, db.cfg.QueryTimeoutDuration())
	defer cancel()

	rows, err := db.pool.Query(ctx, `select schema_name from information_schema.schemata order by schema_name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var schemas []string
	for rows.Next() {
		var schema string
		if err := rows.Scan(&schema); err != nil {
			return nil, err
		}
		if db.cfg.SchemaAllowed(schema) {
			schemas = append(schemas, schema)
		}
	}
	return schemas, rows.Err()
}

func (s *Store) SchemaSummary(ctx context.Context, connection string) (SchemaSummary, error) {
	db, err := s.db(connection)
	if err != nil {
		return SchemaSummary{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, db.cfg.QueryTimeoutDuration())
	defer cancel()

	rows, err := db.pool.Query(ctx, `
		select table_schema, count(*)
		from information_schema.tables
		where table_schema = any($1)
		group by table_schema
		order by table_schema
	`, db.cfg.Schemas)
	if err != nil {
		return SchemaSummary{}, err
	}
	defer rows.Close()
	summary := SchemaSummary{Connection: connection, IntrospectedAt: time.Now().UTC().Format(time.RFC3339)}
	for rows.Next() {
		var item SchemaCount
		if err := rows.Scan(&item.Name, &item.TableCount); err != nil {
			return SchemaSummary{}, err
		}
		summary.Schemas = append(summary.Schemas, item)
	}
	return summary, rows.Err()
}

func (s *Store) ListTables(ctx context.Context, connection, schema string) ([]TableInfo, error) {
	db, err := s.db(connection)
	if err != nil {
		return nil, err
	}
	if !db.cfg.SchemaAllowed(schema) {
		return nil, fmt.Errorf("schema is not allowed: %s", schema)
	}
	ctx, cancel := context.WithTimeout(ctx, db.cfg.QueryTimeoutDuration())
	defer cancel()

	rows, err := db.pool.Query(ctx, `
		select table_schema, table_name, table_type
		from information_schema.tables
		where table_schema = $1
		order by table_name
	`, schema)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tables []TableInfo
	for rows.Next() {
		var table TableInfo
		if err := rows.Scan(&table.Schema, &table.Name, &table.Type); err != nil {
			return nil, err
		}
		tables = append(tables, table)
	}
	return tables, rows.Err()
}

func (s *Store) DescribeTable(ctx context.Context, connection, schema, table string) (TableDescription, error) {
	db, err := s.db(connection)
	if err != nil {
		return TableDescription{}, err
	}
	if !db.cfg.SchemaAllowed(schema) {
		return TableDescription{}, fmt.Errorf("schema is not allowed: %s", schema)
	}
	cacheKey := schema + "." + table
	db.mu.RLock()
	if desc, ok := db.cache[cacheKey]; ok {
		db.mu.RUnlock()
		return desc, nil
	}
	db.mu.RUnlock()

	ctx, cancel := context.WithTimeout(ctx, db.cfg.QueryTimeoutDuration())
	defer cancel()

	desc := TableDescription{Schema: schema, Table: table}
	pk, err := db.primaryKey(ctx, schema, table)
	if err != nil {
		return TableDescription{}, err
	}
	desc.PrimaryKey = pk
	pkSet := map[string]bool{}
	for _, col := range pk {
		pkSet[col] = true
	}
	columns, err := db.columns(ctx, schema, table, pkSet)
	if err != nil {
		return TableDescription{}, err
	}
	desc.Columns = columns
	fks, err := db.foreignKeys(ctx, schema, table)
	if err != nil {
		return TableDescription{}, err
	}
	desc.ForeignKeys = fks
	indexes, err := db.indexes(ctx, schema, table)
	if err != nil {
		return TableDescription{}, err
	}
	desc.Indexes = indexes

	db.mu.Lock()
	db.cache[cacheKey] = desc
	db.mu.Unlock()
	return desc, nil
}

func (s *Store) SampleRows(ctx context.Context, connection, schema, table string, limit int) (QueryResult, error) {
	db, err := s.db(connection)
	if err != nil {
		return QueryResult{}, err
	}
	if !db.cfg.SchemaAllowed(schema) {
		return QueryResult{}, fmt.Errorf("schema is not allowed: %s", schema)
	}
	if limit <= 0 || limit > db.cfg.MaxRows {
		limit = db.cfg.MaxRows
	}
	sql := "select * from " + pgx.Identifier{schema, table}.Sanitize() + " limit $1"
	return db.query(ctx, sql, limit)
}

func (s *Store) ExecuteSQL(ctx context.Context, connection, sql string) (QueryResult, sqlguard.ValidationResult, error) {
	db, err := s.db(connection)
	if err != nil {
		return QueryResult{}, sqlguard.ValidationResult{}, err
	}
	validation := sqlguard.Validate(sql, db.cfg, sqlguard.ModeRead)
	if !validation.Valid {
		return QueryResult{}, validation, errors.New(validation.Reason)
	}
	sql = sqlguard.ApplySelectLimit(sql, db.cfg.MaxRows)
	result, err := db.query(ctx, sql)
	return result, validation, err
}

func (s *Store) ExecuteDML(ctx context.Context, connection, sql string) (DMLResult, sqlguard.ValidationResult, error) {
	db, err := s.db(connection)
	if err != nil {
		return DMLResult{}, sqlguard.ValidationResult{}, err
	}
	validation := sqlguard.Validate(sql, db.cfg, sqlguard.ModeDML)
	if !validation.Valid {
		return DMLResult{}, validation, errors.New(validation.Reason)
	}
	ctx, cancel := context.WithTimeout(ctx, db.cfg.QueryTimeoutDuration())
	defer cancel()
	tag, err := db.pool.Exec(ctx, sql)
	if err != nil {
		return DMLResult{}, validation, err
	}
	return DMLResult{RowsAffected: tag.RowsAffected()}, validation, nil
}

func (s *Store) ExplainSQL(ctx context.Context, connection, sql string) (QueryResult, sqlguard.ValidationResult, error) {
	db, err := s.db(connection)
	if err != nil {
		return QueryResult{}, sqlguard.ValidationResult{}, err
	}
	validation := sqlguard.Validate(sql, db.cfg, sqlguard.ModeExplain)
	if !validation.Valid {
		return QueryResult{}, validation, errors.New(validation.Reason)
	}
	result, err := db.query(ctx, "explain (format json) "+strings.TrimRight(strings.TrimSpace(sql), ";"))
	return result, validation, err
}

func (s *Store) ExecuteDDL(ctx context.Context, connection, sql string) (DDLResult, sqlguard.ValidationResult, error) {
	db, err := s.db(connection)
	if err != nil {
		return DDLResult{}, sqlguard.ValidationResult{}, err
	}
	validation := sqlguard.Validate(sql, db.cfg, sqlguard.ModeDDL)
	if !validation.Valid {
		return DDLResult{}, validation, errors.New(validation.Reason)
	}
	ctx, cancel := context.WithTimeout(ctx, db.cfg.QueryTimeoutDuration())
	defer cancel()
	_, err = db.pool.Exec(ctx, sql)
	if err != nil {
		return DDLResult{}, validation, err
	}
	op := validation.Operation
	tables := validation.TablesDetected
	msg := fmt.Sprintf("DDL %s executed successfully", op)
	if len(tables) > 0 {
		msg += " on " + strings.Join(tables, ", ")
	}
	db.mu.Lock()
	for _, t := range tables {
		delete(db.cache, t)
	}
	db.mu.Unlock()
	return DDLResult{Message: msg}, validation, nil
}

func (s *Store) RefreshSchemaCache(connection string) error {
	db, err := s.db(connection)
	if err != nil {
		return err
	}
	db.mu.Lock()
	db.cache = map[string]TableDescription{}
	db.mu.Unlock()
	return nil
}

func (s *Store) db(connection string) (*database, error) {
	db, ok := s.conns[connection]
	if !ok {
		return nil, fmt.Errorf("unknown connection: %s", connection)
	}
	return db, nil
}

func (db *database) columns(ctx context.Context, schema, table string, pk map[string]bool) ([]ColumnInfo, error) {
	rows, err := db.pool.Query(ctx, `
		select c.column_name,
		       c.data_type,
		       c.is_nullable = 'YES',
		       c.column_default,
		       pg_catalog.col_description((quote_ident(c.table_schema)||'.'||quote_ident(c.table_name))::regclass::oid, c.ordinal_position)
		from information_schema.columns c
		where c.table_schema = $1 and c.table_name = $2
		order by c.ordinal_position
	`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []ColumnInfo
	for rows.Next() {
		var col ColumnInfo
		if err := rows.Scan(&col.Name, &col.Type, &col.Nullable, &col.Default, &col.Comment); err != nil {
			return nil, err
		}
		col.IsPrimaryKey = pk[col.Name]
		columns = append(columns, col)
	}
	return columns, rows.Err()
}

func (db *database) primaryKey(ctx context.Context, schema, table string) ([]string, error) {
	rows, err := db.pool.Query(ctx, `
		select kcu.column_name
		from information_schema.table_constraints tc
		join information_schema.key_column_usage kcu
		  on tc.constraint_schema = kcu.constraint_schema
		 and tc.constraint_name = kcu.constraint_name
		 and tc.table_schema = kcu.table_schema
		 and tc.table_name = kcu.table_name
		where tc.constraint_type = 'PRIMARY KEY'
		  and tc.table_schema = $1
		  and tc.table_name = $2
		order by kcu.ordinal_position
	`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var pk []string
	for rows.Next() {
		var col string
		if err := rows.Scan(&col); err != nil {
			return nil, err
		}
		pk = append(pk, col)
	}
	return pk, rows.Err()
}

func (db *database) foreignKeys(ctx context.Context, schema, table string) ([]ForeignKeyInfo, error) {
	rows, err := db.pool.Query(ctx, `
		select tc.constraint_name,
		       kcu.column_name,
		       ccu.table_schema,
		       ccu.table_name,
		       ccu.column_name
		from information_schema.table_constraints tc
		join information_schema.key_column_usage kcu
		  on tc.constraint_schema = kcu.constraint_schema
		 and tc.constraint_name = kcu.constraint_name
		join information_schema.constraint_column_usage ccu
		  on ccu.constraint_schema = tc.constraint_schema
		 and ccu.constraint_name = tc.constraint_name
		where tc.constraint_type = 'FOREIGN KEY'
		  and tc.table_schema = $1
		  and tc.table_name = $2
		order by tc.constraint_name, kcu.ordinal_position
	`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var fks []ForeignKeyInfo
	for rows.Next() {
		var fk ForeignKeyInfo
		if err := rows.Scan(&fk.Name, &fk.Column, &fk.ForeignSchema, &fk.ForeignTable, &fk.ForeignColumn); err != nil {
			return nil, err
		}
		fks = append(fks, fk)
	}
	return fks, rows.Err()
}

func (db *database) indexes(ctx context.Context, schema, table string) ([]IndexInfo, error) {
	rows, err := db.pool.Query(ctx, `
		select i.relname,
		       ix.indisunique,
		       pg_get_indexdef(ix.indexrelid)
		from pg_class t
		join pg_namespace n on n.oid = t.relnamespace
		join pg_index ix on ix.indrelid = t.oid
		join pg_class i on i.oid = ix.indexrelid
		where n.nspname = $1 and t.relname = $2
		order by i.relname
	`, schema, table)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var indexes []IndexInfo
	for rows.Next() {
		var idx IndexInfo
		if err := rows.Scan(&idx.Name, &idx.Unique, &idx.Definition); err != nil {
			return nil, err
		}
		idx.Columns = parseIndexColumns(idx.Definition)
		indexes = append(indexes, idx)
	}
	return indexes, rows.Err()
}

func (db *database) query(ctx context.Context, sql string, args ...any) (QueryResult, error) {
	ctx, cancel := context.WithTimeout(ctx, db.cfg.QueryTimeoutDuration())
	defer cancel()
	rows, err := db.pool.Query(ctx, sql, args...)
	if err != nil {
		return QueryResult{}, err
	}
	defer rows.Close()

	fields := rows.FieldDescriptions()
	columns := make([]string, len(fields))
	for i, field := range fields {
		columns[i] = field.Name
	}
	result := QueryResult{Columns: columns}
	for rows.Next() {
		values, err := rows.Values()
		if err != nil {
			return QueryResult{}, err
		}
		values = jsonSafeValues(values)
		values = db.masker.MaskRow(columns, values)
		result.Rows = append(result.Rows, values)
	}
	if err := rows.Err(); err != nil {
		return QueryResult{}, err
	}
	result.RowCount = len(result.Rows)
	return result, nil
}

func jsonSafeValues(values []any) []any {
	out := make([]any, len(values))
	for i, value := range values {
		switch v := value.(type) {
		case []byte:
			out[i] = string(v)
		case time.Time:
			out[i] = v.Format(time.RFC3339Nano)
		default:
			out[i] = v
		}
	}
	return out
}

var indexColumnsPattern = regexp.MustCompile(`\((.*)\)`)

func parseIndexColumns(definition string) []string {
	match := indexColumnsPattern.FindStringSubmatch(definition)
	if len(match) < 2 {
		return nil
	}
	raw := strings.Split(match[1], ",")
	columns := make([]string, 0, len(raw))
	for _, col := range raw {
		col = strings.TrimSpace(strings.Trim(col, `"`))
		if col != "" {
			columns = append(columns, col)
		}
	}
	return columns
}
