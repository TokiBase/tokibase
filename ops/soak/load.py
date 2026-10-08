#!/usr/bin/env python3
"""Low-intensity mixed load for the soak (read 70 / write 25 / realtime 5), stdlib only.

  load.py setup                      create the scratch collection and pick the read collections (once)
  load.py run [--minutes 10] [--clients 20] [--think-ms 300]

`run` writes one JSON line to log/load.log, state/last_load.json and appends to loadruns.csv.
Reads go to the biggest FGR collections (list, sort, filter, page 1-10), writes to the scratch
collection soak_items, realtime = SSE connect + subscribe + own write + wait for the event.
"""
import argparse, http.client, json, os, random, statistics, sys, threading, time, urllib.parse

SOAK = os.environ.get("SOAK", "/home/ubuntu/soak")
HOST, PORT = "127.0.0.1", int(os.environ.get("SOAK_PORT", "8099"))
SCRATCH = "soak_items"
STATE = os.path.join(SOAK, "state")


def req(method, path, token=None, body=None, timeout=30):
    c = http.client.HTTPConnection(HOST, PORT, timeout=timeout)
    h = {}
    data = None
    if token:
        h["Authorization"] = token
    if body is not None:
        data = json.dumps(body)
        h["Content-Type"] = "application/json"
    c.request(method, path, data, h)
    r = c.getresponse()
    raw = r.read()
    c.close()
    try:
        return r.status, json.loads(raw) if raw else {}
    except ValueError:
        return r.status, {}


def login():
    email = open(os.path.join(SOAK, ".su-email")).read().strip()
    pw = open(os.path.join(SOAK, ".su-pass")).read().strip()
    st, j = req("POST", "/api/collections/_superusers/auth-with-password", body={"identity": email, "password": pw})
    if st != 200:
        sys.exit("login failed: %s" % st)
    return j["token"]


def setup():
    os.makedirs(STATE, exist_ok=True)
    tok = login()
    st, _ = req("GET", "/api/collections/" + SCRATCH, tok)
    if st == 404:
        st, j = req("POST", "/api/collections", tok, {
            "name": SCRATCH, "type": "base",
            "fields": [{"name": "title", "type": "text"}, {"name": "body", "type": "text"}, {"name": "n", "type": "number"},
                       {"name": "created", "type": "autodate", "onCreate": True}],
            "indexes": ["CREATE INDEX idx_soak_items_created ON soak_items (created)"],
            "listRule": None, "viewRule": None, "createRule": None, "updateRule": None, "deleteRule": None})
        print("create scratch:", st, "" if st == 200 else j)
    st, j = req("GET", "/api/collections?perPage=1000", tok)
    cands = []
    for c in j["items"]:
        n = c["name"]
        if c.get("system") or n.startswith("_") or n == SCRATCH or c["type"] != "base":
            continue
        st, r = req("GET", "/api/collections/%s/records?perPage=1" % n, tok)
        if st == 200 and r.get("totalItems", 0) > 200:
            cands.append({"name": n, "records": r["totalItems"], "created": any(f["name"] == "created" for f in c["fields"])})
    cands.sort(key=lambda x: -x["records"])
    json.dump(cands[:8], open(os.path.join(STATE, "colls.json"), "w"), indent=1)
    print("read collections:", [(c["name"], c["records"]) for c in cands[:8]])


class Rec:
    def __init__(self):
        self.lock = threading.Lock()
        self.lat = []          # all ops, ms
        self.by = {}           # op -> list
        self.status = {}
        self.errors = 0
        self.e5 = 0
        self.samples = []

    def add(self, op, ms, status, err=None):
        with self.lock:
            self.lat.append(ms)
            self.by.setdefault(op, []).append(ms)
            k = str(status)
            self.status[k] = self.status.get(k, 0) + 1
            if status >= 500:
                self.e5 += 1
            if err or not (200 <= status < 300):
                self.errors += 1
                if len(self.samples) < 5:
                    self.samples.append("%s %s %s" % (op, status, err or ""))


def pct(a, p):
    if not a:
        return 0.0
    a = sorted(a)
    return round(a[min(len(a) - 1, int(len(a) * p / 100))], 1)


def timed(rec, op, fn):
    t = time.time()
    try:
        st = fn()
        err = None
    except Exception as e:  # transport error counts as status 0
        st, err = 0, repr(e)[:120]
    rec.add(op, (time.time() - t) * 1000, st, err)
    return st


def worker(i, tok, colls, rec, stop, think):
    rnd = random.Random(i * 7919 + int(time.time()))
    seq = 0
    while not stop.is_set():
        x = rnd.randint(0, 99)
        if x < 70:
            c = rnd.choice(colls)
            q = {"page": rnd.randint(1, 10), "perPage": 30}
            if c["created"]:
                q["sort"] = "-created"; q["filter"] = 'created >= "2020-01-01 00:00:00"'
            else:
                q["sort"] = "-id"; q["filter"] = 'id != ""'
            path = "/api/collections/%s/records?%s" % (c["name"], urllib.parse.urlencode(q))
            timed(rec, "read", lambda: req("GET", path, tok)[0])
        elif x < 95:
            seq += 1
            b = {"title": "s-%d-%d" % (i, seq), "body": "x" * rnd.randint(20, 400), "n": rnd.random()}
            timed(rec, "write", lambda: req("POST", "/api/collections/%s/records" % SCRATCH, tok, b)[0])
        else:
            realtime(i, tok, rec, rnd)
        stop.wait(rnd.expovariate(1.0 / think))


def realtime(i, tok, rec, rnd):
    t = time.time()
    c = http.client.HTTPConnection(HOST, PORT, timeout=8)
    try:
        c.request("GET", "/api/realtime", headers={"Accept": "text/event-stream"})
        r = c.getresponse()
        cid = None
        ev = None
        deadline = time.time() + 8
        got_event = False
        subscribed = False
        title = "rt-%d-%d" % (i, int(t * 1000))
        while time.time() < deadline:
            line = r.fp.readline().decode().strip()
            if line.startswith("event:"):
                ev = line[6:].strip()
            elif line.startswith("data:") and ev == "PB_CONNECT" and not subscribed:
                cid = json.loads(line[5:])["clientId"]
                st, _ = req("POST", "/api/realtime", tok, {"clientId": cid, "subscriptions": [SCRATCH + "/*"]})
                if st != 204:
                    rec.add("rt-connect", (time.time() - t) * 1000, st, "subscribe")
                    return
                subscribed = True
                rec.add("rt-connect", (time.time() - t) * 1000, 200)
                t2 = time.time()
                req("POST", "/api/collections/%s/records" % SCRATCH, tok, {"title": title, "body": "rt", "n": 0})
            elif line.startswith("data:") and ev == SCRATCH + "/*" and title in line:
                rec.add("rt-event", (time.time() - t2) * 1000, 200)
                got_event = True
                break
        if not got_event:
            rec.add("rt-event", 8000, -1, "no event")
    except Exception as e:
        rec.add("rt-connect", (time.time() - t) * 1000, 0, repr(e)[:120])
    finally:
        c.close()


def run(a):
    tok = login()
    colls = json.load(open(os.path.join(STATE, "colls.json")))
    rec, stop = Rec(), threading.Event()
    ths = [threading.Thread(target=worker, args=(i, tok, colls, rec, stop, a.think_ms / 1000.0), daemon=True) for i in range(a.clients)]
    t0 = time.time()
    for t in ths:
        t.start()
    stop.wait(a.minutes * 60)
    stop.set()
    for t in ths:
        t.join(15)
    secs = time.time() - t0
    main = [x for k in ("read", "write") for x in rec.by.get(k, [])]  # p99 over read+write, comparable run to run
    out = {
        "ts": time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime(t0)), "minutes": a.minutes, "clients": a.clients,
        "ops": len(rec.lat), "rps": round(len(rec.lat) / secs, 1),
        "p50_ms": pct(main, 50), "p95_ms": pct(main, 95), "p99_ms": pct(main, 99),
        "read_p99_ms": pct(rec.by.get("read", []), 99), "write_p99_ms": pct(rec.by.get("write", []), 99),
        "rt_event_p99_ms": pct(rec.by.get("rt-event", []), 99), "rt_events": len(rec.by.get("rt-event", [])),
        "http5xx": rec.e5, "errors": rec.errors, "status": rec.status, "error_samples": rec.samples,
    }
    os.makedirs(STATE, exist_ok=True)
    line = json.dumps(out)
    open(os.path.join(STATE, "last_load.json"), "w").write(line)
    with open(os.path.join(SOAK, "log", "load.log"), "a") as f:
        f.write(time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()) + " " + line + "\n")
    csvp = os.path.join(SOAK, "loadruns.csv")
    new = not os.path.exists(csvp)
    with open(csvp, "a") as f:
        if new:
            f.write("ts,ops,rps,p50_ms,p95_ms,p99_ms,rt_event_p99_ms,http5xx,errors\n")
        f.write("%s,%d,%s,%s,%s,%s,%s,%d,%d\n" % (out["ts"], out["ops"], out["rps"], out["p50_ms"], out["p95_ms"], out["p99_ms"], out["rt_event_p99_ms"], out["http5xx"], out["errors"]))
    print(line)
    return 0 if out["errors"] == 0 else 1


if __name__ == "__main__":
    ap = argparse.ArgumentParser()
    ap.add_argument("cmd", choices=["setup", "run"])
    ap.add_argument("--minutes", type=float, default=10)
    ap.add_argument("--clients", type=int, default=20)
    ap.add_argument("--think-ms", type=float, default=300)
    a = ap.parse_args()
    if a.cmd == "setup":
        setup()
    else:
        sys.exit(run(a))
