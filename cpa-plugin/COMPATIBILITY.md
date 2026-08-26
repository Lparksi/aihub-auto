# CPA Compatibility

## Recorded baseline

This plugin was validated locally against CLIProxyAPI (CPA) commit
[`3cd07261dce26beb0624ebc27347c22d2febf84b`](https://github.com/router-for-me/CLIProxyAPI/commit/3cd07261dce26beb0624ebc27347c22d2febf84b).

| Contract | Value |
| --- | --- |
| Native ABI | v1 |
| Plugin RPC schema | v3 |
| Plugin ID | `aihub-auto` |
| Native artifact base name | `aihub-auto` |
| Request limits | `max_request_bytes` and `max_response_bytes`, default 4 MiB |

CPA `main` is intentionally tracked rather than treating the recorded commit as
a permanent version pin. CPA owns the dynamic-loader ABI and public plugin SDK,
so compatibility needs continuous validation as its main branch evolves. The
recorded commit is the known-good local baseline; the scheduled/manual GitHub
workflow rechecks the current CPA `main` using the current CPA Go toolchain.

## Validate an update

1. Check out CPA at the candidate revision and make the plugin's local
   replacement point at that checkout. For the maintained local setup this is:

   ```bash
   cd /Users/mimmer/Projects/aihub-auto/cpa-plugin
   go mod edit -replace github.com/router-for-me/CLIProxyAPI/v7=/Users/mimmer/Projects/CLIProxyAPI
   go mod download
   go test ./...
   go vet ./...
   make build
   make clean
   ```

2. On macOS or Linux with CPA dependencies available, run the real loader
   compatibility fixture. It builds the current-platform library and loads it
   through CPA's **public** `sdk/pluginhost`, then probes scheduler dispatch
   and a real native host HTTP callback failure through the dynamically loaded
   plugin. Plugin ABI regression tests cover successful and error envelopes
   for HTTP, stream, and auth-save callbacks with simulated C-host responses:

   ```bash
   go test -tags=cpa_dynamic_loader ./integration
   ```

   At the recorded local CPA checkout, this tagged command is currently
   blocked before compilation by CPA's own incomplete `go.sum` entry for
   `github.com/sirupsen/logrus`, imported through
   `internal/misc/antigravity_version.go`. Regular plugin tests, vet, and the
   native build do not require that tagged integration package and remain part
   of the required local verification.

3. Run `.github/workflows/cpa-plugin.yml` manually. Its compatibility job
   checks out CPA `main`, rewrites the local `replace` to that checkout, and
   runs the same tagged fixture on Linux.

The dynamic-loader fixture currently supports only native `darwin` and `linux`
hosts because CPA's public loader uses `dlopen` there. Windows builds remain
native artifact checks in the build matrix; do not claim a Windows loader test
until CPA exposes an equivalent public test surface.

## If validation fails

1. Do not release the generated library.
2. Identify whether CPA changed ABI v1, schema v3, a public `pluginapi` type,
   loader behavior, or a documented configuration route.
3. Make the smallest compatible plugin change and add a focused regression
   test. Never change the recorded baseline merely to hide a failure.
4. Re-run the complete validation sequence and update this document with the
new CPA commit, ABI/schema values, constraints, and any migration notes.
5. If CPA changed the ABI or schema incompatibly, ship a separately named
   compatible plugin release or wait for a CPA migration path; do not load an
   ABI-incompatible artifact.
