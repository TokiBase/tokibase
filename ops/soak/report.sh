#!/usr/bin/env bash
# Daily report: markdown table of the last 24 h, also written to reports/<date>.md
SOAK=${SOAK:-/home/ubuntu/soak}
export SOAK
python3 - <<'PY' | tee "$SOAK/reports/$(date -u +%F).md"
import csv, os, re, datetime as dt, statistics as st
S = os.environ["SOAK"]
now = dt.datetime.utcnow()
def p(t): return dt.datetime.strptime(t, "%Y-%m-%dT%H:%M:%SZ")
def num(x):
    try: return float(x)
    except: return None
rows = []
for r in csv.DictReader(open(S + "/metrics.csv")):
    rows.append((p(r["ts"]), r))
last = [r for t, r in rows if now - t <= dt.timedelta(hours=24)]
def col(k, rs=None):
    return [v for v in (num(r[k]) for r in (rs if rs is not None else last)) if v is not None]
start = p(open(S + "/START").read().strip()); end = start + dt.timedelta(days=7)
MB = 1 << 20
rss = col("rss_kb"); wal = col("data_wal_b"); lag = col("replica_lag_s")
r0 = [num(r["restarts"]) for t, r in rows]; r0 = [x for x in r0 if x is not None]
rl = col("restarts")
restarts24 = (max(rl) - min(rl)) if rl else 0
total_restarts = max(r0) if r0 else 0
# load runs
runs = []
if os.path.exists(S + "/loadruns.csv"):
    for r in csv.DictReader(open(S + "/loadruns.csv")): runs.append((p(r["ts"]), r))
runs24 = [r for t, r in runs if now - t <= dt.timedelta(hours=24)]
e5 = sum(int(r["http5xx"]) for r in runs24); errs = sum(int(r["errors"]) for r in runs24)
day1 = [float(r["p99_ms"]) for t, r in runs if t - start <= dt.timedelta(hours=24)]
base = st.median(day1) if day1 else None
p99s = [float(r["p99_ms"]) for r in runs24]
p99now = st.median(p99s) if p99s else None
def count(path, pat, since):
    n = 0
    if os.path.exists(path):
        for l in open(path, errors="replace"):
            m = re.match(r"(\S+Z) .*" + pat, l)
            if m and now - p(m.group(1)[:19] + "Z") <= since: n += 1
    return n
D = dt.timedelta(hours=24)
bok = count(S + "/log/maint.log", "RESULT backup_verify OK", D); bfail = count(S + "/log/maint.log", "RESULT backup_verify FAIL", D)
aok = count(S + "/log/audit.log", "RESULT audit_verify OK", D); afail = count(S + "/log/audit.log", "RESULT audit_verify FAIL", D)
bfail_all = count(S + "/log/maint.log", "RESULT backup_verify FAIL", dt.timedelta(days=30))
afail_all = count(S + "/log/audit.log", "RESULT audit_verify FAIL", dt.timedelta(days=30))
def f(x, n=1): return "-" if x is None else ("%.*f" % (n, x))
day = (now - start).total_seconds() / 86400
checks = [
 ("0 unplanned restarts (whole soak)", total_restarts == 0, "%d" % total_restarts),
 ("RSS <= 700 MB (max 24 h)", bool(rss) and max(rss) / 1024 <= 700, f(max(rss) / 1024 if rss else None) + " MB"),
 ("WAL <= 300 MB (max 24 h)", bool(wal) and max(wal) / MB <= 300, f(max(wal) / MB if wal else None) + " MB"),
 ("0 failed backup verifies (whole soak)", bfail_all == 0, "%d" % bfail_all),
 ("0 audit chain errors (whole soak)", afail_all == 0, "%d" % afail_all),
 ("replica lag < 10 s (max 24 h)", (max(lag) if lag else 0) < 10, f(max(lag) if lag else None) + " s"),
 ("p99 <= 2x day-1 baseline", base is None or p99now is None or p99now <= 2 * base, "%s ms vs baseline %s ms" % (f(p99now), f(base))),
 ("no 5xx (24 h)", e5 == 0, "%d" % e5),
]
print("# Soak report %s (UTC)\n" % now.strftime("%Y-%m-%d %H:%M"))
print("Started %s, ends %s, day %.1f of 7, %d metric samples in the last 24 h.\n" % (start.strftime("%F %H:%MZ"), end.strftime("%F %H:%MZ"), day, len(last)))
print("| Metric (last 24 h) | Value |\n| --- | --- |")
print("| RSS max / avg | %s / %s MB |" % (f(max(rss) / 1024 if rss else None), f(sum(rss) / len(rss) / 1024 if rss else None)))
print("| data.db-wal max | %s MB |" % f(max(wal) / MB if wal else None))
print("| data.db / auxiliary.db (latest) | %s / %s MB |" % (f(col("data_db_b")[-1] / MB if col("data_db_b") else None), f(col("aux_db_b")[-1] / MB if col("aux_db_b") else None)))
print("| replica dir (latest) | %s MB |" % f(col("replica_b")[-1] / MB if col("replica_b") else None))
print("| replica lag max | %s s |" % f(max(lag) if lag else None))
print("| open fds max | %s |" % f(max(col("fds")) if col("fds") else None, 0))
print("| restarts (24 h / total) | %d / %d |" % (restarts24, total_restarts))
print("| load runs / ops | %d / %d |" % (len(runs24), sum(int(r["ops"]) for r in runs24)))
print("| load errors / 5xx | %d / %d |" % (errs, e5))
print("| load p99 median (now / day-1 baseline) | %s / %s ms |" % (f(p99now), f(base)))
print("| backup verify ok / fail | %d / %d |" % (bok, bfail))
print("| audit verify ok / fail | %d / %d |" % (aok, afail))
print("\n## Criteria\n\n| Criterion | Result | Value |\n| --- | --- | --- |")
for n, ok, v in checks: print("| %s | %s | %s |" % (n, "PASS" if ok else "FAIL", v))
print("\nOverall: **%s**" % ("PASS so far" if all(c[1] for c in checks) else "FAIL"))
PY
