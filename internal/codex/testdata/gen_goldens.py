"""Generate the Codex golden files from the old Python tool.

Run with: python3 -I internal/codex/testdata/gen_goldens.py [SCRIPTS_PYTHON_DIR]
(default ~/scripts/python). Every input is synthetic. The clock is fixed, the
zone is UTC and HOME is a throwaway directory holding the fake auth files, so
the output is deterministic. Each case is replayed with codex_status.py
--from-json, which is the Python's own offline mode.
"""
import contextlib, io, json, os, subprocess, sys, tempfile
from pathlib import Path

HERE = Path(__file__).resolve().parent
NOW = 1790000000
H = 3600
D = 86400


def win(used, mins, resets):
    w = {"usedPercent": used, "windowDurationMins": mins, "resetsAt": resets}
    return w


def limit(primary=None, secondary=None, **extra):
    d = {"limitId": extra.pop("limitId", "codex"), "limitName": extra.pop("limitName", None),
         "primary": primary, "secondary": secondary}
    d.update(extra)
    return d


def identity(plan, ws, email="masked@example.test"):
    return {"planType": plan, "planOwnerEmail": email, "workspaceName": ws}


def entry(alias, lim, *, by_id=None, plan="plus", extra=None, path=None):
    e = {"rateLimits": lim}
    if by_id is not None:
        e["rateLimitsByLimitId"] = by_id
    if extra:
        e.update(extra)
    e.update({"identity": identity(plan, alias), "ok": True, "alias": alias,
              "authFile": path or f"~/.codex/auth_{alias}.json"})
    return e


def five_weekly(p5, pw, plan="plus"):
    return limit(win(p5, 300, NOW + 2 * H + 20 * 60), win(pw, 10080, NOW + 3 * D + 5 * H),
                 credits={"hasCredits": False, "unlimited": False, "balance": "0"}, planType=plan)


FILES_A = {"~/.codex/auth.json": '{"k":"a"}\n', "~/.codex/auth_alpha.json": '{"k":"a"}\n',
           "~/.codex/auth_beta.json": '{"k":"b"}\n', "~/.codex/auth_gamma.json": '{"k":"c"}\n'}

CASES = {}

CASES["single_plan"] = {"files": FILES_A, "authFiles": [entry("alpha", five_weekly(52, 31.5))]}

CASES["prolite_weekly"] = {"files": FILES_A, "authFiles": [entry(
    "alpha", limit(win(64, 10080, NOW + 5 * D + 3 * H + 7), None, planType="prolite"), plan="prolite")]}

CASES["blocked_signal"] = {"files": FILES_A, "authFiles": [entry(
    "alpha", limit(win(40, 300, NOW + 90 * 60), win(70, 10080, NOW + 4 * D),
                   rateLimitReachedType="workspace_member_usage_limit_reached"),
    extra={"ordinaryUsageAllowed": False})]}

def by_id_family(base):
    return {
        "codex": base,
        "codex_bengalfox": limit(win(12, 300, NOW + H), win(80, 10080, NOW + 6 * D),
                                  limitId="codex_bengalfox", limitName="GPT-5.3-Codex-Spark"),
        "codex_idle": limit(win(0, 300, NOW + H), None, limitId="codex_idle", limitName="Idle Family"),
    }

base = five_weekly(20, 45)
CASES["model_family"] = {"files": FILES_A, "authFiles": [entry("alpha", base, by_id=by_id_family(base))]}

def reserve(used):
    return limit(win(used, 10080, NOW + 6 * D + 22 * H + 40 * 60), None,
                 limitId="base_model_inference", limitName="gpt-reserve", normalModelSlug="gpt-5.6-luna")

spent = five_weekly(100, 100)
CASES["luna_available"] = {"files": FILES_A, "authFiles": [entry(
    "alpha", spent, by_id={"codex": spent, "base_model_inference": reserve(1)})]}
CASES["luna_spent"] = {"files": FILES_A, "authFiles": [entry(
    "alpha", spent, by_id={"codex": spent, "base_model_inference": reserve(100)})]}

CASES["reset_credits"] = {"files": FILES_A, "authFiles": [entry(
    "alpha", five_weekly(100, 60), extra={"rateLimitResetCredits": {"availableCount": 2, "credits": [
        {"id": "rc-1", "title": "Full reset", "description": "x", "status": "available", "resetType": "full",
         "grantedAt": NOW - 3 * D, "expiresAt": NOW + 2 * D + 5 * H},
        {"id": "rc-2", "title": "Full reset", "status": "available", "resetType": "full",
         "grantedAt": NOW - D, "expiresAt": NOW + 20 * D},
        {"id": "rc-3", "title": "Old reset", "status": "redeemed", "resetType": "full",
         "grantedAt": NOW - 9 * D, "expiresAt": NOW - D},
    ]}})]}

for name, days in (("reset_credit_week", 6.5), ("reset_credit_fortnight", 13), ("reset_credit_far", 40)):
    CASES[name] = {"files": FILES_A, "authFiles": [entry(
        "alpha", five_weekly(10, 20), extra={"rateLimitResetCredits": {"credits": [
            {"id": "rc", "title": "Full reset", "status": "available", "expiresAt": NOW + days * D}]}})]}

credits_lim = five_weekly(33.3333, 41)
credits_lim["credits"] = {"hasCredits": True, "unlimited": False, "balance": "42"}
CASES["credits"] = {"files": FILES_A, "authFiles": [entry("alpha", credits_lim)]}

CASES["multi"] = {"files": FILES_A, "authFiles": [
    entry("alpha", five_weekly(52, 31.5)),
    entry("beta", five_weekly(100, 77)),
    entry("gamma", limit(win(64.25, 10080, NOW + 5 * D), None, planType="prolite"), plan="prolite"),
]}

CASES["multi_blocked"] = {"files": FILES_A, "authFiles": [
    entry("alpha", five_weekly(100, 99.5)),
    entry("beta", five_weekly(100, 100)),
    entry("gamma", limit(win(100, 10080, NOW + 2 * D), None, planType="prolite"), plan="prolite"),
]}

CASES["error_auth"] = {"files": FILES_A, "authFiles": [
    entry("alpha", five_weekly(10, 20)),
    {"error": "codex-status: account/rateLimits/read timed out", "stderr": "some stderr line\nsecond line",
     "ok": False, "alias": "beta", "authFile": "~/.codex/auth_beta.json"},
]}

CASES["no_windows"] = {"files": FILES_A, "authFiles": [
    {"rateLimits": {"limitId": "codex", "primary": None, "secondary": None},
     "ok": True, "alias": "auth", "authFile": "~/.codex/auth.json"},
]}

#: (case, extra args, suffix)
VARIANTS = [
    (["--json", "--all"], "all.json"),
    (["--json", "--no-all"], "single.json"),
    (["--all", "--color", "never"], "all.txt"),
    (["--no-all", "--color", "never"], "single.txt"),
]
EXTRA = {
    "multi": [(["--json", "--all", "--full-pct", "50"], "all.fp50.json"),
              (["--all", "--color", "never", "--full-pct", "50"], "all.fp50.txt"),
              (["--all", "--color", "always", "--true-color", "off"], "all.ansi.txt"),
              (["--all", "--color", "always", "--true-color", "on", "--dark-mode", "on"], "all.neon.txt")],
    "reset_credits": [(["--all", "--color", "always", "--true-color", "on", "--dark-mode", "on"], "all.neon.txt")],
    "luna_available": [(["--all", "--color", "always", "--true-color", "off"], "all.ansi.txt")],
}

RUNNER = r'''
import io, contextlib, sys, time
sys.path.insert(0, sys.argv[1])
NOW = float(sys.argv[2])
time.time = lambda: NOW
import codex_status
sys.argv = ["codex_status.py"] + sys.argv[3:]
buf = io.StringIO()
with contextlib.redirect_stdout(buf):
    try:
        codex_status.run()
    except SystemExit:
        pass
sys.stdout.write(buf.getvalue())
'''


def main():
    scripts = sys.argv[1] if len(sys.argv) > 1 else os.path.expanduser("~/scripts/python")
    out = HERE / "cases"
    out.mkdir(exist_ok=True)
    for name, case in CASES.items():
        with tempfile.TemporaryDirectory() as home:
            for rel, content in case["files"].items():
                p = Path(home) / rel[2:]
                p.parent.mkdir(parents=True, exist_ok=True)
                p.write_text(content)
            inp = out / f"{name}.input.json"
            inp.write_text(json.dumps(case, indent=2) + "\n")
            env = {"HOME": home, "TZ": "UTC", "PATH": "/usr/bin:/bin", "LANG": "C.UTF-8"}
            for args, suffix in VARIANTS + EXTRA.get(name, []):
                proc = subprocess.run(
                    [sys.executable, "-I", "-c", RUNNER, scripts, str(NOW), "--from-json", str(inp)] + args,
                    env=env, capture_output=True, text=True, check=True)
                (out / f"{name}.{suffix}.golden").write_text(proc.stdout)


if __name__ == "__main__":
    main()
