# OpenObserve 1.0 on Kubernetes

Files for the Kubesimplify post *Kubernetes observability in 2026, with OpenObserve 1.0 as the backend*: Helm values, a small instrumented Go service, pipeline, SLO and alert payloads, and a curl MCP client.

```text
manifests/o2-values.yaml               OpenObserve standalone chart values (v1.0.0-rc1, Vortex for logs)
manifests/collector-values.yaml        openobserve-collector chart values
manifests/10-shop.yaml                 checkout app + load generator
manifests/20-ttv-inspect-job.yaml      run ttv-inspect against an index file
manifests/30-alert-sink.yaml           webhook echo server for alerts
manifests/*.json                       function, pipeline, SLO and alert payloads for the API
app/                                   the checkout service (Go, OpenTelemetry SDK)
mcp.sh                                 one JSON-RPC call to the MCP endpoint
backfill.py                            write logs into the previous hour so compaction runs soon
```

Needs: a cluster with about 8 GB free (kiac, kind or k3d), `kubectl`, `helm`, `jq`, `curl`, and `duckdb` for step 5.

## 1. OpenObserve

```bash
kiac create cluster --name o2 --workers 2 --memory 4G --cp-memory 4G
helm repo add openobserve https://charts.openobserve.ai
helm upgrade -i o2 openobserve/openobserve-standalone -n openobserve --create-namespace -f manifests/o2-values.yaml
kubectl -n openobserve get pods,svc

export O2=http://<EXTERNAL-IP>:5080              # from the Service above
export AUTH='root@example.com:Complexpass#123'    # chart default, change it
```

## 2. Collector

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml
kubectl -n cert-manager rollout status deploy/cert-manager-webhook --timeout=240s
kubectl apply -f https://github.com/open-telemetry/opentelemetry-operator/releases/latest/download/opentelemetry-operator.yaml
kubectl -n opentelemetry-operator-system rollout status deploy/opentelemetry-operator-controller-manager --timeout=240s
helm upgrade -i o2c openobserve/openobserve-collector -n openobserve-collector --create-namespace -f manifests/collector-values.yaml
curl -s -u $AUTH "$O2/api/default/streams?type=logs" | jq -r '.list[].name'
```

## 3. App

```bash
container build -t docker.io/library/checkout:demo app      # or docker build
kiac load image docker.io/library/checkout:demo --name o2   # or kind load docker-image ...
kubectl apply -f manifests/10-shop.yaml
kubectl -n shop logs deploy/checkout --tail=1
```

## 4. Pipeline

```bash
curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/default/functions" -d @manifests/function-parse-checkout-json.json
curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/default/pipelines" -d @manifests/pipeline-parse-shop-logs.json
```

## 5. Files

```bash
kubectl -n openobserve debug o2-openobserve-standalone-0 --image=busybox:1.36 \
  --target=openobserve-standalone --container=toolbox --profile=general -- sleep 86400
kubectl -n openobserve exec o2-openobserve-standalone-0 -c toolbox -- sh -c \
  'cd /proc/1/root/data/stream && find files/default -type f | sed "s/.*\.//" | sort | uniq -c'

F=$(kubectl -n openobserve exec o2-openobserve-standalone-0 -c toolbox -- sh -c 'cd /proc/1/root/data/stream && find files/default/logs -name "*.vortex" | head -1')
kubectl -n openobserve exec o2-openobserve-standalone-0 -c toolbox -- cat "/proc/1/root/data/stream/$F" > sample.vortex
head -c 4 sample.vortex | xxd
duckdb -c "INSTALL vortex; LOAD vortex; SELECT count(*) FROM read_vortex('sample.vortex');"

# edit the .ttv path in the manifest first
kubectl apply -f manifests/20-ttv-inspect-job.yaml && kubectl -n openobserve logs -f job/ttv-inspect
```

`python3 backfill.py $O2 checkout_archive` writes 40,000 rows into the previous UTC hour if you do not want to wait for compaction.

## 6. MCP

```bash
./mcp.sh tools/list | jq -r '.result.tools[].name'
./mcp.sh tools/call '{"name":"tool_search","arguments":{"query":"list traces with errors","limit":3}}' | jq -r '.result.content[0].text | fromjson | .tools[].name'
```

## 7. SLO and alerts

```bash
kubectl apply -f manifests/30-alert-sink.yaml
curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/default/slos" -d @manifests/slo-checkout-availability.json
SLO=$(curl -s -u $AUTH "$O2/api/default/slos" | jq -r '.list[0].id')
jq '.template'    manifests/alert-burn-rate.json | curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/default/alerts/templates" -d @-
jq '.destination' manifests/alert-burn-rate.json | curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/default/alerts/destinations" -d @-
jq --arg id "$SLO" '.alert | .query_condition.slo_condition.slo_id = $id' manifests/alert-burn-rate.json \
  | curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/v2/default/alerts" -d @-
curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/v2/default/alerts" -d @manifests/alert-error-spans.json

kubectl -n shop exec deploy/loadgen -- curl -s "http://checkout.shop.svc/chaos?rate=60"   # break
kubectl -n shop logs deploy/alert-sink -f | jq -R -c 'fromjson? | select(.path=="/alerts") | .body | fromjson'
kubectl -n shop exec deploy/loadgen -- curl -s "http://checkout.shop.svc/chaos?rate=2"    # heal
```

On v1.0.0-rc1 in local mode (SQLite) the SLO status never updates, so SLO-backed alerts stay "frozen (unobserved)": the SLO ingest pass writes through the read-only database client. Plain alerts work. Reported upstream.

The values file sets `ZO_SKIP_SSRF_CHECKS=true` so the in-cluster webhook is allowed. Do not do that on anything internet-facing.

## Teardown

```bash
kiac delete cluster --name o2
```

MIT
