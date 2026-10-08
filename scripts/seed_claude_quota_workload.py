#!/usr/bin/env python3
"""Convert historical usage into a workload-only Claude prediction snapshot.

Run once before first enabling prediction, with the service stopped:
  python3 scripts/seed_claude_quota_workload.py usage-report.sqlite auths seed.json
Review the output, then move seed.json to the separate state directory, for
example auths-state/.claude-cache-quota.json.
The SQLite connection is read-only. Output must not already exist. This does not
merge or overwrite live calibration. Historical logs have no quota headers, so
every account has empty windows: no utilization, ratios, or synthetic labels.
Only the last 32 ordinary requests per model and 8 models/account are retained.
API price ratios are workload priors; future live headers learn quota scaling.
"""

import argparse
import hashlib
import json
import math
import os
import sqlite3
from datetime import datetime, timezone
from pathlib import Path


def weighted_work(model, tokens):
    model = model.strip().lower().split("(", 1)[0].replace(".", "-")
    family, version = model.removeprefix("claude-").split("-", 1) if "-" in model.removeprefix("claude-") else ("", "")
    version = version.split("-202", 1)[0]
    base, read = 0, .1
    if family == "opus" and version in ("4-5", "4-6", "4-7", "4-8", "5"):
        base = 1
    elif family == "opus" and version == "5-5":
        base, read = .8, .05
    elif family == "sonnet" and version in ("4-5", "4-6"):
        base = .6
    elif family == "sonnet" and version == "5":
        base = .4
    elif family == "sonnet" and version == "5-5":
        base, read = .4, .05
    elif family == "haiku" and version == "4-5":
        base = .2
    names = ("input_tokens", "output_tokens", "cache_read_tokens", "cache_creation_tokens")
    values = [tokens.get(name, 0) for name in names]
    if not base or any(type(value) is not int or value < 0 for value in values):
        return model, 0
    work = base * (values[0] + 5 * values[1] + read * values[2] + 2 * values[3])
    return "claude-" + family + "-" + version, work if math.isfinite(work) and work > 0 else 0


def convert(database, auth_dir):
    sources = {}
    for path in auth_dir.glob("*.json"):
        try:
            auth = json.loads(path.read_text())
            email = auth.get("email") or auth.get("metadata", {}).get("email", "")
            provider = auth.get("type") or auth.get("provider")
            if provider != "claude" or not isinstance(email, str) or not email.strip():
                continue
            email = email.strip().lower()
            key = hashlib.sha256(email.encode()).hexdigest()
            sources[email] = sources[key] = key
        except (OSError, ValueError, TypeError, AttributeError):
            continue
    accounts = {}
    accepted = 0
    with sqlite3.connect(database.resolve().as_uri() + "?mode=ro", uri=True) as connection:
        for timestamp, payload in connection.execute("SELECT ts, payload FROM usage_events ORDER BY ts, id"):
            try:
                record = json.loads(payload)
                source = record.get("source", "").strip().lower()
                key = sources.get(source)
                if key is None or record.get("failed") or record.get("generate") is False:
                    continue
                model, work = weighted_work(record.get("model", ""), record.get("tokens", {}))
                if not work:
                    continue
                at = datetime.fromisoformat((record.get("timestamp") or timestamp).replace("Z", "+00:00"))
                if at.tzinfo is None:
                    at = at.replace(tzinfo=timezone.utc)
                at = at.astimezone(timezone.utc).isoformat().replace("+00:00", "Z")
                account = accounts.setdefault(key, {"profiles": {}, "windows": {}, "completed_at": at})
                profile = account["profiles"].setdefault(model, {"work": [], "observed_at": at})
                profile["work"] = (profile["work"] + [work])[-32:]
                profile["observed_at"] = max(profile["observed_at"], at)
                account["completed_at"] = max(account["completed_at"], at)
                if len(account["profiles"]) > 8:
                    oldest = min(account["profiles"], key=lambda name: account["profiles"][name]["observed_at"])
                    del account["profiles"][oldest]
                accepted += 1
            except (ValueError, TypeError, AttributeError, OverflowError):
                continue
    accounts = dict(sorted(accounts.items(), key=lambda item: item[1]["completed_at"])[-128:])
    return {"version": 1, "accounts": accounts}, accepted


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("database", type=Path)
    parser.add_argument("auth_dir", type=Path)
    parser.add_argument("output", type=Path)
    args = parser.parse_args()
    state, accepted = convert(args.database, args.auth_dir)
    fd = os.open(args.output, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600)
    with os.fdopen(fd, "w") as output:
        json.dump(state, output, separators=(",", ":"))
        output.flush()
        os.fsync(output.fileno())
    print(f"Seeded {len(state['accounts'])} accounts from {accepted} ordinary token records; no quota calibration created.")


if __name__ == "__main__":
    main()
