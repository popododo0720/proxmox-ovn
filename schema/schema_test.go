package schema

import (
	"encoding/json"
	"os"
	"testing"
)

func TestPVNControlSchemaHasRequiredTablesAndIndexes(t *testing.T) {
	content, err := os.ReadFile("PVN_Control.ovsschema")
	if err != nil {
		t.Fatal(err)
	}
	var document struct {
		Name    string `json:"name"`
		Version string `json:"version"`
		Tables  map[string]struct {
			Columns map[string]json.RawMessage `json:"columns"`
			Indexes [][]string                 `json:"indexes"`
		} `json:"tables"`
	}
	if err := json.Unmarshal(content, &document); err != nil {
		t.Fatal(err)
	}
	if document.Name != "PVN_Control" || document.Version == "" {
		t.Fatalf("schema identity=%q version=%q", document.Name, document.Version)
	}
	required := []string{"Project", "Network", "Subnet", "Port", "IPAllocation", "Router", "RouterInterface", "FloatingIP", "ProviderNetwork", "ProviderSegment", "SecurityGroup", "SecurityGroupRule", "Node", "Operation"}
	for _, name := range required {
		table, ok := document.Tables[name]
		if !ok {
			t.Errorf("missing table %s", name)
			continue
		}
		for _, column := range []string{"id", "revision", "applied_revision", "state", "created_at", "updated_at"} {
			if _, ok := table.Columns[column]; !ok {
				t.Errorf("table %s missing column %s", name, column)
			}
		}
		foundID := false
		for _, index := range table.Indexes {
			if len(index) == 1 && index[0] == "id" {
				foundID = true
			}
		}
		if !foundID {
			t.Errorf("table %s has no unique id index", name)
		}
	}
}
