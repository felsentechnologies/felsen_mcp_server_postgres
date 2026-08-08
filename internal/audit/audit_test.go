package audit

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEventIncludesSourceName(t *testing.T) {
	data, err := json.Marshal(Event{Tool: "execute_script", SourceName: "crm-import.sql"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"source_name":"crm-import.sql"`) {
		t.Fatalf("source name was not serialized in the audit event: %s", data)
	}
}
