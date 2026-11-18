"""Regenerate the golden files from the old Python tool.

Usage (from internal/claude):

    python3 -I testdata/gen_golden.py ~/scripts/python

It renders every case of testdata/cases.json with claude_code_usage.py's own
functions, with the clock fixed at the file's `now` and TZ set to its `tz`,
and writes testdata/golden/<case>.{txt,color.txt,neon.txt,cloud.txt,json}
plus all.txt and all.cloud.txt for the multi-profile reports.
"""

import json
import os
import sys
import time
from pathlib import Path

HERE = Path(__file__).resolve().parent


def main() -> None:
    scripts = Path(os.path.expanduser(sys.argv[1]))
    cases = json.loads((HERE / "cases.json").read_text())
    os.environ["TZ"] = cases["tz"]
    time.tzset()
    now = cases["now"]
    time.time = lambda: now
    sys.path.insert(0, str(scripts))

    import claude_code_usage as ccu
    from libs.common_sub_status import DARK_THEMES, Style

    styles = {
        "txt": Style(False),
        "color.txt": Style(True),
        "neon.txt": Style(True, true_color=True, theme=DARK_THEMES["neon"]),
    }
    out = HERE / "golden"
    out.mkdir(exist_ok=True)
    reports = []
    for case in cases["cases"]:
        os.environ["COLUMNS"] = str(case["width"])
        profile = ccu.Profile(label=case["label"])
        report = ccu.ProfileReport(profile=profile, warnings=list(case.get("warnings") or []))
        if case.get("error"):
            report.error = case["error"]
        else:
            tok = case.get("token")
            if tok is not None:
                report.token_info = ccu.TokenInfo(
                    token="synthetic", source=tok["source"],
                    expires_at_s=tok.get("expires_at_s"),
                    subscription_type=tok.get("subscription_type"))
            res = case["result"]
            report.result = ccu.FetchResult(
                payload=res["payload"], fetched_at_s=res["fetched_at_s"],
                from_cache=res["from_cache"], stale_reason=res["stale_reason"],
                source=res["source"])
        reports.append((case, report))
        for ext, style in styles.items():
            (out / f"{case['name']}.{ext}").write_text(ccu.render_human(style, report) + "\n")
        line = ccu.render_cloud_only(Style(False), report)
        (out / f"{case['name']}.cloud.txt").write_text((line or "") + "\n")
        row = ccu.build_json(report, keychain=case["keychain"])
        (out / f"{case['name']}.json").write_text(json.dumps(row, indent=2) + "\n")

    os.environ["COLUMNS"] = "100"
    style = Style(False)
    (out / "all.txt").write_text("\n\n".join(ccu.render_human(style, r) for _, r in reports) + "\n")
    lines = [line for _, r in reports if (line := ccu.render_cloud_only(style, r))]
    (out / "all.cloud.txt").write_text(("\n".join(lines) if lines else style.dim("No cloud credit.")) + "\n")


if __name__ == "__main__":
    main()
