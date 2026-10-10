package tfstatic

import (
	"crypto/sha1"
	"crypto/sha256"
	"fmt"
	"strconv"
	"strings"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"

	"github.com/luigi23997/azemu/internal/contract"
)

// Computed returns the value Azure would assign to attr of a resource whose evaluated
// configuration is cfg (an object; unknown/null attributes possible), using Azure naming
// conventions. typ is the resource type ("azurerm_storage_account") or, for data sources,
// "data." + type ("data.azurerm_client_config"). ok=false means "no convention: leave unknown".
//
// Resolution order: per-type rule table, generic ARM `id`, then passthrough of a configured
// attribute (a value set in the config is its own attribute value). When a convention applies
// but an input is unknown/null the result is (unknown string, true).
// attr may be a nested path: "identity[0].principal_id" or "identity.0.principal_id".
func Computed(typ string, cfg cty.Value, attr string) (cty.Value, bool) {
	segs := strings.Split(strings.NewReplacer("[", ".", "]", "").Replace(attr), ".")
	c := cfgVal{cfg}
	var v cty.Value
	if r := rules[typ][segs[0]]; r != nil {
		v = r(c)
	} else if p, ok := armProviders[strings.TrimPrefix(typ, "data.")]; ok && segs[0] == "id" {
		v = armID(p)(c)
	} else if pv := c.val(segs[0]); !pv.IsNull() {
		v = pv
	} else {
		return cty.NilVal, false
	}
	return walk(v, segs[1:])
}

// ---- input helpers ----

const zeroSub = "00000000-0000-0000-0000-000000000000"

// part is a string input: ok (known, non-empty), null (unset/empty), or neither (unknown).
type part struct {
	s        string
	ok, null bool
}

func lit(s string) part { return part{s: s, ok: true} }

type cfgVal struct{ v cty.Value }

type rule func(c cfgVal) cty.Value

// val returns the attribute value: unknown if cfg itself is unknown, null if absent.
func (c cfgVal) val(k string) cty.Value {
	v := c.v
	if v.Type() == cty.NilType || v.IsNull() {
		return cty.NullVal(cty.DynamicPseudoType)
	}
	if !v.IsKnown() {
		return cty.DynamicVal
	}
	v, _ = v.Unmark()
	if !v.Type().IsObjectType() || !v.Type().HasAttribute(k) {
		return cty.NullVal(cty.DynamicPseudoType)
	}
	r, _ := v.GetAttr(k).Unmark()
	return r
}

func (c cfgVal) get(k string) part {
	v := c.val(k)
	switch {
	case !v.IsKnown():
		return part{}
	case v.IsNull():
		return part{null: true}
	}
	if sv, err := convert.Convert(v, cty.String); err == nil && sv.IsKnown() && !sv.IsNull() {
		if s := sv.AsString(); s != "" {
			return lit(s)
		}
	}
	return part{null: true}
}

// first returns the first set input; unknown if none is set but one is unknown.
func (c cfgVal) first(keys ...string) part {
	unk := false
	for _, k := range keys {
		p := c.get(k)
		if p.ok {
			return p
		}
		unk = unk || !p.null
	}
	return part{null: !unk}
}

// rg is the resource group name, "rg" when not configured.
func (c cfgVal) rg() part {
	if p := c.get("resource_group_name"); !p.null {
		return p
	}
	return lit("rg")
}

// last is the last "/" segment of an ARM id.
func last(p part) part {
	if p.ok {
		p.s = p.s[strings.LastIndex(p.s, "/")+1:]
	}
	return p
}

func build(ps []part, f func(s []string) string) cty.Value {
	ss := make([]string, len(ps))
	for i, p := range ps {
		if !p.ok {
			return cty.UnknownVal(cty.String)
		}
		ss[i] = p.s
	}
	return cty.StringVal(f(ss))
}

// tpl formats a string from parts; unknown if any part is unset/unknown.
func tpl(format string, ps ...part) cty.Value {
	return build(ps, func(s []string) string {
		a := make([]any, len(s))
		for i := range s {
			a[i] = s[i]
		}
		return fmt.Sprintf(format, a...)
	})
}

func str(s string) cty.Value { return cty.StringVal(s) }

func uuidOf(parts ...string) string {
	h := sha1.Sum([]byte("azemu/" + strings.ToLower(strings.Join(parts, "/"))))
	h[6] = h[6]&0x0f | 0x50
	h[8] = h[8]&0x3f | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", h[0:4], h[4:6], h[6:8], h[8:10], h[10:16])
}

// fakeUUID is a deterministic UUID derived from kind and the name part.
func fakeUUID(kind string, name part) cty.Value {
	return build([]part{name}, func(s []string) string { return uuidOf(kind, s[0]) })
}

func hexOf(n int, parts ...string) string {
	h := sha256.Sum256([]byte("azemu/" + strings.ToLower(strings.Join(parts, "/"))))
	return fmt.Sprintf("%x", h)[:n]
}

// walk indexes into v along segs (object attributes or list/tuple indexes).
func walk(v cty.Value, segs []string) (cty.Value, bool) {
	for _, s := range segs {
		t := v.Type()
		switch {
		case t == cty.DynamicPseudoType:
			return cty.DynamicVal, true
		case t.IsObjectType() && t.HasAttribute(s):
			v = v.GetAttr(s)
		case t.IsListType() || t.IsTupleType():
			i, err := strconv.Atoi(s)
			if err != nil {
				return cty.NilVal, false
			}
			if v.IsKnown() && !v.IsNull() && (i >= v.LengthInt()) {
				return cty.NilVal, false
			}
			v = v.Index(cty.NumberIntVal(int64(i)))
		default:
			return cty.NilVal, false
		}
	}
	return v, true
}

// ---- ARM ids ----

// armProviders maps azurerm type -> "<Namespace>/<type path>" for top-level resources.
var armProviders = map[string]string{
	"azurerm_storage_account":               "Microsoft.Storage/storageAccounts",
	"azurerm_service_plan":                  "Microsoft.Web/serverFarms",
	"azurerm_key_vault":                     "Microsoft.KeyVault/vaults",
	"azurerm_application_insights":          "Microsoft.Insights/components",
	"azurerm_log_analytics_workspace":       "Microsoft.OperationalInsights/workspaces",
	"azurerm_search_service":                "Microsoft.Search/searchServices",
	"azurerm_cognitive_account":             "Microsoft.CognitiveServices/accounts",
	"azurerm_cosmosdb_account":              "Microsoft.DocumentDB/databaseAccounts",
	"azurerm_servicebus_namespace":          "Microsoft.ServiceBus/namespaces",
	"azurerm_eventhub_namespace":            "Microsoft.EventHub/namespaces",
	"azurerm_user_assigned_identity":        "Microsoft.ManagedIdentity/userAssignedIdentities",
	"azurerm_virtual_network":               "Microsoft.Network/virtualNetworks",
	"azurerm_private_endpoint":              "Microsoft.Network/privateEndpoints",
	"azurerm_container_registry":            "Microsoft.ContainerRegistry/registries",
	"azurerm_linux_function_app":            "Microsoft.Web/sites",
	"azurerm_windows_function_app":          "Microsoft.Web/sites",
	"azurerm_function_app_flex_consumption": "Microsoft.Web/sites",
	"azurerm_linux_web_app":                 "Microsoft.Web/sites",
	"azurerm_windows_web_app":               "Microsoft.Web/sites",
}

func armPrefix(rg part) []part { return []part{lit(zeroSub), rg} }

const armFmt = "/subscriptions/%s/resourceGroups/%s/providers/"

func armID(provider string) rule {
	return func(c cfgVal) cty.Value {
		return tpl(armFmt+provider+"/%s", lit(zeroSub), c.rg(), c.get("name"))
	}
}

// childID builds "<parent>/<seg>/<name>" where parent comes from parentFn.
func childID(parentFn func(c cfgVal) part, seg string) rule {
	return func(c cfgVal) cty.Value { return tpl("%s/"+seg+"/%s", parentFn(c), c.get("name")) }
}

func parentOrNamed(idKey, nameKey, provider string) func(c cfgVal) part {
	return func(c cfgVal) part {
		if p := c.get(idKey); !p.null {
			return p
		}
		n := c.get(nameKey)
		if n.null {
			return part{null: true}
		}
		v := tpl(armFmt+provider+"/%s", lit(zeroSub), c.rg(), n)
		if !v.IsKnown() {
			return part{}
		}
		return lit(v.AsString())
	}
}

// ---- rule tables ----

var rules = map[string]map[string]rule{}

func reg(types []string, r map[string]rule) {
	for _, t := range types {
		rules[t] = r
		rules["data."+t] = r
	}
}

func init() {
	name := func(c cfgVal) part { return c.get("name") }
	host := func(format string) rule { return func(c cfgVal) cty.Value { return tpl(format, name(c)) } }
	empty := func(cfgVal) cty.Value { return str("") }
	arm := func(p string) map[string]rule { return map[string]rule{"id": armID(p)} }

	// storage account
	sa := map[string]rule{"id": armID("Microsoft.Storage/storageAccounts")}
	for _, s := range []struct{ svc, dom string }{
		{"blob", "blob"}, {"queue", "queue"}, {"table", "table"}, {"file", "file"}, {"dfs", "dfs"},
	} {
		sa["primary_"+s.svc+"_host"] = host("%s." + s.dom + ".core.windows.net")
		sa["primary_"+s.svc+"_endpoint"] = host("https://%s." + s.dom + ".core.windows.net/")
		sa["secondary_"+s.svc+"_host"] = host("%s-secondary." + s.dom + ".core.windows.net")
		sa["secondary_"+s.svc+"_endpoint"] = host("https://%s-secondary." + s.dom + ".core.windows.net/")
	}
	// ponytail: web endpoints (zone number varies) and the secondary/blob connection strings'
	// exotic variants are left unknown; azemu does not emulate static websites.
	conn := func(c cfgVal) cty.Value {
		return tpl("DefaultEndpointsProtocol=https;AccountName=%s;AccountKey="+contract.AzuriteKey+";EndpointSuffix=core.windows.net", name(c))
	}
	blobConn := func(c cfgVal) cty.Value {
		return tpl("DefaultEndpointsProtocol=https;BlobEndpoint=https://%s.blob.core.windows.net/;AccountName=%[1]s;AccountKey="+contract.AzuriteKey, name(c))
	}
	key := func(cfgVal) cty.Value { return str(contract.AzuriteKey) }
	sa["primary_access_key"], sa["secondary_access_key"] = key, key
	sa["primary_connection_string"], sa["secondary_connection_string"] = conn, conn
	sa["primary_blob_connection_string"], sa["secondary_blob_connection_string"] = blobConn, blobConn
	reg([]string{"azurerm_storage_account"}, sa)

	// storage children: accept storage_account_name or storage_account_id
	acct := func(c cfgVal) part {
		if p := c.get("storage_account_name"); !p.null {
			return p
		}
		return last(c.get("storage_account_id"))
	}
	saParent := parentOrNamed("storage_account_id", "storage_account_name", "Microsoft.Storage/storageAccounts")
	stChild := func(urlFmt, seg string) map[string]rule {
		return map[string]rule{
			"id": func(c cfgVal) cty.Value { return tpl(urlFmt, acct(c), name(c)) },
			"resource_manager_id": func(c cfgVal) cty.Value {
				return childID(saParent, seg)(c)
			},
		}
	}
	reg([]string{"azurerm_storage_container"}, stChild("https://%s.blob.core.windows.net/%s", "blobServices/default/containers"))
	reg([]string{"azurerm_storage_table"}, stChild("https://%s.table.core.windows.net/Tables('%s')", "tableServices/default/tables"))
	reg([]string{"azurerm_storage_queue"}, stChild("https://%s.queue.core.windows.net/%s", "queueServices/default/queues"))

	// web / function apps (+ slots)
	sites := []struct {
		typ, kind string
		slot      bool
	}{
		{"azurerm_linux_function_app", "functionapp,linux", false},
		{"azurerm_windows_function_app", "functionapp", false},
		{"azurerm_function_app_flex_consumption", "functionapp,linux", false},
		{"azurerm_linux_web_app", "app,linux", false},
		{"azurerm_windows_web_app", "app", false},
		{"azurerm_linux_function_app_slot", "functionapp,linux", true},
		{"azurerm_windows_function_app_slot", "functionapp", true},
		{"azurerm_linux_web_app_slot", "app,linux", true},
		{"azurerm_windows_web_app_slot", "app", true},
	}
	for _, s := range sites {
		parent := func(c cfgVal) part { return c.first("function_app_id", "app_service_id") }
		// appName is the app's own name, "<app>-<slot>" for slots (the hostname label).
		appName := name
		if s.slot {
			appName = func(c cfgVal) part {
				p, n := last(parent(c)), name(c)
				if !p.ok || !n.ok {
					return part{}
				}
				return lit(p.s + "-" + n.s)
			}
		}
		r := map[string]rule{
			"default_hostname":                  func(c cfgVal) cty.Value { return tpl("%s.azurewebsites.net", appName(c)) },
			"kind":                              func(cfgVal) cty.Value { return str(s.kind) },
			"outbound_ip_addresses":             empty,
			"possible_outbound_ip_addresses":    empty,
			"outbound_ip_address_list":          func(cfgVal) cty.Value { return cty.ListValEmpty(cty.String) },
			"possible_outbound_ip_address_list": func(cfgVal) cty.Value { return cty.ListValEmpty(cty.String) },
			"custom_domain_verification_id": func(c cfgVal) cty.Value {
				return build([]part{appName(c)}, func(s []string) string { return strings.ToUpper(hexOf(64, "cdvid", s[0])) })
			},
			"identity": func(c cfgVal) cty.Value { return identity(c, appName(c)) },
		}
		if s.slot {
			r["id"] = childID(parent, "slots")
		} else {
			r["id"] = armID("Microsoft.Web/sites")
		}
		reg([]string{s.typ}, r)
	}

	// key vault
	kv := arm("Microsoft.KeyVault/vaults")
	kv["vault_uri"] = host("https://%s.vault.azure.net/")
	reg([]string{"azurerm_key_vault"}, kv)

	vaultName := func(c cfgVal) part { return last(c.get("key_vault_id")) }
	kvVersion := func(c cfgVal) part {
		v, n := vaultName(c), name(c)
		if !v.ok || !n.ok {
			return part{}
		}
		return lit(hexOf(32, "kvsecret", v.s, n.s))
	}
	secretURL := func(withVersion bool) rule {
		return func(c cfgVal) cty.Value {
			f := "https://%s.vault.azure.net/secrets/%s"
			ps := []part{vaultName(c), name(c)}
			if withVersion {
				f += "/%s"
				ps = append(ps, kvVersion(c))
			}
			return tpl(f, ps...)
		}
	}
	secretARM := func(withVersion bool) rule {
		return func(c cfgVal) cty.Value {
			f := "%s/secrets/%s"
			ps := []part{c.get("key_vault_id"), name(c)}
			if withVersion {
				f += "/versions/%s"
				ps = append(ps, kvVersion(c))
			}
			return tpl(f, ps...)
		}
	}
	// ponytail: the data source's secret `value` is deliberately absent (secrets come from .env).
	reg([]string{"azurerm_key_vault_secret"}, map[string]rule{
		"id": secretURL(true), "versionless_id": secretURL(false),
		"version":     func(c cfgVal) cty.Value { return build([]part{kvVersion(c)}, func(s []string) string { return s[0] }) },
		"resource_id": secretARM(true), "resource_versionless_id": secretARM(false),
	})

	// observability: telemetry is disabled locally, so keys are empty.
	ai := arm("Microsoft.Insights/components")
	ai["instrumentation_key"], ai["connection_string"], ai["app_id"] = empty, empty, empty
	reg([]string{"azurerm_application_insights"}, ai)
	law := arm("Microsoft.OperationalInsights/workspaces")
	law["workspace_id"] = func(c cfgVal) cty.Value { return fakeUUID("law", name(c)) }
	reg([]string{"azurerm_log_analytics_workspace"}, law)

	// ai / data services
	cog := arm("Microsoft.CognitiveServices/accounts")
	cog["endpoint"] = func(c cfgVal) cty.Value {
		sub, kind := c.first("custom_subdomain_name", "name"), c.get("kind")
		if !kind.ok && !kind.null {
			return cty.UnknownVal(cty.String)
		}
		if strings.EqualFold(kind.s, "OpenAI") {
			return tpl("https://%s.openai.azure.com/", sub)
		}
		return tpl("https://%s.cognitiveservices.azure.com/", sub)
	}
	reg([]string{"azurerm_cognitive_account"}, cog)
	// ponytail: azurerm_search_service exports no endpoint attribute: only id/name apply.
	reg([]string{"azurerm_search_service"}, arm("Microsoft.Search/searchServices"))
	cosmos := arm("Microsoft.DocumentDB/databaseAccounts")
	cosmos["endpoint"] = host("https://%s.documents.azure.com:443/")
	reg([]string{"azurerm_cosmosdb_account"}, cosmos)
	sb := arm("Microsoft.ServiceBus/namespaces")
	sb["endpoint"] = host("https://%s.servicebus.windows.net:443/")
	reg([]string{"azurerm_servicebus_namespace"}, sb)
	eh := arm("Microsoft.EventHub/namespaces")
	eh["endpoint"] = host("https://%s.servicebus.windows.net:443/")
	reg([]string{"azurerm_eventhub_namespace"}, eh)
	reg([]string{"azurerm_servicebus_queue"}, map[string]rule{
		"id": childID(parentOrNamed("namespace_id", "namespace_name", "Microsoft.ServiceBus/namespaces"), "queues")})
	reg([]string{"azurerm_servicebus_topic"}, map[string]rule{
		"id": childID(parentOrNamed("namespace_id", "namespace_name", "Microsoft.ServiceBus/namespaces"), "topics")})
	reg([]string{"azurerm_eventhub"}, map[string]rule{
		"id": childID(parentOrNamed("namespace_id", "namespace_name", "Microsoft.EventHub/namespaces"), "eventhubs")})

	// identity / registry / plan / network
	uai := arm("Microsoft.ManagedIdentity/userAssignedIdentities")
	uai["principal_id"] = func(c cfgVal) cty.Value { return fakeUUID("principal", name(c)) }
	uai["client_id"] = func(c cfgVal) cty.Value { return fakeUUID("client", name(c)) }
	uai["tenant_id"] = func(cfgVal) cty.Value { return str(contract.TenantID) }
	reg([]string{"azurerm_user_assigned_identity"}, uai)
	acr := arm("Microsoft.ContainerRegistry/registries")
	acr["login_server"] = func(c cfgVal) cty.Value {
		return build([]part{name(c)}, func(s []string) string { return strings.ToLower(s[0]) + ".azurecr.io" })
	}
	reg([]string{"azurerm_container_registry"}, acr)
	reg([]string{"azurerm_service_plan"}, arm("Microsoft.Web/serverFarms")) // os_type: passthrough
	reg([]string{"azurerm_virtual_network"}, arm("Microsoft.Network/virtualNetworks"))
	reg([]string{"azurerm_private_endpoint"}, arm("Microsoft.Network/privateEndpoints"))
	reg([]string{"azurerm_subnet"}, map[string]rule{"id": func(c cfgVal) cty.Value {
		return tpl(armFmt+"Microsoft.Network/virtualNetworks/%s/subnets/%s", lit(zeroSub), c.rg(), c.get("virtual_network_name"), name(c))
	}})
	reg([]string{"azurerm_resource_group"}, map[string]rule{"id": func(c cfgVal) cty.Value {
		return tpl("/subscriptions/%s/resourceGroups/%s", lit(zeroSub), name(c))
	}})
	reg([]string{"azurerm_role_assignment"}, map[string]rule{"id": func(c cfgVal) cty.Value {
		scope := c.get("scope")
		n := c.get("name")
		if n.null {
			n = c.first("role_definition_id", "role_definition_name")
			p := c.get("principal_id")
			if !n.ok || !p.ok || !scope.ok {
				return cty.UnknownVal(cty.String)
			}
			n = lit(uuidOf("roleassignment", scope.s, n.s, p.s))
		}
		return tpl("%s/providers/Microsoft.Authorization/roleAssignments/%s", scope, n)
	}})

	// data-only singletons (registered under data. only)
	cc := func(k string) rule { return func(cfgVal) cty.Value { return str(uuidOf("client-config", k)) } }
	rules["data.azurerm_client_config"] = map[string]rule{
		"tenant_id":       func(cfgVal) cty.Value { return str(contract.TenantID) },
		"subscription_id": func(cfgVal) cty.Value { return str(zeroSub) },
		"object_id":       cc("object"),
		"client_id":       cc("client"),
		"id": func(cfgVal) cty.Value {
			return str(fmt.Sprintf("clientConfigs/clientId=%s;objectId=%s;subscriptionId=%s;tenantId=%s",
				uuidOf("client-config", "client"), uuidOf("client-config", "object"), zeroSub, contract.TenantID))
		},
	}
	rules["data.azurerm_subscription"] = map[string]rule{
		"subscription_id": func(cfgVal) cty.Value { return str(zeroSub) },
		"id":              func(cfgVal) cty.Value { return str("/subscriptions/" + zeroSub) },
		"display_name":    func(cfgVal) cty.Value { return str("azemu") },
		"tenant_id":       func(cfgVal) cty.Value { return str(contract.TenantID) },
		"state":           func(cfgVal) cty.Value { return str("Enabled") },
	}
}

// identity is the app's `identity` block: always one element (azemu always injects a managed
// identity), type taken from the configured block or "SystemAssigned".
func identity(c cfgVal, appName part) cty.Value {
	typ := cty.Object(map[string]cty.Type{
		"type": cty.String, "identity_ids": cty.Set(cty.String), "principal_id": cty.String, "tenant_id": cty.String,
	})
	if !appName.ok {
		return cty.UnknownVal(cty.List(typ))
	}
	t := cty.StringVal("SystemAssigned")
	if cv, ok := walk(c.val("identity"), []string{"0", "type"}); ok && cv.IsKnown() && !cv.IsNull() && cv.Type() == cty.String {
		t = cv
	}
	return cty.ListVal([]cty.Value{cty.ObjectVal(map[string]cty.Value{
		"type":         t,
		"identity_ids": cty.SetValEmpty(cty.String),
		"principal_id": str(uuidOf("principal", appName.s)),
		"tenant_id":    str(contract.TenantID),
	})})
}
