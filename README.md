# agent-quotas

One small daemon that knows how much quota every coding-agent account has
left (Claude Code subscription profiles, Codex auth snapshots, Antigravity
accounts), and resumes your agent sessions when the account they were blocked
on comes back.

Why you might want it:

- **One poller per account.** Status bars, shell reports, web panels and
  watchers all ask the daemon instead of the provider, so the Claude usage
  endpoint stops answering with HTTP 429.
- **Resumes on the event.** When an account goes from blocked to usable, the
  sessions registered on it get `Continue.`, once per failed turn, through
  Paseo, tmux, kitty or `codex queue`. Nothing per-session sleeps in tmux, so a
  reboot loses nothing: launchd starts the daemon again.
- **No shell in the path.** One static Go binary, standard library only,
  reachable only through a 0600 unix socket. Nothing listens on TCP.

## Try it

```sh
go install github.com/NightMachinery/agent-quotas/cmd/agent-quotas@latest
agent-quotas config init        # proposes ~/.config/agent-quotas/config.json; review it
agent-quotas status --direct    # a one-off read without the daemon
agent-quotas install            # launchd agent (macOS): starts now and at login
agent-quotas status             # from the daemon's cache
deus agent-quotas status claude:work   # deusvult=y: force a fresh read of that account only
```

`agent-quotas status --json` is the versioned document other programs should
read. The old reports keep their shapes as subcommands, so a caller can switch
by changing the command name: `claude-code-usage`, `codex-status`,
`agy-status` and `agent-status` (each with `--json`).

Auto-continue:

```sh
agent-quotas continue on            # inside a Paseo agent or a Claude/Codex/agy session
agent-quotas continue status
agent-quotas continue list
agent-quotas continue off
agent-quotas continue stopped --usable   # Paseo agents stopped at a limit whose seat has room now
```

The Paseo engine is on by default; the native one (tmux, kitty, `codex queue`
delivery) is off until you set `"native": {"enabled": true}` in the config,
because it replaces the shell's armed jobs and both would type. Agent hooks
call `agent-quotas hook claude|codex|agy "$TMUX_PANE"` so a limit is noticed at
once rather than at the next poll.

With `"paseo": {"default_auto_continue": true}` in the config, the daemon enables
auto-continue for recently active main agents that have never had a decision.
This setting ships off. An explicit `continue off` opts the agent out. Leave it
on after finishing work. The Paseo engine only resumes the agent's own
quota-failed turn and skips keeper pings. Native terminal resumes use recent
activity and can occasionally reach a session that already finished.

Keeper (cache policy for idle Paseo agents; ships disabled, turn it on with
`"keeper": {"enabled": true}` and `agent-quotas restart`):

```sh
agent-quotas keep status                 # this Paseo agent, or every agent with a file
agent-quotas keep set --cache warm       # ping while idle (up to warm_hours), compact big contexts
agent-quotas keep standby                # stop warming; compact once before the cache expires
agent-quotas keep compact --wait         # /compact now, wait for the result
```

Keepalive pings start with `[agent-quotas keepalive]`, so hooks and logs can
drop them, and a ping that fails on the quota is never auto-continued.

## Paseo plugin

On the host running agent-quotas, install the plugin from a checkout with
Paseo 0.15.4 or later. Enable plugins in Paseo first.

```sh
paseo plugin install /absolute/path/to/agent-quotas/paseo-plugin
```

Open an agent, then choose **Agent quotas** in the Command Center. The agent
panel shows the seat verdict, auto-continue status, cache phase and next
action. It also sets auto-continue and cache policy, puts the agent on
standby, or requests immediate compaction. Cache controls require the keeper.

The plugin reads the local Unix socket on the Paseo daemon host. For a custom
Paseo home, set `PASEO_HOME` in that daemon's environment. The socket follows
`AGENT_QUOTAS_SOCKET`, `AGENT_QUOTAS_STATE_DIR` and `XDG_STATE_HOME`.

## More

- [docs/design.md](docs/design.md): architecture, the socket API and JSON
  schema, polling and back-off, forced refresh, the auto-continue guarantees,
  and the keeper's scheduling, marker and `/v1/agents` API.
