# azemu

Run your Azure Function Apps and App Services locally, wired like Azure: same app settings, same hostnames, managed identity, Easy Auth headers, Storage (Azurite) and Key Vault references. No code changes.

`terraform plan` is the infra contract (app settings, storage accounts, Easy Auth), `azemu.yaml` maps each code folder to its app, and every app is rebuilt into an image on change.

**Status: early (v0.1).** Expect rough edges; see [docs/PLAN.md](docs/PLAN.md) for the design and roadmap. Licensed under [Apache 2.0](LICENSE).

## Install

```
go install github.com/luigi23997/azemu/cmd/azemu@latest
```

The proxy runs from an image built from this repo's `Dockerfile`:

```
docker build -t azemu/proxy:dev .
```

## Prerequisites

Docker (with compose >= 2.24 and buildx), terraform and `az login` (only to produce the plan; see limits). Check with:

```
azemu doctor
```

## Quickstart

```
azemu init --env dev       # discover apps from the pipelines, write azemu.yaml; review it
azemu secrets init         # add the Key Vault secret names the apps reference to .env; fill in the values
azemu up                   # plan, build, start everything in parallel, then watch and rebuild
```

Other commands: `azemu status`, `azemu env <app>` (resolved settings, secrets masked), `azemu logs [app]`, `azemu seed`, `azemu down [--volumes]`.
`azemu up --no-watch` starts and exits; `azemu up func-orders web-backend` starts a subset; `--as you@example.com` sets the Easy Auth identity.

Each app is served on `http://127.0.0.1:<port>` (ports in `.azemu/ports.json`, stable across runs), and inside the network on its real `https://<name>.azurewebsites.net`.

## How it works

```
host                                   docker network "azemu-<project>"
azemu CLI (Go)                         azemu-proxy  (aliases: every emulated hostname)
 ├ terraform plan -> cached model        ├ :443   TLS by SNI, leaf certs from a local CA
 ├ settings resolver (+ .env)            │        <app>.azurewebsites.net -> app container
 ├ generates .azemu/compose.yaml         │        <acct>.{blob,queue,table}.core.windows.net -> azurite
 ├ docker compose build/up (parallel)    ├ :8081  fake managed identity (IDENTITY_ENDPOINT)
 └ watch loop -> batched rebuilds        └ :7001… ingress per app on localhost, injects Easy Auth headers
                                       azurite  (HTTPS, --oauth basic, one account per storage account)
                                       app containers: resolved settings + CA bundle vars
```

Settings are not rewritten: real Azure hostnames resolve to the proxy inside the network and the local CA is trusted. Key Vault references are replaced from `.env`, storage keys by the Azurite key, App Insights is blanked.
`up` runs plan, image builds and platform startup concurrently; a failed build of one app does not stop the others, and in watch mode a failed rebuild leaves the old container serving. Warm `up` (cached images and plan) takes seconds.

## What it can't catch

- RBAC: the fake identity can do anything on emulated storage, so missing role assignments will not show up.
- VNet, private endpoints, firewalls, DNS.
- Easy Auth token validation: the proxy injects the headers for `--as`; it does not run the Entra flow.
- Cold start, scaling, quotas, Windows plans, Flex Consumption specifics.
- External services (OpenAI, data warehouses, Graph...) are real and called as-is.

## Current limits

- Zip apps: python and node only. Docker apps use your Dockerfile as-is.
- The plan needs access to the Terraform state, or set `infra.plan_file` to a `terraform show -json` file (or a saved plan) to skip running terraform.
- Values only known after apply (e.g. a function URL) must be set under `apps.<name>.env` in `azemu.yaml`; `azemu env <app>` warns about them.
- Azure Functions images are amd64-only, so zip apps run under emulation on Apple Silicon.
