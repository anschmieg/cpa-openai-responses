# OpenAI Responses provider for CLIProxyAPI

This CPA v8 plugin publishes explicitly configured models under
`openai-responses/<model>`. It sends POST requests directly to the upstream
`/responses` and `/responses/compact` endpoints over HTTPS. It supports
buffered Responses and SSE. The project key is entered through the **OpenAI Responses Key** page in CPAMC, independently of the ChatGPT OAuth credentials.

The plugin preserves the original Responses JSON and patches only the selected
upstream model and stream flag when needed. Hosted tools, structured output,
multimodal input, background creation, response and conversation references,
and opaque continuation returned by this route use the upstream API's request
shapes. Client-supplied cache keys, cache options, retention, and breakpoints
pass through. Prompt prefix and tool order are preserved. Cached-token counts
come from OpenAI. Because the plugin uses the original body, CPA body rewrite
rules do not alter this route's outbound payload. Use CPA access policies to
control route access. [OpenAI prompt caching](https://developers.openai.com/api/docs/guides/prompt-caching)

Outbound headers are constructed by the plugin. It rejects known Codex client
markers and validates opaque reasoning and compaction before dispatch. OpenAI
and Codex tokens can look alike, so the plugin accepts an opaque token only if
the configured upstream returned it for the same authenticated CPA caller and
upstream credential. The database stores token digests. Full opaque continuation
after restart requires the same state file, CPA caller key, and project key.
When moving a conversation from another route, start fresh or continue it on its
original route. Keep this provider scoped to the non-Codex clients allowed to
use the project key. [OpenAI API credits](https://developers.openai.com/api/docs/guides/chatgpt-api-credits#what-the-credits-cover)

## Install and configure

For CPA's Plugin Store, add this registry URL under `plugins.store-sources`:

```text
https://raw.githubusercontent.com/anschmieg/cpa-openai-responses/main/registry.json
```

In CPAMC, open **Plugins → Store** and install **OpenAI Responses**. The
registry supplies a checksum-verified Linux ARM64 build. Newly installed
instances register with no models until a key and persistent state file are
configured.

In CPAMC, save the state file and optional model allowlist under **Plugins → openai-responses →
Configure**. Then open **OpenAI Responses Key** in the sidebar of the Tokligence
panel and enter the project key in its password field. The form sends the key
under an ephemeral RSA public key so request logs at the `/admin` alias do not
capture the plaintext. The page and the config API never return the newly
entered key.

CPA stores the key with AES-256-GCM in the plugin database. For production, set
`OPENAI_RESPONSES_STORAGE_KEY` to a base64-encoded 32-byte random value in the
Coolify secret environment before adding the first key. Otherwise the plugin
creates `<state_file>.key` as a mode-`0600` file. The key file and database must
both be kept private. Keep the encryption key separate from database backups.
A headless install can still use `api_key_env` (default `OPENAI_RESPONSES_API_KEY`).
The encrypted credential takes precedence for its configured upstream URL.

### Upgrade from v0.1.0

The previous `api_key` field was stored in plain text in CPA YAML. On first load,
v0.2.0 encrypts the legacy value to preserve active routes. Remove the plaintext
`api_key` entry from CPA Configuration → Source after updating the plugin, then
create a new OpenAI project key and save it in **OpenAI Responses Key** before
revoking the old key. Prior configuration backups may still contain the old key.

### Build manually

Build on the target Linux architecture with cgo and a C compiler:

```sh
cd plugins/openai-responses
go build -buildmode=c-shared -o /tmp/openai-responses.so .
```

Install the library as `openai-responses.so` in CPA's configured plugin
directory. Choose a writable state file under the persistent mount with an
existing parent directory. Restart CPA to replace the shared library.

Merge these entries into the native v8 config:

```yaml
plugins:
  enabled: true
  model-refresh-seconds: 3600
  model-refresh-providers:
    - openai-responses
  configs:
    openai-responses:
      enabled: true
      base_url: https://api.openai.com/v1
      state_file: /home/agent/cliproxy-staging/state/openai-responses.db
```

### Model discovery

With `models` omitted or empty, the plugin fetches `/models` after a key is
configured and CPA next registers or refreshes the provider. The refresh interval is shared by all listed providers. Append
`openai-responses` to any existing refresh provider list rather than replacing it.
The catalog does not report Responses compatibility, so
discovery exposes known text model families and skips dedicated audio, image,
realtime, and other specialized variants. Upstream catalog failures fail closed
until the next successful refresh. To pin specific IDs or use a custom model
family, set `models` to a nonempty list; pinned mode does not query `/models`.

`base_url` includes the API prefix, typically `/v1`. Use a trusted HTTPS
upstream. The host owns retry, streaming bootstrap, and response-header policy.
For a single-dispatch route, use one credential, disable request and bootstrap
retries, and keep this provider out of automatic fallback pools.

CPA v8.0.49 exposes create and compact through the executor interface. Native
upstream WebSocket sessions and resource endpoints for retrieving, cancelling,
deleting, or listing stored response input items require separate host routes.
Background creation can return a response ID. Polling that ID uses the official
API directly with the corresponding project credentials.

## Verify

```sh
python3 testing/mock_host.py --binary /path/to/cli-proxy-api \
  --plugin /tmp/openai-responses.so
```

The test starts a fresh CPA instance on loopback and uses a local HTTPS mock.
It checks the authenticated password form, encrypted key persistence, hot reload, discovery,
request bytes and cache controls, signature rejection and caller isolation,
SSE, compaction, errors, and replay after restart.
