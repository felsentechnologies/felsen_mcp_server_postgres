//go:build !cgo

package sqlguard

import (
	"strings"
)

func splitStatements(sql string) ([]string, error) {
	var statements []string
	var current strings.Builder
	inSingle := false
	inDouble := false
	inLineComment := false
	inBlockComment := false
	runes := []rune(sql)

	for i := 0; i < len(runes); i++ {
		ch := runes[i]
		next := rune(0)
		if i+1 < len(runes) {
			next = runes[i+1]
		}

		if inLineComment {
			current.WriteRune(ch)
			if ch == '\n' {
				inLineComment = false
			}
			continue
		}
		if inBlockComment {
			current.WriteRune(ch)
			if ch == '*' && next == '/' {
				current.WriteRune(next)
				i++
				inBlockComment = false
			}
			continue
		}
		if !inSingle && !inDouble && ch == '-' && next == '-' {
			current.WriteRune(ch)
			current.WriteRune(next)
			i++
			inLineComment = true
			continue
		}
		if !inSingle && !inDouble && ch == '/' && next == '*' {
			current.WriteRune(ch)
			current.WriteRune(next)
			i++
			inBlockComment = true
			continue
		}
		if ch == '\'' && !inDouble {
			inSingle = !inSingle
			current.WriteRune(ch)
			if inSingle && next == '\'' {
				current.WriteRune(next)
				i++
			}
			continue
		}
		if ch == '"' && !inSingle {
			inDouble = !inDouble
			current.WriteRune(ch)
			continue
		}
		if ch == ';' && !inSingle && !inDouble {
			addStatement(&statements, current.String())
			current.Reset()
			continue
		}
		current.WriteRune(ch)
	}
	addStatement(&statements, current.String())
	return statements, nil
}

func addStatement(statements *[]string, sql string) {
	sql = strings.TrimSpace(sql)
	if sql != "" {
		*statements = append(*statements, sql)
	}
}
