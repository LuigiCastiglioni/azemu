package infra

import (
	"strings"
	"testing"
)

func mkApp(name string, settings map[string]string, unknown ...string) App {
	return App{Name: name, Host: "functions", Kind: "zip", Runtime: "python", RuntimeVersion: "3.12", AppSettings: settings, UnknownSettings: unknown}
}

func kinds(r DiffResult) string {
	var s []string
	for _, m := range r.Mismatches {
		s = append(s, m.Scope+"/"+m.Field+"/"+m.Kind)
	}
	return strings.Join(s, ",")
}

func TestDiff(t *testing.T) {
	cases := []struct {
		name         string
		static, plan Model
		want         string // joined scope/field/kind
		skipped      int
	}{
		{
			name:   "identical",
			static: Model{Apps: []App{mkApp("a", map[string]string{"K": "v"})}},
			plan:   Model{Apps: []App{mkApp("a", map[string]string{"K": "v"})}},
		},
		{
			name:    "plan-unknown value and key are skipped",
			static:  Model{Apps: []App{mkApp("a", map[string]string{"K": "https://x", "U": "https://x"})}},
			plan:    Model{Apps: []App{mkApp("a", map[string]string{"K": "https://x"}, "U")}},
			skipped: 1,
		},
		{
			name:    "whole plan map unknown",
			static:  Model{Apps: []App{mkApp("a", map[string]string{"K": "v"})}},
			plan:    Model{Apps: []App{mkApp("a", map[string]string{}, "*")}},
			skipped: 1,
		},
		{
			name:   "missing, extra, value",
			static: Model{Apps: []App{mkApp("a", map[string]string{"X": "1", "V": "static"})}},
			plan:   Model{Apps: []App{mkApp("a", map[string]string{"M": "1", "V": "plan"})}},
			want:   "app a/setting M/missing,app a/setting V/value,app a/setting X/extra",
		},
		{
			name:   "static cannot compute what plan knows",
			static: Model{Apps: []App{mkApp("a", map[string]string{}, "U")}},
			plan:   Model{Apps: []App{mkApp("a", map[string]string{"U": "7"})}},
			want:   "app a/setting U/static-unknown",
		},
		{
			name:    "attribute differences, empty plan attribute skipped",
			static:  Model{Apps: []App{{Name: "a", Kind: "docker", Runtime: "node", RuntimeVersion: "20", HostStorage: "st1", EasyAuth: true}}},
			plan:    Model{Apps: []App{{Name: "a", Kind: "zip", Runtime: "python", RuntimeVersion: "", HostStorage: ""}}},
			want:    "app a/easy_auth/value,app a/kind/value,app a/runtime/value",
			skipped: 5,
		},
		{
			name:   "apps present on one side only",
			static: Model{Apps: []App{mkApp("s", nil), mkApp("both", nil)}},
			plan:   Model{Apps: []App{mkApp("p", nil), mkApp("both", nil)}},
			want:   "app p/exists/only-plan,app s/exists/only-static",
		},
		{
			name:    "unnamed plan app excuses a static-only app",
			static:  Model{Apps: []App{mkApp("s", nil)}},
			plan:    Model{Apps: []App{mkApp("", nil)}},
			skipped: 1,
		},
		{
			name:    "plan skipped objects with unknown account: static-only containers are tolerated",
			static:  Model{StorageAccounts: []StorageAccount{{Name: "st", Containers: []string{"a"}}}},
			plan:    Model{StorageAccounts: []StorageAccount{{Name: "st"}}, Warnings: []string{"azurerm_storage_container x: storage account or name unknown until apply, skipped"}},
			skipped: 1,
		},
		{
			name:   "storage and vaults",
			static: Model{StorageAccounts: []StorageAccount{{Name: "st", Containers: []string{"a", "b"}}, {Name: "extra"}}, Vaults: []string{"kv"}},
			plan:   Model{StorageAccounts: []StorageAccount{{Name: "st", Containers: []string{"a", "c"}}}, Vaults: []string{"kv2"}},
			want:   "storage extra/exists/only-static,storage st/containers/only-plan,storage st/containers/only-static,vaults/exists/only-plan,vaults/exists/only-static",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := Diff(&c.static, &c.plan)
			if got := kinds(r); got != c.want {
				t.Errorf("mismatches\n got  %s\n want %s", got, c.want)
			}
			if r.Skipped < c.skipped {
				t.Errorf("skipped = %d, want >= %d", r.Skipped, c.skipped)
			}
		})
	}
}
