package tfstatic

import (
	"strings"

	"github.com/zclconf/go-cty/cty"
)

// azurecafSlugs are the azurecaf_name slugs of the resource types azemu cares about.
// ponytail: small table; any other resource_type leaves the result unknown.
var azurecafSlugs = map[string]string{
	"azurerm_app_service": "app", "azurerm_linux_web_app": "app", "azurerm_windows_web_app": "app",
	"azurerm_key_vault": "kv", "azurerm_function_app": "func", "azurerm_linux_function_app": "func",
	"azurerm_windows_function_app": "func", "azurerm_function_app_flex_consumption": "func",
	"azurerm_storage_account": "st", "azurerm_resource_group": "rg", "azurerm_app_service_plan": "plan",
	"azurerm_service_plan": "plan", "azurerm_cosmosdb_account": "cosmos", "azurerm_log_analytics_workspace": "log",
	"azurerm_application_insights": "appi",
}

// azurecafName emulates the `result` of azurecaf_name (provider aztfmod/azurecaf, not available
// offline): prefixes, slug, name, suffixes joined by the separator, cleaned of characters Azure
// rejects. Random parts make the result unknown.
func azurecafName(cfg cty.Value) (cty.Value, bool) {
	str := func(k string, def string) (string, bool) {
		v := cfgAttr(cfg, k)
		if v.IsNull() {
			return def, true
		}
		if !v.IsKnown() || v.Type() != cty.String {
			return "", false
		}
		return v.AsString(), true
	}
	typ, ok1 := str("resource_type", "")
	name, ok2 := str("name", "")
	sep, ok3 := str("separator", "-")
	if !ok1 || !ok2 || !ok3 {
		return cty.UnknownVal(cty.String), true
	}
	slug, known := azurecafSlugs[typ]
	if !known {
		return cty.NilVal, false
	}
	if rl := cfgAttr(cfg, "random_length"); !rl.IsKnown() || (!rl.IsNull() && rl.Type() == cty.Number && !rl.RawEquals(cty.NumberIntVal(0))) {
		return cty.UnknownVal(cty.String), true
	}
	if v := cfgAttr(cfg, "use_slug"); !v.IsKnown() {
		return cty.UnknownVal(cty.String), true
	} else if !v.IsNull() && v.Type() == cty.Bool && v.False() {
		slug = ""
	}
	var parts []string
	list := func(k string) bool {
		v := cfgAttr(cfg, k)
		if v.IsNull() {
			return true
		}
		if !v.IsWhollyKnown() || !v.CanIterateElements() {
			return false
		}
		for it := v.ElementIterator(); it.Next(); {
			_, e := it.Element()
			if e.Type() == cty.String && !e.IsNull() && e.AsString() != "" {
				parts = append(parts, e.AsString())
			}
		}
		return true
	}
	if !list("prefixes") {
		return cty.UnknownVal(cty.String), true
	}
	if slug != "" {
		parts = append(parts, slug)
	}
	if name != "" {
		parts = append(parts, name)
	}
	if !list("suffixes") {
		return cty.UnknownVal(cty.String), true
	}
	out := strings.Join(parts, sep)
	if typ == "azurerm_storage_account" {
		out = strings.ReplaceAll(out, "-", "")
	}
	clean := strings.Map(func(r rune) rune {
		if r >= 'A' && r <= 'Z' {
			return r + 'a' - 'A'
		}
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_' {
			return r
		}
		return -1
	}, out)
	return cty.StringVal(clean), true
}

// cfgAttr is cfg.attr, or null when the configuration does not set it.
func cfgAttr(cfg cty.Value, k string) cty.Value {
	if !cfg.Type().HasAttribute(k) {
		return cty.NullVal(cty.DynamicPseudoType)
	}
	return cfg.GetAttr(k)
}
