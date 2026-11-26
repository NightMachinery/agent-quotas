# agent-quotas: design

This document is the contract the code follows. When the code and this document
disagree, one of them is a bug.

## Goal

One long-running helper owns every coding-agent account's quota (Claude Code
subscription profiles, Codex auth snapshots, Antigravity accounts) and resumes
registered agent sessions when the account they are blocked on becomes usable
again. Every reader that used to fetch quota on its own (shell reports, status
pages, auto-continue watchers) becomes a client of this one helper.

Why one helper:

- **One poller per account.** The Claude usage endpoint answers repeated calls
  with HTTP 429 and recovers slowly. Several independent readers polling the
  same account were the main source of 429s. A single poller with a cache,
  per-account minimum intervals and back-off removes that.
- **Event-driven resumes.** The helper sees each account move between blocked
  and usable, so it can resume the sessions bound to that account at the moment
  of the transition, instead of one sleeping watcher per session that dies on
  reboot.
- **No shell in the path.** The helper is one static Go binary started by
  launchd. It runs no zsh, no Python and no BrishGarden, so nothing reachable
  over the network can make it run code.

## Vocabulary

- **Account**: one quota-bearing identity. Its **account id** is
  `<provider>:<name>`: `claude:default`, `claude:work`, `codex:<alias>`,
  `agy:<account>`. Codex accounts in a second `CODEX_HOME` are named
  `codex:<home>/<alias>`.
- **Snapshot**: the latest reading of one account (normalized windows, verdict,
  provider-native detail, poll bookkeeping).
- **Verdict**: `usable`, `blocked`, `unknown` (no reading, or the reading cannot
  decide), `signed_out` (no credential at all; not an error).
- **Transition**: a change of an account's verdict, recorded with both readings.
- **Registration**: a session that asked to be resumed (Paseo agent or native
  terminal session).
- **Forced refresh** (`deus`): a read that bypasses the cache for exactly the
  requested accounts.
- **Floor**: the minimum time between two upstream reads of one account, even
  when forced.

## Layout

```
cmd/agent-quotas/        CLI and daemon entry point (one binary)
internal/model/          Snapshot, Window, Verdict, schema version, shared helpers
internal/config/         configuration file, defaults, paths
internal/claude/         Claude usage reader (token lookup, fetch, windows, credit)
internal/codex/          Codex reader (codex app-server JSON-RPC, rate-limit verdicts)
internal/agy/            Antigravity reader (statusline cache, slow `agy -p /usage`)
internal/daemon/         store, pollers, coalescing, transitions, socket server
internal/client/         socket client used by every CLI subcommand
internal/render/         human renderers: agent-status, claude-code-usage, codex-status, agy-status
internal/paseo/          Paseo auto-continue engine (registrations, tick, transcripts)
internal/native/         native auto-continue engine (scopes, targets, delivery)
internal/keeper/         cache policy for idle Paseo agents (pings, compaction, /v1/agents)
internal/launchd/        plist template and install/uninstall
internal/procx/          running external commands with timeouts and process-group cleanup
```

Standard library only.

## Paths

- State: `${XDG_STATE_HOME:-~/.local/state}/agent-quotas/` (mode 0700): the
  socket `agent-quotas.sock` (0600), `snapshots.json` (last snapshot per account,
  so a restart serves the previous reading at once, marked stale), and
  `transitions.jsonl` (verdict transitions, capped at 10,000 lines, rotated to
  `.1`), `daemon.log`.
- Config: `${XDG_CONFIG_HOME:-~/.config}/agent-quotas/config.json`. Optional.
- Existing registrations stay where they are (see Auto-continue).

The daemon refuses to start if the state directory is not owned by the current
user or is group/world accessible, and creates the socket with umask 0177.

## Configuration

JSON, every key optional:

```json
{
  "claude": {
    "profiles": [
      {"name": "default", "config_dir": ""},
      {"name": "work", "config_dir": "~/.claude-work"}
    ],
    "user_agent": "claude-code/2.1.220",
    "token_dir": "~/.keys",
    "min_interval_s": 300
  },
  "codex": {
    "homes": [{"name": "default", "codex_home": "~/.codex"}],
    "auth_backup_dir": "~/tmp/.codex-auths",
    "full_pct": 100,
    "min_interval_s": 300,
    "codex_binary": "codex"
  },
  "agy": {
    "accounts": [
      {"name": "m", "gemini_dir": "~/.gemini"},
      {"name": "cave", "gemini_dir": "~/.agy-accounts/cave"}
    ],
    "statusline_max_age_s": -1,
    "min_interval_s": 60,
    "active_refresh_s": 300,
    "stale_refresh_s": 3600,
    "agy_binary": "agy"
  },
  "poll": {"force_floor_s": 5, "backoff_max_s": 1800},
  "paseo": {"homes": ["~/.paseo"], "registrations_dir": "", "send_behavior": "", "default_auto_continue": false},
  "keeper": {"enabled": false, "default_policy": "off", "ttl_seconds": {"claude": 3600, "codex": 1800},
             "lead_seconds": 300, "quiet_seconds": 60, "warm_hours": 6, "ping_text": "",
             "send_behavior": "steer", "compaction_timeout_seconds": 1200,
             "compaction_file": "~/.compaction.yaml"},
  "native": {"registrations_dir": "", "live_list_command": ["agent_session", "live-all"]},
  "notify": {"command": []}
}
```

Defaults when the file or a key is absent: Claude profile `default` (no config
dir, so `~/.claude.json` and the unsuffixed Keychain service); every directory
`~/.claude-*` that holds a `.claude.json` is **not** guessed, because a wrong
profile would report a real account under a wrong name. Codex home
`~/.codex`. Antigravity account `m` at `~/.gemini`. Paseo home `~/.paseo`.

`agent-quotas config init` writes a config from what it can find (the
directories above, `AGY_ACCOUNTS` and `AGY_ACCOUNT_DIR_*` from the calling
shell's environment) and prints it for review; it never overwrites an existing
file without `--force`. `agent-quotas config show` prints the effective config.

## Snapshot schema (version 1)

Every API document carries `"schema_version": 1`. Additive changes keep the
version; renaming or removing a field bumps it.

```json
{
  "schema_version": 1,
  "generated_at": 1789000000.5,
  "accounts": [ Snapshot, ... ]
}
```

Snapshot:

```json
{
  "id": "claude:work",
  "provider": "claude",            // claude | codex | agy
  "name": "work",
  "label": "work",                 // display name (Codex: workspace name when known)
  "verdict": "usable",             // usable | blocked | unknown | signed_out
  "blocked_reasons": ["5h at 100%"],
  "blocked_until": 1789003600,     // latest reset among blocking windows, or null
  "plan": "max",                   // or null
  "email": null,                   // only when the provider reports it
  "active": true,                  // Codex: this alias is the live auth.json; else null
  "windows": [Window, ...],
  "notes": [{"severity": "info|warning|alarm", "text": "..."}],
  "fetched_at": 1789000000.0,      // when the data was produced upstream, or null
  "source": "api",                 // api | api-cache | config-cache | app-server | statusline | slow | snapshot-file
  "stale": false,
  "stale_reason": null,
  "error": null,                   // why the latest attempt failed; data may still be present
  "native": { ... },               // the provider's legacy per-account JSON row (see Compatibility)
  "poll": {
    "last_attempt_at": 1789000000.0,
    "last_success_at": 1789000000.0,
    "next_poll_at": 1789000300.0,
    "backoff_until": null,
    "consecutive_failures": 0,
    "last_http_status": 200,
    "upstream_calls": 12            // since daemon start
  }
}
```

Window:

```json
{
  "key": "session",                // provider key: session, weekly_all, weekly_scoped, codex-300, gemini-5h, ...
  "family": "Claude",              // model family the window limits ("Claude", "Codex", "Gemini 4 Argon", ...)
  "window": "5h",                  // 5h | weekly | daily | <duration> | provider label
  "label": "5h session",           // provider's own label
  "used_percent": 52.0,            // 0..100, or null when unknown (never invented as 0)
  "resets_at": 1789003600,         // Unix seconds or null
  "window_minutes": 300,           // or null
  "scope": "account",              // account | model | reserve
  "blocking": false                // at or above the full threshold
}
```

Verdict rules, one per provider, ported unchanged:

- **Claude**: blocked when an account-wide window (`session`, `five_hour`,
  `weekly_all`, `seven_day`) is at 100%. A model-scoped weekly window blocks only
  that family: `UsableFor(family)` adds the windows whose key is
  `weekly_scoped` or `seven_day_<family>` and whose label names the family
  (the rule of `quota_usable` in the old Paseo watcher). No applicable window or
  a window without a number means `unknown`, never usable.
- **Codex**: the `.quota.blocked` verdict of the regular allowance
  (`codex_rate_limits.py`): a window at or above `full_pct`, or an explicit
  signal (`ordinaryUsageAllowed: false`, `spendControlReached: true`, a non-null
  `rateLimitReachedType`). The Luna Reserve and per-model families never make a
  blocked account usable; an account reporting no windows is usable.
  `blocked_until` is the latest reset among blocking windows.
- **Antigravity**: per model pool. The account verdict is `blocked` only when
  every general pool (`gemini-*`, `3p-*`) is at 0% remaining; `UsableFor(model)`
  applies `agy-best`'s rule (every applicable pool, general and model-specific,
  has room). An empty pool whose reset time has passed counts as having room,
  as it stops blocking in the windows: only agy's own runs refresh the
  reading, so while every agent of the account waits on that pool, nothing
  else would show it come back. A missing or too-old cache gives `unknown`.

## Polling and back-off

One goroutine per account. The next poll is the earliest of:

- `last_success + min_interval` (per provider, configurable);
- for a blocked account, `blocked_until + grace` (grace 90 s), so a reset is
  seen within about a minute and auto-continue fires on time (this replaces the
  sleeping one-shot jobs of the old engines);
- a wake-up: a forced refresh, a hook kick, or a registration change.

Back-off: on HTTP 429 or a transport error the account waits
`max(Retry-After, 60 s * 2^(failures-1))`, capped at `backoff_max_s` (1800 s).
During back-off, readers get the cached snapshot with `stale: true` and the
reason; forced refreshes are refused with the back-off deadline instead of
reaching upstream. A 401 or missing credential is not retried faster than
`min_interval`.

Antigravity's statusline cache is a local file written by agy's own hook, so
polling it costs nothing upstream; it is read every `min_interval` (60 s). The
slow path (`agy -p /usage`, 2 to 15 s, a full CLI start-up) is never scheduled;
it runs only on a forced refresh of that account or when the cache has nothing
for it and the account is signed in, at most once per `min_interval`.

An Antigravity account is signed in when its
`antigravity-cli/antigravity-oauth-token` is a non-empty file. On macOS, the
account in the default `~/.gemini` (normally `m`) also counts as signed in
when agy's keychain item exists. agy 1.5 tries the token file first and then
the keychain, even in file mode, so m keeps working after a keychain-mode run
has removed its file. The check runs `security find-generic-password -s gemini
-a antigravity` without `-w` or `-g`, so it never reads the secret. The item
is one per macOS user, not one per Gemini root, so other accounts ignore it.
Neither check opens a credential. Only exit 44 (no such item) means no
keychain sign-in; a lookup that times out (5 s, which a swapping Mac exceeds)
or fails otherwise leaves the sign-in `unknown`, and the read goes on to the
quota check rather than reporting the account signed out.

`agent-quotas agy-auth ACCOUNT` (or `--dir GEMINI_ROOT`) prints where that
sign-in is stored, `file-present`, `keychain`, `missing` or `unknown`, and
exits 0, 0, 1 or 1. It reads only local state and needs no daemon. The shell's agy helpers
(`agy-best`, `agy-acc-login`, the `agy-status` fallback) ask it, passing the
root they resolved, so this rule is written only in `agy.StoredAuth`. Where the
binary is missing or too old to know the command (exit 2), they fall back to
the token file alone.

Codex starts `codex app-server` per auth snapshot and asks for rate limits over
JSON-RPC, as `codex_status.py` does, including the refresh-token recovery from
`auth_backup_dir`. Account discovery (which `auth_<alias>.json` files exist,
which one `auth.json` matches) runs before every poll round of a Codex home.

## Forced refresh (`deus`)

- API: `?refresh=1` on a read, or `POST /v1/refresh`. Only the accounts the
  request names are refreshed; a read of all accounts with `refresh=1`
  refreshes all.
- CLI: `--refresh`, or the environment variable `deusvult` being non-empty,
  which is what the shell's `deus` alias sets. So `deus agent-quotas status
  claude:work` refreshes exactly `claude:work`.
- Coalescing: concurrent forced refreshes of one account share one upstream
  call; the second caller waits for the first one's result.
- Floor: a forced refresh within `force_floor_s` (5 s) of the previous upstream
  attempt returns that attempt's result instead of calling again. A loop calling
  `deus` therefore reaches upstream at most once per floor.
- Back-off wins over force: a 429-backed-off account is not called early.
- Antigravity: a forced refresh runs the slow path, as `isDeus` did in
  `agy-status`.

## Socket API

HTTP/1.1 over the unix socket, JSON bodies. Nothing listens on TCP. The socket
file is 0600 inside a 0700 directory, so only the owner can connect; the
daemon also rejects a peer whose uid differs (`LOCAL_PEERCRED` on macOS,
`SO_PEERCRED` on Linux) where the platform allows reading it with the standard
library, and otherwise relies on the file modes.

Read endpoints:

- `GET /v1/health` → `{schema_version, pid, started_at, version, accounts: N}`
- `GET /v1/accounts[?id=ID&id=ID][&refresh=1][&provider=claude]` → document above
- `GET /v1/accounts/{id}[?refresh=1]` → `{schema_version, generated_at, account: Snapshot}`
- `GET /v1/transitions?since=EPOCH` → `{schema_version, transitions: [...]}`
- `GET /v1/continue` → every registration, Paseo and native, with its status

Acting endpoints:

- `POST /v1/refresh` `{"ids": ["claude:work"]}`
- `POST /v1/continue/check` `{"kind": "native", "scope": "claude-work"}` or
  `{"kind": "paseo", "path": "<registration file>"}` (empty: all): queue a check now

The CLI implements `continue on|off` and hook marking locally,
so it has the caller's environment (session ids, `TMUX_PANE`). It writes the
registration files and then asks the daemon to check. Paseo also accepts
explicit agent IDs through `POST /v1/agents/{id}/auto-continue`. A daemon that is
down finds the registrations when it starts. There is no config-reload
endpoint: after editing the config, `agent-quotas restart` (launchd kickstart).

Errors are `{"error": "...", "code": "..."}` with a 4xx/5xx status.

## Compatibility outputs

The CLI keeps the old readers' outputs so consumers can switch by changing a
command name only:

- `agent-quotas claude-code-usage [--json] [--all] [--profile-label L] [--cloud-credit]`:
  the `build_json` object (or array with `--all`) of `claude_code_usage.py`, and
  its human report.
- `agent-quotas codex-status [--json] [--all|--no-all] [--full-pct N]`: the
  `codex_status.py` document (`authFiles`, `averageUsage` with
  `firstTimeToReset`) and its human report.
- `agent-quotas agy-status [--json] [--account A]`: the `agy-status --json`
  array and its human report. The shell's `agy-status` is a thin wrapper over
  it, so it also has what only the old zsh engine had:
  - `--flat`: one account's flat bucket array (`agy_status_json_p=y`), the
    account from `--account`, else `$AGY_ACCOUNT_ID`, else `m`. The reading's
    notes go to stderr; an account with an error, no sign-in or no buckets
    exits 1, since the array cannot say so.
  - `--cache-only`: reads the statusline caches in this process and never
    starts agy, even under `deus`; a missing cache, or one with nothing
    younger than `--max-age` seconds, is that account's error. The quota
    watchers use it with `--max-age 3600`. `--max-age` also works with
    `--direct`; the daemon always uses `statusline_max_age_s`.
  - `--credits`: after the human report, each signed-in account's
    pay-as-you-go balance from `agy -p /credits`. It has no cache, so it costs
    an agy start-up per account (in parallel), though no quota.
- `agent-quotas agy-auth ACCOUNT | --dir GEMINI_ROOT`: not a report but the
  sign-in check the shell's agy helpers share (see the Antigravity reader).
- `agent-quotas agent-status [--json] [claude codex agy]`: the combined
  document `{generated_at, providers: [{agent, title, error, accounts}]}` that
  the control page reads, and the terminal report.

Each provider reader stores its legacy per-account row as the snapshot's
`native` field, so these subcommands assemble documents from snapshots without
re-reading upstream. Aggregates (Codex `averageUsage`) are computed in the
client from the rows. Golden tests compare each output to captured output of the
old tools on synthetic data.

When the daemon is not running, every read subcommand exits 3 with
`agent-quotas: daemon not running (start it with: agent-quotas install, or agent-quotas serve)`.
`--direct` performs a one-off in-process read instead (no cache, no
coalescing; meant for debugging and for the first run).

## Auto-continue

Two engines, both inside the daemon, both triggered the same way. Each has
a switch: `paseo.enabled` (default on; safe beside the old Python workers,
which take the same lock and record the same claim) and `native.enabled`
(default off, because the old armed tmux jobs would type a second
`Continue.`; turning it on is part of the cutover).

- an account transition from `blocked` (or `unknown` after `blocked`) to
  `usable`: check every registration bound to that account;
- daemon start-up: check every registration;
- a safety tick per registration (its `poll`, default 300 s, minimum 30 s);
- for an Antigravity agent waiting for quota, its registration's `wake_at`
  (see "Antigravity agents");
- an explicit check or a hook kick.

A check never blocks a quota poller. Paseo checks run independently, with
one check in flight per registration and the per-registration lock. All Paseo
CLI calls in the daemon, including keeper and HTTP calls, share a limit of
two running commands per resolved Paseo home.

### Paseo engine (port of `paseo_auto_continue.py`)

Registrations stay in
`${XDG_STATE_HOME:-~/.local/state}/agent-sessions/auto-continue-paseo/<hash16(home)>-<agent>.json`
with the same JSON fields (`version`, `enabled`, `home`, `agent_id`,
`binding`, `poll`, `status`, `registered_at`, `claimed_failure`) plus
`set_by` (`cli`, `api` or `default`). They use the same
`.lock` files taken with `flock`. Because both the old Python workers and the
daemon take that lock and record `claimed_failure` before sending, running both
during the cutover cannot send twice for one failed turn.

`paseo.default_auto_continue` (false by default) uses the periodic Paseo
listing to enable agents with real activity in the last 24 hours. Where
agent-desk applies its own inherited defaults (through the API above), leave
this off: it enables main agents whatever the desk's setting says. It skips
archived, internal and child agents and requires a supported seat. A default
decision can only be written once. It never overwrites a prior decision,
including `off` recorded before an agent was ever enabled. New cache-only
files have `set_by: null` for an undecided auto-continue policy. Older files
without `set_by` may represent an explicit opt-out, so the default leaves all
such files alone. Cache policy and auto-continue defaults are independent.

Scheduled checks read the exact agent's persisted JSON first and retry a
decode error once. A known running agent or a known state with no new quota
failure needs no CLI call. When a record cannot provide a status, a round
lazily shares one `paseo ls -g --json` per home to identify agents still
running. An absent row is inconclusive because global listings omit
archived and background agents. An apparent binding change, archival, or
new quota failure goes through the live check before changing the
registration or sending.

A timed-out check is retried after 20 s, then 40 s, doubling up to its normal
poll interval. The scheduler checks retry deadlines every five seconds.
It logs the first timeout in a streak. The same backoff applies when
Paseo cannot be started. The send path keeps its separate handling for
uncertain outcomes.

The live check retains the following guarantees:

1. Disabled registration: nothing.
2. Agent gone or archived: disable (`agent unavailable`).
3. Binding changed (provider resolves to another agent, profile, native home or
   launch configuration hash): disable (`provider changed; re-arm explicitly`).
4. Failure identity: `lastStatus` `error`/`closed` with a quota error text in
   `lastError` (digest of error, `lastUserMessageAt`, `attentionTimestamp`); or,
   for Claude and Codex agents that are `idle`/`closed`, the last turn in the
   provider transcript (Claude: last user/assistant entry is
   `isApiErrorMessage` with `error: "rate_limit"` or quota text; Codex: last root
   `task_complete` with a usage-limit error, rollout found through the UUIDv7
   day). Digests are byte-compatible with the Python ones, so an existing
   `claimed_failure` stays claimed. For Antigravity agents, see "Antigravity
   agents" below.
5. Pending permission, or no failure while not in error: waiting.
6. Failure already claimed: waiting for a new one. A keeper ping that fails
   on quota is marked handled without a resume.
7. Quota for that agent's own binding (Claude profile + model family; Codex
   active auth of that `CODEX_HOME`; Antigravity account + model, by
   `agy-best`'s rule over its pools) is read from the daemon store with a
   refresh if the snapshot is older than 60 s (subject to the floor and
   back-off). Not usable: waiting for quota recovery. Unknown (a failed or
   stale reading): the check is postponed and the registration left as it
   is, exactly as the old watcher did on a reader error.
8. Re-inspect and re-read the stored record; if anything changed (status,
   permission, binding, failure identity), skip.
9. Record `claimed_failure` (atomic write, fsync) **before** sending.
10. `paseo send <id> <prompt> --behavior <PASEO_SEND_BEHAVIOR|steer> --home <home> --no-wait --json`,
    with Paseo marker variables removed from the environment. The prompt is
    `Continue.` for a main agent. A subagent (its record has
    `parentAgentId` or the `paseo.parent-agent-id` label) is told to
    re-coordinate first: message its parent with where it stopped and what it
    would do next, then wait for the answer. While it was stopped, the parent
    may have given its task to a replacement, done it itself or changed plans,
    and a child that simply continued would duplicate or undo that work. The
    parent is looked up in the same home first: when it is archived or has no
    record (deleted, or another daemon's agent), the child is told not to
    message it, since Paseo unarchives an agent a prompt is sent to, and to
    stop unless the work is clearly still wanted, telling the user either way.
    A Paseo agent started by an agent outside Paseo records no parent and is
    resumed as a main agent.
11. A failed or timed-out send disables the registration
    (`send uncertain; inspect agent and re-arm explicitly`).

A successfully completed turn needs no cleanup of its registration. The legacy
Python fallback checks quota failures too, but lacks the keeper-ping filter.

`stopped` and `stopped --usable` (the pickers' listing) are ported too, with
Antigravity quota read from the store instead of `agy-status`.

#### Antigravity agents

A provider whose `extends` chain reaches neither `claude` nor `codex` is an
Antigravity provider when its command is `agy` (or it is Paseo's
`antigravity`); its identity is agent `agy`, profile the account
(`AGY_ACCOUNT_ID`, default `m`) and native home agy's app data directory
(`--app_data_dir`, relative to `--gemini_dir`; else `<gemini dir>/antigravity-cli`).
Any other provider is still refused.

agy-acp ends a turn that hits the account's quota with an ordinary reply,
`⚠ Individual quota reached. Please upgrade your subscription to increase your
limits. Resets in 2h19m26s.`, so Paseo records the agent `idle` with no
`lastError`, and agy's own transcript does not contain the notice either. The
failure is read from the end of Paseo's timeline instead (`paseo logs <id>
--tail 30 --json`): the last model output (a reply, reasoning, or a tool call
of the agent's own) is a reply whose first line opens with `⚠` and matches the
quota pattern. Messages sent to the agent after it are skipped, since a turn
one of them started hit the limit again or is still running. The identity is a
digest of the agent and the notice's `messageId`. "Resets in" plus the record's
`lastActivityAt` estimates the reset.

No account transition announces that reset: agy's account reading changes
only while some agy agent runs, and the account as a whole often stays usable
(other pools have room) while the agent's model is blocked. So a check that
finds the agent waiting for quota stores `wake_at`, the notice's reset plus
10 s, in the registration, and the scheduler checks it once at that time
rather than up to a safety tick later.

The answer is cached in `<registrations dir>/agy-stops.cache`, keyed by the
record's `lastActivityAt`: any new turn moves it, while Paseo closing an idle
agent does not. A `closed` agent gets only the cached answer, because reading
its timeline makes Paseo load the agent again, which starts agy (about 4 s, and
an error for an old session).

### Native engine (port of `agent-auto-continue.zsh` and the armed-job engine)

Registrations stay in `<agent-sessions>/auto-continue/<scope>/<agent>-<id>`
(tab-separated: agent, id, transcript, targets, registered-at) with the
`.blocked_at`, `.blocked-<agent>-<id>`, `.kicked` and `.fn_ran` dotfiles,
so existing registrations need no re-arming.

The old design armed a one-shot tmux job for the reset time and trusted it.
The daemon instead acts on the observed transition:

- Scope → account: `claude-<profile>` → `claude:<profile>`; `codex-<alias>` →
  `codex:<alias>`; `agy` → the first configured Antigravity account.
- Each check prunes registrations whose session is no longer live (from
  `live_list_command`, the `agent_session live-all` listing), as the watcher did.
- Blocked: the first blocked check writes `.blocked_at` (the activity
  reference). For Claude, which families are blocked is remembered per check.
- Usable after blocked, plus the continue grace (default 120 s after the
  observed reset, as `agent_usage_continue_grace_s`): the eligible
  registrations (activity gate: transcript modified within poll + slack before
  `.blocked_at`, or a hook mark, or no transcript to ask) of the families that
  were blocked are resumed, then `.blocked_at` and marks are cleared.
- Delivery per target kind, with the same re-checks: `tmux:<pane>` (pane alive,
  not dead, still hosting a live agent; `send-keys -l` text then `Enter`),
  `kitty:<id>` (window present in `kitty @ ls`; `send-text`), `codex:<thread>`
  (`codex queue --thread`), `claude-bg:<id>` (scratch tmux attach), and
  `frontmost` (Hammerspoon; only when idle and unlocked).
- Idle gate per target kind (`idle_min_s` per kind; default 0 except
  `frontmost` 600); an unreadable idle time holds back the targets that need it.
- Every unreachable or held-back target produces a notification naming it, and
  every delivery appends a line to the log.
- The configurable resume prompt asks for a brief reply if the work is already
  complete. Native delivery can reach a finished session because the activity
  gate uses transcript recency as well as any per-session hook mark.

Registration commands (`agent-quotas continue on|off|status|list`) inside a
Paseo agent (`PASEO_AGENT_ID`) register that agent with the Paseo engine;
otherwise they register the native session from the same environment variables
the shell used (`CLAUDE_CODE_SESSION_ID`, `CLAUDE_CONFIG_DIR`, `CODEX_THREAD_ID`,
`CODEX_HOME`, `ANTIGRAVITY_CONVERSATION_ID`, `TMUX_PANE`). `agent-quotas hook
claude|agy [PANE]` reads the hook payload on stdin and kicks the daemon.

## Keeper: cache policy for idle Paseo agents

The daemon is the one per-machine process that sends timed prompts to idle
agents: auto-continue, keepalive pings, and compaction before the prompt cache
expires. agent-desk shows and toggles the policy through this API and keeps the
history; it stores no policy of its own. The keeper ships **disabled**
(`keeper.enabled: false`): the read API still answers and cache operations refuse with `keeper_disabled`.
The Paseo auto-continue engine and its registration endpoint are independent
of the cache keeper switch.

Terms:

- **cache policy**, per Paseo agent: `off` (default), `compact` (compact once,
  shortly before the cache expires, if the context is big) or `warm` (keep the
  cache alive with pings while idle, up to the warm ceiling, then behave as
  `compact`).
- **keeper prompt**: a prompt the keeper sends: a ping (it starts with the
  marker below) or the raw `/compact`. A **keeper turn** is a turn started by
  a keeper prompt.
- **real activity**: the last model response of a turn that is not a keeper
  turn, or the last prompt of one if it has no response yet. Read from the
  provider transcript (Claude: the timestamp of the last assistant entry that
  is a real model call; Codex: the last `token_count` event of a root turn),
  attributing each response to the latest prompt before it. Without a readable
  transcript: Paseo's `lastActivityAt`, ignored when the last user message is
  one of the keeper's own sends (within 15 s of it); then the value the keeper
  last recorded (`cache.activity_at`). The source is exposed as
  `last_activity_source: transcript|paseo`. Paseo `updatedAt` is never used: it
  moves for titles, labels and attention.
- **cache activity**: the last model call of any kind (keeper turns included),
  a successful ping's send time, or a completed compaction, whichever is
  latest. Keeper pings and their replies count here, not as real activity.
- **TTL**: the provider's prompt-cache lifetime, from config
  (`keeper.ttl_seconds`, default `{"claude": 3600, "codex": 1800, "agy": 3600}`), with one
  exception: a Claude seat on **overage** (an account-wide window is at 100%
  while `extra_usage` is enabled and its spend limit is not reached) uses 300 s,
  Claude's cache lifetime on extra usage. Exposed as `ttl_reason: config|overage`.
  The 1 h Claude TTL for subscription seats is an assumption (Claude Code
  requests the 1 h cache on subscriptions); change `ttl_seconds` if that stops
  holding. Gemini's cache under agy is best effort: measured on agy
  transcripts, nearly every model call within 5 minutes of the previous one
  read the context from cache, about half after a 5 to 60 minute pause, and
  almost none after an hour. So `agy` uses 1 h for timing compaction, and the
  keeper refuses `warm` for agy agents: a ping is a full turn on the account's
  quota and would not keep that cache. Overage is informational in practice: an overage seat's verdict is
  `blocked`, so the keeper does not send to it.
- **cache expiry**: cache activity + TTL.
- **lead**: how long before expiry the keeper acts (`keeper.lead_seconds`,
  default 300).
- **big**: the context, read from the transcript (Claude: the last real model
  call's `input_tokens + cache_creation_input_tokens + cache_read_input_tokens`;
  Codex: the last `token_count`'s `last_token_usage.input_tokens`; agy: the
  last `PLANNER_RESPONSE`'s `input_tokens + cache_read_tokens` in
  `brain/<session>/.system_generated/logs/transcript_full.jsonl`), is at least
  `min_context_tokens` from `~/.compaction.yaml` (default 128000 when the file
  or key is absent). Unknown (no transcript, or a compaction since the last
  model call) means not big.
- **idle stretch**: the time from one real activity to the next.
- **warm ceiling**: `warm_until` = start of the idle stretch + `warm_hours`,
  where the start is the later of the real activity and the time `warm` was
  set. Every real turn re-arms it.
- **sendable**: Paseo status `idle` (not `running`, not `error`), no pending
  permission, not archived, the agent's seat not `blocked` in the store (an
  `unknown` seat is sendable: a ping that fails costs nothing and is not
  continued), no unclaimed quota-failed turn (that one belongs to
  auto-continue), and quiet: the agent's lock was not held by anyone else in
  the last `keeper.quiet_seconds` (60) and Paseo's `lastUserMessageAt` is not
  within that window. An agent Paseo has unloaded (`closed`) is not sendable
  for pings (reloading it to keep a cache warm costs more than it saves); it is
  for compaction.

### Phase

GET reports `cache.phase`, derived, never stored:

- `off`: policy `off`.
- `compact`: policy `compact`, set directly.
- `standby`: policy `compact`, set by standby.
- `warming`: policy `warm`, before `warm_until`.
- `dormant`: policy `warm`, at or after `warm_until`: behaves as `compact`
  until the next real turn re-arms warming. The policy is not rewritten.

### Storage

One file per agent per machine, the existing Paseo registration file
(`agent-sessions/auto-continue-paseo/<hash16(home)>-<agent>.json`), gains an
optional `cache` object. An agent with a cache policy but no auto-continue gets
a policy-only file with `"enabled": false`, `"set_by": null` and its
`binding`. `continue on|off` never removes `cache`; `keep set --cache off`
never touches auto-continue. Every write is under the same `.lock` (flock) as
auto-continue checks and sends.

```json
"cache": {
  "policy": "warm",
  "warm_hours": 6,
  "set_at": 1795028400, "set_by": "agent|desk|cli|default",
  "standby_at": null,
  "activity_at": 1795032000,
  "pings": {"since": 1795032000, "count": 2},
  "last_ping": {"at": 1795032000, "ok": true, "error": ""},
  "compaction": {
    "id": "c-7f3a2b", "reason": "policy|standby|manual",
    "state": "queued|sent|completed|failed|canceled",
    "requested_at": 0, "sent_at": 0, "finished_at": 0,
    "tokens_before": 210000, "tokens_after": 31000, "error": ""
  }
}
```

`activity_at` is the last real activity the keeper saw (the fallback above).
`pings.count` counts keeper pings since the real activity `pings.since`; it
restarts at 0 when real activity moves. `compaction` is the latest one only;
earlier ones are in the daemon log and the desk's history. `tokens_after` is
the context of the first real model call after the compaction, null until one
exists.

Only agents with a file are keeper targets (and so the only ones whose
`lock_path` exists).

### Scheduling

Each agent with a policy other than `off` has one **next action** and a time,
recomputed after every observation, policy change and send. Let `due` = cache
expiry − lead, and the **mode** at `due` be `warm` when the phase is `warming`
and `due` is before `warm_until`, else `compact`:

- mode `compact`: at `due`, if big and not compacted in this idle stretch:
  compact. Otherwise nothing (the cache is allowed to go cold).
- mode `warm`: at `due`, if big and not compacted in this idle stretch:
  compact (that also refreshes the cache); otherwise ping. So a big agent is
  compacted once, then pinged.
- A queued compaction is due at once.
- A real turn starts a new idle stretch: `warm_until`, "compacted in this
  stretch" and `pings_since_activity` all follow it.

The keeper observes each registered agent every 60 s from its Paseo record,
transcript and the quota store. A due action that passes the file checks also
needs a live `paseo inspect` under the registration lock. The keeper
recomputes the action and every send gate under the lock. A busy agent is
observed again at the next scheduled check. Once the cache expires, the
keeper skips that ping.

Compactions in flight are followed every 10 s. A record showing a running
turn postpones the CLI check until its status changes or the compaction
timeout expires. When multiple compactions for a home need checking, they
share one global listing for live status. Agents absent from that list may
need an individual inspect. An idle or closed agent's timeline still comes
from `paseo logs`. The keeper refreshes its agent directory and applies
configured defaults on startup and every five minutes, with one listing per
home.

Keeper observations run independently on a pool of eight workers. CLI
observation calls have a 30 s deadline, with 60 s for a listing. Timeout
streaks are logged once per agent check or per home listing. A retry starts
at 20 s and doubles up to the usual interval, which remains 10 s for
in-flight compactions. A send retains the CLI's 60 s timeout and the
handling for uncertain delivery.

### Sending and the marker

- Every ping starts with the marker **`[agent-quotas keepalive]`**. The marker
  is part of the API: agent-desk's prompt hook drops prompts that start with
  it (and the raw `/compact`) from its instruction log, and auto-continue keys
  on it. `keeper.ping_text` is configurable, one line; text without the marker
  gets the marker and a space prefixed. Default:
  `[agent-quotas keepalive] Automatic cache keepalive from agent-quotas, not the user: reply with one word and make no tool calls.`
- Ping: `paseo send <id> <ping text> --behavior steer --no-wait --json --home
  <home>`, Paseo marker variables removed. The agent is idle when it is sent,
  so the behaviour only matters in the race noted below
  (`keeper.send_behavior` overrides it; `queue` would turn the race into a
  separate turn after the running one).
- Compaction: `paseo send <id> /compact ...` the same way, the text unchanged.
- Residual race: Paseo has no "send only if idle". A turn that starts between
  the recheck and the send would receive the ping as a steer. Every sender that
  takes the lock (below) closes this; only senders that do not remain.
- **Pings and auto-continue:** a ping turn that fails on the quota is not a
  failed turn worth continuing. The Paseo engine recognises it from the
  transcript (the failed turn's prompt starts with the marker) or, without a
  transcript, from `cache.last_ping.at` within 15 s of the record's
  `lastUserMessageAt`, and marks it claimed without sending `Continue.`
  (status `keeper ping failed on quota; not continued`).

### Standby

`standby` ends warming at once and sticks: the policy becomes **`compact`**
(`set_by` the caller: `agent`, `desk` or `cli`; `standby_at` recorded), so a
big agent is compacted at its next expiry and nothing is sent after that.
Setting `warm` again leaves standby and starts a new ceiling from that moment.

### Compaction state

`state` moves `queued` → `sent` → `completed`, or ends `failed` / `canceled`:

- `queued`: requested, waiting until the agent is sendable.
- `sent`: recorded before `/compact` is sent (at most one send per request).
- `completed`: the agent is idle again and Paseo's timeline (`paseo logs
  --json --since <sent_at>`) shows a `compaction` item with status
  `completed`, or the transcript has a compaction since `sent_at` (agy writes
  only a `CHECKPOINT` entry; Paseo's ACP timeline shows nothing).
- `failed`: the send failed (`error`: the CLI's error), or no completion within
  `keeper.compaction_timeout_seconds` (1200) of `sent_at` (`error:
  "timeout"`), or the agent was archived (`error: "archived"`).
- `canceled`: the agent is idle again, the timeline since `sent_at` shows the
  turn happened, and no completed compaction (Claude's "Compaction canceled.",
  an interrupt, a Stop); or a newer request replaced a `queued` one (`error`
  names the newer id).

A compaction request while one is `sent` returns that one. A manual compaction
(`POST .../compact`) ignores `big`: the desk asked for it. A `queued`
compaction waits for the agent to become sendable for up to the same timeout,
then fails with `error: "not idle"`.

### API

Read (answered with the keeper disabled too, always from memory; see
"Serving reads from memory" below):

- `GET /v1/agents[?policy=warm|compact|off|any][&all=1][&home=DIR]` →
  `{schema_version, generated_at, keeper_enabled, [listed_at,] agents:
  [Agent]}`: every agent with a file (`policy` filters; `any` means not
  `off`), plus, with `all=1`, every agent the last `paseo ls` of that home
  listed; `listed_at` (only with `all=1`) is when that listing ran, absent
  before the first.
- `GET /v1/agents/{id}[?home=DIR]` → `{schema_version, generated_at, agent: Agent}`.
  Returns 404 when Paseo does not know the agent.
- **Long-poll:** `GET /v1/agents/{id}?wait_compaction=<compaction id>&timeout=60`
  answers when that compaction is terminal (`completed|failed|canceled`) or on
  timeout (capped at 120 s), with the same document. Polling GET stays the
  fallback.

```json
{
  "id": "…", "home": "~/.paseo", "title": "…", "provider": "claude",
  "keeper_enabled": true, "auto_continue_engine_enabled": true,
  "status": "idle", "pending_permission": false, "archived": false,
  "last_activity_at": 1795032000, "last_activity_source": "transcript",
  "cache_activity_at": 1795032000,
  "context_tokens": 210000, "min_context_tokens": 128000, "big": true,
  "ttl_seconds": 3600, "ttl_reason": "config", "cache_expires_at": 1795035600,
  "seat": {"account": "claude:work", "verdict": "usable", "overage": false},
  "cache": { …the stored object…, "phase": "warming" },
  "warm_until": 1795053600,
  "pings_since_activity": 2,
  "sendable": {"ok": true, "reason": ""},
  "next_action": {"kind": "ping|compact|none", "at": 1795035300, "reason": "…"},
  "auto_continue": {"enabled": true, "status": "waiting for a quota-failed turn", "set_by": "cli"},
  "lock_path": "…/<hash16>-<agent>.lock",
  "observed_at": 1795035240
}
```

`observed_at` is when the data behind the document was read (null for an
agent known only from a registration or a listing and not observed yet).

`seat` is the account's verdict for Claude and Codex agents. For an
Antigravity agent (`agy:<account>`) it is the verdict for the agent's model,
by `agy-best`'s rule, and a blocked one carries `available_at`, the latest
reset among that model's empty pools; the account's own verdict can be usable
while a Gemini pool is empty, because the Claude/GPT pools still have room.

`cache_expires_at` is the keeper's estimate. The desk's cold-cache dialog can
show "idle longer than the TTL and big" from `cache_expires_at < now && big`.
Times are Unix seconds; unknown values are null.

Act:

- `POST /v1/agents/{id}/auto-continue[?home=DIR]` `{"enabled": true}` or
  `{"enabled": false}` records an API decision and returns the same envelope
  as `GET /v1/agents/{id}`. It preserves cache state and, for a repeated on
  with the same binding, preserves the claimed turn. Older registrations
  with no recorded source omit `auto_continue.set_by`. `"set_by"` names who
  decided (`api` when absent, or `agent`, `desk`, `cli`). `"set_by":
  "default"` records a default instead, such as agent-desk's inherited
  setting: it replaces only no decision or an earlier default, so it can
  follow a setting that changed, while a decision made for the agent stands;
  with `"reset": true` it replaces that decision too, and the agent follows
  the default again. A default that would change nothing, or turn off an
  agent with no registration, writes nothing.

Cache actions return 503 `keeper_disabled` while the keeper is disabled.

- `POST /v1/agents/{id}/policy` `{"cache": "off|compact|warm", "warm_hours": 6,
  "set_by": "agent|desk|cli"}` (`set_by` defaults to `desk`; `default` is
  reserved for the keeper) → the Agent.
  Creates a policy-only file when needed. 404 when Paseo does not know the
  agent.
- `POST /v1/agents/{id}/standby` `{"set_by": …}` → the Agent (policy
  `compact`).

  Both answer with the same envelope as `GET /v1/agents/{id}`, the agent
  observed right after the write (reusing the `paseo inspect` the request
  made to check the agent):

  ```json
  {"schema_version": 1, "generated_at": 1795035300.2,
   "agent": {"id": "…", "cache": {"policy": "compact", "standby_at": 1795035300.1,
             "phase": "standby", …}, "observed_at": 1795035300.2, …}}
  ```
- `POST /v1/agents/{id}/compact` → `{"compaction_id": "c-…", "state":
  "queued|sent|failed"}` (`failed` when the immediate send failed). An agent
  without a file gets one (policy `off`, `set_at` 0) to hold the compaction. The desk long-polls (or polls) `GET /v1/agents/{id}` until
  `cache.compaction.id` matches and `state` is terminal, then delivers. On
  `failed`/`canceled` the desk should still deliver (the message matters more
  than the compaction) and say so.

CLI (same calls, daemon required; the id defaults to `PASEO_AGENT_ID`, and
`set_by` is `agent` then, else `cli`; `keep status` with no id at all lists
the agents with a file):
`agent-quotas keep status [id] [--all] [--json]`,
`keep set [id] --cache off|compact|warm [--warm-hours N]`,
`keep standby [id]`, `keep compact [id] [--wait]`.

Config (`keeper`): `enabled` (default **false** until the owner turns it on),
`default_policy` (default `off`; applied once, with `set_by: "default"`, to
agents whose policy was never set (no file, no `cache` object, or one created
only to hold a manual compaction, `set_at` 0), when the daemon first sees them,
and only to main agents that are not archived and had real activity in the
last 24 h; a subagent, one whose Paseo record has a `parentAgentId` or the
`paseo.parent-agent-id` label, is left to its parent, which sets its policy
explicitly if it wants one), `ttl_seconds`, `lead_seconds`, `quiet_seconds`,
`warm_hours`, `ping_text`, `send_behavior`, `compaction_timeout_seconds`.

### Serving reads from memory

Every GET answers from the keeper's memory, never from a per-request
recomputation: no `paseo inspect`, `paseo ls` or transcript scan runs inside a
GET. Measured on mb2 before this (23 agents with a file, about 380 Paseo
agents, a heavily loaded machine): `paseo inspect` took about 3 s per agent
and `paseo ls` about 4 s, so one agent's GET took 8 to 45 s and `all=1` up to
two minutes. From memory, a GET of 200 synthetic agents takes about 2 ms.

- **What is kept:** per agent, its last **observation** (the registration
  file, Paseo's stored record and, when one was made, `paseo inspect`, the
  transcript activity, the seat and the quota-failure check) and when it was
  made (`observed_at`); per Paseo home, the last `paseo ls`.
- **When it changes:** each observation (the 60 s rounds, the 10 s rounds for
  compactions in flight, the inspect before an action), each write the keeper
  makes (policy, standby, compaction request, ping and compaction records,
  bookkeeping), each listing (agents listed without a file are observed then,
  from files only; agents no longer listed and without a file are dropped).
  A registration that disappears leaves the file list at the next 10 s round.
- **What a read computes:** the derived fields only (phase, `warm_until`,
  `next_action`, `sendable`, `cache_expires_at`), from the kept observation
  and the time of the answer, with no I/O. So `next_action` and `phase` are
  current while the data behind them is as old as `observed_at` (at most about
  60 s for an agent with a file, 5 minutes for one only listed).
- **Start:** the daemon observes every registration from files before the
  first read is answered; reads wait for that at most 5 s, then answer with
  what is there (agents with a file not yet observed appear with
  `observed_at` null and no cache data, and only when no `policy` filter is
  given). `all=1` adds nothing until the first listing ends.
- **An agent never observed** (created since the last listing): its GET
  observes it once from its files (no external command) and keeps the result.
  When that takes more than 2 s the answer is 503 `not_observed`; ask again.
- **Long-poll** (`wait_compaction`) wakes on every keeper write, which
  updates memory first, and re-reads memory every 5 s otherwise.

### Other senders and the lock

The lock path is stable and documented: the registration file's path with
`.json` replaced by `.lock`, where `hash16(home)` is the first 16 hex digits of
SHA-256 of the JSON string of the Paseo home's absolute path (as today). It is
returned as `lock_path`, so callers need not compute it. Only agents with a
file are keeper targets, so a sender takes `lock_path` only when that file
exists; with no file there is nothing to race. Any process that sends to such
an agent (the desk's deliveries in particular) should take it with `flock`
(exclusive, short: only around the `paseo send`, which uses `--no-wait`). The
keeper takes the lock without waiting; finding it held counts as someone
else's send. It rechecks the agent under the lock and stays quiet for
`quiet_seconds` after anyone else's hold or send, so a ping or `/compact`
never lands right after a user message.

## launchd

`agent-quotas install` writes `~/Library/LaunchAgents/<label>.plist`
(label `io.github.nightmachinery.agent-quotas`; `RunAtLoad`, `KeepAlive`,
stdout/stderr to the state directory, `PATH` set to
include the directories of the binaries it calls) and bootstraps it with
`launchctl bootstrap gui/<uid>`. A LaunchAgent runs in the user's GUI session,
so the Keychain reads that failed over ssh succeed here, and ssh clients get
the answer from the socket. `agent-quotas uninstall` boots it out and removes
the plist. `--print` prints the plist instead.

## Deviations recorded during the port

Each package's ported behaviour matches the old tool (golden tests compare
outputs byte for byte on synthetic data); the places where it deliberately
differs:

- Claude: the legacy row carries one extra key, `token_expires_at` (the human
  report's "token expired" line needs it). No relogin, no old response cache
  (`~/tmp/.claude-usage`): the daemon caches. HTTP 401/403 are plain errors,
  retried at the normal interval, not backed off like 429.
- Codex: `swap` is not ported (it rewrites `auth.json`; it stays in the old
  tool). Single-auth `--no-all` output names the active snapshot instead of
  `active`/`auth.json`. Tokens the app-server refreshes inside the isolated
  temporary home are discarded, as the Python did.
- Antigravity: the slow path runs unforced at most once per `slow_fallback_s`
  (an hour) for an account whose cache has nothing; a cached window whose reset
  has passed no longer blocks and marks the reading stale. Only agy's TUI
  writes the statusline cache, so agy run any other way (Paseo's `agy acp`,
  `agy -p`) spends quota while the cache stands still. When the account's
  `antigravity-cli/conversations` or `conversation_summaries.db` is newer than
  the cache's newest capture, a normal read takes the slow path instead, at
  most once per `active_refresh_s` (300 s; -1 turns it off). The slow path
  rewrites the cache, so that capture time spaces the runs; a failed run
  serves the old cache. `agy -p /usage` itself touches `conversations` while
  it runs, so the slow path stamps its reading when the run ends; stamped at
  the start, every run would look like use and start the next one. A slow read
  keeps each cached pool the run did not print, as a `cache` row, where the
  old tool showed only the run's rows, with one exception: /usage lists a
  model's own pool (Gemini 4 Argon) only while that model is in use, so a
  model pool an earlier run printed and this one leaves out is retired, from
  the cache and the report. The general pools, and pools only the statusline
  hook reports, are never retired. The daemon (never a command run by
  hand) also re-reads an account nobody uses: when its newest reading is
  older than `stale_refresh_s` (an hour; -1 turns it off), or a pool's reset
  has passed since that pool was read (one run, `blocked_grace_s` after the
  reset). A failed run waits a full interval. The old flat bucket
  array (`agy_status_json_p=y`) is not ported.
- Native engine: one extra dotfile, `.blocked-state` (the blocked groups and
  their resets, for per-family grace); an unreadable idle time or a failed
  live listing holds back rather than acts; scopes of a second Codex home are
  not modelled. Pickers, `agent-usage-auto-continue-at` and
  `agent-usage-reset-wait` stay in the shell.

## Migration

Per consumer, the exact change and the order are in the run's cutover plan
(outside this repository). In short: install the daemon, write the config,
verify `agent-quotas status` against the old readers, then switch the control
page command, then the shell readers to thin wrappers, then the auto-continue
entry points, and only then stop the old tmux watchers.
