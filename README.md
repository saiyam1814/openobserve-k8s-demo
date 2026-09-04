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

# copy one file of each type out of the pod (they are binary columnar files) and check the 4-byte signatures
for ext in parquet ttv vortex; do
  F=$(kubectl -n openobserve exec o2-openobserve-standalone-0 -c toolbox -- sh -c \
    "cd /proc/1/root/data/stream && find files/default -name '*.$ext' 2>/dev/null | head -1")
  kubectl -n openobserve exec o2-openobserve-standalone-0 -c toolbox -- cat "/proc/1/root/data/stream/$F" > sample.$ext
done
for f in sample.parquet sample.ttv sample.vortex; do printf '%-16s ' "$f"; head -c 4 "$f" | xxd | cut -c10-; done
duckdb -c "INSTALL vortex; LOAD vortex; SELECT count(*) FROM read_vortex('sample.vortex');"

# inspect an index file with OpenObserve's own ttv-inspect: the Job needs the node that holds the volume and a file path
NODE=$(kubectl -n openobserve get pod o2-openobserve-standalone-0 -o jsonpath='{.spec.nodeName}')
TTV=$(kubectl -n openobserve exec o2-openobserve-standalone-0 -c toolbox -- sh -c \
  "cd /proc/1/root/data/stream && find files/default/index/default_logs -name '*.ttv' 2>/dev/null | head -1")
kubectl -n openobserve delete job ttv-inspect --ignore-not-found
sed -e "s#NODE_NAME#$NODE#" -e "s#TTV_PATH#/data/stream/$TTV#" manifests/20-ttv-inspect-job.yaml | kubectl apply -f -
kubectl -n openobserve wait --for=condition=complete job/ttv-inspect --timeout=180s
kubectl -n openobserve logs job/ttv-inspect
```

`python3 backfill.py $O2 checkout_archive` writes 40,000 rows into the previous UTC hour if you do not want to wait for compaction.

## 6. MCP

`mcp.sh` reads `O2` and `AUTH` from step 1, so export them again if this is a new shell.

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

On v1.0.0-rc1 in local mode (SQLite) the SLO status never updates, so SLO-backed alerts stay "frozen (unobserved)": the SLO ingest pass writes through the read-only database client. Plain alerts work. Upstream issue: https://github.com/openobserve/openobserve/issues/14189 (also present in v1.0.0-rc2).

The values file sets `ZO_SKIP_SSRF_CHECKS=true` so the in-cluster webhook is allowed. Do not do that on anything internet-facing.

## 8. Compaction, size on disk, query timing

```bash
# open hour vs closed hour on disk
kubectl -n openobserve exec o2-openobserve-standalone-0 -c toolbox -- sh -c '
  cd /proc/1/root/data/stream
  PREV=$(date -u -d @$(( $(date +%s) - 3600 )) +%Y/%m/%d/%H); CUR=$(date -u +%Y/%m/%d/%H)
  echo "closed hour $PREV (KB, file)"
  du -ak files/default/logs/default/$PREV files/default/index/default_logs/$PREV files/default/bloom/default_logs/$PREV | grep "\."
  echo "current hour $CUR: $(ls files/default/logs/default/$CUR | wc -l) files"'

# bytes in, bytes on disk, index size, per signal
for t in logs metrics traces; do
  curl -s -u $AUTH "$O2/api/default/streams?type=$t" | jq -r --arg t $t \
    '[.list[].stats] | "\($t): \(length) streams, \(map(.doc_num)|add) rows, \(map(.storage_size)|add|round) MB in, \(map(.compressed_size)|add|round) MB on disk, \(map(.index_size)|add|round) MB index"'
done

# three queries over the last 12 hours of logs; took is ms, scan_size is MB
NOW=$(date +%s); FROM=$((NOW-43200))
q() { curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/default/_search?type=logs" \
  -d "{\"query\":{\"sql\":\"$1\",\"start_time\":${FROM}000000,\"end_time\":${NOW}000000,\"size\":5}}" \
  | jq -c '{took, total, scan_records, scan_size, idx_scan_size}'; }
q "SELECT count(*) AS rows FROM \\\"default\\\""
q "SELECT k8s_namespace_name, count(*) AS rows FROM \\\"default\\\" GROUP BY k8s_namespace_name"
q "SELECT _timestamp, k8s_namespace_name, body FROM \\\"default\\\" WHERE match_all('readonly database')"
```

## Teardown

```bash
kiac delete cluster --name o2
```

MIT
