package postgres

type SchemaSummary struct {
	Connection     string        `json:"connection"`
	Schemas        []SchemaCount `json:"schemas"`
	IntrospectedAt string        `json:"introspected_at"`
}

type SchemaCount struct {
	Name       string `json:"name"`
	TableCount int    `json:"table_count"`
}

type TableInfo struct {
	Schema string `json:"schema,omitempty"`
	Name   string `json:"name"`
	Type   string `json:"type"`
}

type ColumnInfo struct {
	Name         string  `json:"name"`
	Type         string  `json:"type"`
	Nullable     bool    `json:"nullable"`
	Default      *string `json:"default"`
	IsPrimaryKey bool    `json:"is_primary_key"`
	Comment      *string `json:"comment,omitempty"`
}

type ForeignKeyInfo struct {
	Name          string `json:"name"`
	Column        string `json:"column"`
	ForeignSchema string `json:"foreign_schema"`
	ForeignTable  string `json:"foreign_table"`
	ForeignColumn string `json:"foreign_column"`
}

type IndexInfo struct {
	Name       string   `json:"name"`
	Columns    []string `json:"columns,omitempty"`
	Unique     bool     `json:"unique"`
	Definition string   `json:"definition"`
}

type TableDescription struct {
	Schema      string           `json:"schema"`
	Table       string           `json:"table"`
	Columns     []ColumnInfo     `json:"columns"`
	PrimaryKey  []string         `json:"primary_key"`
	ForeignKeys []ForeignKeyInfo `json:"foreign_keys"`
	Indexes     []IndexInfo      `json:"indexes"`
}

type QueryResult struct {
	Columns  []string `json:"columns"`
	Rows     [][]any  `json:"rows"`
	RowCount int      `json:"row_count"`
}

type DMLResult struct {
	RowsAffected int64 `json:"rows_affected"`
}
