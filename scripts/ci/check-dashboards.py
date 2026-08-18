"""Validate every PromQL expression in every committed dashboard.

A dashboard is only as good as its queries, and a typo in one shows up as an
empty panel months later rather than as an error now. promtool has no
"check dashboard", but it does have "check rules" — so every expression is
written out as a recording rule and handed to the real parser.

Grafana template variables are substituted first: they are Grafana syntax, not
PromQL, and the parser is right to reject them.
"""
import glob
import io
import json
import os
import re
import subprocess
import sys

SUBST = {
    "$__rate_interval": "5m",
    "$__interval": "5m",
    "$__range": "1h",
    "$container": "x",
    "$node": "x",
    "$job": "x",
    "$instance": "x",
    "$datasource": "prometheus",
    "$device": "x",
    "$maxmount": "x",
    "$disk": "x",
}


def exprs_in(node, out):
    if isinstance(node, dict):
        if isinstance(node.get("expr"), str) and node["expr"].strip():
            out.append(node["expr"])
        for v in node.values():
            exprs_in(v, out)
    elif isinstance(node, list):
        for v in node:
            exprs_in(v, out)


def clean(e):
    for k, v in SUBST.items():
        e = e.replace(k, v)
    # Any surviving $var is a template variable this script does not know about;
    # substitute a harmless literal rather than fail on Grafana syntax.
    e = re.sub(r"\$\{?[A-Za-z_][A-Za-z0-9_]*\}?", "x", e)
    return e


def main(promtool, dashboard_dir):
    files = sorted(glob.glob(os.path.join(dashboard_dir, "*.json")))
    if not files:
        print("no dashboards found in", dashboard_dir)
        return 1

    bad = 0
    for f in files:
        d = json.load(io.open(f, encoding="utf-8"))
        found = []
        exprs_in(d, found)
        found = [e for e in dict.fromkeys(found)]
        if not found:
            print("%-34s   0 expressions" % os.path.basename(f))
            continue

        rules = {"groups": [{"name": "dashboard-check", "rules": [
            {"record": "check:e%d" % i, "expr": clean(e)} for i, e in enumerate(found)
        ]}]}
        tmp = os.path.join(os.path.dirname(dashboard_dir), ".dashboard-check.yml")
        io.open(tmp, "w", encoding="utf-8", newline="\n").write(json.dumps(rules))
        r = subprocess.run([promtool, "check", "rules", tmp],
                           capture_output=True, text=True)
        os.remove(tmp)
        if r.returncode != 0:
            bad += 1
            print("%-34s FAIL" % os.path.basename(f))
            for line in (r.stdout + r.stderr).splitlines():
                if "expr" in line.lower() or "error" in line.lower() or "parse" in line.lower():
                    print("    " + line.strip())
        else:
            print("%-34s OK   %3d expressions" % (os.path.basename(f), len(found)))
    return 1 if bad else 0


if __name__ == "__main__":
    sys.exit(main(sys.argv[1], sys.argv[2]))
