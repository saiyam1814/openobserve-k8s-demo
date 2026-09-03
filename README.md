# OpenObserve 1.0 on a Kubernetes cluster: the demo

Companion repo for the Kubesimplify post **Kubernetes observability in 2026, with OpenObserve 1.0 as the backend**. Everything the post runs is here: the Helm values, the collector values, a small instrumented Go service, the pipeline, SLO and alert payloads, and a curl-based MCP client.

Tested on 2 September 2026 with OpenObserve v1.0.0-rc1, kiac v0.5.1 (Kubernetes v1.36.1), Helm 4.1, DuckDB 1.4. kind or k3d work the same, only the image load command differs.

## What is in here

```text
app/                 checkout: a Go HTTP service with OpenTelemetry traces and JSON logs carrying trace ids
manifests/
  o2-values.yaml                       OpenObserve standalone chart values (1.0.0-rc1, Vortex for logs)
  collector-values.yaml                openobserve-collector chart values (agent + gateway via the OTel operator)
  10-shop.yaml                         the checkout app, a load generator, and its OTLP auth secret
  20-ttv-inspect-job.yaml              runs OpenObserve's ttv-inspect against an index file on the data volume
  30-alert-sink.yaml                   an HTTP echo server that receives alert webhooks
  function-parse-checkout-json.json    VRL function: parse the JSON log body into fields
  pipeline-parse-shop-logs.json        realtime pipeline: default -> function -> default
  slo-checkout-availability.json       SLO: POST /checkout spans not in ERROR, 99% over 7 days
  alert-burn-rate.json                 template + destination + burn-rate alert on the SLO
  alert-error-spans.json               a plain scheduled alert on failed spans
mcp.sh               one JSON-RPC call to the MCP endpoint, via curl
backfill.py          writes synthetic logs into the previous UTC hour so compaction has a closed hour to work on
```

## Prerequisites

- A Kubernetes cluster with about 8 GB free. On an Apple silicon Mac: `brew install saiyam1814/tap/kiac` and `container system start`.
- `kubectl`, `helm`, `jq`, `curl`
- `duckdb` 1.4.2 or newer for the file-reading step (`brew install duckdb`)
- Go 1.26 only if you want to change the demo app

## 1. Cluster and OpenObserve

```bash
kiac create cluster --name o2 --workers 2 --memory 4G --cp-memory 4G
helm repo add openobserve https://charts.openobserve.ai
helm upgrade -i o2 openobserve/openobserve-standalone -n openobserve --create-namespace -f manifests/o2-values.yaml
kubectl -n openobserve get pods,svc
```

Set two variables for the rest of the steps. The IP is the EXTERNAL-IP of the `o2-openobserve-standalone` Service. The credentials are the chart's default root user; change them after the demo.

```bash
export O2=http://<EXTERNAL-IP>:5080
export AUTH='root@example.com:Complexpass#123'
```

`manifests/o2-values.yaml` pins the image, asks for a LoadBalancer Service and sets three things worth knowing:

- `ZO_FILE_FORMAT: parquet,logs=vortex` writes log streams as Vortex files.
- `ZO_MAX_FILE_RETENTION_TIME: 60` rotates the WAL every 60 seconds instead of 600, so files appear fast. Demo pacing, not a production value.
- `ZO_COMPACT_DELETE_FILES_DELAY_MINUTES: 10` deletes compacted-away files after 10 minutes instead of 120. Same.

It also sets `ZO_SKIP_SSRF_CHECKS=true` so an in-cluster webhook destination is allowed. Do not do that on an internet-facing instance.

## 2. Collect everything the cluster emits

```bash
kubectl apply -f https://github.com/cert-manager/cert-manager/releases/download/v1.19.1/cert-manager.yaml
kubectl -n cert-manager rollout status deploy/cert-manager-webhook --timeout=240s
kubectl apply -f https://github.com/open-telemetry/opentelemetry-operator/releases/latest/download/opentelemetry-operator.yaml
kubectl -n opentelemetry-operator-system rollout status deploy/opentelemetry-operator-controller-manager --timeout=240s
helm upgrade -i o2c openobserve/openobserve-collector -n openobserve-collector --create-namespace -f manifests/collector-values.yaml

curl -s -u $AUTH "$O2/api/default/streams?type=logs"    | jq -r '.list[].name'
curl -s -u $AUTH "$O2/api/default/streams?type=metrics" | jq '.list | length'
```

## 3. The demo app

```bash
container build -t docker.io/library/checkout:demo app     # or: docker build -t docker.io/library/checkout:demo app
kiac load image docker.io/library/checkout:demo --name o2  # or: kind load docker-image docker.io/library/checkout:demo
kubectl apply -f manifests/10-shop.yaml
kubectl -n shop logs deploy/checkout --tail=1
```

The fully qualified image name matters: the kubelet normalises `checkout:demo` to `docker.io/library/checkout:demo`, and a bare `checkout:demo` in the node's image store will not match.

## 4. Pipeline: parse the JSON log body into fields

```bash
curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/default/functions" -d @manifests/function-parse-checkout-json.json
curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/default/pipelines" -d @manifests/pipeline-parse-shop-logs.json
```

## 5. Look at the files

```bash
kubectl -n openobserve debug o2-openobserve-standalone-0 --image=busybox:1.36 \
  --target=openobserve-standalone --container=toolbox --profile=general -- sleep 86400
kubectl -n openobserve exec o2-openobserve-standalone-0 -c toolbox -- sh -c \
  'cd /proc/1/root/data/stream && find files/default -type f | sed "s/.*\.//" | sort | uniq -c'

# copy a file out and check its magic bytes
F=$(kubectl -n openobserve exec o2-openobserve-standalone-0 -c toolbox -- sh -c 'cd /proc/1/root/data/stream && find files/default/logs -name "*.vortex" | head -1')
kubectl -n openobserve exec o2-openobserve-standalone-0 -c toolbox -- cat "/proc/1/root/data/stream/$F" > sample.vortex
head -c 4 sample.vortex | xxd

# inspect an index file: edit the path in the manifest to an existing .ttv first
kubectl apply -f manifests/20-ttv-inspect-job.yaml
kubectl -n openobserve wait --for=condition=complete job/ttv-inspect --timeout=180s
kubectl -n openobserve logs job/ttv-inspect

# read the Vortex file with DuckDB, with OpenObserve out of the loop
duckdb -c "INSTALL vortex; LOAD vortex; SELECT count(*) FROM read_vortex('sample.vortex');"
```

Optional: `python3 backfill.py $O2 checkout_archive` writes 40,000 rows into the previous UTC hour so the compactor has a closed hour to process within minutes.

## 6. MCP over curl

```bash
./mcp.sh tools/list | jq -r '.result.tools[].name'
./mcp.sh tools/call '{"name":"tool_search","arguments":{"query":"list traces with errors","limit":3}}' | jq -r '.result.content[0].text | fromjson | .tools[].name'
```

To use it from Claude Code, the setup page under IAM in the UI prints the `claude mcp add` command with your credentials. Prefer a read-only credential.

## 7. SLO and alerts

```bash
kubectl apply -f manifests/30-alert-sink.yaml
curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/default/slos" -d @manifests/slo-checkout-availability.json

# template, destination, alert: the three objects in alert-burn-rate.json, POSTed in that order
jq '.template'    manifests/alert-burn-rate.json | curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/default/alerts/templates" -d @-
jq '.destination' manifests/alert-burn-rate.json | curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/default/alerts/destinations" -d @-
jq --arg id "$(curl -s -u $AUTH "$O2/api/default/slos" | jq -r '.list[0].id')" '.alert | .query_condition.slo_condition.slo_id = $id' manifests/alert-burn-rate.json \
  | curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/v2/default/alerts" -d @-
curl -s -u $AUTH -H 'Content-Type: application/json' -X POST "$O2/api/v2/default/alerts" -d @manifests/alert-error-spans.json

# break the app, then watch the sink
kubectl -n shop exec deploy/loadgen -- curl -s "http://checkout.shop.svc/chaos?rate=60"
kubectl -n shop logs deploy/alert-sink -f | jq -R -c 'fromjson? | select(.path=="/alerts") | .body | fromjson'

# heal
kubectl -n shop exec deploy/loadgen -- curl -s "http://checkout.shop.svc/chaos?rate=2"
```

**Known issue on v1.0.0-rc1 in local mode (SQLite):** the SLO status never updates because the SLO ingest pass writes through the read-only database client (`attempt to write a readonly database`), so SLO-backed alerts stay "frozen (unobserved)". The SLO page itself is correct. Plain scheduled alerts are unaffected. Reported upstream.

## Teardown

```bash
kiac delete cluster --name o2
```

## License

MIT
