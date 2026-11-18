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
    "agy_binary": "agy"
  },
  "poll": {"force_floor_s": 5, "backoff_max_s": 1800},
  "paseo": {"homes": ["~/.paseo"], "registrations_dir": "", "send_behavior": ""},
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
  has room). A missing or too-old cache gives `unknown`.

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
- `POST /v1/continue/on` `{"kind": "paseo", "agent_id": "...", "home": "...", "poll": 300}`
  or `{"kind": "native", "agent": "claude", "session_id": "...", "transcript": "...", "env": {...}, "frontmost": false}`
- `POST /v1/continue/off` `{"kind": ..., ...}`
- `POST /v1/continue/check` `{"scope": "claude-work"}` or `{"kind": "paseo", "agent_id": ...}`: run one check now
- `POST /v1/hook` `{"agent": "claude", "pane": "%3", "payload": {...}}`: the
  hook kick (marks the session, then checks its scope)
- `POST /v1/config/reload`

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
  array and its human report.
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

Two engines, both inside the daemon, both triggered the same way:

- an account transition from `blocked` (or `unknown` after `blocked`) to
  `usable`: check every registration bound to that account;
- daemon start-up: check every registration;
- a safety tick per registration (its `poll`, default 300 s, minimum 30 s);
- an explicit check or a hook kick.

A check never blocks a poller: checks run on a separate worker with a
per-registration lock.

### Paseo engine (port of `paseo_auto_continue.py`)

Registrations stay in
`${XDG_STATE_HOME:-~/.local/state}/agent-sessions/auto-continue-paseo/<hash16(home)>-<agent>.json`
with the same JSON fields (`version`, `enabled`, `home`, `agent_id`,
`binding`, `poll`, `status`, `registered_at`, `claimed_failure`) and the same
`.lock` files taken with `flock`. Because both the old Python workers and the
daemon take that lock and record `claimed_failure` before sending, running both
during the cutover cannot send twice for one failed turn.

`tick` is ported with every guarantee:

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
   `claimed_failure` stays claimed.
5. Pending permission, or no failure while not in error: waiting.
6. Failure already claimed: waiting for a new one.
7. Quota for that agent's own binding (Claude profile + model family; Codex
   active auth of that `CODEX_HOME`) is read from the daemon store with a
   refresh if the snapshot is older than 60 s (subject to the floor and
   back-off). Not usable or unknown: waiting for quota recovery.
8. Re-inspect and re-read the stored record; if anything changed (status,
   permission, binding, failure identity), skip.
9. Record `claimed_failure` (atomic write, fsync) **before** sending.
10. `paseo send <id> Continue. --behavior <PASEO_SEND_BEHAVIOR|steer> --home <home> --no-wait --json`
    with Paseo marker variables removed from the environment.
11. A failed or timed-out send disables the registration
    (`send uncertain; inspect agent and re-arm explicitly`).

`stopped` and `stopped --usable` (the pickers' listing) are ported too, with
Antigravity quota read from the store instead of `agy-status`.

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
- The text is the old `agent_auto_continue_text` (self-limiting by default).

Registration commands (`agent-quotas continue on|off|status|list`) inside a
Paseo agent (`PASEO_AGENT_ID`) register that agent with the Paseo engine;
otherwise they register the native session from the same environment variables
the shell used (`CLAUDE_CODE_SESSION_ID`, `CLAUDE_CONFIG_DIR`, `CODEX_THREAD_ID`,
`CODEX_HOME`, `ANTIGRAVITY_CONVERSATION_ID`, `TMUX_PANE`). `agent-quotas hook
claude|agy [PANE]` reads the hook payload on stdin and kicks the daemon.

## launchd

`agent-quotas install` writes `~/Library/LaunchAgents/<label>.plist`
(label `io.github.nightmachinery.agent-quotas`; `RunAtLoad`, `KeepAlive`,
`ProcessType` Background, stdout/stderr to the state directory, `PATH` set to
include the directories of the binaries it calls) and bootstraps it with
`launchctl bootstrap gui/<uid>`. A LaunchAgent runs in the user's GUI session,
so the Keychain reads that failed over ssh succeed here, and ssh clients get
the answer from the socket. `agent-quotas uninstall` boots it out and removes
the plist. `--print` prints the plist instead.

## Migration

Per consumer, the exact change and the order are in the run's cutover plan
(outside this repository). In short: install the daemon, write the config,
verify `agent-quotas status` against the old readers, then switch the control
page command, then the shell readers to thin wrappers, then the auto-continue
entry points, and only then stop the old tmux watchers.
