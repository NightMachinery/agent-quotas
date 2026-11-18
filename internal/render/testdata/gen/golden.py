"""Writes the render golden files from the old Python renderers.

Run from internal/render/testdata with the old scripts checkout on the path:

    cd internal/render/testdata && TZ=UTC python3 -I gen/golden.py ~/scripts/python

Inputs are synthetic and written next to the outputs, so the Go tests read
exactly what Python read. NOW and the zone are fixed.
"""
import json
import sys
import time

sys.path.insert(0, sys.argv[1])
from agy_status_render import render  # noqa: E402
from agent_status_render import render_section, section_model  # noqa: E402

NOW = 1700000000
CASES = [(80, False, "auto"), (80, True, "auto"), (120, False, "auto"), (120, True, "auto"),
         (100, False, "y"), (80, False, "y"), (120, False, "n"), (40, False, "auto")]


def bucket(identity, group, used, seconds, source="cache", label="Weekly Limit Remaining"):
    return {"bucket_id": identity, "group": group, "label": label,
            "remaining_percent": 100 - used, "resets_at": NOW + seconds if seconds is not None else None,
            "resets_at_iso": "", "source": source, "used_percent": used}


def agy_rows():
    return [
        {"account": "m", "label": "m", "signed_in": None, "auth_status": "file-present",
         "tier": "Google AI Pro", "email": "masked@example.test", "message": None, "error": None, "buckets": [
             bucket("gemini-weekly", "Gemini Models", 6, 4 * 86400 + 7 * 3600),
             bucket("3p-weekly", "Claude and GPT models", 75, 2 * 86400 + 11 * 3600),
             bucket("gemini-4-argon-weekly", "Gemini 4 Argon", 95, 12 * 60, label="Gemini 4 Argon Weekly Limit"),
             bucket("gemini-5h", "Gemini Models", 6, 3600 + 25 * 60, label="5h Limit Remaining"),
             bucket("gemini-4-argon-5h", "Gemini 4 Argon", 33.35, -3600, label="Gemini 4 Argon 5 Hour Limit"),
             bucket("future-model-daily-limit", "Future Model", 0, None, label="Daily Limit Remaining"),
             bucket("odd-pool-weekly", "", 100, 600)]},
        {"account": "cave", "label": "研究 Café", "signed_in": True, "auth_status": "verified",
         "tier": "Pro", "email": "another-very-long-address@example.test", "message": None, "error": None, "buckets": [
             bucket("3p-5h", "Claude and GPT models", 12.5, 13 * 60, source="live"),
             bucket("gemini-weekly", "Gemini Models", 70, 86400, source="cache")]},
        {"account": "fm2", "label": "fm2", "signed_in": False, "auth_status": "missing", "tier": None,
         "email": None, "message": "Not signed in; run agy-acc-login fm2", "error": None, "buckets": []},
        {"account": "broken", "label": "broken", "signed_in": None, "auth_status": "file-present", "tier": None,
         "email": None, "message": None, "error": "Quota check failed (exit 124); file credentials exist, validity unverified",
         "buckets": []},
    ]


def agy_cache():
    return {"account": {"tier": "Google AI Pro"}, "buckets": {
        "gemini-weekly": {"remaining_percent": 94, "captured_at": NOW - 150},
        "3p-weekly": {"remaining_percent": 25, "captured_at": NOW - 30},
        "gemini-argon-wtu-weekly": {"remaining_percent": 5, "captured_at": NOW - 7200},
        "gemini-5h": {"remaining_percent": 94, "captured_at": NOW - 100},
        "gemini-4-argon-5h": {"remaining_percent": 66.65},
        "future-model-daily-limit": {"remaining_percent": 100, "captured_at": NOW - 10},
    }}


def claude():
    return {"agent": "claude", "exit": 1, "report": [
        {"profile": "default", "plan": "max", "cache": {"from_cache": True, "fetched_at": NOW - 120},
         "windows": [{"key": "session", "label": "5h session", "utilization_percent": 52,
                      "resets_at": NOW + 3600 + 25 * 60},
                     {"key": "weekly_all", "label": "7d all models", "utilization_percent": 71.5,
                      "resets_at": NOW + 3 * 86400},
                     {"key": "weekly_scoped", "label": "7d Fable", "utilization_percent": 90,
                      "resets_at": NOW + 4 * 86400 + 7 * 3600},
                     {"key": "weekly_scoped", "label": "7d Sonnet", "utilization_percent": None,
                      "resets_at": None},
                     {"key": "overage", "label": "Monthly overage", "utilization_percent": 100.0,
                      "resets_at": NOW + 20 * 86400}],
         "warnings": ["Usage endpoint answered slowly"],
         "relogin": "claude /login",
         "extra_usage": {"is_enabled": True, "monthly_limit": 50.0, "used_credits": 12,
                         "utilization": None, "spend_limit_reached": False},
         "cloud_credit": {"limit_dollars": 2500.0, "remaining_dollars": 2248.03, "expires_at": NOW + 86400 + 7200,
                          "locked_reason": "billing review", "available": True}},
        {"profile": "work", "error": "Login required"},
        {"profile": "spare", "plan": "pro", "cache": {"from_cache": True, "stale_reason": "HTTP 429; serving cache"},
         "token_expired": True, "windows": []}]}


def codex():
    window = {"windowDurationMins": 10080, "usedPercent": 61, "resetsAt": NOW + 2 * 86400 + 11 * 3600}
    return {"agent": "codex", "exit": 0, "report": {"authFiles": [
        {"alias": "研究", "ok": True, "identity": {"planType": "pro", "planOwnerEmail": "masked@example.test",
                                                  "workspaceName": "Lab workspace"},
         "rateLimits": {"credits": {"hasCredits": True, "unlimited": False, "balance": "12.50"}},
         "quota": {"windows": [dict(window, windowDurationMins=300, usedPercent=88.25), window],
                   "blocked": True, "reasons": ["spend limit", "weekly at 100%"],
                   "resetCredits": {"availableCount": 2, "credits": [
                       {"title": "Full reset", "status": "available", "expiresAt": NOW + 4 * 86400},
                       {"title": "Full reset", "status": "available", "expiresAt": NOW + 5 * 86400},
                       {"title": "Used", "status": "used", "expiresAt": NOW + 3600}]},
                   "lunaReserve": {"label": "Reserve", "limitId": "reserve", "available": True,
                                   "model": "gpt-reserve", "windows": [dict(window, usedPercent=20)]},
                   "limits": [{"limitId": "reserve", "label": "dup"},
                              {"limitId": "model-x", "limitName": "gpt-model-x",
                               "windows": [dict(window, windowDurationMins=1440, usedPercent=5)]}]}},
        {"alias": "spare", "ok": True, "identity": {"planType": "plus"},
         "quota": {"windows": [], "lunaReserve": {"limitId": "reserve", "available": False}}},
        {"alias": "broken", "ok": False, "error": "Account unavailable"},
        {"alias": "silent", "ok": False}],
        "averageUsage": {"usableCount": 2, "blockedCount": 1,
                         "windows": [window, dict(window, windowDurationMins=300, usedPercent=44.125)],
                         "firstTimeToReset": NOW + 3600, "firstTimeToResetAlias": "研究"}}}


def agy():
    return {"agent": "agy", "exit": 1, "report": agy_rows(), "cache_files": {"m": "agy-cache.json"}}


def write_json(path, value):
    with open(path, "w") as f:
        json.dump(value, f, ensure_ascii=False, indent=1)
        f.write("\n")


def main():
    write_json("agy-rows.json", agy_rows())
    write_json("agy-cache.json", agy_cache())
    lines = [json.dumps(e, ensure_ascii=False) for e in (claude(), codex(), agy())]
    lines[1:1] = ["invalid", json.dumps({"agent": "gemini", "exit": 0, "report": [1]}),
                  json.dumps({"agent": "codex", "exit": 2, "report": None})]
    with open("envelopes.jsonl", "w") as f:
        f.write("\n".join(lines) + "\n")

    envelopes = []
    for line in lines:
        try:
            envelope = json.loads(line)
            if not isinstance(envelope, dict):
                raise ValueError()
        except ValueError:
            envelope = {"agent": "Status", "report": None}
        envelopes.append(envelope)

    for columns, color, absolute in CASES:
        tag = f"{columns}-{'color' if color else 'plain'}-{absolute}"
        rows = [dict(r, _display_cache_file="agy-cache.json") if r["account"] == "m" else r for r in agy_rows()]
        with open(f"agy-{tag}.txt", "w") as f:
            f.write(render(rows, columns, color, absolute, NOW))
        with open(f"agent-status-{tag}.txt", "w") as f:
            for envelope in envelopes:
                f.write(render_section(envelope, max(40, columns), color, absolute, NOW))

    with open("agent-status.json", "w") as f:
        json.dump({"generated_at": NOW + 0.25, "providers": [section_model(e, NOW + 0.25) for e in envelopes]},
                  f, ensure_ascii=False)
        f.write("\n")


assert time.localtime(0).tm_gmtoff == 0, "run with TZ=UTC"
main()
