package tfstatic

import (
	"testing"

	"github.com/zclconf/go-cty/cty"

	"github.com/luigi23997/azemu/internal/contract"
)

func obj(kv ...any) cty.Value {
	m := map[string]cty.Value{}
	for i := 0; i < len(kv); i += 2 {
		switch v := kv[i+1].(type) {
		case string:
			m[kv[i].(string)] = cty.StringVal(v)
		case cty.Value:
			m[kv[i].(string)] = v
		}
	}
	return cty.ObjectVal(m)
}

var unkStr = cty.UnknownVal(cty.String)

const rgPrefix = "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg1/providers/"

func TestComputed(t *testing.T) {
	const saConn = "DefaultEndpointsProtocol=https;AccountName=stg;AccountKey=" + contract.AzuriteKey + ";EndpointSuffix=core.windows.net"
	vaultARM := rgPrefix + "Microsoft.KeyVault/vaults/kv1"
	tests := []struct {
		typ  string
		cfg  cty.Value
		attr string
		want string
	}{
		// storage account (+ data)
		{"azurerm_storage_account", obj("name", "stg"), "primary_blob_endpoint", "https://stg.blob.core.windows.net/"},
		{"azurerm_storage_account", obj("name", "stg"), "primary_queue_endpoint", "https://stg.queue.core.windows.net/"},
		{"azurerm_storage_account", obj("name", "stg"), "primary_table_endpoint", "https://stg.table.core.windows.net/"},
		{"azurerm_storage_account", obj("name", "stg"), "primary_file_endpoint", "https://stg.file.core.windows.net/"},
		{"azurerm_storage_account", obj("name", "stg"), "primary_dfs_endpoint", "https://stg.dfs.core.windows.net/"},
		{"data.azurerm_storage_account", obj("name", "stg"), "primary_blob_host", "stg.blob.core.windows.net"},
		{"azurerm_storage_account", obj("name", "stg"), "secondary_table_endpoint", "https://stg-secondary.table.core.windows.net/"},
		{"azurerm_storage_account", obj("name", "stg"), "secondary_queue_host", "stg-secondary.queue.core.windows.net"},
		{"azurerm_storage_account", obj("name", "stg"), "primary_access_key", contract.AzuriteKey},
		{"azurerm_storage_account", obj("name", "stg"), "secondary_access_key", contract.AzuriteKey},
		{"data.azurerm_storage_account", obj("name", "stg"), "primary_connection_string", saConn},
		{"azurerm_storage_account", obj("name", "stg"), "primary_blob_connection_string",
			"DefaultEndpointsProtocol=https;BlobEndpoint=https://stg.blob.core.windows.net/;AccountName=stg;AccountKey=" + contract.AzuriteKey},
		{"azurerm_storage_account", obj("name", "stg", "resource_group_name", "rg1"), "id", rgPrefix + "Microsoft.Storage/storageAccounts/stg"},
		{"azurerm_storage_account", obj("name", "stg"), "primary_blob_endpoint", "https://stg.blob.core.windows.net/"},
		{"azurerm_storage_account", obj("name", unkStr), "primary_blob_endpoint", "?"},
		{"azurerm_storage_account", cty.DynamicVal, "primary_blob_endpoint", "?"},
		{"azurerm_storage_account", obj("name", "stg", "resource_group_name", unkStr), "id", "?"},
		{"azurerm_storage_account", obj("name", "stg", "account_replication_type", "LRS"), "account_replication_type", "LRS"},

		// storage children
		{"azurerm_storage_container", obj("name", "c1", "storage_account_name", "stg"), "id", "https://stg.blob.core.windows.net/c1"},
		{"azurerm_storage_container", obj("name", "c1", "storage_account_id", rgPrefix+"Microsoft.Storage/storageAccounts/stg"), "id", "https://stg.blob.core.windows.net/c1"},
		{"azurerm_storage_container", obj("name", "c1", "storage_account_id", rgPrefix+"Microsoft.Storage/storageAccounts/stg"), "resource_manager_id",
			rgPrefix + "Microsoft.Storage/storageAccounts/stg/blobServices/default/containers/c1"},
		{"azurerm_storage_container", obj("name", "c1", "storage_account_id", unkStr), "id", "?"},
		{"azurerm_storage_table", obj("name", "t1", "storage_account_name", "stg"), "id", "https://stg.table.core.windows.net/Tables('t1')"},
		{"azurerm_storage_queue", obj("name", "q1", "storage_account_name", "stg"), "id", "https://stg.queue.core.windows.net/q1"},
		{"azurerm_storage_queue", obj("name", "q1", "storage_account_id", rgPrefix+"Microsoft.Storage/storageAccounts/stg"), "resource_manager_id",
			rgPrefix + "Microsoft.Storage/storageAccounts/stg/queueServices/default/queues/q1"},

		// apps
		{"azurerm_linux_function_app", obj("name", "fn1"), "default_hostname", "fn1.azurewebsites.net"},
		{"azurerm_windows_function_app", obj("name", "fn1"), "default_hostname", "fn1.azurewebsites.net"},
		{"azurerm_function_app_flex_consumption", obj("name", "fn1"), "default_hostname", "fn1.azurewebsites.net"},
		{"azurerm_linux_web_app", obj("name", "web1"), "default_hostname", "web1.azurewebsites.net"},
		{"data.azurerm_windows_web_app", obj("name", "web1"), "default_hostname", "web1.azurewebsites.net"},
		{"data.azurerm_linux_function_app", obj("name", "fn1"), "default_hostname", "fn1.azurewebsites.net"},
		{"azurerm_linux_function_app", obj("name", unkStr), "default_hostname", "?"},
		{"azurerm_linux_function_app", obj("name", "fn1"), "kind", "functionapp,linux"},
		{"azurerm_windows_function_app", obj("name", "fn1"), "kind", "functionapp"},
		{"azurerm_linux_web_app", obj("name", "w"), "kind", "app,linux"},
		{"azurerm_windows_web_app", obj("name", "w"), "kind", "app"},
		{"azurerm_linux_function_app", obj("name", "fn1"), "outbound_ip_addresses", ""},
		{"azurerm_linux_function_app", obj("name", "fn1", "resource_group_name", "rg1"), "id", rgPrefix + "Microsoft.Web/sites/fn1"},
		{"azurerm_linux_function_app_slot", obj("name", "stage", "function_app_id", rgPrefix+"Microsoft.Web/sites/fn1"), "default_hostname", "fn1-stage.azurewebsites.net"},
		{"azurerm_linux_function_app_slot", obj("name", "stage", "function_app_id", rgPrefix+"Microsoft.Web/sites/fn1"), "id", rgPrefix + "Microsoft.Web/sites/fn1/slots/stage"},
		{"azurerm_windows_web_app_slot", obj("name", "stage", "app_service_id", rgPrefix+"Microsoft.Web/sites/w1"), "default_hostname", "w1-stage.azurewebsites.net"},
		{"azurerm_linux_web_app_slot", obj("name", "stage", "app_service_id", unkStr), "default_hostname", "?"},
		{"azurerm_linux_function_app", obj("name", "fn1"), "identity[0].tenant_id", contract.TenantID},
		{"azurerm_linux_function_app", obj("name", "fn1"), "identity.0.principal_id", uuidOf("principal", "fn1")},
		{"data.azurerm_linux_function_app", obj("name", "fn1"), "identity.0.principal_id", uuidOf("principal", "fn1")},
		{"azurerm_linux_function_app", obj("name", "fn1"), "identity.0.type", "SystemAssigned"},
		{"azurerm_linux_function_app", obj("name", "fn1",
			"identity", cty.ListVal([]cty.Value{obj("type", "UserAssigned")})), "identity.0.type", "UserAssigned"},
		{"azurerm_linux_function_app", obj("name", unkStr), "identity.0.principal_id", "?"},

		// key vault
		{"azurerm_key_vault", obj("name", "kv1"), "vault_uri", "https://kv1.vault.azure.net/"},
		{"data.azurerm_key_vault", obj("name", "kv1"), "vault_uri", "https://kv1.vault.azure.net/"},
		{"azurerm_key_vault", obj("name", "kv1", "resource_group_name", "rg1"), "id", vaultARM},
		{"azurerm_key_vault", obj("name", unkStr), "vault_uri", "?"},
		{"azurerm_key_vault_secret", obj("name", "s1", "key_vault_id", vaultARM), "versionless_id", "https://kv1.vault.azure.net/secrets/s1"},
		{"data.azurerm_key_vault_secret", obj("name", "s1", "key_vault_id", vaultARM), "id", "https://kv1.vault.azure.net/secrets/s1/" + hexOf(32, "kvsecret", "kv1", "s1")},
		{"azurerm_key_vault_secret", obj("name", "s1", "key_vault_id", vaultARM), "resource_versionless_id", vaultARM + "/secrets/s1"},
		{"azurerm_key_vault_secret", obj("name", "s1", "key_vault_id", vaultARM), "resource_id", vaultARM + "/secrets/s1/versions/" + hexOf(32, "kvsecret", "kv1", "s1")},
		{"azurerm_key_vault_secret", obj("name", "s1", "key_vault_id", unkStr), "versionless_id", "?"},
		{"azurerm_key_vault_secret", obj("name", "s1", "key_vault_id", vaultARM, "value", "v"), "value", "v"},

		// observability
		{"azurerm_application_insights", obj("name", "ai"), "instrumentation_key", ""},
		{"azurerm_application_insights", obj("name", "ai"), "connection_string", ""},
		{"data.azurerm_application_insights", obj("name", "ai"), "app_id", ""},
		{"azurerm_log_analytics_workspace", obj("name", "law"), "workspace_id", uuidOf("law", "law")},

		// ai / data
		{"azurerm_cognitive_account", obj("name", "cog", "kind", "FormRecognizer"), "endpoint", "https://cog.cognitiveservices.azure.com/"},
		{"azurerm_cognitive_account", obj("name", "cog", "kind", "OpenAI", "custom_subdomain_name", "sub"), "endpoint", "https://sub.openai.azure.com/"},
		{"azurerm_cognitive_account", obj("name", "cog", "kind", unkStr), "endpoint", "?"},
		{"azurerm_search_service", obj("name", "srch", "resource_group_name", "rg1"), "id", rgPrefix + "Microsoft.Search/searchServices/srch"},
		{"azurerm_cosmosdb_account", obj("name", "cos"), "endpoint", "https://cos.documents.azure.com:443/"},
		{"azurerm_servicebus_namespace", obj("name", "sb"), "endpoint", "https://sb.servicebus.windows.net:443/"},
		{"azurerm_eventhub_namespace", obj("name", "eh"), "endpoint", "https://eh.servicebus.windows.net:443/"},
		{"azurerm_servicebus_queue", obj("name", "q", "namespace_id", rgPrefix+"Microsoft.ServiceBus/namespaces/sb"), "id", rgPrefix + "Microsoft.ServiceBus/namespaces/sb/queues/q"},
		{"azurerm_eventhub", obj("name", "h", "namespace_name", "eh", "resource_group_name", "rg1"), "id", rgPrefix + "Microsoft.EventHub/namespaces/eh/eventhubs/h"},

		// identity & misc
		{"azurerm_user_assigned_identity", obj("name", "id1"), "principal_id", uuidOf("principal", "id1")},
		{"azurerm_user_assigned_identity", obj("name", "id1"), "client_id", uuidOf("client", "id1")},
		{"data.azurerm_user_assigned_identity", obj("name", "id1"), "tenant_id", contract.TenantID},
		{"azurerm_user_assigned_identity", obj("name", unkStr), "client_id", "?"},
		{"azurerm_service_plan", obj("name", "sp", "resource_group_name", "rg1", "os_type", "Linux"), "id", rgPrefix + "Microsoft.Web/serverFarms/sp"},
		{"azurerm_service_plan", obj("name", "sp", "os_type", "Linux"), "os_type", "Linux"},
		{"azurerm_container_registry", obj("name", "MyReg"), "login_server", "myreg.azurecr.io"},
		{"azurerm_resource_group", obj("name", "rg1", "location", "westeurope"), "id", "/subscriptions/00000000-0000-0000-0000-000000000000/resourceGroups/rg1"},
		{"data.azurerm_resource_group", obj("name", "rg1", "location", "westeurope"), "location", "westeurope"},
		{"azurerm_virtual_network", obj("name", "vn", "resource_group_name", "rg1"), "id", rgPrefix + "Microsoft.Network/virtualNetworks/vn"},
		{"azurerm_subnet", obj("name", "sn", "virtual_network_name", "vn", "resource_group_name", "rg1"), "id", rgPrefix + "Microsoft.Network/virtualNetworks/vn/subnets/sn"},
		{"azurerm_private_endpoint", obj("name", "pe", "resource_group_name", "rg1"), "id", rgPrefix + "Microsoft.Network/privateEndpoints/pe"},
		{"azurerm_role_assignment", obj("name", "00000000-0000-0000-0000-0000000000aa", "scope", "/subscriptions/x"), "id",
			"/subscriptions/x/providers/Microsoft.Authorization/roleAssignments/00000000-0000-0000-0000-0000000000aa"},
		{"azurerm_role_assignment", obj("scope", "/subscriptions/x", "role_definition_name", "Reader", "principal_id", "p"), "id",
			"/subscriptions/x/providers/Microsoft.Authorization/roleAssignments/" + uuidOf("roleassignment", "/subscriptions/x", "Reader", "p")},
		{"azurerm_key_vault", obj("name", "kv1"), "name", "kv1"},

		// client config / subscription
		{"data.azurerm_client_config", cty.EmptyObjectVal, "tenant_id", contract.TenantID},
		{"data.azurerm_client_config", cty.EmptyObjectVal, "subscription_id", zeroSub},
		{"data.azurerm_client_config", cty.EmptyObjectVal, "object_id", uuidOf("client-config", "object")},
		{"data.azurerm_client_config", cty.EmptyObjectVal, "client_id", uuidOf("client-config", "client")},
		{"data.azurerm_subscription", cty.EmptyObjectVal, "display_name", "azemu"},
		{"data.azurerm_subscription", cty.EmptyObjectVal, "id", "/subscriptions/" + zeroSub},
		{"data.azurerm_subscription", cty.EmptyObjectVal, "subscription_id", zeroSub},
	}
	for _, tc := range tests {
		t.Run(tc.typ+"."+tc.attr, func(t *testing.T) {
			got, ok := Computed(tc.typ, tc.cfg, tc.attr)
			if !ok {
				t.Fatalf("ok=false, want %q", tc.want)
			}
			if tc.want == "?" {
				if got.IsKnown() {
					t.Fatalf("got %#v, want unknown", got)
				}
				return
			}
			if !got.IsKnown() || got.Type() != cty.String || got.AsString() != tc.want {
				t.Fatalf("got %#v, want %q", got, tc.want)
			}
		})
	}
}

func TestComputedNoConvention(t *testing.T) {
	for _, tc := range []struct{ typ, attr string }{
		{"azurerm_storage_account", "primary_web_endpoint"},
		{"azurerm_cognitive_account", "primary_access_key"},
		{"azurerm_search_service", "primary_key"},
		{"azurerm_cosmosdb_account", "primary_key"},
		{"azurerm_servicebus_namespace", "default_primary_connection_string"},
		{"azurerm_log_analytics_workspace", "primary_shared_key"},
		{"data.azurerm_key_vault_secret", "value"},
		{"azurerm_unknown_thing", "id"},
		{"azurerm_unknown_thing", "name"},
	} {
		if v, ok := Computed(tc.typ, obj("name", cty.NullVal(cty.String)), tc.attr); ok {
			t.Errorf("%s.%s: want ok=false, got %#v", tc.typ, tc.attr, v)
		}
	}
	// nil cfg must not panic
	if _, ok := Computed("azurerm_storage_account", cty.NilVal, "primary_blob_endpoint"); !ok {
		t.Error("nil cfg: convention should still apply (unknown)")
	}
}

func TestComputedIdentityTypes(t *testing.T) {
	v, ok := Computed("azurerm_linux_web_app", obj("name", "w"), "identity")
	if !ok || !v.Type().IsListType() || v.LengthInt() != 1 {
		t.Fatalf("identity: %#v %v", v, ok)
	}
	if _, ok := Computed("azurerm_linux_web_app", obj("name", "w"), "identity.5.type"); ok {
		t.Error("out-of-range index should be ok=false")
	}
}
