# ntindle fork of CLIProxyAPI

This fork tracks [router-for-me/CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) and adds
routing by quota reset time, live model discovery for Claude and Codex subscriptions, client API
keys tied to providers, and a container image with a single data volume. It is built for one
setup: each coding tool talking only to its own provider through the proxy (Claude Code to Claude
subscriptions, Codex to ChatGPT subscriptions, the Muse CLI to a Meta account), with several
accounts of each.

Everything upstream documents still applies. This page covers only what the fork adds.

## What the fork adds

| Setting | Default | Effect |
| --- | --- | --- |
| `routing.fill-first-order: "soonest-reset"` | `"id"` | With `routing.strategy: "fill-first"`, drain the credential whose weekly quota window resets soonest instead of the lowest credential ID. |
| `routing.quota-refresh-interval` | `"30m"` | How often idle Claude and Codex OAuth credentials have their quota windows read from the provider usage endpoint. `"0"` turns it off. |
| `oauth.providers.codex.websockets` | `false` | Every Codex OAuth credential uses the upstream WebSocket transport for downstream WebSocket requests, unless its auth file sets `websockets` itself. |
| `oauth.providers.codex.live-models` | `false` | Read the Codex model catalog from ChatGPT with each Codex OAuth credential and add what the bundled catalog lacks. |
| `upstream.claude.live-models` | `false` | Read the model list from Anthropic with each Claude OAuth credential and add what the bundled catalog lacks. |
| `client.native-model-lists` | `false` | Claude Code is only shown Claude models; Codex is only shown Codex models. |
| `client.key-scopes` | none | Limit a client API key to the credentials of the providers it lists. |
| (no setting) | on | Serve the Muse Code client endpoints (`/muse-code/models` and friends) so the Muse CLI can use the proxy. |

`docker/fork/config.default.yaml`, the configuration the image writes on first start, has every
one of them turned on. `config.example.yaml` is left exactly as upstream ships it, so it does not
mention them.

### Soonest-reset order

Subscription quota is use-it-or-lose-it: whatever is left in a window when it resets is gone.
`fill-first-order: "soonest-reset"` picks, among the credentials that are available:

1. credentials without a fully used window before credentials with one;
2. the soonest reset of the longest running window (the weekly budget), unknown reset times last;
3. the soonest reset of the shortest running window (the 5-hour session window);
4. the lowest credential ID.

A window that has already rolled over counts as unknown, because the provider starts the next
one only when the credential is used again.

Reset times come from the rate-limit headers of proxied responses
(`anthropic-ratelimit-unified-*`, `x-codex-*`). A credential that is not in use reports nothing,
so the quota refresher reads the usage endpoint for it (`/api/oauth/usage` for Claude,
`/backend-api/wham/usage` for Codex). It only polls a credential whose snapshot is older than
`quota-refresh-interval`, and only when the provider has at least two credentials to order.

With `routing.session-affinity: true` an established session stays on its credential; the order
above decides new sessions and rebinding after a credential becomes unavailable.

### Live models

Upstream ships a model catalog and refreshes it from its own repository every three hours. With
`live-models` on, the proxy also asks the provider what each account can use (at startup, then
hourly, and within five minutes of a credential being added) and registers the models the
catalog does not list yet. A discovered model takes its capability metadata from the catalog
model with the closest name and its name, context window and reasoning levels from the provider.
Catalog models are never removed or changed. For Codex, the provider's own catalog entries are
also what Codex clients are served.

### Key scopes

By default any client API key can reach every signed-in account: the proxy resolves a model name
to whichever providers serve it and translates between protocols on the way. `client.key-scopes`
ties a key to providers instead:

```yaml
access:
  api-keys:
    - "sk-claude-4f1c..."
    - "sk-codex-9a2e..."
    - "sk-muse-77b0..."

client:
  key-scopes:
    - key-prefix: "sk-claude-"
      providers: ["claude"]
    - key-prefix: "sk-codex-"
      providers: ["codex"]
    - key-prefix: "sk-muse-"
      providers: ["meta"]
```

A key that starts with `key-prefix` only sees the models of those providers, in every model list
the proxy serves, and a request for any other model is answered `400 model_not_found` without
touching a credential. When several entries match a key the longest prefix wins, and a full key
is a valid prefix. A key that matches no entry is unrestricted.

`providers` takes the provider a credential is registered under (`claude`, `codex`, `meta`,
`xai`, `gemini`, `antigravity`, `kimi`, `devin`, or the name of an OpenAI-compatible provider);
`anthropic`, `openai`, `chatgpt`, `muse` and `grok` are accepted as aliases.

Key scopes cover model lists and every model request, over HTTP and WebSocket. They do not
cover Codex's web search (`/v1/alpha/search`) and voice (`/v1/live`, `/v1/realtime`) endpoints,
which always use a Codex credential, and they do not apply in Home mode.

### Muse Code endpoints

The Muse CLI asks its endpoint for more than `/v1/responses`: it reads its model catalog from
`/muse-code/models` at the root of the same origin and does not start without it. The fork serves
these Muse Code endpoints by forwarding them to Meta with a Meta credential and returning the
answer unchanged:

| Endpoint | Used for |
| --- | --- |
| `/muse-code/models` | Model catalog |
| `/muse-code/config` | Client configuration |
| `/muse-code/search` | Web search tool |
| `/muse-code/browser_open` | Web fetch tool |

They take the same client API keys as the rest of the proxy. A key whose scope does not include
`meta` gets `404`. The endpoints that mint or revoke Meta credentials, and telemetry, are not
served. There is no setting; the routes answer `503` when no Meta credential is signed in, and
they are not available in Home mode.

To point Muse at the proxy, pin the endpoint in Muse's `settings.json` (Muse only sends its key
to an endpoint named there, not to one passed with `--base-url`) and give it a client API key:

```json
{
  "endpoint_transport": {
    "base_url": "https://proxy.example.com:8317/v1",
    "auth": "bearer"
  }
}
```

```bash
export META_API_KEY="sk-muse-77b0..."
```

## Container image

`ghcr.io/ntindle/cliproxyapi:latest` is built by `.github/workflows/fork-image.yml` from
`docker/fork/`, which layers on top of the repository's own `Dockerfile`.

| | |
| --- | --- |
| Port | `8317` (API, WebSocket and management panel at `/management.html`) |
| Volume | `/data`: `config.yaml`, `auths/`, `logs/`, `plugins/`, `static/` |
| `MANAGEMENT_PASSWORD` | Management key. Without it the panel is disabled unless `management.secret-key` is set. |
| `CLIPROXY_API_KEYS` | Comma-separated client API keys written to `config.yaml` when it is first created. A random key is generated when unset. |
| `TZ` | Time zone for logs. |
| `CLIPROXY_HEALTHCHECK=off` | Disables the container health check, which probes `GET /healthz` over plain HTTP. |

On first start the entrypoint writes `/data/config.yaml` from `docker/fork/config.default.yaml`
with every fork setting above turned on. After that the file belongs to the proxy and its panel.

Build locally from the repository root:

```bash
docker buildx bake -f docker/fork/docker-bake.hcl --load
```

## Staying in sync with upstream

`.github/workflows/fork-sync-upstream.yml` runs every Monday at 09:23 UTC and on demand. It
merges the latest upstream release tag into `main`, runs `.github/scripts/fork-check.sh`, pushes
the merge and publishes a new image. If the merge conflicts or the checks fail it pushes nothing
and opens an issue titled "Upstream sync needs attention" with the conflicting files.

Run it on demand with `gh workflow run fork-sync-upstream.yml`. Add `-f upstream_ref=<tag>` to
merge a specific upstream tag, and `-f branch=<name>` to rehearse the merge on another branch
without publishing an image.

Do not use the "Sync fork" button on GitHub: this fork removes upstream's workflow files and the
button would bring them back.

To merge by hand:

```bash
git fetch https://github.com/router-for-me/CLIProxyAPI.git 'refs/tags/v*:refs/tags/v*'
```

```bash
git merge v8.0.13
```

```bash
git checkout HEAD -- .github/workflows
```

```bash
bash .github/scripts/fork-check.sh
```

The fork changes these upstream files, each by a few lines, so these are where conflicts appear:

| File | Change |
| --- | --- |
| `internal/config/config_types.go`, `internal/config/sdk_config.go` | The settings above. |
| `sdk/cliproxy/service_config.go` | Builds the soonest-reset selector. |
| `sdk/cliproxy/auth/conductor_selection.go` | Treats that selector as built in and keeps it off the ID-ordered scheduler fast path. |
| `sdk/cliproxy/service_lifecycle.go` | Starts the quota refresher and the live model sync. |
| `sdk/cliproxy/service_models.go` | Adds live models when a Claude or Codex credential registers its models. |
| `internal/watcher/synthesizer/file.go` | Applies the Codex WebSocket default to auth files. |
| `internal/registry/codex_client_models.go` | Lays live Codex catalog entries over the installed catalog. |
| `sdk/api/handlers/claude/code_handlers.go`, `sdk/api/handlers/openai/codex_client_models.go` | Filter the model list shown to each native client. |
| `sdk/api/handlers/handlers_execution.go`, `sdk/api/handlers/handlers_stream.go` | Apply key scopes to the providers resolved for a request. |
| `sdk/api/handlers/handlers_interceptors.go` | Applies key scopes to every model list. |
| `internal/api/server_routes.go` | Registers the Muse Code endpoints. |

Everything else the fork adds lives in files upstream does not have.
