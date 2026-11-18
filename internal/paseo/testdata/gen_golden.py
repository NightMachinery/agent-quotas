"""Regenerate the golden files of internal/paseo from the old Python tool.

Usage: python3 -I gen_golden.py SCRIPTS_PYTHON_DIR OUT_DIR

SCRIPTS_PYTHON_DIR holds paseo_auto_continue.py. Every input is synthetic.
"""
import json
import os
from pathlib import Path
import sys
import tempfile

sys.path.insert(0, sys.argv[1])
out = Path(sys.argv[2])
import paseo_auto_continue as mod  # noqa: E402

CASES = [
    '"plain"',
    '"caf\\u00e9 \\u2019 \\ud83d\\ude00 tab\\t nl\\n quote\\" bs\\\\ ctl\\u0001 del\\u007f"',
    '[null, true, false, 0, -0, 1, -17, 12345678901234567890]',
    '[0.0, -0.0, 1.0, 1.5, 0.1, 1e16, 1e15, 1234567890123456.7, 1e-5, 0.0001, 1e22, 2.5e-7, 1E2, -3.25e+3, 1e400]',
    '{"b": 1, "a": {"z": [1, {"y": null}], "\\u00e9": "x"}, "A": [], "e": {}}',
    '{"env": {"CODEX_HOME": "~/codex-x", "N": 3, "F": 2.50, "T": true, "U": "\\u00fc"}, "command": ["/x/bin/agy", "acp"], "model": null}',
    '["Usage limit reached", "2026-11-01T00:00:00.000Z", 1789000000123]',
    '{"version": 1, "enabled": true, "home": "/tmp/example-home/.paseo", "agent_id": "00000000-0000-4000-8000-000000000001", "binding": {"agent": "codex", "profile": "codex", "native_home": "/tmp/example-codex", "config_hash": "abc", "provider": "codex-work", "cwd": "/tmp/pr\\u00f6ject"}, "poll": 300, "status": "waiting for a quota-failed turn", "registered_at": 1789000000.1234567, "claimed_failure": null}',
]
digests = []
for text in CASES:
    value = json.loads(text)
    digests.append({'json': text, 'dumps': json.dumps(value, sort_keys=True),
                    'indent': json.dumps(value, indent=2), 'digest': mod.digest(value)})
for home in ('/tmp/example-home/.paseo', '/tmp/héme/.paseo'):
    digests.append({'json': json.dumps(home), 'dumps': json.dumps(home), 'indent': json.dumps(home, indent=2),
                    'digest': mod.digest(home), 'file_prefix': mod.digest(str(home))[:16]})
(out / 'digests.json').write_text(json.dumps(digests, indent=1) + '\n')

# Provider identities under a synthetic HOME.
with tempfile.TemporaryDirectory() as tmp:
    fake_home = Path(tmp) / 'user'
    (fake_home / '.claude-work').mkdir(parents=True)
    (fake_home / '.claude').mkdir()
    os.environ['HOME'] = str(fake_home)
    home = Path(tmp) / 'daemon'
    home.mkdir()
    config = json.loads((out / 'providers-config.json').read_text())
    (home / 'config.json').write_text(json.dumps(config))
    rows = []
    for provider in ['claude', 'codex', 'claude-work', 'claude-work-alias', 'claude-explicit-default',
                     'codex-work', 'codex-nested', 'codex-unicode', 'cycle-a', 'agy-acp', 'nowhere']:
        try:
            identity, _ = mod.provider_config(home, provider)
            identity['native_home'] = identity['native_home'].replace(str(fake_home.resolve()), '~')
            rows.append({'provider': provider, 'identity': identity})
        except mod.HandoffError as error:
            rows.append({'provider': provider, 'error': str(error)})
    for provider in ['agy-acp', 'agy-main', 'antigravity', 'other-acp', 'copilot', 'agy-child']:
        rows.append({'provider': provider, 'agy_account': mod.agy_account(home, provider)})
    (out / 'providers-golden.json').write_text(json.dumps(rows, indent=1) + '\n')

# Failure identities of the transcript fixtures and of lastError records.
failures = {}
claude_session = '00000000-0000-4000-8000-0000000000aa'
codex_thread = '01a1bbc1-518f-7bb0-b332-515f9a0527ee'
with tempfile.TemporaryDirectory() as tmp:
    native = Path(tmp) / 'claude'
    (native / 'projects' / '-tmp-project').mkdir(parents=True)
    (native / 'projects' / '-tmp-project' / (claude_session + '.jsonl')).write_bytes(
        (out / 'claude-transcript.jsonl').read_bytes())
    record = {'persistence': {'sessionId': claude_session}}
    failures['claude'] = mod.claude_limit_failure(record, {'native_home': str(native)})
    import time
    codex = Path(tmp) / 'codex'
    created = int(codex_thread.replace('-', '')[:12], 16) / 1000
    day = time.strftime('%Y/%m/%d', time.localtime(created))
    log = codex / 'sessions' / day / ('rollout-2026-01-01T00-00-00-' + codex_thread + '.jsonl')
    log.parent.mkdir(parents=True)
    log.write_bytes((out / 'codex-rollout.jsonl').read_bytes())
    record = {'persistence': {'sessionId': codex_thread, 'metadata': {'threadId': codex_thread}}}
    failures['codex'] = mod.codex_limit_failure(record, {'native_home': str(codex)})
failures['last_error'] = mod.quota_failure({'lastStatus': 'error', 'lastError': 'You’ve hit your usage limit',
                                            'lastUserMessageAt': '2026-11-01T00:00:00.000Z',
                                            'attentionTimestamp': 1789000000123})
(out / 'failures-golden.json').write_text(json.dumps(failures, indent=1) + '\n')
