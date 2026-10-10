package tfstatic

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zclconf/go-cty/cty"
)

type plan map[string]any

func run(t *testing.T, dir string, vf ...string) (plan, []string) {
	t.Helper()
	js, warns, err := Evaluate(context.Background(), Options{Dir: filepath.Join("testdata", dir), VarFiles: vf, DataDir: t.TempDir()})
	if err != nil {
		t.Fatalf("Evaluate: %v", err)
	}
	return decode(t, js), warns
}

func decode(t *testing.T, js []byte) plan {
	t.Helper()
	var p plan
	if err := json.Unmarshal(js, &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// res returns (after, after_unknown) of the resource_changes entry at addr.
func (p plan) res(t *testing.T, addr string) (after, unknown map[string]any) {
	t.Helper()
	for _, r := range p["resource_changes"].([]any) {
		if r.(map[string]any)["address"] == addr {
			c := r.(map[string]any)["change"].(map[string]any)
			return c["after"].(map[string]any), c["after_unknown"].(map[string]any)
		}
	}
	t.Fatalf("no resource %s in %v", addr, p.addrs())
	return nil, nil
}

func (p plan) addrs() (a []string) {
	for _, r := range p["resource_changes"].([]any) {
		a = append(a, r.(map[string]any)["address"].(string))
	}
	return
}

// jat walks JSON by keys / list indexes ("site_config", 0, "x").
func jat(v any, path ...any) any {
	for _, p := range path {
		switch p := p.(type) {
		case string:
			if pl, ok := v.(plan); ok {
				v = map[string]any(pl)
			}
			v = v.(map[string]any)[p]
		case int:
			v = v.([]any)[p]
		}
	}
	return v
}

// fakeComputed gives storage accounts their table endpoint, like Azure would.
func fakeComputed(t *testing.T) {
	old := computed
	t.Cleanup(func() { computed = old })
	computed = func(typ string, cfg cty.Value, attr string) (cty.Value, bool) {
		if typ == "azurerm_storage_account" && attr == "primary_table_endpoint" {
			return cty.StringVal("https://" + cfg.GetAttr("name").AsString() + ".table.core.windows.net/"), true
		}
		return cty.NilVal, false
	}
}

func TestVariablesLocalsDynamicBlocksAndComputed(t *testing.T) {
	fakeComputed(t)
	p, warns := run(t, "basic", "vars/base.tfvars", "vars/override.tfvars")

	a, _ := p.res(t, `azurerm_linux_function_app.app["a"]`)
	// later var file wins; local.prefix is declared in another file and built from local.short
	if a["name"] != "euprod-a" {
		t.Errorf("name = %v", a["name"])
	}
	// optional() defaults applied, dynamic block picked the runtime branch
	if got := jat(a, "site_config", 0, "application_stack", 0, "python_version"); got != "3.12" {
		t.Errorf("python_version = %v", got)
	}
	if n := len(jat(a, "site_config", 0, "application_stack").([]any)); n != 1 {
		t.Errorf("a has %d application_stack blocks, want 1", n)
	}
	// computed attribute of another resource, filled by the hook
	if got := jat(a, "app_settings", "TABLE"); got != "https://euprodst.table.core.windows.net/" {
		t.Errorf("TABLE = %v", got)
	}
	if _, ok := jat(a, "app_settings").(map[string]any)["EXTRA"]; ok {
		t.Error("EXTRA must only be set for b")
	}
	if got := jat(a, "tags", "owner"); got != "me" {
		t.Errorf("tags.owner = %v", got)
	}

	// index is reported for for_each instances (count is a number)
	for _, r := range p["resource_changes"].([]any) {
		if m := r.(map[string]any); m["address"] == `azurerm_linux_function_app.app["a"]` && m["index"] != "a" {
			t.Errorf("index = %v", m["index"])
		}
	}

	b, _ := p.res(t, `azurerm_linux_function_app.app["b"]`)
	// the docker branch of the dynamic block, merge() with the conditional object and the tfvars map
	if got := jat(b, "site_config", 0, "application_stack", 0, "docker", 0, "image_name"); got != "img" {
		t.Errorf("image_name = %v", got)
	}
	if got := jat(b, "app_settings"); !equal(got, map[string]any{"K": "v", "EXTRA": "1", "TABLE": "https://euprodst.table.core.windows.net/"}) {
		t.Errorf("b app_settings = %v", got)
	}

	// azurerm_ only: terraform_data is skipped, and its broken reference never warned (never evaluated)
	for _, ad := range p.addrs() {
		if strings.HasPrefix(ad, "terraform_data") {
			t.Errorf("unexpected %s", ad)
		}
	}
	if len(warns) != 0 {
		t.Errorf("warnings: %v", warns)
	}
}

func TestUnknownComputedKeepsKeys(t *testing.T) {
	// default Computed has no convention for this fake: attribute stays unknown, per key
	old := computed
	t.Cleanup(func() { computed = old })
	computed = func(string, cty.Value, string) (cty.Value, bool) { return cty.NilVal, false }

	p, _ := run(t, "basic", "vars/base.tfvars")
	b, unk := p.res(t, `azurerm_linux_function_app.app["b"]`)
	s := jat(b, "app_settings").(map[string]any)
	if s["K"] != "v" || s["EXTRA"] != "1" {
		t.Errorf("known settings lost: %v", s)
	}
	if _, ok := s["TABLE"]; ok {
		t.Errorf("unknown setting must be omitted: %v", s)
	}
	if got := jat(unk, "app_settings"); !equal(got, map[string]any{"TABLE": true}) {
		t.Errorf("after_unknown.app_settings = %v", got)
	}
	// id is always asked for; no convention -> unknown
	if _, unk := p.res(t, "azurerm_storage_account.st"); unk["id"] != true {
		t.Errorf("id not marked unknown: %v", unk)
	}
}

func TestVarFilePrecedenceAndEnv(t *testing.T) {
	// TF_VAR_* is the weakest source; terraform.tfvars.json style files work too
	t.Setenv("TF_VAR_env", "fromenv")
	p, _ := run(t, "basic", "vars/base.tfvars")
	if a, _ := p.res(t, `azurerm_linux_function_app.app["a"]`); a["name"] != "eutest-a" {
		t.Errorf("tfvars must beat TF_VAR: %v", a["name"])
	}
	p, _ = run(t, "basic", "vars/base.tfvars", "vars/extra.tfvars.json")
	if a, _ := p.res(t, `azurerm_linux_function_app.app["a"]`); jat(a, "tags", "owner") != "json" {
		t.Errorf("tfvars.json not applied: %v", a["tags"])
	}
	// a var only in env
	t.Setenv("TF_VAR_env", "envonly")
	p, _ = run(t, "basic", "vars/extra.tfvars.json", "vars/apps_only.tfvars")
	if a, _ := p.res(t, `azurerm_linux_function_app.app["x"]`); a["name"] != "euenvonly-x" {
		t.Errorf("TF_VAR not applied: %v", a["name"])
	}
}

func TestModulesCountForEachNesting(t *testing.T) {
	fakeComputed(t)
	p, warns := run(t, "modules")
	want := []string{
		`azurerm_key_vault.kv`,
		`module.each["a"].azurerm_storage_account.this`,
		`module.each["a"].module.grand.azurerm_storage_queue.q`,
		`module.each["b"].azurerm_storage_account.this`,
		`module.each["b"].module.grand.azurerm_storage_queue.q`,
		`module.solo[0].azurerm_storage_account.this`,
		`module.solo[0].module.grand.azurerm_storage_queue.q`,
	}
	if got := p.addrs(); strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("addresses:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	// module input from another module's output; outputs of for_each modules via a for expression
	if a, _ := p.res(t, `module.each["b"].azurerm_storage_account.this`); a["account_tier"] != "stsolo" || a["name"] != "stb" {
		t.Errorf("each[b] = %v", a)
	}
	kv, _ := p.res(t, "azurerm_key_vault.kv")
	if kv["name"] != "kv-sta-stb" {
		t.Errorf("kv name = %v", kv["name"])
	}
	if !equal(kv["tags"], map[string]any{"splat": "stsolo", "dyn": "stb"}) {
		t.Errorf("kv tags = %v", kv["tags"])
	}
	// planned_values nests child modules by address
	root := jat(p, "planned_values", "root_module").(map[string]any)
	var kids []string
	for _, k := range root["child_modules"].([]any) {
		kids = append(kids, k.(map[string]any)["address"].(string))
	}
	if strings.Join(kids, ",") != `module.each["a"],module.each["b"],module.solo[0]` {
		t.Errorf("child_modules = %v", kids)
	}
	if got := jat(root["child_modules"], 2, "child_modules", 0, "address"); got != "module.solo[0].module.grand" {
		t.Errorf("nested address = %v", got)
	}
	if len(warns) != 0 {
		t.Errorf("warnings: %v", warns)
	}
}

func TestUnknownCountForEachAndDiscovery(t *testing.T) {
	old := computed
	t.Cleanup(func() { computed = old })
	computed = func(string, cty.Value, string) (cty.Value, bool) { return cty.NilVal, false }
	p, warns := run(t, "unknown")

	var got []string
	for _, a := range p.addrs() {
		if !strings.Contains(a, "module.self") {
			got = append(got, a)
		}
	}
	// for_each skipped, count assumed 1 (the real count could differ, hence the warning)
	if strings.Join(got, ",") != "azurerm_key_vault.kv,azurerm_storage_account.st,azurerm_storage_queue.counted[0]" {
		t.Errorf("addresses: %v", got)
	}
	all := strings.Join(warns, "\n")
	for _, w := range []string{"for_each unknown", "count unknown", "module.self skipped"} {
		if !strings.Contains(all, w) {
			t.Errorf("no %q warning in %v", w, warns)
		}
	}
	// primary_blob_endpoint was discovered from the for expression and shows up on the account
	if _, unk := p.res(t, "azurerm_storage_account.st"); unk["primary_blob_endpoint"] != true || unk["primary_access_key"] != true {
		t.Errorf("unknown = %v", unk)
	}
	// the key survives the conditional with an unknown branch even though that branch is not chosen
	kv, _ := p.res(t, "azurerm_key_vault.kv")
	if jat(kv, "tags", "maybe") != "x" {
		t.Errorf("tags = %v", kv["tags"])
	}
}

func TestMissingVariablesListedTogether(t *testing.T) {
	_, _, err := Evaluate(context.Background(), Options{Dir: "testdata/missing"})
	if err == nil || !strings.Contains(err.Error(), "alpha, beta") || strings.Contains(err.Error(), "gamma") {
		t.Fatalf("err = %v", err)
	}
}

func TestCycleDegradesToUnknown(t *testing.T) {
	p, warns := run(t, "cycle")
	a, unk := p.res(t, "azurerm_resource_group.rg")
	if _, ok := a["location"]; ok || unk["location"] != true {
		t.Errorf("location = %v unknown = %v", a["location"], unk)
	}
	if len(warns) == 0 || !strings.Contains(strings.Join(warns, "\n"), "cycle") {
		t.Errorf("warnings: %v", warns)
	}
}

func TestFunctions(t *testing.T) {
	p, warns := run(t, "funcs")
	a, unk := p.res(t, "azurerm_resource_group.rg")
	want := map[string]any{
		"cidr": "10.0.2.0/24", "fmt": "x-007", "lookup": "dflt", "try": "fallback", "can": "true", "file": "hello", "tpl": "name=me",
		"sha": "ca978112ca1bbdcafac231b39a23dc4da786eff8147c4e72b9807785afee48bb", "b64": "aGk=", "json": `{"a":[1,2]}`,
		"startswith": "true", "replace": "a-b-c", "ws": "default", "coalesce": "z", "one": "null",
	}
	if got := a["tags"]; !equal(got, want) {
		t.Errorf("tags = %v\nwant %v", got, want)
	}
	// expressions with unsupported functions are unknown, with a warning naming the function
	if got := jat(unk, "tags"); !equal(got, map[string]any{"nofn": true, "prov": true}) {
		t.Errorf("after_unknown.tags = %v", got)
	}
	all := strings.Join(warns, "\n")
	if !strings.Contains(all, "no_such_fn") || !strings.Contains(all, "provider::foo::") {
		t.Errorf("warnings: %v", warns)
	}
}

func TestDeterministic(t *testing.T) {
	fakeComputed(t)
	opts := Options{Dir: "testdata/modules", DataDir: t.TempDir()}
	var first []byte
	for i := 0; i < 5; i++ {
		js, _, err := Evaluate(context.Background(), opts)
		if err != nil {
			t.Fatal(err)
		}
		if first == nil {
			first = js
		} else if string(first) != string(js) {
			t.Fatal("output differs between runs")
		}
	}
}

func TestRemoteModuleFromManifest(t *testing.T) {
	root, data := t.TempDir(), t.TempDir()
	write := func(p, s string) {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(s), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write(filepath.Join(root, "main.tf"), `
module "net" {
  source = "Azure/net/azurerm"
}
resource "azurerm_resource_group" "rg" {
  name     = module.net.name
  location = "x"
}`)
	write(filepath.Join(root, ".terraform/modules/net/main.tf"), `output "name" { value = "from-remote" }`)
	write(filepath.Join(data, "modules/modules.json"),
		`{"Modules":[{"Key":"","Source":"","Dir":"."},{"Key":"net","Source":"registry.terraform.io/Azure/net/azurerm","Dir":".terraform/modules/net"}]}`)
	js, warns, err := Evaluate(context.Background(), Options{Dir: root, DataDir: data})
	if err != nil || len(warns) != 0 {
		t.Fatalf("err=%v warns=%v", err, warns)
	}
	if !strings.Contains(string(js), `"name":"from-remote"`) {
		t.Errorf("remote module output not used: %s", js)
	}
}

func TestSplitUnknownShapes(t *testing.T) {
	v := cty.ObjectVal(map[string]cty.Value{
		"a": cty.UnknownVal(cty.String),
		"b": cty.TupleVal([]cty.Value{cty.StringVal("x"), cty.UnknownVal(cty.String)}),
		"c": cty.UnknownVal(cty.Map(cty.String)),
		"d": cty.NumberIntVal(3),
		"e": cty.NullVal(cty.String),
	})
	k, u := split(v)
	b, _ := json.Marshal([]any{k, u})
	if string(b) != `[{"b":["x",null],"d":3,"e":null},{"a":true,"b":[false,true],"c":true}]` {
		t.Errorf("got %s", b)
	}
}

func equal(a, b any) bool {
	x, _ := json.Marshal(a)
	y, _ := json.Marshal(b)
	return string(x) == string(y)
}

func TestAzurecafAndProviderFunction(t *testing.T) {
	cfg := cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("My_App"), "resource_type": cty.StringVal("azurerm_linux_web_app"),
		"suffixes": cty.TupleVal([]cty.Value{cty.StringVal("dev")}), "random_length": cty.NumberIntVal(0)})
	if v, ok := azurecafName(cfg); !ok || v.AsString() != "app-my_app-dev" {
		t.Errorf("azurecaf = %v %v", v, ok)
	}
	if _, ok := azurecafName(cty.ObjectVal(map[string]cty.Value{"name": cty.StringVal("x"), "resource_type": cty.StringVal("azurerm_nothing")})); ok {
		t.Error("unknown resource_type must have no convention")
	}
	f := funcs(".")
	id, err := f["provider::azurerm::parse_resource_id"].Call([]cty.Value{cty.StringVal("/subscriptions/s/resourceGroups/rg/providers/Microsoft.Storage/storageAccounts/acc")})
	if err != nil || id.GetAttr("resource_name").AsString() != "acc" || id.GetAttr("resource_group_name").AsString() != "rg" {
		t.Errorf("parse_resource_id = %v %v", id, err)
	}
	// replace(): only /.../ is a regex
	r, _ := f["replace"].Call([]cty.Value{cty.StringVal("a1b2"), cty.StringVal("[0-9]"), cty.StringVal("")})
	r2, _ := f["replace"].Call([]cty.Value{cty.StringVal("a1b2"), cty.StringVal("/[0-9]/"), cty.StringVal("")})
	if r.AsString() != "a1b2" || r2.AsString() != "ab" {
		t.Errorf("replace = %v %v", r, r2)
	}
}
