package infra

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

type obj = map[string]any

// at walks nested objects; a list on the way is replaced by its first element
// (terraform blocks are one-element lists). Returns nil if anything is missing.
func at(v any, path ...string) any {
	for _, p := range path {
		if l, ok := v.([]any); ok {
			if len(l) == 0 {
				return nil
			}
			v = l[0]
		}
		m, ok := v.(obj)
		if !ok {
			return nil
		}
		v = m[p]
	}
	if l, ok := v.([]any); ok && len(l) > 0 {
		if _, isObj := l[0].(obj); isObj {
			return l[0]
		}
	}
	return v
}

func str(v any, path ...string) string {
	s, _ := at(v, path...).(string)
	return s
}

func boolean(v any, path ...string) bool {
	b, _ := at(v, path...).(bool)
	return b
}

// FromPlanJSON distils `terraform show -json <plan>` into a Model.
func FromPlanJSON(b []byte, env string) (*Model, error) {
	var plan obj
	if err := json.Unmarshal(b, &plan); err != nil {
		return nil, fmt.Errorf("parse plan json: %w", err)
	}
	root := at(plan, "planned_values", "root_module")
	if root == nil {
		return nil, fmt.Errorf("plan json has no planned_values.root_module")
	}
	// address -> after_unknown
	unknown := map[string]any{}
	if rcs, ok := plan["resource_changes"].([]any); ok {
		for _, rc := range rcs {
			if a := str(rc, "address"); a != "" {
				unknown[a] = at(rc, "change", "after_unknown")
			}
		}
	}

	m := &Model{Env: env}
	accounts := map[string]*StorageAccount{}
	account := func(name string) *StorageAccount {
		if accounts[name] == nil {
			accounts[name] = &StorageAccount{Name: name}
		}
		return accounts[name]
	}
	warn := func(f string, a ...any) { m.Warnings = append(m.Warnings, fmt.Sprintf(f, a...)) }

	var walk func(mod obj)
	walk = func(mod obj) {
		res, _ := mod["resources"].([]any)
		for _, r := range res {
			typ, addr, vals := str(r, "type"), str(r, "address"), at(r, "values")
			if vals == nil {
				continue
			}
			switch typ {
			case "azurerm_function_app_flex_consumption", "azurerm_linux_function_app", "azurerm_linux_web_app":
				m.Apps = append(m.Apps, app(typ, vals, unknown[addr]))
			case "azurerm_windows_function_app", "azurerm_windows_web_app":
				warn("%s %s: unsupported (windows apps)", typ, str(vals, "name"))
			case "azurerm_storage_account":
				account(str(vals, "name"))
			case "azurerm_storage_container", "azurerm_storage_table", "azurerm_storage_queue", "azurerm_storage_share":
				name := str(vals, "name")
				if typ == "azurerm_storage_share" {
					warn("azurerm_storage_share %s: Azurite has no Files service, skipped", name)
					continue
				}
				acct := str(vals, "storage_account_name")
				if acct == "" { // .../storageAccounts/<name>
					if _, after, ok := strings.Cut(str(vals, "storage_account_id"), "/storageAccounts/"); ok {
						acct = strings.SplitN(after, "/", 2)[0]
					}
				}
				if acct == "" || name == "" {
					warn("%s %s: storage account or name unknown until apply, skipped", typ, addr)
					continue
				}
				a := account(acct)
				switch typ {
				case "azurerm_storage_container":
					a.Containers = append(a.Containers, name)
				case "azurerm_storage_table":
					a.Tables = append(a.Tables, name)
				default:
					a.Queues = append(a.Queues, name)
				}
			case "azurerm_key_vault":
				if n := str(vals, "name"); n != "" {
					m.Vaults = append(m.Vaults, n)
				}
			}
		}
		kids, _ := mod["child_modules"].([]any)
		for _, k := range kids {
			if km, ok := k.(obj); ok {
				walk(km)
			}
		}
	}
	walk(root.(obj))

	for _, a := range accounts {
		sort.Strings(a.Containers)
		sort.Strings(a.Tables)
		sort.Strings(a.Queues)
		m.StorageAccounts = append(m.StorageAccounts, *a)
	}
	sort.Slice(m.StorageAccounts, func(i, j int) bool { return m.StorageAccounts[i].Name < m.StorageAccounts[j].Name })
	sort.Slice(m.Apps, func(i, j int) bool { return m.Apps[i].Name < m.Apps[j].Name })
	sort.Strings(m.Vaults)
	sort.Strings(m.Warnings)
	return m, nil
}

func app(typ string, vals, unk any) App {
	a := App{Name: str(vals, "name"), ResourceType: typ, Kind: "zip", Host: "functions", AppSettings: map[string]string{}}
	if typ == "azurerm_linux_web_app" {
		a.Host = "appservice"
	}
	stack := at(vals, "site_config", "application_stack")
	a.CommandLine = str(vals, "site_config", "app_command_line")

	switch typ {
	case "azurerm_function_app_flex_consumption":
		a.Runtime, a.RuntimeVersion = str(vals, "runtime_name"), str(vals, "runtime_version")
		// https://<acct>.blob.core.windows.net/<container>
		if h, ok := strings.CutPrefix(str(vals, "storage_container_endpoint"), "https://"); ok {
			a.HostStorage, _, _ = strings.Cut(h, ".")
		}
		a.HostStorageMI = str(vals, "storage_authentication_type") != "StorageAccountConnectionString"
	case "azurerm_linux_function_app":
		a.HostStorage = str(vals, "storage_account_name")
		a.HostStorageMI = boolean(vals, "storage_uses_managed_identity")
		if img := str(stack, "docker", "image_name"); img != "" {
			a.Kind, a.DockerImage = "docker", img
			if tag := str(stack, "docker", "image_tag"); tag != "" {
				a.DockerImage += ":" + tag
			}
		}
	case "azurerm_linux_web_app":
		if img := str(stack, "docker_image_name"); img != "" {
			a.Kind, a.DockerImage = "docker", stripRegistry(img)
		}
	}
	if a.Kind == "zip" && a.Runtime == "" {
		for _, r := range [][2]string{{"python", "python_version"}, {"node", "node_version"}, {"dotnet-isolated", "dotnet_version"}, {"java", "java_version"}} {
			if v := str(stack, r[1]); v != "" {
				a.Runtime, a.RuntimeVersion = r[0], v
				break
			}
		}
	}

	a.EasyAuth = boolean(vals, "auth_settings_v2", "auth_enabled") || boolean(vals, "auth_settings", "enabled")

	if s, ok := at(vals, "app_settings").(obj); ok {
		for k, v := range s {
			if v == nil {
				continue
			}
			a.AppSettings[k] = fmt.Sprint(v)
		}
	}
	switch u := at(unk, "app_settings").(type) {
	case bool: // whole map unknown
		if u {
			a.UnknownSettings = append(a.UnknownSettings, "*")
		}
	case obj:
		for k, v := range u {
			if b, _ := v.(bool); b {
				a.UnknownSettings = append(a.UnknownSettings, k)
			}
		}
	}
	sort.Strings(a.UnknownSettings)
	return a
}

// stripRegistry drops a leading registry host ("acr.azurecr.io/repo:tag" -> "repo:tag").
func stripRegistry(img string) string {
	img = strings.TrimPrefix(strings.TrimPrefix(img, "https://"), "http://")
	if first, rest, ok := strings.Cut(img, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		return rest
	}
	return img
}
