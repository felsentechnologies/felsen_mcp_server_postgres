//go:build cgo

package sqlguard

import "github.com/pganalyze/pg_query_go/v6/parser"

func splitStatements(sql string) ([]string, error) {
	return parser.SplitWithScanner(sql, true)
}
