#!/usr/bin/env python3
"""Idempotently wire the load-test routes into a running relay via its control
API: a provider, three noAuth hosts (fast/slow/stream mocks), a model per host,
bindings, one policy granting all three, and a relay key. Writes the key to
results/key.txt for the harness. Safe to re-run (GET-or-create)."""
import json, os, sys, urllib.request, urllib.error

CTRL = os.environ.get("LT_CTRL", "http://localhost:8081")
TOK = os.environ["RELAY_ADMIN_TOKEN"]
HOSTS = [  # (name, mock service dns, model)
    ("mock-fast", "mock-fast:9990", "fast"),
    ("mock-slow", "mock-slow:9990", "slow"),
    ("mock-stream", "mock-stream:9990", "stream"),
    ("recorded", "recorded:4010", "recorded"),
]


def call(method, path, body=None):
    data = json.dumps(body).encode() if body is not None else None
    req = urllib.request.Request(CTRL + path, data=data, method=method,
        headers={"Authorization": "Bearer " + TOK, "Content-Type": "application/json"})
    try:
        r = urllib.request.urlopen(req, timeout=15)
        return r.status, json.loads(r.read() or "{}")
    except urllib.error.HTTPError as e:
        return e.code, e.read().decode()[:300]


def get_or_create(plural, name, body, label):
    st, res = call("GET", f"/{plural}/{name}")
    if st < 300 and isinstance(res, dict) and res.get("metadata", {}).get("id"):
        print(f"  {label}: exists {res['metadata']['id']}")
        return res
    st, res = call("POST", f"/{plural}", body)
    if st >= 300:
        print(f"  {label}: HTTP {st} -> {res}")
        return {}
    print(f"  {label}: created {res.get('metadata', {}).get('id')}")
    return res


prov = get_or_create("providers", "loadtest",
    {"metadata": {"name": "loadtest", "displayName": "Loadtest"}, "spec": {}}, "provider")
pid = prov.get("metadata", {}).get("id")
if not pid:
    sys.exit("FATAL: provider not created")

for hname, dns, model in HOSTS:
    h = get_or_create("hosts", hname,
        {"metadata": {"name": hname, "displayName": hname, "owner": {"kind": "user"}},
         "spec": {"baseURL": f"http://{dns}", "noAuth": True}}, f"host {hname}")
    hid = h.get("metadata", {}).get("id")
    m = get_or_create("models", model,
        {"metadata": {"name": model, "displayName": model, "owner": {"kind": "provider", "id": pid}},
         "spec": {"snapshots": [{"name": model, "originalName": model}], "pointer": model,
                  "capabilities": {"chat": True}}}, f"model {model}")
    mid = m.get("metadata", {}).get("id")
    get_or_create("host-bindings", f"{model}-on-{hname}",
        {"metadata": {"name": f"{model}-on-{hname}", "displayName": model, "owner": {"kind": "user"}},
         "spec": {"modelId": mid, "hostId": hid, "adapter": "openai"}}, f"binding {model}")

# policy granting all three mock hosts, then reload so the grant resolves
call("POST", "/reload")
grants = ["@" + h[0] for h in HOSTS]
pol = get_or_create("policies", "loadtest",
    {"metadata": {"name": "loadtest", "displayName": "Loadtest", "owner": {"kind": "user"}},
     "spec": {"models": grants, "enabled": True}}, "policy")
ppid = pol.get("metadata", {}).get("id")
# ensure grants are set (in case policy pre-existed)
st, cur = call("GET", "/policies/loadtest")
if st < 300:
    cur.pop("$schema", None)
    cur["spec"]["models"] = grants
    call("PUT", f"/policies/by-id/{ppid}", cur)

st, key = call("POST", "/relay-keys",
    {"metadata": {"name": "loadtest-key", "displayName": "loadtest-key"}, "spec": {"policyId": ppid}})
plain = key.get("plaintext") if isinstance(key, dict) else None
if not plain:
    sys.exit(f"FATAL: relay-key not created: {key}")
call("POST", "/reload")

os.makedirs("results", exist_ok=True)
with open("results/key.txt", "w") as f:
    f.write(plain)
print(f"OK: routes wired (fast/slow/stream), key -> results/key.txt")
