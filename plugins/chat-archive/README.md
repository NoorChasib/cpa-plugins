# Chat Archive

Chat Archive is a CPA (CLIProxyAPI) plugin that records every chat exchange CPA serves, masks secrets and uploads the result to your private chat archive server. It captures requests, responses (including streams and Responses WebSocket turns), retries, completions and usage records, splits them into content-addressed pieces and keeps them in a local outbox until the archive confirms them.

It never stores or sends raw credentials: client keys become caller fingerprints, upstream keys and account identifiers become keyed hashes, and every string is masked with the shared `mask/v1` rules before it touches disk.

**Requirements:** CPA v8.0.60 or newer (plugin RPC schema 7 or newer), Linux amd64 with glibc 2.36 or newer (the official CPA image qualifies), and a running chat archive server with a source token for this CPA.

## Install

1. In CPA's Plugin Store, add the catalog source if you have not already:

   ```text
   https://raw.githubusercontent.com/NoorChasib/cpa-plugins/main/registry.json
   ```

2. Install **Chat Archive**. CPA writes a plugin config holding only `enabled` and `store`, so the plugin starts in the `unconfigured` state: it registers, passes every request through untouched (it still strips `X-Archive-*` hint headers) and captures nothing.

## First use

1. Put two secrets into CPA's environment (for example as locked Coolify variables), then redeploy CPA once:
   - `CHAT_ARCHIVE_SOURCE_TOKEN`: the plugin's source token minted by the archive (`archive source add --kind cpa-plugin --label <name> --out-file <path>`). Read the file yourself; never paste the token into chat or logs.
   - `CHAT_ARCHIVE_HMAC_KEY_K1`: a random key of at least 32 bytes, kept in your password manager.

2. Merge the plugin config (CPA's plugin config page or `plugins.configs.chat-archive` in `config.yaml`):

   ```yaml
   plugins:
     configs:
       chat-archive:
         enabled: true
         priority: 100
         archive-url: "http://archive.example.ts.net:8790"
         source-token-env: CHAT_ARCHIVE_SOURCE_TOKEN
         hmac-key-id: k1
         hmac-key-env: CHAT_ARCHIVE_HMAC_KEY_K1
   ```

   Set `archive-url` to your archive's address on your tailnet or WireGuard network, as CPA's container reaches it. A container that cannot resolve tailnet names needs the archive's IP address instead.

   Secrets are accepted only by environment variable name (`*-env`) or by an absolute `0600` file owned by CPA's user (`*-file`), never as values in the config. Environment variables must exist when CPA starts: the plugin reads the environment CPA was started with.

3. Open **Chat Archive** in CPA's management sidebar. It should show `State: running`, a small pending count and a recent "Last archive ack". The page reads the plugin's authenticated status route with your console session and shows counters only.

4. Check the archive side: its `/v1/status` lists this CPA as a `cpa-plugin` source with a heartbeat younger than two minutes.

The outbox lives in `plugins/data/chat-archive/` under CPA's working directory (`/CLIProxyAPI/plugins/data/chat-archive/` in the official image), with directories `0700` and files `0600`. Set `capture: off` to stop capturing at once without uninstalling.

More: [reference](REFERENCE.md) (every setting, state and status field), [operations](docs/operations.md) (alerts, requeue, troubleshooting), [development](docs/development.md) (build and acceptance), [verification](docs/verification.md).
