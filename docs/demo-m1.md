# M1 demo — a metrics-only diagnosis of a broken pod

This is the M1 milestone gate from [spec 001 §6](../specs/001-build-spec.md):
*"real diagnosis of a manually broken pod, metrics only."* It walks the whole
M1 path — break a pod, scrape it with Prometheus, hand the alert to
`bloodhound hunt`, read the report, price the case.

**Read this first.** Every command below was run; the outputs are pasted from
real terminals, not composed. But one step could not be completed on the
machine this was authored on, and it is the step that matters most:

| Step | Status |
|---|---|
| kind cluster, real pod in `CrashLoopBackOff` | ✅ executed |
| kube-state-metrics + Prometheus scraping it | ✅ executed |
| `mcp-prom` tools against that live Prometheus | ✅ executed |
| `bloodhound hunt --alert` reaching the model | ❌ **not executed — requires `ANTHROPIC_API_KEY`** |
| Full `hunt` → report → `cost`, model substituted by a scripted provider | ✅ executed ([artifact](demo-m1/case)) |

So: the plumbing is demonstrated end to end against real infrastructure, and
the *reasoning* is not. There is no LLM-authored report in this repository
yet, and nothing here pretends otherwise. §5 says exactly what is missing and
what to run once you have a key.

---

## 0. What you need

- Go 1.25+ and Docker. That is enough for §4, which is also what
  `make test-integration` runs in CI.
- `kind` and `kubectl` as well, for §1–§3.
- An `ANTHROPIC_API_KEY` for §5. Nothing else in this document needs one, and
  nothing else in this repository spends money.

```sh
make build                          # or:
go build -o bin/bloodhound ./cmd/bloodhound
go build -o bin/mcp-prom   ./mcp/prom
```

## 1. Break a pod

[`demo-m1/broken-workload.yaml`](demo-m1/broken-workload.yaml) deploys two
services into namespace `shop`: `checkout`, whose container exits immediately
on start (standing in for a bad build shipped under tag `v2.1.4`), and `cart`,
which is healthy. The healthy one is not decoration — an investigation that
can only see one workload has not discriminated between anything.

This is eval scenario
[S01](../evals/scenarios/S01-bad-image-tag/scenario.yaml) reduced to what a
metrics-only hound can observe.

```sh
kind create cluster --name bloodhound-demo --wait 120s
kubectl apply -f docs/demo-m1/broken-workload.yaml
```

After a couple of minutes the backoff is visible:

```
$ kubectl -n shop get pods
NAME                        READY   STATUS             RESTARTS       AGE
cart-65b6db79fd-p5r42       1/1     Running            0              5m10s
checkout-67c4dd6bc9-mx46b   0/1     CrashLoopBackOff   5 (2m3s ago)   5m10s

$ kubectl -n shop logs -l app=checkout --previous --tail=5
checkout v2.1.4 starting
panic: missing config key PAYMENTS_URL
```

## 2. Scrape it

`bloodhound` reads Kubernetes state through Prometheus, so the cluster needs
kube-state-metrics and a Prometheus scraping it. Both run as host-network
containers here rather than as in-cluster manifests: fewer moving parts, and
`mcp-prom` reaches Prometheus on loopback with no port-forward to babysit.

```sh
kind export kubeconfig --name bloodhound-demo --kubeconfig /tmp/bh-kubeconfig
chmod 644 /tmp/bh-kubeconfig      # the kube-state-metrics image runs as nobody

docker run -d --name ksm-demo --network host \
  -v /tmp/bh-kubeconfig:/kubeconfig:ro \
  registry.k8s.io/kube-state-metrics/kube-state-metrics:v2.15.0 \
  --kubeconfig=/kubeconfig --port=18080 --telemetry-port=18081

docker run -d --name prom-demo --network host \
  -v "$PWD/docs/demo-m1/prometheus.yml:/etc/prometheus/prometheus.yml:ro" \
  prom/prometheus:v3.5.0 \
  --config.file=/etc/prometheus/prometheus.yml \
  --web.listen-address=127.0.0.1:9090
```

Confirm the target is up and the crash loop is in the time series database:

```
$ curl -s 'http://127.0.0.1:9090/api/v1/query?query=up{job="kube-state-metrics"}' | jq -c .data.result
[{"metric":{"__name__":"up","instance":"127.0.0.1:18080","job":"kube-state-metrics"},"value":[1785283590.819,"1"]}]

$ curl -sG http://127.0.0.1:9090/api/v1/query \
    --data-urlencode 'query=max_over_time(kube_pod_container_status_waiting_reason{namespace="shop",reason="CrashLoopBackOff"}[10m])' \
    | jq -c '.data.result[].metric'
{"container":"checkout","instance":"127.0.0.1:18080","job":"kube-state-metrics",
 "namespace":"shop","pod":"checkout-67c4dd6bc9-mx46b","reason":"CrashLoopBackOff","uid":"6850a632-…"}
```

> **A real finding worth keeping.** The obvious query —
> `kube_pod_container_status_waiting_reason{reason="CrashLoopBackOff"} == 1` —
> returns **empty** most of the time. kube-state-metrics only emits that series
> while the container is actually sitting in the backoff wait; between restarts
> the container is `terminated`, the series vanishes, and Prometheus marks it
> stale. `max_over_time(...[10m])` is the query that works. This is precisely
> the kind of instantaneous-vs-windowed mistake the metrics-hound prompt tells
> the model to avoid, and it showed up on the first real cluster we pointed at.

## 3. What `mcp-prom` sees

`mcp-prom` is a standalone MCP server (see [its README](../mcp/prom/README.md)),
so it can be driven by any MCP client. Against the cluster above, real output:

**`series_metadata {"match": "{namespace=\"shop\"}"}`** — discovery, alphabetical,
with the cap marked rather than silently applied:

```json
{
  "metrics": [ { "name": "kube_configmap_created", "type": "gauge",
                 "help": "[STABLE] Unix creation timestamp",
                 "labels": { "namespace": ["shop"], "job": ["kube-state-metrics"] } }, "…" ],
  "truncation": {
    "metrics_returned": 25,
    "metrics_total": 58,
    "note": "33 metrics dropped; sorted alphabetically. Narrow the match selector."
  }
}
```

Long help strings come back truncated with a `…`, e.g. `"Maximum number of
replicas that can be scheduled above the desired number of replicas during a
rolling updat…"`.

**`query_instant`** on the windowed CrashLoopBackOff query returns the one
sample, with the labels Prometheus itself attached (`job`, `instance`):

```json
{ "result_type": "vector",
  "samples": [ { "labels": { "container": "checkout", "namespace": "shop",
                             "pod": "checkout-67c4dd6bc9-mx46b",
                             "reason": "CrashLoopBackOff", "job": "kube-state-metrics" },
                 "timestamp": 1785283728, "value": "1" } ],
  "truncation": { "samples_returned": 1, "samples_total": 1 } }
```

**The 24h range limit is an error, not a clamp** — a silently shrunk window
would make the model reason about a different span than it asked for:

```
### query_range {"query":"up","start":"2026-07-27T12:08:48Z","end":"2026-07-29T00:08:48Z"}
is_error=true
range 36h exceeds 24h limit; narrow start/end and retry
```

## 4. Run the hunt

Craft an Alertmanager-shaped alert —
[`demo-m1/alert.json`](demo-m1/alert.json), with the real pod name — and hand
it to the CLI:

```sh
./bin/bloodhound hunt \
  --alert docs/demo-m1/alert.json \
  --work work \
  --mcp-prom ./bin/mcp-prom \
  --prom-url http://127.0.0.1:9090
```

### 4a. Without a key (executed)

This is where the demo stops on a machine with no credentials. Intake
succeeds, the case directory and its first checkpoint are written, the MCP
session connects — and the first model call fails:

```
$ ./bin/bloodhound hunt --alert docs/demo-m1/alert.json --work work \
    --mcp-prom ./bin/mcp-prom --prom-url http://127.0.0.1:9090
bloodhound: case c-20260729T000905-9d5cf5: phase investigate: metrics-hound: model call:
  calling anthropic messages API: no Anthropic credentials found. …
    1. ANTHROPIC_API_KEY env var: not set
$ echo $?
1
```

The failure is recorded, not lost — which is the point of the checkpoint
design (spec 002 §4.1). The case is left resumable:

```
$ ./bin/bloodhound cost --work work c-20260729T000905-9d5cf5
case c-20260729T000905-9d5cf5 (KubePodCrashLooping) — phase failed
PHASE        STATUS     IN  OUT  USD     TOOLS  WALL
intake       completed  0   0    0.0000  0      0s
investigate  failed     0   0    0.0000  0      4ms
TOTAL                   0   0    0.0000  0      4ms
```

Once a key is available, `bloodhound hunt --resume c-20260729T000905-9d5cf5`
picks up at `investigate` without re-running intake.

### 4b. With the model scripted (executed)

To exercise the rest of the path without a key, the same run is available as
an integration test with a scripted provider standing in for Anthropic.
Everything else is real: the real CLI, the real orchestrator, the real
`mcp-prom` binary over stdio, and a real Prometheus container scraping a real
`/metrics` endpoint the test serves itself.

```sh
BLOODHOUND_DEMO_WORK=./work \
  go test -tags integration -run TestHuntAgainstRealPrometheus ./cmd/bloodhound/
```

The resulting case directory is committed at
[`docs/demo-m1/case/`](demo-m1/case) — see
[its README](demo-m1/case/README.md) for exactly which parts of it are real.
`report.txt`, verbatim:

```
bloodhound report — case c-20260729T000956-5afef8
==============================================================================
alert:   KubePodCrashLooping
firing:  2026-07-28T10:05:00Z
scope:   namespace=shop pod=checkout-7d9f container=checkout severity=critical
hound:   metrics

HYPOTHESIS  (confidence 0.78)
onset 2026-07-29T00:07:56Z
checkout-7d9f is in CrashLoopBackOff and its restart counter is still climbing
across the alert window; the container never reaches a ready state, which is
the signature of a container that fails immediately on start rather than one
degrading under load.

EVIDENCE
  #   TOOL           QUERY                          OBSERVED                     CAPTURE
  1   query_range    kube_pod_container_status_res… the restart counter rises m… mcp/001-query_range.json

DEAD ENDS
  - a traffic spike overloaded checkout
    ruled out by: no request-rate series exists in this namespace's metric
                  surface, so the theory cannot be supported from metrics
                  alone

------------------------------------------------------------------------------
spend: 7100 in + 620 out tokens · $0.0000 · 2 tool calls · 6ms
```

**The evidence citation resolves, and it resolves to real data.** The finding
cites `mcp/001-query_range.json`; that file holds an actual Prometheus
response, complete with the `job` and `instance` labels the server attaches
and the exposition never emits:

```json
{ "resolved_step_seconds": 1,
  "series": [ { "labels": { "__name__": "kube_pod_container_status_restarts_total",
                            "container": "checkout", "instance": "127.0.0.1:35647",
                            "job": "kube-state-metrics", "namespace": "shop",
                            "pod": "checkout-7d9f" },
                "stats": { "min": 1, "max": 37, "avg": 19, "last": 37 },
                "points": [[1785283787,"1"],[1785283788,"5"], "…", [1785283796,"37"]] } ],
  "truncation": { "series_total": 1, "series_returned": 1, "points_thinned": false } }
```

The model never wrote that `capture_ref`. It cited tool call **#2** by index
and the loop resolved the index to the filename (spec 002 §3.4) — an
unresolvable index is a validation error. That mechanism is what the M4
grader's evidence-honesty check will hang off, so it is mechanical from day
one.

```
$ ./bin/bloodhound cost --work work c-20260729T000956-5afef8
case c-20260729T000956-5afef8 (KubePodCrashLooping) — phase done
PHASE        STATUS     IN    OUT  USD     TOOLS  WALL
intake       completed  0     0    0.0000  0      0s
investigate  completed  7100  620  0.0000  2      6ms
report       completed  0     0    0.0000  0      17ms
TOTAL                   7100  620  0.0000  2      23ms
```

`USD` is `0.0000` because no paid call was made. The token counts are the
scripted provider's canned `Usage` values travelling through the real
accounting middleware — the mechanism is real, the numbers are not a
measurement of anything.

## 5. What is still unverified

Everything above is executed output. This is not:

- **A model-authored diagnosis.** No run in this repository has reached
  Anthropic. Whether metrics-hound actually *finds* the crash loop — picks
  useful queries, reads the counter correctly, resists the traffic-spike
  story — is untested against a real model.
- **Real spend and latency figures.** Nothing here measures what an
  investigation costs or how long it takes. Spec 001 §6 puts those numbers in
  M4, produced by the eval harness. Do not quote the table in §4b as a cost
  benchmark; it is $0 by construction.
- **Accuracy of any kind.** One hand-picked scenario is a demo, not a
  measurement. The 18-scenario grader is M4.

To close the first gap, run §1–§3, then:

```sh
export ANTHROPIC_API_KEY=sk-ant-…
./bin/bloodhound hunt \
  --alert docs/demo-m1/alert.json \
  --work work \
  --mcp-prom ./bin/mcp-prom \
  --prom-url http://127.0.0.1:9090
./bin/bloodhound cost --work work <case-id>
```

Budgets default to 12 tool calls / 50k tokens / 3m wall clock
(`--max-tool-calls`, `--max-tokens`, `--max-wall-clock`), so a single run is
bounded by construction. If it dies partway, `hunt --resume <case-id>`
continues from the last checkpoint and counts the spend already recorded
against the budget — a crash loop cannot multiply the bill.

## 6. Tear down

```sh
docker rm -f prom-demo ksm-demo
kind delete cluster --name bloodhound-demo
```

## Reproducing just the Prometheus half

Everything in §4b, minus the cluster, is a CI job:

```sh
make test-integration
```

That starts a Prometheus container scraping a test-owned `/metrics` endpoint at
a 250ms interval, waits for real samples to land, and drives the real
`mcp-prom` binary against it. No API key, no paid call. Without Docker the
tests skip with a message rather than failing, so `make check` still passes on
a laptop that has never installed it.
