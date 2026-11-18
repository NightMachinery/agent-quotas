#!/usr/bin/env zsh
# Regenerates the golden files from the old shell engine, on synthetic data
# only: nothing is typed, sent or notified. Needs the scripts repository
# (NIGHTDIR, default ~/scripts).
# Run with: zsh -f internal/native/testdata/gen-golden.zsh
setopt errexit pipefail typesetsilent

typeset -gr here="${0:A:h}"
typeset -gr root="${NIGHTDIR:-${HOME}/scripts}"
typeset -g tmp
tmp="$(command mktemp -d "${TMPDIR:-/tmp}/aq-native-golden.XXXXXX")"
trap 'command rm -rf -- "${tmp}"' EXIT

unset night_basic_plugin_loaded_p
source "${root}/zshlang/basic/basic.plugin.zsh"
function aliasfn { :; }
function aliasfnq { :; }
source "${root}/zshlang/auto-load/others/agent-usage.zsh"
source "${root}/zshlang/auto-load/others/agent-auto-continue.zsh"

function seconds-fmt-short {
    integer secs="${1:?}"
    printf '%dh:%dm:%ds\n' $(($secs/3600)) $(($secs%3600/60)) $(($secs%60))
}

# 1. Fire reports (h-agent-usage-arm-fire), with the same stubs as
# zshlang/tests/agent-usage-idle.zsh.
typeset -g log="${tmp}/log" idle_now=5 locked_p=n
function h-agent-usage-idle-s { [[ "${idle_now}" == fail ]] && return 1; print -r -- "${idle_now}"; }
function h-agent-usage-screen-locked-p { [[ "${locked_p}" == y ]]; }
function h-agent-usage-continue-send { [[ "${1}" == *gone* ]] && return 1; return 0; }
function h-agent-usage-arm-notify { print -r -- "${1}" >> "${log}"; }
function h-agent-usage-arm-log { :; }
function fire {
    agent_usage_arm_action=continue agent_usage_arm_targets="${*}" h-agent-usage-arm-fire job 'Reset'
}
{
    : > "${log}"
    fire tmux:%1 kitty:3 codex:t claude-bg:b frontmost
    idle_now=fail
    fire tmux:%1 codex:t
    agent_usage_continue_idle_min_s_tmux=600 fire tmux:%1 codex:t
    idle_now=900
    agent_usage_continue_idle_min_s_tmux=600 fire tmux:%1
    idle_now=5 locked_p=y
    agent_usage_continue_idle_min_s_frontmost=0 fire frontmost
    locked_p=n
    agent_usage_continue_idle_min_s_frontmost=0 fire frontmost
    fire tmux:%gone kitty:3
    command cat -- "${log}"
} > "${here}/fire-reports.golden"

# 2. A registration file as h-agent-auto-continue-register writes it.
typeset -g agent_auto_continue_dir="${tmp}/continue"
function h-agent-auto-continue-watch-ensure { :; }
function agent-auto-continue-check { :; }
h-agent-auto-continue-register claude 00000000-0000-4000-8000-000000000001 \
    /tmp/example/projects/p/00000000-0000-4000-8000-000000000001.jsonl claude-work \
    tmux:%7 kitty:12 >/dev/null
command cp -- "${agent_auto_continue_dir}/claude-work/claude-00000000-0000-4000-8000-000000000001" \
    "${here}/registration.golden"
