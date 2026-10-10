package infra

import (
	"os"
	"reflect"
	"testing"
)

func TestFromPlanJSON_Contoso(t *testing.T) {
	b, err := os.ReadFile("../../testdata/plan/contoso-dev.json")
	if err != nil {
		t.Fatal(err)
	}
	m, err := FromPlanJSON(b, "dev")
	if err != nil {
		t.Fatal(err)
	}

	var names []string
	for _, a := range m.Apps {
		names = append(names, a.Name)
	}
	want := []string{"eund-app-contoso-backend", "eund-app-contoso-frontend", "eund-func-billing", "eund-func-catalog", "eund-func-media", "eund-func-orders"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("apps = %v, want %v", names, want)
	}

	w := m.App("eund-func-orders")
	if w.Host != "functions" || w.Kind != "zip" || w.Runtime != "python" || w.RuntimeVersion != "3.12" ||
		w.HostStorage != "eundstfuncorders" || w.HostStorageMI || !w.EasyAuth {
		t.Errorf("orders = %+v", w)
	}
	if w.AppSettings["STORAGE_ACCOUNT_NAME"] != "eundstcontoso" {
		t.Errorf("orders settings = %v", w.AppSettings)
	}

	be := m.App("eund-app-contoso-backend")
	if be.Host != "appservice" || be.Kind != "docker" || be.DockerImage != "contoso-web-backend:latest" || be.EasyAuth {
		t.Errorf("backend = %+v", be)
	}
	if !reflect.DeepEqual(be.UnknownSettings, []string{"CATALOG_FUNCTION_BASE_URL"}) {
		t.Errorf("backend unknown = %v", be.UnknownSettings)
	}
	if !m.App("eund-app-contoso-frontend").EasyAuth {
		t.Error("frontend should have easy auth")
	}

	accts := map[string]StorageAccount{}
	for _, a := range m.StorageAccounts {
		accts[a.Name] = a
	}
	if len(accts) != 6 { // 4 function host accounts + project + fund wiki
		t.Errorf("accounts = %d", len(accts))
	}
	if a := accts["eundstcontoso"]; len(a.Containers) != 5 || len(a.Tables) != 8 {
		t.Errorf("eundstcontoso = %+v", a)
	}
	if a := accts["eundstcatalog"]; !reflect.DeepEqual(a.Containers, []string{"catalog-content"}) || len(a.Tables) != 4 {
		t.Errorf("eundstcatalog = %+v", a)
	}
	if !reflect.DeepEqual(m.Vaults, []string{"eundkvcontoso"}) {
		t.Errorf("vaults = %v", m.Vaults)
	}
}

func TestFromPlanJSON_Types(t *testing.T) {
	const js = `{"planned_values":{"root_module":{"resources":[
	 {"type":"azurerm_linux_function_app","address":"a","values":{"name":"lf","storage_account_name":"st1","storage_uses_managed_identity":true,
	   "site_config":[{"application_stack":[{"node_version":"20"}]}],"auth_settings":[{"enabled":true}]}},
	 {"type":"azurerm_linux_function_app","address":"b","values":{"name":"ld","storage_account_name":"st1",
	   "site_config":[{"application_stack":[{"docker":[{"image_name":"img","image_tag":"v1"}]}]}]}},
	 {"type":"azurerm_linux_web_app","address":"c","values":{"name":"wa","site_config":[{"app_command_line":"gunicorn x",
	   "application_stack":[{"docker_image_name":"reg.azurecr.io/team/img:1"}]}]}},
	 {"type":"azurerm_windows_web_app","address":"d","values":{"name":"win"}},
	 {"type":"azurerm_storage_share","address":"e","values":{"name":"sh"}},
	 {"type":"azurerm_storage_queue","address":"f","values":{"name":"q","storage_account_id":"/subscriptions/x/resourceGroups/r/providers/Microsoft.Storage/storageAccounts/st2"}}
	]}}}`
	m, err := FromPlanJSON([]byte(js), "dev")
	if err != nil {
		t.Fatal(err)
	}
	lf, ld, wa := m.App("lf"), m.App("ld"), m.App("wa")
	if lf.Runtime != "node" || lf.RuntimeVersion != "20" || !lf.HostStorageMI || !lf.EasyAuth {
		t.Errorf("lf = %+v", lf)
	}
	if ld.Kind != "docker" || ld.DockerImage != "img:v1" {
		t.Errorf("ld = %+v", ld)
	}
	if wa.DockerImage != "team/img:1" || wa.CommandLine != "gunicorn x" {
		t.Errorf("wa = %+v", wa)
	}
	if len(m.Warnings) != 2 {
		t.Errorf("warnings = %v", m.Warnings)
	}
	if len(m.StorageAccounts) != 1 || m.StorageAccounts[0].Name != "st2" || m.StorageAccounts[0].Queues[0] != "q" {
		t.Errorf("accounts = %+v", m.StorageAccounts)
	}
}
