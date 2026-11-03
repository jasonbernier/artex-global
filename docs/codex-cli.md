# Codex CLI provider

The `codex-cli` provider runs `codex exec` using the existing ChatGPT sign-in of
the operating-system account running ARTEX. No OpenAI API key is needed. Your
plan's model access and shared Codex usage limits still apply.

## Native installation

1. Install/update Codex CLI. Use a release supporting `exec --json`,
   `--output-schema`, `--output-last-message`, `--ephemeral`, and
   `--ignore-user-config`.
2. Run `codex login` as the same OS account that will run ARTEX, and choose ChatGPT
   sign-in. Verify with `codex login status`. An existing ChatGPT login needs no
   further sign-in.
3. Build/run this version of ARTEX. Create a profile in **System → LLM** with
   **Codex CLI (ChatGPT)** as its format. Leave the model blank for Codex's built-in
   default, or enter a model ID available to your subscription. Click **Test
   connection**, then activate the profile or assign it to a task.

Alternatively, with no active database profile, configure the server environment:

```powershell
$env:ARTEX_LLM_PROVIDER = 'codex-cli'
# Optional, when the native executable is not on the service's PATH:
$env:ARTEX_CODEX_BIN = 'C:\path\to\codex.exe'
$env:ARTEX_CODEX_TIMEOUT = '5m'
# Optional: $env:ARTEX_LLM_MODEL = 'a-model-available-to-your-account'
.\start.bat
```

On Linux/macOS, export the same variables and run `./start.sh`. If `codex` is on
PATH, leave `ARTEX_CODEX_BIN` unset. On Windows, point it to the native executable,
not a `.cmd`/`.bat` launcher. Persisted active profiles take precedence over the
environment configuration, as with API providers.

The adapter ignores Codex's user `config.toml` while retaining its credential
store, including an explicitly configured `CODEX_HOME`. This avoids inheriting
custom providers, MCP tools, plugins or hooks. Configure the model and reasoning
effort in ARTEX. The adapter requires ChatGPT authentication and removes ambient
API-key variables from its child process; it never falls back to API-key billing.

## Docker and service accounts

Prebuilt images need a new release before they include this provider. Build ARTEX
from this source first. To install the CLI in an image built from this source, set
the optional `CODEX_CLI_VERSION` build argument to a version supporting the flags
above:

```sh
# First prepare dist/<architecture>/artex as documented in Dockerfile.
docker build --build-arg CODEX_CLI_VERSION=<version> -t artex:codex .
```

Use that image for the `artex` service, and add a persistent credential volume
(for example `codex-auth:/root/.codex` for the default root container user, with
`codex-auth:` declared under top-level `volumes`). Log in inside the container with
`docker compose exec artex codex login --device-auth` if device login is enabled
for your account. Verify with `docker compose exec artex codex login status`, then
create the profile in the UI. For a non-root user, mount the volume at that user's
`.codex` directory instead. Keep credentials out of source control and images.

Host keychain sign-in and a Windows executable are not automatically available in
a Linux container. A service running as another OS account similarly needs its
own Codex sign-in. No HTTP bridge port is opened by this provider.

## Behavior and limits

- Each model request starts one ephemeral CLI process in a temporary directory.
  ARTEX supplies its system instructions, history, tool schemas and tool results.
  Threads are not reused across workers or chats.
- Codex returns one structured assistant turn. ARTEX validates tool names and
  JSON-object arguments, then processes tool requests through its existing
  harness and approvals. CLI shell, web search, hooks, apps and subagents are
  disabled. The CLI sandbox is read-only; no approval-bypass flag is used.
- Both streaming and non-streaming ARTEX paths work. Streaming is **buffered**:
  output appears after the entire CLI reply is validated.
- Token counts are reported when available. API price estimates in ARTEX do not
  represent subscription billing. Each fresh invocation includes Codex's own
  instruction overhead, so long histories can consume usage allowance quickly.
- CLI output-token caps and HTTP proxy/base URL/session headers do not apply.
  ARTEX context compaction still works. Temperature and stop sequences return a
  clear unsupported-setting error if requested.
- `ARTEX_CODEX_TIMEOUT` bounds each call (default `5m`). Cancellation terminates
  the CLI child. The usual connection-test timeout remains 30 seconds.
- Profile request-rate settings are shared by that provider's callers. Lower
  worker concurrency if you hit subscription limits. Auth and usage failures
  surface as provider errors.

## Validation

```sh
go test ./codexcli ./agent ./server ./db ./llmpool
# Optional harmless live tests; consume subscription usage:
ARTEX_TEST_CODEX_LIVE=1 go test ./codexcli -run TestLiveCLI -v
```

On PowerShell, set `$env:ARTEX_TEST_CODEX_LIVE='1'` before the live tests.
Ordinary tests use a fake CLI subprocess, require no credentials, and make no
network requests. Database integration tests require PostgreSQL and otherwise skip.

See official [non-interactive usage](https://learn.chatgpt.com/docs/non-interactive-mode),
[CLI flags](https://learn.chatgpt.com/docs/cli/reference) and
[authentication](https://learn.chatgpt.com/docs/auth).
