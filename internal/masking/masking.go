package masking

import (
	"regexp"
	"strings"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

var defaultSensitive = []string{
	`(?i)password`,
	`(?i)passwd`,
	`(?i)token`,
	`(?i)secret`,
	`(?i)api[_-]?key`,
	`(?i)cpf`,
	`(?i)e-?mail|email`,
	`(?i)phone|telefone|celular`,
}

type Masker struct {
	enabled bool
	allow   map[string]bool
	rules   []*regexp.Regexp
}

func New(cfg config.MaskingConfig) (*Masker, error) {
	enabled := true
	if cfg.Enabled != nil {
		enabled = *cfg.Enabled
	}
	patterns := append([]string{}, defaultSensitive...)
	patterns = append(patterns, cfg.SensitiveColumns...)
	m := &Masker{
		enabled: enabled,
		allow:   map[string]bool{},
	}
	for _, col := range cfg.AllowColumns {
		m.allow[strings.ToLower(col)] = true
	}
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, err
		}
		m.rules = append(m.rules, re)
	}
	return m, nil
}

func (m *Masker) MaskRow(columns []string, row []any) []any {
	out := make([]any, len(row))
	copy(out, row)
	for i, col := range columns {
		if i < len(out) && m.ShouldMask(col) && out[i] != nil {
			out[i] = "***MASKED***"
		}
	}
	return out
}

func (m *Masker) ShouldMask(column string) bool {
	if m == nil || !m.enabled {
		return false
	}
	name := strings.ToLower(column)
	if m.allow[name] {
		return false
	}
	for _, re := range m.rules {
		if re.MatchString(column) {
			return true
		}
	}
	return false
}
