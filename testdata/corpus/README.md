# Static-evaluator corpus

Real, public `azurerm` Terraform projects used to prove that `tfstatic.Evaluate` (the offline
evaluator behind `infra.mode: static`) works beyond any single project. They are test data,
not examples: files are copied verbatim from upstream unless listed under "Changes".

Run: `go test ./internal/infra -run Corpus -v` (`AZEMU_CORPUS_OFFLINE=1` skips projects that need
the Terraform registry). `azemu infra verify` runs the same static-vs-plan diff on any project.

Per project: `expected.json` (hand-written oracle, schema in `internal/infra/corpus_test.go`),
`terraform.tfvars` (written here; required variables only), optional `plan.json` (a real
`terraform plan` run with empty state, slimmed to the resource types azemu reads, GUIDs zeroed;
nothing was applied). `*.local/` projects are private and gitignored.

| Project | Upstream | Commit | License |
|---|---|---|---|
| provider-function-python | hashicorp/terraform-provider-azurerm `examples/app-service/function-python` | 01ed5b7fc85f922595b2b4622b66f7d0b910d000 | MPL-2.0 (SPDX headers kept) |
| provider-docker-basic | hashicorp/terraform-provider-azurerm `examples/app-service/docker-basic` | 01ed5b7fc85f922595b2b4622b66f7d0b910d000 | MPL-2.0 (SPDX headers kept) |
| flex-consumption | Azure-Samples/azure-functions-flex-consumption-samples `IaC/terraformazurerm` | 46c638a8f1053f6863f478e736290ba0646504fa | MIT, Microsoft (`LICENSE`) |
| sap-cloud-sdk | Azure-Samples/app-service-javascript-sap-cloud-sdk-quickstart `infra-terraform` | b74f5af8fe9080159750e33b12e339a5218510b5 | MIT, Microsoft (`LICENSE`) |
| dfe-platform | DFE-Digital/education-benchmarking-and-insights `platform/terraform` | 70eb5660b21cada02de2f6b5b1f523679d4a2590 | MIT, Department for Education - Digital (`LICENSE`) |
| libre-devops-linux-function-app | libre-devops/terraform-azurerm-linux-function-app (module root + `examples/complete`) | 9da059a83f78ab332cc8b00083c73ec7a7b3122a | MIT, Craig Thacker / Libre DevOps (`LICENSE`) |

## Changes to upstream files

- sap-cloud-sdk: only the modules the root uses are copied (`modules/{applicationinsights,loganalytics,keyvault,appserviceplan}`, `modules_local/{appservicenode,apim-api}`).
  The 65 KB dashboard JSON heredoc in `modules/applicationinsights/applicationinsights.tf` is replaced by a short valid one that keeps the `${...}` interpolations.
  `../src/api/*.openapi.json` and `apim-api-policy.xml` are deliberately absent: they are only read inside `module.apim` (`count = var.useAPIM`, false).
- dfe-platform: module READMEs, the `modules/functions` stub and `scripts/ip-address.sh` (run by `data "external"`) are not copied. `terraform.tfvars` is new (upstream has `__placeholder__` tokens).
- libre-devops-linux-function-app: `tests/`, `examples/minimal` and the example `app/` are not copied; `examples/complete/terraform.tfvars` is new. Terraform root is `examples/complete` (`"dir"` in expected.json); its `module "linux_function_app"` uses `source = "../../"` and four registry modules.
- Others: `terraform.tfvars`, `expected.json`, `plan.json` added only.

## Oracles

| Project | expected.json | plan.json |
|---|---|---|
| provider-function-python | hand | yes |
| provider-docker-basic | hand | yes |
| flex-consumption | hand | yes (container endpoint and thus host storage unknown in plan) |
| sap-cloud-sdk | hand (regexes: names depend on the subscription id via azurecaf) | no: a real plan leaves the app name and the whole app_settings unknown (azurecaf `result`, `vault_uri`) |
| dfe-platform | hand | no: data sources (key vault, subnets, mssql, ...) need real Azure resources |
| libre-devops-linux-function-app | hand | yes (api app_settings unknown in plan: merge() of an unknown identity client id) |
| *.local (private) | `terraform console` over the real var files + module logic | no |

`TestCorpusOraclesAgree` cross-checks expected.json against plan.json without the evaluator, so a
wrong oracle is caught before it blames the evaluator.

## Constructs the evaluator must handle

- **provider-function-python**: `file("./SampleApp/...")` relative to the working dir in a resource azemu does not model (must not abort the run); `"${var.prefix}-..."` names; no app_settings at all (empty map, not missing).
- **provider-docker-basic**: `docker_image_name = "nginx:latest"` + `docker_registry_url` (image is `nginx:latest`).
- **flex-consumption**: variables without `type`, with defaults; `provider "azurerm" { subscription_id = var.x }`; `<account>.primary_blob_endpoint` interpolated into `storage_container_endpoint` (naming convention: `https://<name>.blob.core.windows.net/`) which defines the host storage account; `identity.0.principal_id` legacy index syntax; `//` comments; setting `AzureWebJobsStorage = ""` must stay `""`, not be dropped as null; `storage_authentication_type = "SystemAssignedIdentity"` means identity-based host storage.
- **sap-cloud-sdk**:
  - Names from `azurecaf_name` (provider not available offline): needs an emulation of azurecaf slugs (`app-`, `kv-`) over `resource_token = substr(replace(lower(base64encode(sha256("${env}${location}${data.azurerm_client_config.current.subscription_id}"))), "[^A-Za-z0-9_]", ""), 0, 13)`; the `replace` pattern is NOT wrapped in `/.../` so it is a literal no-op; sha256 is hex, base64 of it is lowercase-alnum; the subscription id is unknown offline (needs a stand-in). When the corpus was written the app name was empty and the vault missing.
  - Per-attribute (lazy) evaluation across modules: `module.keyvault` takes `module.api.IDENTITY_PRINCIPAL_ID` while `module.api` takes `module.keyvault.AZURE_KEY_VAULT_ENDPOINT` (a cycle at module level, none per attribute).
  - `identity.0.principal_id` / `identity[0].principal_id` of a `dynamic "identity"` block built from `for_each = { for k, v in var.identity : k => v if var.identity != [] }`; output warned "does not have an attribute named principal_id" (computed identity attributes need placeholders).
  - `count = var.useAPIM ? 1 : 0` on a module whose body calls `file()` on missing files: prune before evaluating.
  - `merge()`, `module.x.output` chains, `count = length(var.list)`, variable names starting with `_` (`_APIKey`), `data.azurerm_subscription`, `<<DASH` heredoc with `${}`.
  - KV refs built as `@Microsoft.KeyVault(SecretUri=${vault_uri}secrets/${name})` where `vault_uri` ends with `/` and is computed from the vault name.
- **dfe-platform**:
  - 16 calls of 2 local modules (plus `modules/config`); typed object variables with `optional(type, default)` and `default = {}` (os_type defaults to `"Windows"`, so the 8 `-fa` apps are `azurerm_windows_function_app`, not modelled, one warning each; the 8 `-fc-fa` apps are Linux flex).
  - Hyphenated variable and local names (`app-settings`, `var.environment-prefix`, `local.function-app-settings`); `var.env_configs[var.environment].search` (map of objects indexed by a variable) through a module output.
  - `count = cond ? 1 : 0` windows/linux split and `one(concat(a[*].id, b[*].id))`, `[*].identity[0].principal_id` splats over count resources; `try(jsondecode(x).functionkey.value, null)`.
  - `for_each = toset([...])` with `each.key`, referenced as `azurerm_storage_account.storage["platformstorage"].name`; `moved {}` blocks.
  - A whole module call inside `/* ... */`; `#checkov:skip` comments; `lifecycle { ignore_changes = [app_settings["X"]] }`; `<<BODY` heredoc containing `$schema`; `data "external"` (must never be run); `backend "azurerm" {}`; providers `azapi` and `mssql` not available offline; ~15 data sources whose attributes are unknown (`versionless_id`, `.value`, `.id`).
  - `merge(var.app-settings, {...})` where some values are unknown: keep per-key unknown (do not collapse the map). Secret names inside `@Microsoft.KeyVault(SecretUri=${...versionless_id})` come from `azurerm_key_vault_secret.name`; `versionless_id` is `https://<vault>.vault.azure.net/secrets/<name>` without trailing slash.
  - `storage_container_endpoint = "${account.primary_blob_endpoint}${container.name}"` and `vault_uri` (`https://<kv>.vault.azure.net/`, trailing slash).
  - A bool in a `map(string)` (`"WEBSITES_ENABLE_APP_SERVICE_STORAGE" = true` becomes `"true"`).
- **libre-devops-linux-function-app**:
  - `provider::azurerm::parse_resource_id(...)` (provider-defined function; when the corpus was written "no functions in namespace", which made `local.rg` unknown and collapsed every app's app_settings to `*`). Input is a computed id (unknown): result must be an object with unknown/placeholder attributes, not a hard error. The same call with a literal id must parse (`resource_name`, `resource_group_name`, ...).
  - `terraform.workspace` (= `default`); `backend "azurerm" {}`; `check {}` and `validation {}` blocks referencing other fields; registry modules `libre-devops/{tags,rg,log-analytics-workspace,application-insights}/azurerm` (need init).
  - `for_each` over a map of `optional()`-typed objects keyed by `(local.api_name) = {...}` computed keys; `merge([for k, v in ... : {...}]...)` with `...` expansion; `coalesce`, `try`, `lookup`, `substr(replace(replace(lower(k), ...)))`; conditionals that return an object or `{}`; `dynamic` blocks with `for_each = cond ? [x] : []`; nested dynamic blocks (`ip_restriction` > `headers`).
  - Expected extraction: both apps' names are the map keys; storage account names are `substr("st" + key without dashes, 0, 24)`; `storage_uses_managed_identity` is `true` for the keyless app and `null` for the keys-on one (host_storage_mi false).
  - app_settings of the api app: in real Terraform the whole map is unknown (merge() over an unknown identity client id). expected.json asks for the keys instead (`AzureWebJobsStorage__clientId`, `APPLICATIONINSIGHTS_*`), which is what Azure ends up with; the evaluator should keep per-key unknowns here too.
- **\*.local** (private, gitignored): a `source.json` (`{"dir": "/abs/root", "var_files": [...]}`) points at the project; covers flex + docker web apps, `terraform.tfvars` maps of objects, local modules.
