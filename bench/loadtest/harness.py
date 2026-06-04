#!/usr/bin/env python3
"""Run the load-test matrix against the running stack and emit a report.

For each scenario it drives the loadgen container (constant arrival rate, off
the relay hosts) at the nginx LB, while sampling Prometheus for server-side
truth — relay's own overhead histogram, CPU, RSS, goroutines across both pods.
Client percentiles + server resources are merged into report-<ts>.md + JSON."""
import json, os, subprocess, sys, time, urllib.request

HERE = os.path.dirname(os.path.abspath(__file__))
PROM = os.environ.get("LT_PROM", "http://localhost:9099")
LB = os.environ.get("LT_LB_URL", "http://nginx:80/openai/v1/chat/completions")
COMPOSE = ["docker", "compose", "-f", os.path.join(HERE, "compose.yml")]


def prom(q):
    try:
        u = PROM + "/api/v1/query?query=" + urllib.request.quote(q)
        r = json.loads(urllib.request.urlopen(u, timeout=5).read())
        res = r["data"]["result"]
        return float(res[0]["value"][1]) if res else 0.0
    except Exception:
        return 0.0


def run_scenario(sc, key):
    name, model, rate, dur = sc["name"], sc["model"], sc["rate"], sc["duration"]
    print(f"\n▸ {name}: model={model} rate={rate}/s dur={dur}")
    cpu0 = prom("sum(process_cpu_seconds_total{job='relay'})")
    oc0 = prom("sum(relay_overhead_seconds_count)")
    os0 = prom("sum(relay_overhead_seconds_sum)")
    t0 = time.time()
    # loadgen prints its JSON result to stdout (human summary goes to stderr),
    # so we capture stdout directly — no mounted file, no container-user perms.
    p = subprocess.Popen(COMPOSE + ["run", "--rm", "-T", "loadgen",
        "-url", LB, "-key", key, "-model", model, "-rate", str(rate),
        "-duration", dur, "-name", name],
        stdout=subprocess.PIPE, stderr=subprocess.PIPE, text=True)
    maxg = maxrss = 0.0
    while p.poll() is None:
        maxg = max(maxg, prom("sum(go_goroutines{job='relay'})"))
        maxrss = max(maxrss, prom("sum(process_resident_memory_bytes{job='relay'})"))
        time.sleep(1)
    out, err = p.communicate()
    wall = time.time() - t0
    cpu1 = prom("sum(process_cpu_seconds_total{job='relay'})")
    oc1 = prom("sum(relay_overhead_seconds_count)")
    os1 = prom("sum(relay_overhead_seconds_sum)")

    client = {}
    i = out.find("{")
    if i >= 0:
        try:
            client = json.loads(out[i:])
        except Exception:
            pass
    reqs = oc1 - oc0
    return {
        "name": name, "model": model, "targetRps": rate,
        "client": client,
        "server": {
            "requests": int(reqs),
            "cpuCores": round((cpu1 - cpu0) / wall, 2) if wall else 0,
            "cpuPerReqUs": round((cpu1 - cpu0) / reqs * 1e6, 1) if reqs else 0,
            "overheadMeanMs": round((os1 - os0) / reqs * 1e3, 3) if reqs else 0,
            "peakGoroutines": int(maxg),
            "peakRssMiB": round(maxrss / 1e6, 1),
        },
        "stderr": err.strip().splitlines()[-1] if err.strip() else "",
    }


def report(rows):
    ts = subprocess.run(["date", "-u", "+%Y%m%dT%H%M%SZ"], capture_output=True, text=True).stdout.strip()
    md = [f"# Relay load-test report — {ts}",
          f"\nImage: `{os.environ.get('RELAY_IMAGE','?')}`  ·  topology: 2 relay pods + nginx LB + valkey + postgres\n",
          "## Client-side (vegeta, constant arrival rate)\n",
          "| scenario | model | target | achieved | success | p50 | p99 | p999 | max |",
          "|---|---|---|---|---|---|---|---|---|"]
    for r in rows:
        c = r["client"]
        md.append(f"| {r['name']} | {r['model']} | {r['targetRps']}/s | {c.get('achievedRps',0):.0f}/s | "
                  f"{c.get('success',0):.1f}% | {c.get('p50ms',0):.2f}ms | {c.get('p99ms',0):.2f}ms | "
                  f"{c.get('p999ms',0):.2f}ms | {c.get('maxMs',0):.2f}ms |")
    md += ["\n## Server-side (Prometheus, both pods)\n",
           "| scenario | reqs | overhead mean | CPU cores | CPU/req | peak goroutines | peak RSS |",
           "|---|---|---|---|---|---|---|"]
    for r in rows:
        s = r["server"]
        md.append(f"| {r['name']} | {s['requests']} | {s['overheadMeanMs']}ms | {s['cpuCores']} | "
                  f"{s['cpuPerReqUs']}µs | {s['peakGoroutines']} | {s['peakRssMiB']}MiB |")
    out = os.path.join(HERE, "results", f"report-{ts}.md")
    open(out, "w").write("\n".join(md) + "\n")
    json.dump(rows, open(os.path.join(HERE, "results", f"report-{ts}.json"), "w"), indent=2)
    print("\n".join(md))
    print(f"\n▸ written: {out}")


def main():
    key = open(os.path.join(HERE, "results", "key.txt")).read().strip()
    matrix = json.load(open(os.path.join(HERE, "matrix.json")))
    if os.environ.get("LT_SOAK") == "1":
        matrix = [m for m in matrix if m.get("soak")] or matrix
    else:
        matrix = [m for m in matrix if not m.get("soak")]
    rows = [run_scenario(sc, key) for sc in matrix]
    report(rows)


if __name__ == "__main__":
    main()
