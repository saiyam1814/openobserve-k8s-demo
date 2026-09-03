#!/usr/bin/env python3
"""Backfill synthetic checkout logs into the PREVIOUS UTC hour so the compactor
picks the hour up within minutes (it only compacts closed hours). Demo helper."""
import json, random, sys, time, urllib.request, base64
from datetime import datetime, timedelta, timezone

O2 = sys.argv[1] if len(sys.argv) > 1 else "http://192.168.64.10:5080"
STREAM = sys.argv[2] if len(sys.argv) > 2 else "checkout_archive"
BATCHES, PER = 40, 1000
auth = base64.b64encode(b"root@example.com:Complexpass#123").decode()
hour = (datetime.now(timezone.utc) - timedelta(hours=1)).replace(minute=0, second=0, microsecond=0)
levels = ["info"] * 90 + ["warn"] * 7 + ["error"] * 3
msgs = {"info": ["checkout complete", "payment captured", "inventory reserved", "checkout started"],
        "warn": ["retrying payment gateway", "slow inventory lookup"],
        "error": ["payment failed", "inventory service unavailable"]}
pods = [f"checkout-58d566cd9-{s}" for s in ("4tls8", "wjz4g", "c6fc5", "vvmbv")]
nodes = ["kiac-o2-worker-1", "kiac-o2-worker-2"]
total = 0
for b in range(BATCHES):
    recs = []
    for i in range(PER):
        ts = hour + timedelta(seconds=random.uniform(0, 3540))
        lvl = random.choice(levels)
        recs.append({
            "_timestamp": int(ts.timestamp() * 1_000_000),
            "level": lvl, "msg": random.choice(msgs[lvl]), "service": "checkout", "version": "1.0.0",
            "order_id": f"ord-{random.randint(0, 999999):06d}", "amount": round(random.uniform(5, 205), 2),
            "sku": random.choice(["kube-tshirt", "otel-sticker", "gpu-mug", "rust-hoodie"]),
            "trace_id": "%032x" % random.getrandbits(128), "span_id": "%016x" % random.getrandbits(64),
            "kubernetes_namespace_name": "shop", "kubernetes_pod_name": random.choice(pods),
            "kubernetes_container_name": "checkout", "kubernetes_host": random.choice(nodes),
            "k8s_cluster_name": "kiac-o2",
        })
    req = urllib.request.Request(f"{O2}/api/default/{STREAM}/_json", data=json.dumps(recs).encode(),
                                 headers={"Authorization": "Basic " + auth, "Content-Type": "application/json"})
    r = json.load(urllib.request.urlopen(req, timeout=60))
    total += r["status"][0]["successful"]
    if b % 10 == 9:
        print(f"batch {b+1}/{BATCHES}: {total} records into {STREAM} for hour {hour:%Y-%m-%d %H:00} UTC", flush=True)
print("done", total)
