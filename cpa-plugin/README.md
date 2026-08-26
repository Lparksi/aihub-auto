# aihub-auto CPA plugin

This is a Go `c-shared` foundation for CLIProxyAPI (CPA) plugins. It uses CPA
ABI v1 and plugin RPC schema v3, verified against CPA commit
`3cd07261dce26beb0624ebc27347c22d2febf84b`.

## Status

Phase 6 adds ABI-boundary integration coverage, an opt-in real CPA dynamic
loader fixture, and release/compatibility automation. Phase 4 adds versioned,
atomic non-secret state primitives, account/plan scoped default and Luna
key-pool metadata, and scoped session/Responses affinity primitives. Phase 3
adds in-memory candidate scheduling for explicitly attributed AIHub accounts.
It retains Phase 2's CPA schema v3 auth parsing and refresh, authenticated
model discovery, and OAuth-scoped OpenAI chat-completions execution. Every
outbound AIHub request uses CPA's host HTTP transport. Credentials are never
part of plugin configuration or error messages.

CPA does not supply credentials metadata to `auth.login.start`, so this plugin
does **not** advertise an interactive login. Import credentials only through
CPA's authenticated management API: `POST /v0/management/plugins/aihub-auto/credentials`
with `{"email":"...","password":"..."}`. It calls the confirmed
`POST /api/v1/auth/login` endpoint through CPA transport, then saves only the
returned token material with CPA's required `host.auth.save` callback. The
password is neither persisted nor returned. Existing positively identified
AIHub credential files can also be imported with `auth.parse`, then renewed
with `auth.refresh`.

## Management API and dashboard

CPA authorizes Management API calls before they reach the plugin. The plugin
still matches every registered method and path exactly, requires
`Content-Type: application/json` for every mutation, and rejects unknown JSON
fields. Management responses contain only non-secret configuration and
aggregate runtime metadata: no password, access token, refresh token, API-key
material, raw session key, alias key, or manual-lock auth identifier is ever
returned.

Authenticated Management API endpoints are:

- `GET /v0/management/plugins/aihub-auto/status`
- `POST /v0/management/plugins/aihub-auto/credentials` with
  `{"email":"...","password":"..."}`
- `POST /v0/management/plugins/aihub-auto/config` with
  `{"config_yaml":"..."}`
- `POST /v0/management/plugins/aihub-auto/lock` with `{"auth_id":"..."}`
- `POST /v0/management/plugins/aihub-auto/unlock` with `{}`
- `POST /v0/management/plugins/aihub-auto/reconsider` with `{}`
- `POST /v0/management/plugins/aihub-auto/sessions/clear` with `{}`
- `POST /v0/management/plugins/aihub-auto/aliases/clear` with `{}`
- `POST /v0/management/plugins/aihub-auto/pools/reconcile` with `{}`

The browser resource is `GET /v0/resource/plugins/aihub-auto/dashboard`. It
contains no external dependencies or stored secrets and renders status using
`textContent`; it fetches the same-origin authenticated status endpoint only.
The resource itself is a CPA resource and is not a substitute for management
authorization.

Manual lock, session/alias clears, and reconsideration are runtime-only. A
manual lock affects scheduler selection only while this plugin process is
running. The available pool lifecycle is `NoopLifecycle`, so reconciliation
reports non-secret metadata but does not claim AIHub API-key creation,
deletion, or other external key operations.

## Local CPA setup

This module resolves the CPA module itself from the local authoritative
checkout:

```bash
cd /Users/mimmer/Projects/aihub-auto/cpa-plugin
go mod tidy
go test ./...
```

`go.mod` contains:

```text
require github.com/router-for-me/CLIProxyAPI/v7 v7.0.0
replace github.com/router-for-me/CLIProxyAPI/v7 => /Users/mimmer/Projects/CLIProxyAPI
```

The local `replace` avoids fetching CPA source, but it does not make Go builds
fully offline: transitive dependencies may still be read from the module cache
or downloaded when absent. For offline builds, populate the Go module cache
first (or vendor every required dependency) and run with network access
disabled only after that preparation.

## Install in CPA

This plugin is an **additional CPA deployment option**. It is not the
standalone aihub-auto Router or Tauri desktop application.

1. Build on the same operating system and architecture as the CPA process:

   ```bash
   cd /Users/mimmer/Projects/aihub-auto/cpa-plugin
   make build
   ```

   Native artifact names are platform-specific:

   | CPA host platform | File to copy |
   | --- | --- |
   | macOS | `aihub-auto.dylib` |
   | Linux | `aihub-auto.so` |
   | Windows | `aihub-auto.dll` |

2. Copy that library (not the generated C header) into CPA's `plugins.dir`.
   CPA's YAML spelling is `plugins.dir`; when it is empty, CPA resolves it to
   the `plugins` directory relative to the CPA working directory. For example:

   ```yaml
   plugins:
     enabled: true
     dir: /absolute/path/to/cliproxy-plugins
     configs:
       aihub-auto:
         enabled: true
         priority: 0
         model_patterns: ["gpt-*", "o3"]
         mode: balanced
         price_band_min: 0
         price_band_max: 1
   ```

   CPA derives the plugin ID from the filename. The file must therefore retain
   the ID-bearing name `aihub-auto` (optionally with CPA's `-vVERSION` suffix)
   and the configuration key must be the same ID: `aihub-auto`.

3. Restart or reload CPA, then confirm its Plugins management view reports a
   registered, enabled `aihub-auto` plugin. CPA authenticates management API
   requests before forwarding any registered management route to this plugin.

The browser dashboard resource URL is
`GET /v0/resource/plugins/aihub-auto/dashboard` on the CPA server, for example
`http://127.0.0.1:8317/v0/resource/plugins/aihub-auto/dashboard`. The resource
is not a management-authenticated endpoint and shows non-secret status only.
The matching management status endpoint is
`GET /v0/management/plugins/aihub-auto/status` and remains protected by CPA's
management authorization.

## Configuration

CPA supplies plugin YAML in lifecycle requests. All fields are optional:

```yaml
enabled: true
priority: 0
provider_id: aihub-auto
base_url: https://aihub.top
max_response_bytes: 4194304
max_request_bytes: 4194304
management_enabled: true
mode: balanced
price_band_min: 0
price_band_max: 1
manual_lock_auth_id: ""
model_patterns: []
state_dir: ""
default_pool_size: 4
luna_pool_size: 4
session_ttl: 24h
```

`enabled` and `priority` are CPA-owned fields. The plugin accepts them because
CPA injects them into lifecycle YAML, but does not expose or interpret them as
plugin configuration.

`base_url` must be a complete HTTP(S) origin without a path, credentials,
query, or fragment. It defaults to `https://aihub.top` and is normalized by
removing a trailing slash.

`max_request_bytes` conservatively caps accepted executor payloads and management
mutation bodies. It must be positive and defaults to 4 MiB; the native ABI also
rejects requests larger than this default before copying them out of C memory.

`max_response_bytes` caps both normal and stream executor bodies. CPA's ABI
does expose host stream reads, but the phase-two executor response remains the
ABI-prescribed buffered chunk envelope; SSE is therefore buffered up to this
limit before it is returned. Oversized responses fail safely.

`mode` controls actual candidate scoring: `economy` favors the lowest healthy
price tier, `balanced` weights price and first-token latency equally, and
`speed` favors lower first-token latency. `price_band_min` and
`price_band_max` are an inclusive rate-multiplier filter. `manual_lock_auth_id`
uses that eligible candidate when it is available; an unavailable or tripped
lock safely falls back to the scored candidate.

`model_patterns` is the explicit list of AIHub model names or trailing-asterisk
prefixes that `model.route` sends to this plugin executor. An empty list is the
default and deliberately leaves every model unhandled, preserving CPA built-in
routing. For example, `['gpt-*', 'o3']` owns `gpt-4` and `o3` only.

`scheduler.pick` considers only `aihub-auto` candidates whose immutable
`aihub_auto_account_id` and `aihub_auto_plan` attributes match the CPA request
metadata. If CPA omits that metadata, the plugin safely selects only when all
eligible plugin candidates have exactly one account/plan scope; otherwise it
returns unhandled and CPA continues with its built-in scheduler. The plugin
creates its own attributes from imported auth: the confirmed account ID, a
truthful `unknown` plan when AIHub does not return a plan, and conservative
defaults of group `1`, rate multiplier `1`, and TTFT `1000` ms. CPA or an
operator can provide more specific values. The optional
`aihub_auto_group_id`, `aihub_auto_rate_multiplier`,
`aihub_auto_ttft_ms`, `aihub_auto_provider_available`, and comma-separated
`aihub_auto_models` refine selection; missing metrics use those conservative
plugin defaults.

When CPA supplies the non-secret, already-hashed `aihub_auto_session_key`
metadata, `scheduler.pick` reuses a compatible binding. The optional
`aihub_auto_pool: luna` selects the isolated Luna namespace; absent or other
values use the default namespace. Invalid or no-longer-eligible bindings are
cleared before normal selection.

`state_dir`, when set, must be an absolute path. The `internal/state` store
writes a versioned JSON snapshot using a same-directory temporary file, `fsync`,
and atomic rename; it uses mode `0600` on Unix. It persists only routing
breaker/observation records, hashed session/Responses alias records, and remote
key metadata. Corrupt, unreadable, or unsupported-version state is ignored
safely. No API key, access token, refresh token, password, request body, or raw
session identifier is serializable by this format. Management status reports
`in-memory runtime state only` when `state_dir` is unset, `persistent state
configured and healthy` after a successful persistent snapshot write, and
`persistent state save failed` if a later write fails.

`default_pool_size`, `luna_pool_size`, and `session_ttl` configure the
phase-four pool and affinity packages. Pool identity is always `(account_id,
plan, pool, group_id)`; default and Luna pools, and every account/plan pair,
remain isolated. Session affinity is compatible only with the exact account,
plan, default/Luna pool, and model. Expired or explicitly invalidated bindings
and Responses aliases are removed. A stream may select a fallback only before
output starts; a broken post-output stream clears affinity instead of replaying
partial output.

The phase-four packages provide persistence and lifecycle-safe algorithms;
the currently available CPA executor request schema exposes only the selected
auth's `StorageJSON` and does not provide host APIs for creating, listing, or
storing additional API keys. Key creation, mutation, list, and deletion remain
intentionally deferred until a confirmed CPA auth-storage/lifecycle contract is
available. Existing confirmed AIHub calls remain exactly: `POST /api/v1/auth/login`,
`POST /api/v1/auth/refresh`, `GET /api/v1/auth/me`, `GET /v1/models`, and
`POST /v1/chat/completions`.

Configuration has no secret, token, password, or API-key fields. Future auth
integration must use CPA's auth store rather than plugin configuration.

## Build

Native builds require a C toolchain for the target platform. Tests do not
cross-compile or require a cross-compiler. `make build`, `make build-darwin`,
`make build-linux`, and `make build-windows` intentionally reject a different
host OS instead of silently attempting an unsupported CGO cross-build. Build
each release artifact on that native OS with its native C compiler.

```bash
make test
make build-darwin
make build-linux     # run on Linux
make build-windows   # run on Windows
```

`make build-windows` must run on Windows; a Windows cross-compiler alone is
not a supported release workflow. The GitHub Actions matrix follows this same
native-build rule. Run `make clean` after local artifact validation.

Artifacts are written to `dist/` as `.dylib`, `.so`, or `.dll` together with
their Go-generated C headers.

## Layout

- `cmd/aihub-auto-plugin`: CPA ABI v1 C export shim.
- `internal/abi`: schema v3 envelopes, lifecycle, auth/model/executor dispatch.
- `internal/configuration`: typed, validated, non-secret YAML configuration.
