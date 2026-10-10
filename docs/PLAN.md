# azemu — local Azure "deployment" emulator: implementation plan

## Context

Today the only way to know if code works in Azure is: PR → merge to dev → Azure DevOps pipeline → deploy. That loop is slow. `azemu` lets a developer run one command and get the same Function Apps / App Services, wired to emulated Storage and Key Vault, with the **same app settings Azure would give them, and no code changes**.

Decisions already made with the user:
- **Generic tool.** It must not depend on one repo's layout. A reference project (fictional here: `contoso`) drives the tests; it is not the template.
- **The infra contract is `terraform plan`.** It's slow, but infra changes rarely, so the result is cached.
- **The code contract is the Azure DevOps pipelines.** They are used to *discover* which folder deploys to which app. That discovery seeds an `azemu.yaml` the user reviews. Pipelines are never executed.
- **Every app is rebuilt into an image on every change.** This covers Docker apps and zip apps alike, for full fidelity.
- **Deployment is parallel.** It covers: plan ∥ image builds ∥ platform startup, then apps start concurrently, and changes to several apps rebuild concurrently.
- **External services stay real.** Azure OpenAI, data warehouses, Graph and third-party APIs are passed through untouched.
- **Written in Go.** It ships as a single binary and drives Docker through the `docker compose` CLI.

Facts from the reference project that shape the design:
- **Storage uses managed identity with hard-coded hostnames.** Code builds `https://{account}.blob.core.windows.net` and authenticates with `DefaultAzureCredential`. So azemu must keep the real hostnames and provide a fake managed-identity endpoint.
- **Key Vault is used only through `@Microsoft.KeyVault(SecretUri=...)` references** in app settings. Resolving them from `.env` is enough; no Key Vault API is needed.
- **Some settings live outside Terraform.** `WEBSITES_PORT` is set by a pipeline script, and `AzureWebJobsStorage` is injected by the platform on Flex Consumption.
- **Deploy logic is in inline PowerShell** (`az acr build`, `*.ps1 -FunctionAppName`). Pipeline discovery therefore needs heuristics, not only standard tasks.
- **The backend trusts the Easy Auth header `x-ms-client-principal-name`**, so azemu must inject it at the entry point.

## Architecture

```
host                                   docker network "azemu-<project>"
────                                   ─────────────────────────────────────────────
azemu CLI (Go)                         azemu-proxy  (aliases: every emulated hostname)
 ├ terraform plan → cached model         ├ :443   TLS by SNI, leaf certs from local CA
 ├ pipelines → azemu.yaml (init)         │        <app>.azurewebsites.net → app container
 ├ settings resolver (+ .env)            │        <acct>.{blob,queue,table}.core.windows.net → azurite
 ├ generates .azemu/compose.yaml         ├ :8081  fake managed identity (IDENTITY_ENDPOINT)
 ├ docker compose build/up (parallel)    └ :7001…  ingress per app, published to localhost,
 └ watch loop → batched rebuilds                  injects Easy Auth headers
                                       azurite  (HTTPS, --oauth basic, AZURITE_ACCOUNTS = all accounts)
                                       app containers (one per Function App / Web App)
                                         env: resolved settings + IDENTITY_* + CA bundle vars
```

How the pieces fit:
- **Settings are not rewritten.** Inside a container, the real Azure hostnames resolve to the proxy and the local CA is trusted. There are only two exceptions:
  - Key Vault references are replaced with values from `.env`.
  - Storage account keys are swapped for the Azurite key, which also keeps real keys out of local files.
- **Building and running both go through `docker compose`.** Compose builds in parallel through BuildKit, starts containers in parallel with health-gated `depends_on`, handles networks and aliases, and gives `logs` and `ps` for free.
- **The proxy is the azemu binary itself** (`azemu proxy`), built as a multi-arch image and published to GHCR. In development it is built from the repo's `Dockerfile`.

## Parallel execution (`azemu up`)

```
preflight checks (docker, compose ≥2.24, terraform, az login)        ── all concurrent
│
├─[A] infra: cache hit → load ~0s │ miss → terraform init + plan + show -json (30–120s)
├─[B] build: docker compose build <all apps>   ← needs only azemu.yaml, so it never waits for A
├─[C] platform: CA → compose up -d --wait azemu-proxy azurite
│
├─ A+C → create containers/tables/queues (one goroutine per account) → seed steps
├─ A   → resolve settings → write .azemu/env/<app>.env (one goroutine per app, 0600)
└─ A+B+C+seeds → docker compose up -d --wait <all apps>   ← containers start concurrently
→ print status table with per-phase timings
```

- Orchestration uses `errgroup` with these joins. A failure in one app's build marks only that app as failed; the others still start, and the error is shown in the status table.
- `kind`/`runtime` are stored in `azemu.yaml` at init time, so builds can start before the plan finishes. After the plan finishes, a mismatch with Terraform produces a warning to run `azemu init --update`.
- **Watch loop (rebuilding changed apps in batches):**
  1. fsnotify watches each app's build context, filtered through that app's dockerignore patterns (using `moby/patternmatcher`). Only changes that would alter the image trigger a rebuild.
  2. Changes are debounced for 300ms and collected into a batch of changed apps.
  3. `docker compose build A B …` builds the batch in parallel.
  4. `docker compose up -d --no-deps --wait A B …` replaces the changed containers.
  5. While an app is building, further edits mark it dirty, which causes at most one follow-up build.
  6. **A failed build leaves the old container running.**
- Changes to `.env` or `azemu.yaml` re-resolve the settings and recreate only the affected containers, with no rebuild. A change to `.tf` files prints a hint to run `azemu up --refresh-infra`.

## Repository layout (new repo in `azure_emulator/`)

```
go.mod
cmd/azemu/main.go              # stdlib flag; subcommands: init up down status env logs seed secrets doctor proxy
internal/config/config.go      # azemu.yaml types, load/save, {env} templating
internal/infra/plan.go         # terraform init/plan/show runner, TF_DATA_DIR isolation, cache
internal/infra/model.go        # plan JSON → Model; one handler per azurerm resource type; unknown-value fallbacks
internal/discover/pipelines.go # YAML walk, variables, ${{ parameters }} defaults, templates (one level deep)
internal/discover/scripts.go   # inline-script heuristics (az / docker / func / ps1 params)
internal/discover/match.go     # join discovered (folder, name/image) ↔ Terraform resources
internal/settings/resolve.go   # final env per app
internal/build/dockerfile.go   # generated Dockerfile + Dockerfile.dockerignore for zip apps
internal/stack/compose.go      # compose.yaml generation + docker compose wrappers
internal/stack/storage.go      # create Azurite containers/tables/queues (Azure SDK for Go)
internal/proxy/proxy.go        # SNI router, ingress ports, Easy Auth headers
internal/proxy/identity.go     # fake managed-identity endpoint + JWT minting (stdlib crypto)
internal/proxy/ca.go           # local CA, on-the-fly leaf certs (tls.Config.GetCertificate)
internal/watch/watch.go        # fsnotify + ignore filter + debounce + batching
internal/up/up.go              # the parallel execution graph above
Dockerfile                     # proxy image (multi-arch azemu binary)
testdata/                      # sanitised plan JSON, pipeline YAML fixtures, golden Dockerfiles
```

Dependencies are deliberately few: `gopkg.in/yaml.v3`, `github.com/fsnotify/fsnotify`, `github.com/moby/patternmatcher`, `golang.org/x/sync/errgroup`, `github.com/joho/godotenv`, and the Azure SDK for Go storage packages (`azblob`, `aztables`, `azqueue`). Everything else uses the standard library, including JWTs, the CA and the CLI.

## Components in detail

### 1. Infra: `internal/infra`
- **Command line.** Taken from `azemu.yaml.infra`, which init fills from the infra pipeline's `TerraformTaskV4`/`TerraformCLI` `commandOptions` and `backendServiceArm`/`-backend-config`. For the reference project this gives `-var-file=configs/_tenant.tfvars -var-file=configs/{env}/terraform.tfvars` and `-backend-config=configs/{env}/backend.tfvars`.
- **Running it.** Set `TF_DATA_DIR=.azemu/tf/<env>` so the user's own `.terraform` is never touched, then:
  1. `terraform init -input=false -reconfigure <backend-config>`
  2. `terraform plan -refresh=false -lock=false -input=false -out=.azemu/tf/<env>/plan.bin`
  3. `terraform show -json plan.bin`, after which the raw plan is deleted.
- **Cache.** Keyed on the sha256 of all `*.tf`, `*.tfvars` and `.terraform.lock.hcl` files under the infra directory (modules included), the var-file arguments and `terraform version`. A cache entry is valid for 24h, and `--refresh-infra` forces a new plan.
  - `ponytail:` the key ignores remote state changes; if someone else applies infra, azemu is stale for up to the TTL. Upgrade path: include the `terraform state pull` serial in the key.
- **Model**, stored in `.azemu/infra/<env>.json` with mode 0600 because it holds secrets. It contains only the fields azemu needs:
  - `Apps[]`:
    - name and host type (functions or appservice);
    - kind (zip or docker), runtime and version, docker image name;
    - `app_settings` and the list of `unknownSettings`;
    - whether Easy Auth is enabled, the host storage account, `app_command_line`.
  - `StorageAccounts[]`: name, containers, tables, queues.
  - `Vaults[]`.
- **Resource handlers:**
  - `azurerm_function_app_flex_consumption`, `azurerm_linux_function_app`, `azurerm_linux_web_app`.
  - `azurerm_storage_account`, `_container`, `_table`, `_queue`.
  - `azurerm_key_vault`.
  - `azurerm_windows_*` apps produce an "unsupported" warning. File shares produce a warning because Azurite has no Files service.
- **Values only known after apply.** Values that exist in state come through as concrete values. Unknown attributes are filled by convention: `<name>.azurewebsites.net` and `https://<acct>.<svc>.core.windows.net/`. If an app setting itself is unknown, azemu lists it and lets the user override it under `apps.<x>.env`.

### 2. Discovery: `azemu init` (`internal/discover`)
- **Finding pipelines.** Look for `azure-pipelines*.yml`, `pipelines/**`, `.azure-pipelines/**`, and prefer files whose name or `environment:` matches `--env`.
- **Resolving YAML values.** Resolve `variables:` (both map and list forms) and `$(var)` substitution, take `${{ parameters.x }}` from parameter defaults, and inline `template:` files one level deep. Values that can't be resolved (variable groups) stay as placeholders and mark the match as low confidence.
- **Standard-task extractors:**
  - `AzureFunctionApp@1/2` (appName, package)
  - `AzureWebApp@1`
  - `AzureWebAppContainer@1`
  - `AzureRmWebAppDeployment@4`
  - `Docker@2` (Dockerfile, buildContext, repository)
  - `ArchiveFiles@2` (links a package back to its source folder)
  - `TerraformTaskV4` and `TerraformCLI` (infra)
- **Heuristics for inline scripts:**
  - a `cd`/`Set-Location`/`pushd` followed by `docker build` or `az acr build` (`-f`/`--file`, `-t`/`--image`);
  - `az functionapp deployment source config-zip --name --src`;
  - `az webapp deploy --name --src-path`;
  - `func azure functionapp publish <name>`;
  - PowerShell parameter pairs `-*AppName` with `-*Path`, and hashtables `@{ Path=…; Name=… }` (a PowerShell hashtable mapping paths to app names).
- **Matching to Terraform:**
  1. Exact match of the resolved app name with a resource name.
  2. Otherwise match the image repository against `docker_image_name`.
  3. Otherwise score token overlap between folder name and resource name (`func-orders` ↔ `eund-func-orders`).
  4. If still ambiguous, ask interactively on a TTY, or write a commented TODO.
- **Output:**
  - **`azemu.yaml`**, with a source comment on each entry, for example `# from pipelines/azure-pipelines-dev.yml:287 AzureCLI@2 (heuristic)`.
  - **`.gitignore` entries** for `.azemu/` and `.env*`.
  - **A `.env` template** listing every referenced secret (the same thing `azemu secrets init` produces).

`azemu.yaml` (example for the reference project):
```yaml
env: dev
infra:
  dir: ../contoso-infra/terraform
  var_files: [configs/_tenant.tfvars, configs/{env}/terraform.tfvars]
  backend_config: [configs/{env}/backend.tfvars]
secrets: .env                     # also accepts .env.{env}
identity: { as: dev@example.com } # Easy Auth principal; defaults to git config user.email
apps:
  func-orders:                    # local key = folder name (stable across envs)
    path: apps/func-orders
    resource: { dev: eund-func-orders }
    kind: zip
    runtime: python:3.12
  web-backend:
    path: apps/web-backend
    resource: { dev: eund-app-contoso-backend }
    kind: docker
    dockerfile: Dockerfile
    port: 8000                    # WEBSITES_PORT lives in a pipeline script; fallback is Dockerfile EXPOSE
seed:
  - image: mcr.microsoft.com/azure-cli
    run: az storage blob upload-batch -d prompt-artifacts -s /src/apps/func-orders/seed-artifacts --account-name eundstcontoso
```

### 3. Settings resolver: `internal/settings`
The environment for each app is built in this order, with later layers overriding earlier ones:
1. `app_settings` from the plan.
2. **Settings the platform would inject:**
   - **Function Apps:**
     - `AzureWebJobsStorage`: a connection string for the host storage account, real-shaped but carrying the Azurite key. If the app uses identity-based storage, `AzureWebJobsStorage__accountName` instead.
     - `FUNCTIONS_WORKER_RUNTIME`, which on Flex comes from `runtime_name`.
     - `FUNCTIONS_EXTENSION_VERSION=~4`, `AzureWebJobsScriptRoot=/home/site/wwwroot`, `AzureFunctionsJobHost__Logging__Console__IsEnabled=true`.
   - **All apps:** `WEBSITE_SITE_NAME`, `WEBSITE_HOSTNAME`, `WEBSITES_PORT`, plus `WEBSITE_INSTANCE_ID` on App Services only (on Functions it makes the host assume Azure's key store and fail to start).
3. **Emulation variables:**
   - `IDENTITY_ENDPOINT=http://azemu-proxy:8081/msi/token`, plus a random `IDENTITY_HEADER`.
   - CA trust:
     - `REQUESTS_CA_BUNDLE`, `SSL_CERT_FILE` and `CURL_CA_BUNDLE` point to a **combined** bundle (system CAs + local CA). The bundle must be combined because `REQUESTS_CA_BUNDLE` replaces certifi instead of adding to it.
     - `NODE_EXTRA_CA_CERTS` points to the local CA only.
4. **Rewrites:**
   - Key Vault references: `@Microsoft.KeyVault(SecretUri=https://<v>.vault.azure.net/secrets/<name>[/ver])` and `VaultName=…;SecretName=…` are looked up in `.env`, first by exact name, then normalised (upper case, `-`→`_`). **All missing secrets are reported in one error.** A reference to a vault Terraform doesn't declare produces a warning.
   - Storage connection strings for emulated accounts get `AccountKey` swapped for the Azurite key.
   - `APPLICATIONINSIGHTS_CONNECTION_STRING` and `APPINSIGHTS_INSTRUMENTATIONKEY` are blanked, so local runs don't send telemetry to the real App Insights. `--keep-telemetry` disables this.
5. `apps.<x>.env` overrides from `azemu.yaml`.

`azemu env <app>` prints the result with secrets masked (`--show-secrets` to reveal them). This is the main debugging tool, and it works without Docker.

### 4. Builds: `internal/build`
- **Docker apps:** build the user's Dockerfile and context as-is, with BuildKit on the native platform. A per-app `platform:` override exists; the pipeline builds `linux/amd64`, but native is much faster on Apple Silicon.
- **Zip apps:** azemu writes `.azemu/build/<app>/Dockerfile` and `Dockerfile.dockerignore`, so nothing is added to the user's repository. BuildKit picks up `<Dockerfile>.dockerignore` automatically.
  - **Python Function App:** base image `mcr.microsoft.com/azure-functions/python:4-python<ver>`.
    1. If there's a `uv.lock` and no `requirements.txt`, a first stage runs `uv export --frozen --no-dev --no-emit-project --no-hashes`.
    2. `RUN --mount=type=cache,target=/root/.cache/pip pip install -r requirements.txt`, so even a dependency change is quick.
    3. `COPY . /home/site/wwwroot`.
  - **Node Function App:** base image `azure-functions/node:4-node<ver>`, then `npm ci --omit=dev` (with an npm cache mount), plus `npm run build` if a build script exists.
  - **The dockerignore** is generated from `.funcignore` plus defaults (`.git`, `.venv`, `__pycache__`, `node_modules`, `tests`, `local.settings.json`). This matches what the zip deploy excludes.
- Output images are tagged `azemu/<project>-<app>:dev`. In compose, each service has a `build:` section pointing at the context and Dockerfile above.

### 5. Platform: `internal/proxy` and `internal/stack`
- **CA:** an ECDSA root CA in `~/.azemu/ca/`, created once and reused across projects. Leaf certificates are minted per SNI on demand and cached in memory.
- **Proxy routing table** (`.azemu/proxy.json`, mounted read-only):
  - `<app>.azurewebsites.net` → `<app-container>:<port>`.
  - Storage hostnames → `azurite:{10000|10001|10002}`.
  - Any other host is not resolved to the proxy at all, because aliases exist only for emulated names.
- **Azurite:** runs with `--oauth basic` and HTTPS, using a certificate from the local CA. `AZURITE_ACCOUNTS` lists every storage account, all with the well-known Azurite key. Ports 10000–10002 are published so Azure Storage Explorer works from the host. Data lives in the named volume `azemu-<project>-azurite`.
- **Fake managed identity:**
  - Handles `GET /msi/token?resource=…&api-version=2019-08-01` and checks the `X-IDENTITY-HEADER`.
  - Returns `{access_token, expires_on, resource, token_type}`. The token is an RS256-signed JWT with the requested audience, `iss=https://sts.windows.net/<tenant>/`, `oid`/`tid`, and a 1h expiry.
  - Any audience gets a token: storage, and `api://…` for calls between apps.
  - (v0.2: audiences that are real and not emulated are forwarded to the host's `az account get-access-token`.)
- **Ingress:** one port per app (7001…), with assignments kept stable in `.azemu/ports.json`. If the app has Easy Auth enabled, the proxy injects `X-MS-CLIENT-PRINCIPAL-NAME`, `-ID` and `X-MS-CLIENT-PRINCIPAL` (base64-encoded claims) for the identity set in `identity.as`.
- **Storage objects:** after Azurite is healthy, containers, tables and queues are created in parallel with the Azure SDK for Go. The operation is idempotent: "already exists" counts as success.
- **Seeds:** each runs as a one-off container on the azemu network. It gets the same managed-identity and CA environment as the apps, plus `AZURE_STORAGE_KEY` and `AZURE_STORAGE_AUTH_MODE=key`, with the repository mounted at `/src`. Seeds run once, on the first `up` or when the seed list changes, and again with `azemu seed`.

### 6. CLI
| Command | What it does |
|---|---|
| `azemu init [--env] [--update]` | Discover from pipelines, then write `azemu.yaml`, the `.env` template and `.gitignore` entries |
| `azemu up [--env] [--refresh-infra] [--as] [--no-watch] [app…]` | Run the parallel deployment above, then watch for changes |
| `azemu status` | Table of each app's Azure hostname, local URL, state, last build time and duration |
| `azemu env <app> [--show-secrets]` | The resolved settings for one app |
| `azemu logs [app…]` | `docker compose logs -f` |
| (part of `up`) | Also starts `azemu-logs` (Dozzle, read-only, filtered to the project, no `depends_on`) on `127.0.0.1:7000` (`_logs` in `ports.json`); `up` and `status` print its URL |
| `azemu down [--volumes]` | Stop the stack; `--volumes` also wipes Azurite data |
| `azemu seed` | Run the seed steps again |
| `azemu secrets init` | Write the missing secret names into `.env` |
| `azemu doctor` | Check Docker, compose version, buildx, terraform, `az login`, free ports |

## Milestones (risky spikes first)

0. **Spikes (de-risking).** Each spike becomes a test in `testdata/` or `-tags=integration`.
   - **a.** Run a Python container using `azure-storage-blob`, `azure-data-tables` and `DefaultAzureCredential` against `https://acct.blob.core.windows.net`, going through proxy → Azurite with `--oauth basic` and the fake managed identity. Check two things:
     - whether Azurite accepts production-style hosts with the Host header kept. If not, the proxy rewrites to path style; verify that SharedKey signatures still validate.
     - whether the token claims pass Azurite's checks.
   - **b.** Check whether the Functions Python 3.12 images have **arm64** builds. If not, run them under emulation with `platform: linux/amd64`. Measure cold start.
   - **c.** Run `terraform plan -refresh=false` with `show -json` on the reference project's dev. Measure the time, check that `app_settings` are known and that sensitive values are present.
   - **d.** Run the Durable Functions task hub on Azurite through a real-shaped `AzureWebJobsStorage` connection string.
1. **Infra and settings, without Docker:** `config`, `infra`, `settings`, plus `azemu env` and `secrets init`. This is useful on its own.
2. **Platform:** CA, proxy, identity endpoint, Azurite, storage objects, compose generation for the platform services.
3. **Apps:** generated Dockerfiles, Docker apps, the parallel `up` graph, `status`, `logs`, `down`, Easy Auth ingress, seeds.
4. **Watch:** fsnotify, dockerignore filtering, debounced parallel batch rebuilds, recreating containers when `.env` changes.
5. **Discovery:** `azemu init` from pipelines. During milestones 1–4, the reference project's `azemu.yaml` is written by hand.
6. **Release:** `doctor`, README (including the "what it can't catch" section), goreleaser (darwin/linux/windows binaries plus the multi-arch proxy image on GHCR), Apache-2.0 licence.

**After v0.1:**
- delegating real audiences to the host's `az` login;
- function host keys seeded from the plan's `azurerm_function_app_host_keys` data sources;
- a Key Vault data-plane API;
- .NET and Java zip apps, and App Service zip builds for Python;
- lints: RBAC role grants, plaintext secrets in `app_settings`;
- the Service Bus, Cosmos DB and Event Hubs emulators.

## Verification

- **Unit tests** (`go test ./...`, table-driven, using fixtures):
  - plan JSON → Model, using a sanitised reference-project plan;
  - Key Vault reference parsing and `.env` lookup, including normalisation and the single error listing every missing secret;
  - connection-string key swap;
  - golden Dockerfiles generated for uv-based and requirements-based Python apps;
  - pipeline discovery against copies of the reference project's pipelines, which must find all 4 functions and both web apps, plus synthetic fixtures for each standard task;
  - watch batching with an injected clock: two apps changed within 300ms give one batch with two apps, and an edit during a build gives exactly one follow-up build.
- **Integration tests** (`go test -tags=integration ./...`, needs Docker): spikes a and d as automated tests, plus JWT acceptance by Azurite.
- **End-to-end on the reference project:**
  1. `azemu init --env dev`, then review `azemu.yaml`.
  2. Fill `.env` (`API_KEY`, `GRAPH_CLIENT_SECRET`, `WAREHOUSE_PASSWORD`, `AUTH_FRONTEND_SECRET`).
  3. `azemu up`: all 6 apps report healthy.
  4. `curl localhost:<port>/api/health` on each function.
  5. A frontend page that calls backend → function → Azurite table works.
  6. `azemu env func-orders` matches the Azure portal's settings, except Key Vault values and keys.
- **Speed targets** (timings are printed for each phase):
  - warm `up` (plan cached, images cached): under 30s for 6 apps;
  - editing one Python function file: serving again within about 15s, which is mostly the Functions host starting;
  - editing two apps at once: their build timestamps overlap, which shows they run in parallel;
  - a failed build: the previous container keeps serving.
