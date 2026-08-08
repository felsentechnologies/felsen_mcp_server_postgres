package sqlguard

import (
	"fmt"
	"strconv"
	"strings"
	"unicode"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

type Mode string

const (
	ModeRead    Mode = "read"
	ModeDML     Mode = "dml"
	ModeExplain Mode = "explain"
	ModeDDL     Mode = "ddl"
)

type ValidationResult struct {
	Valid          bool     `json:"valid"`
	ReadOnly       bool     `json:"read_only"`
	Operation      string   `json:"operation"`
	TablesDetected []string `json:"tables_detected"`
	Warnings       []string `json:"warnings"`
	Reason         string   `json:"reason,omitempty"`
}

type tokenKind uint8

const (
	tokenIdentifier tokenKind = iota
	tokenString
	tokenNumber
	tokenSymbol
)

type token struct {
	text   string
	quoted bool
	kind   tokenKind
}

func Validate(sql string, cfg config.ConnectionConfig, mode Mode) ValidationResult {
	sql = strings.TrimSpace(sql)
	if sql == "" {
		return invalid("sql is required")
	}
	stmts, err := splitStatements(sql)
	if err != nil {
		return invalid("invalid SQL syntax: " + err.Error())
	}
	if len(stmts) != 1 {
		return invalid("exactly one SQL statement is allowed")
	}

	tokens, err := tokenize(sql)
	if err != nil {
		return invalid(err.Error())
	}
	if len(tokens) == 0 {
		return invalid("sql is required")
	}
	if err := validateStatementTokens(tokens); err != nil {
		return invalid(err.Error())
	}
	if err := validateFunctionSchemas(tokens, cfg); err != nil {
		return invalid(err.Error())
	}

	op := operationFromTokens(tokens)
	readOnly := op == "select"
	switch mode {
	case ModeRead, ModeExplain, ModeDML, ModeDDL:
	default:
		return invalid("unsupported SQL validation mode")
	}

	if mode == ModeDDL {
		if !isDDLOperation(op) {
			return invalid("ddl mode requires an allowlisted DDL statement")
		}
		if !cfg.DDLAllowed() {
			return invalid("DDL is not enabled for this connection")
		}
		tables, err := DetectDDLTablesWithError(tokens)
		if err != nil {
			return invalid(err.Error())
		}
		refs, err := detectTableRefs(tokens, op)
		if err != nil {
			return invalid(err.Error())
		}
		tables = appendUnique(tables, refs...)
		if len(tables) == 0 {
			return invalid("DDL statement must identify an object")
		}
		for _, table := range tables {
			schema, _ := SplitTable(table)
			if !cfg.SchemaAllowed(schema) {
				return ValidationResult{Valid: false, ReadOnly: false, Operation: op, TablesDetected: tables, Reason: "schema is not allowed: " + schema}
			}
		}
		return ValidationResult{
			Valid:          true,
			ReadOnly:       false,
			Operation:      op,
			TablesDetected: tables,
			Warnings:       []string{"DDL is consequential; require explicit approval before execution"},
		}
	}

	if mode == ModeRead || mode == ModeExplain {
		if op != "select" {
			return invalidWithOperation(op, readOnly, "read and explain modes allow only SELECT")
		}
	}
	if mode == ModeDML {
		switch op {
		case "insert", "update", "delete":
		default:
			return invalidWithOperation(op, readOnly, "execute_dml allows only INSERT, UPDATE or DELETE")
		}
	}

	tables, err := detectTableRefs(tokens, op)
	if err != nil {
		return invalidWithOperation(op, readOnly, err.Error())
	}
	if mode == ModeDML && len(tables) == 0 {
		return invalidWithOperation(op, readOnly, "DML statement must identify a target table")
	}
	for _, table := range tables {
		schema, name := SplitTable(table)
		if !cfg.SchemaAllowed(schema) {
			return ValidationResult{Valid: false, ReadOnly: readOnly, Operation: op, TablesDetected: tables, Reason: "schema is not allowed: " + schema}
		}
		if mode == ModeDML {
			operations := []string{op}
			if op == "insert" && containsSequence(tokens, "do", "update") {
				operations = append(operations, "update")
			}
			for _, operation := range operations {
				if !cfg.DMLAllowed(schema, name, operation) {
					return ValidationResult{Valid: false, ReadOnly: readOnly, Operation: op, TablesDetected: tables, Reason: "DML is not allowed for " + schema + "." + name + " (" + operation + ")"}
				}
			}
		}
	}

	warnings := []string{}
	if op == "select" && !HasLimit(sql) {
		warnings = append(warnings, "query has no LIMIT; execution will be wrapped with a configured limit")
	}
	return ValidationResult{Valid: true, ReadOnly: readOnly, Operation: op, TablesDetected: tables, Warnings: warnings}
}

func DetectTables(sql string) []string {
	tokens, err := tokenize(sql)
	if err != nil {
		return nil
	}
	tables, err := detectTableRefs(tokens, operationFromTokens(tokens))
	if err != nil {
		return nil
	}
	return tables
}

func DetectDDLTables(sql string) []string {
	tokens, err := tokenize(sql)
	if err != nil {
		return nil
	}
	tables, err := DetectDDLTablesWithError(tokens)
	if err != nil {
		return nil
	}
	return tables
}

func DetectDDLTablesWithError(tokens []token) ([]string, error) {
	if len(tokens) == 0 {
		return nil, fmt.Errorf("DDL statement must identify an object")
	}
	if hasUnsafeDDLCascade(tokens, operationFromTokens(tokens)) {
		return nil, fmt.Errorf("CASCADE is only allowed as a foreign-key referential action")
	}

	switch operationFromTokens(tokens) {
	case "create":
		return parseCreateTables(tokens)
	case "alter":
		return parseAlterTables(tokens)
	case "drop":
		return parseDropTables(tokens)
	case "truncate":
		return parseTruncateTables(tokens)
	default:
		return nil, fmt.Errorf("unsupported DDL operation")
	}
}

func hasUnsafeDDLCascade(tokens []token, op string) bool {
	for i, tok := range tokens {
		if !isKeyword(tok, "cascade") {
			continue
		}
		if (op == "create" || op == "alter") && i >= 2 && isKeyword(tokens[i-2], "on") &&
			(isKeyword(tokens[i-1], "delete") || isKeyword(tokens[i-1], "update")) {
			continue
		}
		return true
	}
	return false
}

func SplitTable(table string) (string, string) {
	parts := strings.Split(strings.TrimSpace(table), ".")
	if len(parts) == 1 {
		return "public", strings.Trim(parts[0], "\"")
	}
	return strings.Trim(parts[0], "\""), strings.Trim(parts[1], "\"")
}

func HasLimit(sql string) bool {
	tokens, err := tokenize(sql)
	if err != nil {
		return false
	}
	return containsKeyword(tokens, "limit")
}

func ApplySelectLimit(sql string, maxRows int) string {
	if maxRows <= 0 {
		maxRows = 1
	}
	return "select * from (" + strings.TrimRight(strings.TrimSpace(sql), ";") + ") as mcp_limited_rows limit " + strconv.Itoa(maxRows)
}

func invalid(reason string) ValidationResult {
	return ValidationResult{Valid: false, Reason: reason, Warnings: []string{}}
}

func invalidWithOperation(op string, readOnly bool, reason string) ValidationResult {
	return ValidationResult{Valid: false, ReadOnly: readOnly, Operation: op, Reason: reason, Warnings: []string{}}
}

func operation(sql string) string {
	tokens, err := tokenize(sql)
	if err != nil {
		return ""
	}
	return operationFromTokens(tokens)
}

func operationFromTokens(tokens []token) string {
	if len(tokens) == 0 || tokens[0].kind != tokenIdentifier || tokens[0].quoted {
		return ""
	}
	return strings.ToLower(tokens[0].text)
}

func isDDLOperation(op string) bool {
	switch op {
	case "create", "alter", "drop", "truncate":
		return true
	}
	return false
}

func tokenize(sql string) ([]token, error) {
	runes := []rune(sql)
	tokens := make([]token, 0, len(runes)/2)
	for i := 0; i < len(runes); {
		ch := runes[i]
		if unicode.IsSpace(ch) {
			i++
			continue
		}
		if ch == '-' && i+1 < len(runes) && runes[i+1] == '-' {
			return nil, fmt.Errorf("SQL comments are not allowed")
		}
		if ch == '/' && i+1 < len(runes) && runes[i+1] == '*' {
			return nil, fmt.Errorf("SQL comments are not allowed")
		}
		if ch == '$' {
			if i+1 < len(runes) && unicode.IsDigit(runes[i+1]) {
				start := i
				i += 2
				for i < len(runes) && unicode.IsDigit(runes[i]) {
					i++
				}
				tokens = append(tokens, token{text: string(runes[start:i]), kind: tokenSymbol})
				continue
			}
			return nil, fmt.Errorf("dollar-quoted strings are not allowed")
		}
		if ch == '\'' {
			start := i
			i++
			closed := false
			for i < len(runes) {
				if runes[i] == '\\' && i+1 < len(runes) {
					i += 2
					continue
				}
				if runes[i] != '\'' {
					i++
					continue
				}
				if i+1 < len(runes) && runes[i+1] == '\'' {
					i += 2
					continue
				}
				i++
				closed = true
				break
			}
			if !closed {
				return nil, fmt.Errorf("unterminated string literal")
			}
			tokens = append(tokens, token{text: string(runes[start:i]), kind: tokenString})
			continue
		}
		if ch == '"' {
			i++
			var value strings.Builder
			closed := false
			for i < len(runes) {
				if runes[i] == '"' {
					if i+1 < len(runes) && runes[i+1] == '"' {
						value.WriteRune('"')
						i += 2
						continue
					}
					i++
					closed = true
					break
				}
				value.WriteRune(runes[i])
				i++
			}
			if !closed {
				return nil, fmt.Errorf("unterminated quoted identifier")
			}
			tokens = append(tokens, token{text: value.String(), quoted: true, kind: tokenIdentifier})
			continue
		}
		if isIdentifierStart(ch) {
			start := i
			i++
			for i < len(runes) && isIdentifierPart(runes[i]) {
				i++
			}
			tokens = append(tokens, token{text: string(runes[start:i]), kind: tokenIdentifier})
			continue
		}
		if unicode.IsDigit(ch) {
			start := i
			i++
			for i < len(runes) && (unicode.IsDigit(runes[i]) || runes[i] == '.') {
				i++
			}
			tokens = append(tokens, token{text: string(runes[start:i]), kind: tokenNumber})
			continue
		}
		tokens = append(tokens, token{text: string(ch), kind: tokenSymbol})
		i++
	}
	return tokens, nil
}

func validateStatementTokens(tokens []token) error {
	for i, tok := range tokens {
		if tok.kind == tokenSymbol && tok.text == ";" && i != len(tokens)-1 {
			return fmt.Errorf("exactly one SQL statement is allowed")
		}
		if tok.kind != tokenIdentifier {
			continue
		}
		word := strings.ToLower(tok.text)
		switch word {
		case "pg_sleep", "pg_terminate_backend", "pg_cancel_backend",
			"pg_notify", "pg_read_file", "pg_read_binary_file", "pg_ls_dir",
			"pg_ls_waldir", "pg_ls_logdir", "pg_ls_archive_statusdir", "pg_ls_tmpdir",
			"pg_stat_file", "pg_reload_conf", "pg_rotate_logfile",
			"pg_create_physical_replication_slot", "pg_drop_replication_slot",
			"pg_create_logical_replication_slot",
			"pg_logical_slot_get_changes", "pg_logical_slot_peek_changes",
			"dblink", "dblink_connect", "dblink_exec", "lo_import",
			"lo_export", "lo_open", "lo_put", "lo_unlink", "lo_create",
			"lo_truncate", "set_config", "current_setting", "nextval", "setval",
			"pg_advisory_lock", "pg_advisory_xact_lock",
			"pg_advisory_unlock", "pg_advisory_unlock_all":
			return fmt.Errorf("SQL operation or function %q is not allowed", tok.text)
		}
		if tok.quoted {
			continue
		}
		switch word {
		case "begin", "commit", "rollback", "savepoint", "release",
			"copy", "listen", "notify", "vacuum", "analyze",
			"reset", "call", "prepare", "execute", "deallocate",
			"grant", "revoke":
			return fmt.Errorf("SQL operation or function %q is not allowed", tok.text)
		}
		if word == "do" && !containsSequence(tokens[:i], "on", "conflict") {
			return fmt.Errorf("SQL operation or function %q is not allowed", tok.text)
		}
	}
	for _, sequence := range [][]string{
		{"for", "update"},
		{"for", "no", "key", "update"},
		{"for", "share"},
		{"for", "key", "share"},
	} {
		if containsSequence(tokens, sequence...) {
			return fmt.Errorf("row-locking clauses are not allowed")
		}
	}
	if operationFromTokens(tokens) == "select" && containsKeyword(tokens, "into") {
		return fmt.Errorf("SELECT INTO is not allowed in read-only SQL")
	}
	return nil
}

func validateFunctionSchemas(tokens []token, cfg config.ConnectionConfig) error {
	for i := 0; i+3 < len(tokens); i++ {
		if tokens[i].kind != tokenIdentifier || tokens[i+1].text != "." ||
			tokens[i+2].kind != tokenIdentifier || tokens[i+3].text != "(" {
			continue
		}
		schema := identifierValue(tokens[i])
		if strings.EqualFold(schema, "pg_catalog") || cfg.SchemaAllowed(schema) {
			continue
		}
		return fmt.Errorf("function schema is not allowed: %s", schema)
	}
	return nil
}

func detectTableRefs(tokens []token, op string) ([]string, error) {
	var tables []string
	for i := 0; i < len(tokens); i++ {
		isRelationKeyword := isKeyword(tokens[i], "from") || isKeyword(tokens[i], "join") ||
			isKeyword(tokens[i], "references") || isKeyword(tokens[i], "using") ||
			isKeyword(tokens[i], "update") || isKeyword(tokens[i], "into")
		if op == "create" && isKeyword(tokens[i], "like") {
			isRelationKeyword = true
		}
		if op == "create" && isKeyword(tokens[i], "inherits") {
			isRelationKeyword = true
		}
		if op == "create" && i > 0 && isKeyword(tokens[i-1], "partition") && isKeyword(tokens[i], "of") {
			isRelationKeyword = true
		}
		if op == "create" && i > 0 && isKeyword(tokens[i-1], "as") && isKeyword(tokens[i], "table") {
			isRelationKeyword = true
		}
		if !isRelationKeyword {
			continue
		}
		if isKeyword(tokens[i], "update") && !(op == "update" && i == 0) {
			continue
		}
		if isKeyword(tokens[i], "into") && op != "insert" && op != "select" {
			continue
		}
		if isKeyword(tokens[i], "using") && op != "delete" {
			continue
		}
		start := i + 1
		if isKeyword(tokens[i], "inherits") && start < len(tokens) && tokens[start].text == "(" {
			start++
		}
		if isKeyword(tokens[i], "using") && start < len(tokens) && tokens[start].text == "(" {
			continue
		}
		allowTrailingParentheses := isKeyword(tokens[i], "into") || isKeyword(tokens[i], "references")
		table, next, err := relationAtWithOptions(tokens, start, allowTrailingParentheses)
		if err != nil {
			return nil, err
		}
		tables = appendUnique(tables, table)
		if err := rejectRelationList(tokens, next); err != nil {
			return nil, err
		}
		i = next - 1
	}
	if op == "delete" {
		for i := 0; i+1 < len(tokens); i++ {
			if isKeyword(tokens[i], "delete") && isKeyword(tokens[i+1], "from") {
				table, _, err := relationAt(tokens, i+2)
				if err != nil {
					return nil, err
				}
				tables = appendUnique(tables, table)
				break
			}
		}
	}
	return tables, nil
}

func relationAt(tokens []token, start int) (string, int, error) {
	return relationAtWithOptions(tokens, start, false)
}

func relationAtWithTrailingParentheses(tokens []token, start int) (string, int, error) {
	return relationAtWithOptions(tokens, start, true)
}

func relationAtWithOptions(tokens []token, start int, allowTrailingParentheses bool) (string, int, error) {
	if start >= len(tokens) || tokens[start].kind != tokenIdentifier {
		return "", start, fmt.Errorf("table references must use simple schema.table identifiers")
	}
	if isKeyword(tokens[start], "only") || isKeyword(tokens[start], "table") ||
		isKeyword(tokens[start], "if") || isKeyword(tokens[start], "exists") {
		return "", start, fmt.Errorf("qualified table references are required; modifiers are not allowed here")
	}
	first := identifierValue(tokens[start])
	next := start + 1
	if next < len(tokens) && tokens[next].text == "." {
		if next+1 >= len(tokens) || tokens[next+1].kind != tokenIdentifier {
			return "", next, fmt.Errorf("invalid qualified table reference")
		}
		second := identifierValue(tokens[next+1])
		next += 2
		if next < len(tokens) && tokens[next].text == "." {
			return "", next, fmt.Errorf("database-qualified table references are not allowed")
		}
		first = first + "." + second
	}
	if next < len(tokens) && tokens[next].text == "*" {
		return "", next, fmt.Errorf("table functions and inheritance wildcards are not allowed")
	}
	if next < len(tokens) && tokens[next].text == "(" && !allowTrailingParentheses {
		return "", next, fmt.Errorf("table functions and inheritance wildcards are not allowed")
	}
	return normalizeTable(first), next, nil
}

func rejectRelationList(tokens []token, start int) error {
	for i := start; i < len(tokens); i++ {
		if tokens[i].text == "," {
			return fmt.Errorf("multiple table references in one relation clause are not allowed")
		}
		if tokens[i].text == "(" {
			return nil
		}
		if tokens[i].kind == tokenIdentifier && !tokens[i].quoted {
			switch strings.ToLower(tokens[i].text) {
			case "where", "group", "order", "having", "limit", "offset",
				"union", "intersect", "except", "returning", "window",
				"join", "left", "right", "full", "inner", "cross", "on",
				"set", "values":
				return nil
			}
		}
	}
	return nil
}

func parseCreateTables(tokens []token) ([]string, error) {
	i := 1
	if i < len(tokens) && (isKeyword(tokens[i], "temporary") || isKeyword(tokens[i], "temp") || isKeyword(tokens[i], "unlogged")) {
		return nil, fmt.Errorf("temporary and unlogged tables are not allowed")
	}
	if i < len(tokens) && isKeyword(tokens[i], "table") {
		i++
		i = skipIfNotExists(tokens, i)
		table, _, err := relationAtWithTrailingParentheses(tokens, i)
		if err != nil {
			return nil, err
		}
		return []string{table}, nil
	}
	if i < len(tokens) && isKeyword(tokens[i], "unique") {
		i++
	}
	if i >= len(tokens) || !isKeyword(tokens[i], "index") {
		return nil, fmt.Errorf("only CREATE TABLE and CREATE INDEX are allowed")
	}
	i++
	if i < len(tokens) && isKeyword(tokens[i], "concurrently") {
		return nil, fmt.Errorf("CREATE INDEX CONCURRENTLY is not allowed")
	}
	i = skipIfNotExists(tokens, i)
	var index string
	next := i
	var err error
	if i >= len(tokens) || !isKeyword(tokens[i], "on") {
		index, next, err = relationAt(tokens, i)
		if err != nil {
			return nil, err
		}
	}
	for next < len(tokens) && !isKeyword(tokens[next], "on") {
		next++
	}
	if next >= len(tokens) {
		return nil, fmt.Errorf("CREATE INDEX must identify its target table with ON")
	}
	targetStart := next + 1
	if targetStart < len(tokens) && isKeyword(tokens[targetStart], "only") {
		targetStart++
	}
	target, _, err := relationAtWithTrailingParentheses(tokens, targetStart)
	if err != nil {
		return nil, err
	}
	return appendUnique(nil, index, target), nil
}

func parseAlterTables(tokens []token) ([]string, error) {
	if len(tokens) < 2 || !isKeyword(tokens[1], "table") {
		return nil, fmt.Errorf("only ALTER TABLE is allowed")
	}
	i := skipIfExists(tokens, 2)
	if i < len(tokens) && isKeyword(tokens[i], "only") {
		i++
	}
	table, next, err := relationAt(tokens, i)
	if err != nil {
		return nil, err
	}
	for _, forbidden := range []string{"rename", "set", "owner", "attach", "detach", "inherit", "no", "replica"} {
		if containsKeyword(tokens[next:], forbidden) {
			return nil, fmt.Errorf("ALTER TABLE %s is not allowed", forbidden)
		}
	}
	return []string{table}, nil
}

func parseDropTables(tokens []token) ([]string, error) {
	if len(tokens) < 2 || (!isKeyword(tokens[1], "table") && !isKeyword(tokens[1], "index")) {
		return nil, fmt.Errorf("only DROP TABLE and DROP INDEX are allowed")
	}
	if isKeyword(tokens[1], "index") && len(tokens) > 2 && isKeyword(tokens[2], "concurrently") {
		return nil, fmt.Errorf("DROP INDEX CONCURRENTLY is not allowed")
	}
	i := skipIfExists(tokens, 2)
	table, next, err := relationAt(tokens, i)
	if err != nil {
		return nil, err
	}
	if next < len(tokens) && tokens[next].text == "," {
		return nil, fmt.Errorf("dropping multiple objects is not allowed")
	}
	return []string{table}, nil
}

func parseTruncateTables(tokens []token) ([]string, error) {
	i := 1
	if i < len(tokens) && isKeyword(tokens[i], "table") {
		i++
	}
	if i < len(tokens) && isKeyword(tokens[i], "only") {
		return nil, fmt.Errorf("TRUNCATE ONLY is not allowed")
	}
	table, next, err := relationAt(tokens, i)
	if err != nil {
		return nil, err
	}
	if next < len(tokens) && tokens[next].text == "," {
		return nil, fmt.Errorf("truncating multiple objects is not allowed")
	}
	return []string{table}, nil
}

func skipIfNotExists(tokens []token, i int) int {
	if i+2 < len(tokens) && isKeyword(tokens[i], "if") &&
		isKeyword(tokens[i+1], "not") && isKeyword(tokens[i+2], "exists") {
		return i + 3
	}
	return i
}

func skipIfExists(tokens []token, i int) int {
	if i+1 < len(tokens) && isKeyword(tokens[i], "if") && isKeyword(tokens[i+1], "exists") {
		return i + 2
	}
	return i
}

func containsKeyword(tokens []token, value string) bool {
	for _, tok := range tokens {
		if isKeyword(tok, value) {
			return true
		}
	}
	return false
}

func containsSequence(tokens []token, values ...string) bool {
	if len(values) == 0 {
		return false
	}
	for i := 0; i+len(values) <= len(tokens); i++ {
		matches := true
		for j, value := range values {
			if !isKeyword(tokens[i+j], value) {
				matches = false
				break
			}
		}
		if matches {
			return true
		}
	}
	return false
}

func isKeyword(tok token, value string) bool {
	return tok.kind == tokenIdentifier && !tok.quoted && strings.EqualFold(tok.text, value)
}

func identifierValue(tok token) string {
	if tok.quoted {
		return tok.text
	}
	return strings.ToLower(tok.text)
}

func normalizeTable(table string) string {
	table = strings.TrimSpace(table)
	if table == "" {
		return ""
	}
	if !strings.Contains(table, ".") {
		return "public." + table
	}
	return table
}

func appendUnique(tables []string, values ...string) []string {
	seen := make(map[string]bool, len(tables)+len(values))
	for _, table := range tables {
		seen[table] = true
	}
	for _, table := range values {
		if table != "" && !seen[table] {
			seen[table] = true
			tables = append(tables, table)
		}
	}
	return tables
}

func isIdentifierStart(ch rune) bool {
	return ch == '_' || unicode.IsLetter(ch)
}

func isIdentifierPart(ch rune) bool {
	return ch == '_' || ch == '$' || unicode.IsLetter(ch) || unicode.IsDigit(ch)
}
