#!/usr/bin/env zsh
# Writes the agy legacy golden files from the old zsh agy-status on synthetic
# accounts, a fake agy and a fake HOME. No credentials, network or real state.
#
#   zsh -f testdata/gen/legacy.zsh ~/scripts   (from internal/agy)
setopt err_exit pipe_fail extended_glob
typeset -g test_root="${1:A}" out_dir="${0:A:h:h}"
typeset -g test_tmp="$(command mktemp -d /private/tmp/agy-golden.XXXXXX)"
trap 'command rm -rf -- "${test_tmp}"' EXIT
export TZ=UTC HOME="${test_tmp}/home"
command mkdir -p "${HOME}" "${test_tmp}/bin"
source "${test_root}/zshlang/basic/core.zsh"
function ecerr { print -ru2 -- "$@" }
function isdbg { return 1 }
source "${test_root}/zshlang/basic/return-aliases.zsh"
source "${test_root}/zshlang/basic/compatibility.zsh"
source "${test_root}/zshlang/basic/enhancers.zsh"
source "${test_root}/zshlang/basic/macros.zsh"
source "${test_root}/zshlang/basic/assert.zsh"
source "${test_root}/zshlang/basic/eval.zsh"
function assert-args { return 0 }
function isDeus { test -n "${deusvult-}" }
function h-color-mode-p { [[ "${1}" == always ]] }
function date-unix-to-3339 { command gdate -d "@${1}" --rfc-3339=s }
function silent { "$@" >/dev/null 2>&1 }
typeset -gx NIGHTDIR="${test_root}"
typeset -gx AGY_ACCOUNTS='m cave fm2 broke'
typeset -gx AGY_ACCOUNTS_DIR="${test_tmp}/accounts"
typeset -gx AGY_ACCOUNT_DIR_M="${test_tmp}/gemini"
typeset -gx AGY_ACCOUNT_LABEL_CAVE='Cave lab'
typeset -gx AGY_MAIN_STATUS_CACHE_FILE="${test_tmp}/main-cache.json"
typeset -gx agy_status_cache_file="${AGY_MAIN_STATUS_CACHE_FILE}"
typeset -gx agy_status_statusline_max_age_s=-1
typeset -gx AGY_FAKE_USAGE="${out_dir}/usage.txt"
command cat > "${test_tmp}/bin/agy" <<'SH'
#!/bin/sh
[ "${AGY_ACCOUNT_ID}" = broke ] && exit 3
command cat -- "${AGY_FAKE_USAGE}"
SH
command chmod +x "${test_tmp}/bin/agy"
typeset -gx PATH="${test_tmp}/bin:${PATH}"
source "${test_root}/zshlang/auto-load/others/antigravity.zsh"
source "${test_root}/zshlang/auto-load/others/agy-status.zsh"
local id
for id in m cave broke ; do
    local -a reply
    h-agy-account-info "${id}"
    command mkdir -p "${reply[4]}"
    print -r -- 'inert fixture' > "${reply[4]}/antigravity-oauth-token"
done
command mkdir -p "${AGY_ACCOUNTS_DIR}/fm2/antigravity-cli"
command cp "${out_dir}/m-cache.json" "${AGY_MAIN_STATUS_CACHE_FILE}"
agy-status --json > "${out_dir}/legacy-normal.json" 2>/dev/null || true
command cp "${AGY_ACCOUNTS_DIR}/cave/antigravity-cli/quota-cache.json" "${out_dir}/cave-cache-after.json"
deusvult=1 agy-status --account m --json > "${out_dir}/legacy-forced.json" 2>/dev/null
command cp "${AGY_MAIN_STATUS_CACHE_FILE}" "${out_dir}/m-cache-after.json"
print -r -- "ok"
