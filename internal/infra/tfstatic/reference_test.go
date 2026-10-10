package tfstatic

import (
	"bytes"
	"context"
	"os"
	"strings"
	"testing"
)

// TestReferenceInfra is an opt-in smoke test against any Terraform root you point it at:
//
//	AZEMU_REFERENCE_INFRA=/path/to/terraform/root \
//	AZEMU_REFERENCE_VARFILES=configs/_tenant.tfvars,configs/dev/terraform.tfvars \
//	go test ./internal/infra/tfstatic -run TestReferenceInfra -v
//
// It only asserts generic invariants: evaluation succeeds, at least one azurerm app resource
// comes out, every app has a name, and two runs produce identical output.
func TestReferenceInfra(t *testing.T) {
	dir := os.Getenv("AZEMU_REFERENCE_INFRA")
	if dir == "" {
		t.Skip("AZEMU_REFERENCE_INFRA not set")
	}
	var varFiles []string
	for _, f := range strings.Split(os.Getenv("AZEMU_REFERENCE_VARFILES"), ",") {
		if f = strings.TrimSpace(f); f != "" {
			varFiles = append(varFiles, f)
		}
	}
	eval := func() []byte {
		js, warns, err := Evaluate(context.Background(), Options{Dir: dir, VarFiles: varFiles, DataDir: t.TempDir()})
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%d bytes, warnings: %v", len(js), warns)
		return js
	}
	first := eval()

	apps := 0
	for _, r := range decode(t, first)["resource_changes"].([]any) {
		m := r.(map[string]any)
		typ, _ := m["type"].(string)
		switch typ {
		case "azurerm_function_app_flex_consumption", "azurerm_linux_function_app", "azurerm_windows_function_app",
			"azurerm_linux_web_app", "azurerm_windows_web_app":
			apps++
			if name, _ := jat(m, "change", "after", "name").(string); name == "" {
				t.Errorf("%v: app has no name", m["address"])
			}
		}
	}
	if apps == 0 {
		t.Error("no azurerm app resource evaluated")
	}

	if !bytes.Equal(first, eval()) {
		t.Error("output differs between two runs")
	}
}
