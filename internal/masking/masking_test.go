package masking

import (
	"testing"

	"github.com/sirvonfelsen/felsen_mcp_server_postgres/internal/config"
)

func TestMaskRow(t *testing.T) {
	masker, err := New(config.MaskingConfig{})
	if err != nil {
		t.Fatal(err)
	}
	row := masker.MaskRow([]string{"id", "email", "api_token"}, []any{1, "user@example.com", "abc"})
	if row[0] != 1 {
		t.Fatalf("non-sensitive value changed: %#v", row)
	}
	if row[1] != "***MASKED***" || row[2] != "***MASKED***" {
		t.Fatalf("sensitive values not masked: %#v", row)
	}
}

func TestAllowColumnOverride(t *testing.T) {
	masker, err := New(config.MaskingConfig{AllowColumns: []string{"email"}})
	if err != nil {
		t.Fatal(err)
	}
	if masker.ShouldMask("email") {
		t.Fatal("allow_columns should bypass masking")
	}
}
