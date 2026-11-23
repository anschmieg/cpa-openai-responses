# OpenAI Responses provider for CLIProxyAPI

This CPA v8 plugin publishes explicitly configured models under
`openai-responses/<model>`. It sends POST requests directly to the upstream
`/responses` and `/responses/compact` endpoints over HTTPS. It supports
buffered Responses and SSE. The project key is configured in CPAMC independently of the ChatGPT OAuth
credentials.

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
instances register with no models until a key, persistent state file, and
model list are configured.

### Build manually

Build on the target Linux architecture with cgo and a C compiler:

```sh
cd plugins/openai-responses
go build -buildmode=c-shared -o /tmp/openai-responses.so .
```

Install the library as `openai-responses.so` in CPA's configured plugin
directory. Choose a writable state file under the persistent mount with an
existing parent directory. Restart CPA to replace the shared library.

Merge these entries into the native v8 config and list exact authorized
upstream model IDs:

```yaml
plugins:
  enabled: true
  configs:
    openai-responses:
      enabled: true
      base_url: https://api.openai.com/v1
      state_file: /home/agent/cliproxy-staging/state/openai-responses.db
      models:
        - gpt-5.4
```

In CPAMC, open **Plugins → openai-responses → Configure** and save the project
API key in `api_key`, along with the state file and models. The patched Tokligence
CPAMC v1.29.1 panel masks this field. CPA stores the key in the protected provider
configuration and reloads the plugin. It publishes the configured models once
a key is present. If you configure an environment key for a headless install,
`api_key_env` selects its name and defaults to `OPENAI_RESPONSES_API_KEY`.
The CPAMC value takes precedence when both are present.

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
It checks an authenticated CPAMC-style key update and hot reload, discovery,
request bytes and cache controls, signature rejection and caller isolation,
SSE, compaction, errors, and replay after restart.
