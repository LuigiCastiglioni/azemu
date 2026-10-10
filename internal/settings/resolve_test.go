package settings

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/infra"
)

func fixture(t *testing.T, secrets string) (*config.Config, *infra.Model) {
	t.Helper()
	dir := t.TempDir()
	if secrets != "" {
		if err := os.WriteFile(filepath.Join(dir, ".env"), []byte(secrets), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &config.Config{Root: dir, Secrets: ".env", Apps: map[string]*config.App{
		"fn":  {Resource: map[string]string{"dev": "func-a"}, Env: map[string]string{"OVERRIDE": "yes"}},
		"web": {Resource: map[string]string{"dev": "web-a"}, Port: 8000},
	}}
	m := &infra.Model{
		Vaults:          []string{"kv1"},
		StorageAccounts: []infra.StorageAccount{{Name: "st1"}},
		Apps: []infra.App{
			{Name: "func-a", Host: "functions", Runtime: "python", HostStorage: "hoststore", AppSettings: map[string]string{
				"K1":                                    "@Microsoft.KeyVault(SecretUri=https://kv1.vault.azure.net/secrets/API-KEY/)",
				"K2":                                    "@Microsoft.KeyVault(VaultName=kv1;SecretName=other-one)",
				"K3":                                    "@Microsoft.KeyVault(SecretUri=https://kv1.vault.azure.net/secrets/API-KEY/abc123)",
				"CONN":                                  "DefaultEndpointsProtocol=https;AccountName=st1;AccountKey=realkey==;EndpointSuffix=core.windows.net",
				"EXT":                                   "DefaultEndpointsProtocol=https;AccountName=elsewhere;AccountKey=realkey==",
				"APPLICATIONINSIGHTS_CONNECTION_STRING": "InstrumentationKey=x",
				"OVERRIDE":                              "no",
			}},
			{Name: "web-a", Host: "appservice", Kind: "docker", AppSettings: map[string]string{
				"S": "@Microsoft.KeyVault(SecretUri=https://kv1.vault.azure.net/secrets/third/)"}},
		},
	}
	return cfg, m
}

func TestResolve(t *testing.T) {
	cfg, m := fixture(t, "API_KEY=k1\nOTHER_ONE=k2\n")
	got, err := Resolve(cfg, m, "fn", "dev", Options{IdentityHeader: "hdr"})
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"K1": "k1", "K2": "k2", "K3": "k1", "OVERRIDE": "yes",
		"CONN":                                  "DefaultEndpointsProtocol=https;AccountName=st1;AccountKey=" + contract.AzuriteKey + ";EndpointSuffix=core.windows.net",
		"EXT":                                   "DefaultEndpointsProtocol=https;AccountName=elsewhere;AccountKey=realkey==",
		"APPLICATIONINSIGHTS_CONNECTION_STRING": "",
		"AzureWebJobsStorage":                   "DefaultEndpointsProtocol=https;AccountName=hoststore;AccountKey=" + contract.AzuriteKey + ";EndpointSuffix=core.windows.net",
		"FUNCTIONS_WORKER_RUNTIME":              "python",
		"WEBSITE_HOSTNAME":                      "func-a.azurewebsites.net",
		"IDENTITY_HEADER":                       "hdr",
		"NODE_EXTRA_CA_CERTS":                   contract.CACertFile,
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q", k, got[k], v)
		}
	}
	if _, ok := got["WEBSITES_PORT"]; ok {
		t.Error("fn has no port")
	}

	// all missing secrets reported in one error, vault warning aside
	cfg2, m2 := fixture(t, "")
	_, err = Resolve(cfg2, m2, "fn", "dev", Options{})
	if err == nil || !strings.Contains(err.Error(), "API-KEY, other-one") || !strings.Contains(err.Error(), "secrets init") {
		t.Errorf("err = %v", err)
	}

	web, err := Resolve(cfg, m, "web", "dev", Options{})
	if err == nil || !strings.Contains(err.Error(), "third") {
		t.Errorf("web err = %v (web=%v)", err, web)
	}
}

func TestSecretNames(t *testing.T) {
	_, m := fixture(t, "")
	want := []string{"API-KEY", "other-one", "third"}
	if got := SecretNames(m); !reflect.DeepEqual(got, want) {
		t.Errorf("got %v want %v", got, want)
	}
}
