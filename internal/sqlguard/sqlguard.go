package sqlguard

import (
	"regexp"
	"strings"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

type Mode string

const (
	ModeRead    Mode = "read"
	ModeDML     Mode = "dml"
	ModeExplain Mode = "explain"
)

type ValidationResult struct {
	Valid          bool     `json:"valid"`
	ReadOnly       bool     `json:"read_only"`
	Operation      string   `json:"operation"`
	TablesDetected []string `json:"tables_detected"`
	Warnings       []string `json:"warnings"`
	Reason         string   `json:"reason,omitempty"`
}

var tablePattern = regexp.MustCompile(`(?is)\b(?:from|join|into|update|delete\s+from)\s+((?:"[^"]+"|[a-zA-Z_][\w$]*)(?:\s*\.\s*(?:"[^"]+"|[a-zA-Z_][\w$]*))?)`)

func Validate(sql string, cfg config.ConnectionConfig, mode Mode) ValidationResult {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return invalid("sql is required")
	}
	stmts, err := splitStatements(sql)
	if err != nil {
		return invalid(err.Error())
	}
	if len(stmts) != 1 {
		return invalid("exactly one SQL statement is allowed")
	}

	op := operation(sql)
	readOnly := op == "select"
	switch {
	case op == "select", op == "insert", op == "update", op == "delete":
	default:
		return invalid("only SELECT, INSERT, UPDATE and DELETE are supported")
	}

	tables := DetectTables(sql)
	for _, table := range tables {
		schema, name := SplitTable(table)
		if !cfg.SchemaAllowed(schema) {
			return ValidationResult{Valid: false, ReadOnly: readOnly, Operation: op, TablesDetected: tables, Reason: "schema is not allowed: " + schema}
		}
		if mode == ModeDML && !cfg.DMLAllowed(schema, name, op) {
			return ValidationResult{Valid: false, ReadOnly: readOnly, Operation: op, TablesDetected: tables, Reason: "DML is not allowed for " + schema + "." + name}
		}
	}

	if mode == ModeRead && op != "select" {
		return ValidationResult{Valid: false, ReadOnly: readOnly, Operation: op, TablesDetected: tables, Reason: "read mode allows only SELECT"}
	}
	if mode == ModeExplain && op != "select" {
		return ValidationResult{Valid: false, ReadOnly: readOnly, Operation: op, TablesDetected: tables, Reason: "explain_sql allows only SELECT"}
	}
	if mode == ModeDML && readOnly {
		return ValidationResult{Valid: false, ReadOnly: readOnly, Operation: op, TablesDetected: tables, Reason: "execute_dml requires INSERT, UPDATE or DELETE"}
	}

	warnings := []string{}
	if op == "select" && !HasLimit(sql) {
		warnings = append(warnings, "query has no LIMIT; execution will be wrapped with a configured limit")
	}
	return ValidationResult{Valid: true, ReadOnly: readOnly, Operation: op, TablesDetected: tables, Warnings: warnings}
}

func DetectTables(sql string) []string {
	matches := tablePattern.FindAllStringSubmatch(sql, -1)
	seen := map[string]bool{}
	var tables []string
	for _, match := range matches {
		if len(match) < 2 {
			continue
		}
		table := normalizeTable(match[1])
		if table == "" || seen[table] {
			continue
		}
		seen[table] = true
		tables = append(tables, table)
	}
	return tables
}

func SplitTable(table string) (string, string) {
	parts := strings.Split(table, ".")
	if len(parts) == 1 {
		return "public", parts[0]
	}
	return parts[0], parts[1]
}

func HasLimit(sql string) bool {
	return regexp.MustCompile(`(?is)\blimit\b`).MatchString(sql)
}

func ApplySelectLimit(sql string, maxRows int) string {
	if HasLimit(sql) {
		return sql
	}
	return "select * from (" + strings.TrimRight(strings.TrimSpace(sql), ";") + ") as mcp_limited_rows limit " + strconvItoa(maxRows)
}

func invalid(reason string) ValidationResult {
	return ValidationResult{Valid: false, Reason: reason, Warnings: []string{}}
}

func operation(sql string) string {
	trimmed := strings.TrimSpace(strings.TrimLeft(sql, "("))
	fields := strings.Fields(trimmed)
	if len(fields) == 0 {
		return ""
	}
	return strings.ToLower(fields[0])
}

func normalizeTable(table string) string {
	table = strings.ReplaceAll(table, `"`, "")
	table = regexp.MustCompile(`\s+`).ReplaceAllString(table, "")
	table = strings.Trim(table, ".")
	if table == "" {
		return ""
	}
	if !strings.Contains(table, ".") {
		return "public." + table
	}
	return table
}

func strconvItoa(i int) string {
	if i <= 0 {
		i = 1
	}
	var b [20]byte
	pos := len(b)
	for i > 0 {
		pos--
		b[pos] = byte('0' + i%10)
		i /= 10
	}
	return string(b[pos:])
}
