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
requests and discard all saved conversation snapshots. Service restarts preserve successful snapshots and restore those whose original
cache TTL has not expired. Restarting does not reset the cache lifetime.

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
account/model/session. Only its successful completion with cache usage replaces
the snapshot; failures, missing cache usage, and ineligible requests retain the
last successful snapshot without extending its TTL. Expired snapshots are discarded
instead of deliberately paying to rewrite them.

The switch does not force 1h TTL onto client requests. To use the 50-minute
interval, configure the client to send 1h cache controls and verify the final
upstream request. Otherwise the existing 5m TTL is used.

## Bounds and eligibility

- At most 8 account/model/session snapshots are held in memory. The least recently
  used conversation is replaced when capacity is reached. Request bodies and the
  encrypted persistence file have no fixed size limit.
- Session IDs separate conversations; without one, identity is derived from the
  original account, model, upstream URL, system/tools, and first user message.
- Successful upstream conversation snapshots are encrypted on disk in the auth
  directory. Authorization headers are excluded and reconstructed from the original
  account when replaying. Pending or unsuccessful requests are never saved.
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
chat replaces and resumes that session. The separate quota-reserve switch below
controls whether ordinary chats can use the last 1% of a Claude OAuth account.

## Reserve Claude quota for keepalive

In **Configuration → Advanced → Claude**, enable **Reserve 1% quota for
keepalive** after enabling keepalive, or set:

```yaml
oauth:
  providers:
    claude:
      cache-keepalive: true
      cache-keepalive-reserve-quota: true
```

The reserve switch is off by default. With it on, ordinary chats skip a Claude
OAuth account when either the observed 5-hour or weekly utilization is at least
99%. Keepalive may still use the original account's remainder; real upstream
cooldowns and disabled credentials still apply. A valid reset releases the
reservation. The filter applies to normal, mixed-provider, pinned, affinity,
and plugin credential selection. It has no effect in Home mode or on API keys.

The service also estimates whether the next ordinary request would cross 99%.
For each account and model, it keeps the last 32 ordinary request workloads and
uses an upper sample. Input, output, cache reads and cache writes contribute
using [Anthropic API pricing ratios](https://platform.claude.com/docs/en/about-claude/pricing)
as a workload prior: output costs 5x input for supported models, 5-minute
writes 1.25x, 1-hour writes 2x, and cache reads normally 0.1x (0.05x for
Opus/Sonnet 5.5). Unknown historical cache-write TTL uses the conservative 2x
weight. The service learns the conversion from workload to quota utilization
only after at least three positive changes in actual upstream quota headers
within the same reset window. API prices alone do not determine a subscription
account's 5-hour or weekly allowance. Missing or stale quota data leaves only
the observed threshold rule; prediction never invents a quota percentage.

In-flight chats reserve their estimated work until completion, including the
end of a stream. Keepalive updates observed utilization without becoming a
normal-chat workload sample or calibrating an overlapping chat. Forecasts and
workload samples are saved in `auths/.claude-cache-quota.json` with owner-only
permissions and restored after restart. The predictor's synchronous snapshot
write adds disk latency at request completion. Requests elsewhere on the same
account, upstream rounding, and unexpected chat size can still cross the 1%
line; this is a conservative forecast, not a hard guarantee.

For an existing installation, `scripts/seed_claude_quota_workload.py` can import
ordinary token history from the usage-report SQLite database before the first
predictive run. Stop the service, run the script with the SQLite path, auth
directory and a new output path, then move its output to
`auths/.claude-cache-quota.json` before restart. The script opens SQLite
read-only, refuses to overwrite existing predictor state and imports only
workload samples; historical logs contain no quota-header calibration.

## Manage individual sessions

Open **Logs → Claude keepalive** to see current sessions above the event history.
Each account/model/session has one row with its model, hashed identity, last user
prompt, state, last chat time, next renewal time, an individual keepalive
switch, and a delete action. Search current sessions by prompt, model, or identity. Event history has
its own search and outcome filters.

Switching a session off cancels its active replay and saves the preference
immediately. Later successful chats update its preview but do not re-enable it.
The choice survives global toggles, config reloads, and restarts. Switching it
back on uses the remaining cache TTL; it never extends or revives an expired
snapshot. Disabled identities remain listed after restart so they can be enabled
again even before a new chat arrives.

Only the full SHA-256 session IDs are persisted in
`oauth.providers.claude.cache-keepalive-disabled-sessions`. Prompt previews come
from the latest successful eligible upstream snapshot, include only user text
(excluding tool results and images), and are capped at 2,000 Unicode characters.
They are exposed only through authenticated management and are reconstructed
from encrypted successful snapshots after restart.

`GET /v8/management/observability/claude-cache-keepalive` includes
`session_details` and `disabled_sessions` alongside event history.
`PATCH /v8/management/observability/claude-cache-keepalive/sessions/:id` accepts
`{"enabled": false}` or `{"enabled": true}` under normal management
authentication. Unknown identities return 404, invalid IDs/bodies return 400,
and failed config saves leave the preference unchanged. Manual switch events
contain no prompts.

`DELETE /v8/management/observability/claude-cache-keepalive/sessions/:id` removes
that session's successful snapshot, invalidates pending chat completions, cancels
its renewal, and clears its disabled preference. Disabled-only rows can also be
deleted. Deletion preserves event history, chat history, and authentication files.
A future successful eligible chat can create a new active snapshot for the same
identity. Deleted snapshots stay removed after restart.

The endpoint accepts lowercase 64-character hexadecimal IDs and returns
`{"status":"ok"}` on success. Invalid IDs return 400, unknown identities return
404, and an unavailable keeper/configuration returns 503. Configuration or
snapshot persistence failures return 500 and retain the session; unreadable
recovery files also block deletion. Failed snapshot saves restore the disabled
preference, and a failed preference rollback is reported explicitly.

## Restart recovery

Recovery is automatic when the configured auth directory is available. The
service writes `.claude-cache-keepalive.bin` using AES-256-GCM and a random local
key in `.claude-cache-keepalive.key`. Both files have owner-only permissions.
Keep the two files together when backing up or moving the auth directory; losing
the key makes saved state unreadable. Docker deployments must persist the auth
directory, as they already do for provider credentials.

Successful chats and renewal results are saved immediately using atomic file
replacement. Recovery does not depend on graceful shutdown. Startup loads the
bounded history and still-valid successful snapshots, preserving account/session
identity, last chat time, the original TTL anchor, pause/miss state and configured
per-session exclusions. The scheduler checks restored due sessions as soon as
the original credential and executor setup is complete. Bearer tokens, cookies
and API-key authorization headers are not saved; replays resolve current
credentials as before.

Stopping the service cancels workers without disabling or erasing recovery
state. Turning the global keepalive switch off deliberately discards snapshots
but retains history. After a restart longer than a cache's remaining TTL, the
snapshot is skipped rather than deliberately paying to rewrite expired content.
Persistence errors produce operational warnings and do not fail real chats.
Unreadable or corrupt recovery files are retained rather than overwritten.

Upgrading from a memory-only version cannot recover snapshots already lost in
a prior restart. A new successful eligible chat populates durable state for the
next restart.

## Observe costs and cache hits

Open **Logs → Claude keepalive**, or select **View keepalive logs** next to the
configuration switch. The view supports model/account/session search, outcome
filters, manual refresh and automatic refresh every 10 seconds. It shows the
effective enable state, tracked and paused session counts, cache-read tokens,
replay duration and upstream HTTP status when available.

`GET /v8/management/observability/claude-cache-keepalive` returns the same operational
snapshot under normal management authentication. The service retains the newest
200 events, persisted alongside the encrypted recovery state, including
enable/disable, tracked sessions, successful
renewals, cache misses, failures, cancellations and expirations. Disabling renewal
preserves this history; restarting restores it. Reads do not consume usage records
and do not require file logging. Events contain hashed account/session identifiers
and operational metadata, never prompts, headers, credentials or raw upstream
error messages. New successful chats are required to initially populate tracking; historic
events and snapshots lost by older memory-only versions cannot be reconstructed. This is an event history,
not a list of separate renewal tasks: repeated successful chats and renewals can
produce several rows with the same account/session ID while using one tracked
session slot.

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
