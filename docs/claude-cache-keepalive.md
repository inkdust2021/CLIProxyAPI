# Claude prompt-cache keepalive

CLIProxyAPI can renew eligible Claude prompt caches while the service runs. It is
disabled by default. No separate plugin, client patch, or shared library is needed.

## Enable or disable

In the Management Center, open **Configuration → Advanced → Claude** and toggle
**Claude prompt cache keepalive**, then save. The panel reads
`/v8/management/config.yaml` and saves changes through
`PATCH /v8/management/config`. The switch works with plugins disabled and is
searchable as `oauth.providers.claude.cache-keepalive`.

Set this switch in `config.yaml`:

```yaml
oauth:
  providers:
    claude:
      cache-keepalive: true
```

The legacy `claude.cache-keepalive` spelling is also accepted. Configuration file
hot reload applies the switch. Set it to `false` to cancel pending background
requests and discard all saved conversation snapshots. Restarting also clears
the snapshots; send a real chat request after enabling to populate them.

## How renewal works

After a real Claude request successfully completes and reports a cache read or
write, the service remembers its **final upstream request**. This is the request
after translation, cloaking, signing, tool-name mapping, and payload rules. Both
streaming and non-streaming chats are supported, including other client protocols
that route to the native Claude executor.

The service replays this snapshot with `max_tokens: 1`, preserving its model,
system prompt, tools, messages, metadata, cache breakpoints, thinking/effort,
Anthropic beta headers, and original streaming mode. The reply is consumed in the
background and is not appended to the user's conversation. Keeping the original
stream mode also preserves the signed CCH prefix: `max_tokens` is excluded from
the CCH signature.

Replays stay on the original credential; each replay resolves its current token
and uses the existing Claude HTTP transport and proxy settings. Removed, disabled,
or cooling-down credentials/models do not fail over to another account. A changed
upstream URL also stops replaying that snapshot.

Renewal follows the shortest TTL actually present in the request:

| Cache markers | Renewal interval |
| --- | --- |
| Default/explicit 5m, or mixed 5m and 1h | 4 minutes |
| Exclusively 1h | 50 minutes |

A 30-second scheduler checks for due sessions. TTL starts when the original or
renewal request starts, rather than when its response finishes. In-flight real
requests are not replayed. A new chat cancels an older replay for the same
account/model/session and replaces its snapshot. Expired snapshots are discarded
instead of deliberately paying to rewrite them.

The switch does not force 1h TTL onto client requests. To use the 50-minute
interval, configure the client to send 1h cache controls and verify the final
upstream request. Otherwise the existing 5m TTL is used.

## Bounds and eligibility

- At most 8 account/model/session snapshots are held in memory, each at most
  4 MiB. The least recently used conversation is replaced when capacity is reached.
- Session IDs separate conversations; without one, identity is derived from the
  original account, model, upstream URL, system/tools, and first user message.
- Conversations and authorization headers are not persisted. Authorization is
  reconstructed from the original account when replaying.
- Claude Code subagents/background requests, token-counting requests, requests
  without cache markers, and requests that report no cache usage are excluded.
- Manual `thinking.type: enabled` requests are excluded because their required
  thinking budget cannot fit within a 1-token output limit. Adaptive thinking and
  effort are preserved.
- Requests declaring server-side tools are excluded because a replay could perform
  additional billable work. Ordinary client-side tools are preserved.
- Detached keepalive is unavailable in Home mode, where credentials are scoped to
  individual execution sessions. Kimi and other delegated executors are unaffected.

An upstream/network error pauses the snapshot immediately; two consecutive
successful responses without a cache read also pause it. A successful new real
chat replaces and resumes that session. This feature does not block ordinary
chats or reserve a global quota slice.

## Observe costs and cache hits

Open **Logs → Claude keepalive**, or select **View keepalive logs** next to the
configuration switch. The view supports model/account/session search, outcome
filters, manual refresh and automatic refresh every 10 seconds. It shows the
effective enable state, tracked and paused session counts, cache-read tokens,
replay duration and upstream HTTP status when available.

`GET /v8/management/observability/claude-cache-keepalive` returns the same read-only
snapshot under normal management authentication. The service retains the newest
200 events in memory, including enable/disable, tracked sessions, successful
renewals, cache misses, failures, cancellations and expirations. Disabling renewal
preserves this history; restarting clears it. Reads do not consume usage records
and do not require file logging. Events contain hashed account/session identifiers
and operational metadata, never prompts, headers, credentials or raw upstream
error messages. New successful chats are required to start tracking; historic
events from before this version cannot be reconstructed.

Renewal requests have their own usage records with source
`claude-cache-keepalive`, credential identity, and actual token usage. They are
accounted independently of concurrent normal chats. Pause events are logged
without upstream response bodies or credential values.

Cache reads remain billable, and a renewal that misses can create a new cache
entry. Reducing the output cap does not make the entire request cost one token.
Compare `cache_read_input_tokens` and `cache_creation_input_tokens` with the
disabled baseline before deciding whether keepalive saves money for your usage.
Replaying an old prefix cannot preserve a future request's changed tools, system
instructions, model, thinking/effort, or metadata.

Validation uses deterministic scheduling tests and local HTTP upstreams for
streaming/non-streaming prefix equality, account isolation, fresh credentials,
config save/load and reload, cancellation, failure suspension, and resource
bounds. Actual upstream cache hit rates and savings require deployment telemetry;
no real account was used for automated verification.

## Research provenance

This implementation uses the periodic prefix-replay mechanism as a reference and
is implemented independently against CLIProxyAPI v8. No external runtime dependency
or plugin code is imported.

- [Claude-Code-Cache-Keepalive](https://github.com/romantcig/Claude-Code-Cache-Keepalive),
  revision `5e0d5558760eeb56d5dcfd2bd7bbcc95ff520f12`, MIT: its Windows-only static
  patch is not used.
- [cpa_claude_cache_keep](https://github.com/Banben07/cpa_claude_cache_keep),
  revision `9b8637d05834085a402431420304e31917c885d9`, version 0.8.12, MIT: its
  CPA v7 shared library, global quota guard, and global concurrent ping attribution
  are not imported. Native final-request replay avoids reapplying transformations
  and changing the prefix during loopback replay.
- [Anthropic prompt caching documentation](https://platform.claude.com/docs/en/build-with-claude/prompt-caching)
  defines TTL, cache-hit renewal, prefix matching, and billable cache reads.
