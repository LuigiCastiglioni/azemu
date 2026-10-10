// Package settings computes the final environment of each app container.
package settings

import (
	"fmt"
	"net/url"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/joho/godotenv"

	"github.com/luigi23997/azemu/internal/config"
	"github.com/luigi23997/azemu/internal/contract"
	"github.com/luigi23997/azemu/internal/infra"
)

type Options struct {
	IdentityHeader string // value for IDENTITY_HEADER (must match proxy config)
	KeepTelemetry  bool
}

var (
	kvRef   = regexp.MustCompile(`@Microsoft\.KeyVault\(([^)]*)\)`)
	acctKey = regexp.MustCompile(`(?i)AccountKey=[^;]*`)
	acctRe  = regexp.MustCompile(`(?i)AccountName=([^;]+)`)
)

// Normalize turns a Key Vault secret name into its .env form (upper case, '-' -> '_').
func Normalize(name string) string { return strings.ToUpper(strings.ReplaceAll(name, "-", "_")) }

// ReadSecrets loads the secrets file for env; a missing file is an empty set.
func ReadSecrets(cfg *config.Config, env string) (map[string]string, error) {
	s, err := godotenv.Read(cfg.Abs(config.Expand(cfg.Secrets, env)))
	if os.IsNotExist(err) {
		return map[string]string{}, nil
	}
	return s, err
}

// Lookup finds a secret by exact name, then normalized.
func Lookup(secrets map[string]string, name string) (string, bool) {
	if v, ok := secrets[name]; ok {
		return v, true
	}
	v, ok := secrets[Normalize(name)]
	return v, ok
}

// parseRef extracts vault and secret name from the inside of @Microsoft.KeyVault(...).
func parseRef(inner string) (vault, secret string) {
	kv := map[string]string{}
	for _, p := range strings.Split(inner, ";") {
		if k, v, ok := strings.Cut(p, "="); ok {
			kv[strings.ToLower(strings.TrimSpace(k))] = strings.TrimSpace(v)
		}
	}
	if raw := kv["secreturi"]; raw != "" {
		u, err := url.Parse(raw)
		if err != nil {
			return "", ""
		}
		parts := strings.Split(strings.Trim(u.Path, "/"), "/")
		if len(parts) >= 2 && parts[0] == "secrets" {
			return strings.SplitN(u.Hostname(), ".", 2)[0], parts[1]
		}
		return "", ""
	}
	return kv["vaultname"], kv["secretname"]
}

// Resolve returns the env of app key for env. Errors list every missing secret at once.
func Resolve(cfg *config.Config, m *infra.Model, key, env string, opts Options) (map[string]string, error) {
	name := cfg.ResourceName(key, env)
	if name == "" {
		return nil, fmt.Errorf("app %q has no resource name for env %q in azemu.yaml", key, env)
	}
	app := m.App(name)
	if app == nil {
		return nil, fmt.Errorf("app %q: resource %q not found in the terraform plan for env %q", key, name, env)
	}
	out := map[string]string{}
	for k, v := range app.AppSettings { // 1. plan
		out[k] = v
	}

	// 2. platform-injected
	if app.Host == "functions" {
		if app.HostStorageMI {
			out["AzureWebJobsStorage__accountName"] = app.HostStorage
		} else if app.HostStorage != "" {
			out["AzureWebJobsStorage"] = "DefaultEndpointsProtocol=https;AccountName=" + app.HostStorage + ";AccountKey=" + contract.AzuriteKey + ";EndpointSuffix=core.windows.net"
		}
		if app.Runtime != "" {
			out["FUNCTIONS_WORKER_RUNTIME"] = app.Runtime
		}
		out["FUNCTIONS_EXTENSION_VERSION"] = "~4"
		out["AzureWebJobsScriptRoot"] = "/home/site/wwwroot"
		out["AzureFunctionsJobHost__Logging__Console__IsEnabled"] = "true"
	}
	out["WEBSITE_SITE_NAME"] = name
	out["WEBSITE_HOSTNAME"] = name + ".azurewebsites.net"
	if app.Host != "functions" { // with WEBSITE_INSTANCE_ID the Functions host assumes Azure's key store and never starts
		out["WEBSITE_INSTANCE_ID"] = "azemu-" + name
	}
	if p := cfg.Apps[key].Port; p > 0 {
		out["WEBSITES_PORT"] = fmt.Sprint(p)
	}

	// 3. emulation
	out["IDENTITY_ENDPOINT"] = contract.IdentityEndpoint()
	out["IDENTITY_HEADER"] = opts.IdentityHeader
	for _, k := range []string{"REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CURL_CA_BUNDLE"} {
		out[k] = contract.CABundleFile
	}
	out["NODE_EXTRA_CA_CERTS"] = contract.CACertFile

	// 4. rewrites
	emulated := map[string]bool{}
	for _, a := range m.StorageAccounts {
		emulated[strings.ToLower(a.Name)] = true
	}
	for _, a := range m.Apps {
		if a.HostStorage != "" {
			emulated[strings.ToLower(a.HostStorage)] = true
		}
	}
	vaults := map[string]bool{}
	for _, v := range m.Vaults {
		vaults[v] = true
	}
	secrets, err := ReadSecrets(cfg, env)
	if err != nil {
		return nil, err
	}
	missing := map[string]bool{}
	warned := map[string]bool{}
	for k, v := range out {
		v = kvRef.ReplaceAllStringFunc(v, func(ref string) string {
			vault, secret := parseRef(kvRef.FindStringSubmatch(ref)[1])
			if secret == "" {
				return ref
			}
			if vault != "" && !vaults[vault] && !warned[vault] {
				warned[vault] = true
				fmt.Fprintf(os.Stderr, "warning: %s references key vault %q which is not in the terraform plan\n", name, vault)
			}
			if s, ok := Lookup(secrets, secret); ok {
				return s
			}
			missing[secret] = true
			return ref
		})
		if mm := acctRe.FindStringSubmatch(v); mm != nil && emulated[strings.ToLower(mm[1])] {
			v = acctKey.ReplaceAllString(v, "AccountKey="+contract.AzuriteKey)
		}
		out[k] = v
	}
	if len(missing) > 0 {
		var names []string
		for n := range missing {
			names = append(names, n)
		}
		sort.Strings(names)
		return nil, fmt.Errorf("missing secrets in %s: %s (run `azemu secrets init` to add them, then fill in values)",
			config.Expand(cfg.Secrets, env), strings.Join(names, ", "))
	}
	if !opts.KeepTelemetry {
		for _, k := range []string{"APPLICATIONINSIGHTS_CONNECTION_STRING", "APPINSIGHTS_INSTRUMENTATIONKEY"} {
			if _, ok := out[k]; ok {
				out[k] = ""
			}
		}
	}

	// 5. azemu.yaml overrides
	for k, v := range cfg.Apps[key].Env {
		out[k] = v
	}
	return out, nil
}

// SecretNames returns every Key Vault secret name referenced by any app, sorted, deduplicated.
func SecretNames(m *infra.Model) []string {
	set := map[string]bool{}
	for _, a := range m.Apps {
		for _, v := range a.AppSettings {
			for _, ref := range kvRef.FindAllStringSubmatch(v, -1) {
				if _, s := parseRef(ref[1]); s != "" {
					set[s] = true
				}
			}
		}
	}
	names := make([]string, 0, len(set))
	for n := range set {
		names = append(names, n)
	}
	sort.Strings(names)
	return names
}
