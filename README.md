# k8s-autoscale-demo

A minimal, self-contained demonstration of auto-scalable Kubernetes setup.  
Three services — one generates load, two scale under it.

```
[ Load Generator ]  →  [ API Service ]        →  [ Compute Service ]
  Go, 1 pod (fixed)     Next.js, HPA              Go, HPA
  no HPA                min 2 / max 5             min 2 / max 5
                        pod-1  pod-2  …            pod-1  pod-2  …
```

Runs entirely on **localhost via minikube** — no cloud account, no image registry needed.

---

## Goals

This project demonstrates exactly three operational concerns in a real Kubernetes cluster:

1. **Health** — know if a service is alive and ready to serve traffic
2. **Logs** — structured access + application logs for all services, visible via `kubectl logs`
3. **Auto-scaling** — API and Compute pods scale out under load, scale back down when idle

The Load Generator makes the demo self-contained — no external tools (`hey`, `k6`, `curl` loops) needed.  
Watch everything work by tailing a single log stream.

---

## Repository Layout

```
k8s-autoscale-demo/
│
├── api/                          # Next.js API service
│   ├── src/app/api/
│   │   ├── health/route.ts       # GET  /api/health
│   │   └── compute/route.ts      # POST /api/compute  →  calls compute svc
│   ├── Dockerfile
│   └── package.json
│
├── compute/                      # Go compute service
│   ├── main.go                   # GET /health   POST /compute
│   ├── Dockerfile
│   └── go.mod
│
├── loadgen/                      # Go load generator
│   ├── main.go                   # Sends traffic, logs stats every second
│   ├── Dockerfile
│   └── go.mod
│
├── k8s/
│   ├── namespace.yaml
│   ├── api/
│   │   ├── deployment.yaml       # replicas: 2
│   │   ├── service.yaml          # NodePort (minikube-friendly)
│   │   └── hpa.yaml              # min 2 / max 5
│   ├── compute/
│   │   ├── deployment.yaml       # replicas: 2
│   │   ├── service.yaml          # ClusterIP (internal only)
│   │   └── hpa.yaml              # min 2 / max 5
│   └── loadgen/
│       ├── deployment.yaml       # replicas: 1, no HPA
│       └── configmap.yaml        # TARGET_RPS, WORKERS, PAYLOAD_N
│
└── docs/
    ├── howto.md                  # Build → deploy, step by step
    └── monitoring.md             # Reading logs, watching HPA react
```

---

## Services At a Glance

### Load Generator (Go) — no external port

Single fixed pod. Sends a configurable stream of `POST /api/compute` requests and logs
one stats line per second to stdout. Also polls the k8s API for pod count changes and
logs a scale event line when it notices API or Compute scaling up or down.

```json
{"time":"…","level":"info","msg":"starting","target":"http://api-service.demo:3000","rps":10,"workers":5}
{"time":"…","level":"info","msg":"stats","sent":10,"ok":10,"errors":0,"p50_ms":42,"p99_ms":198}
{"time":"…","level":"info","msg":"stats","sent":10,"ok":9,"errors":1,"p50_ms":45,"p99_ms":312}
{"time":"…","level":"info","msg":"scale_event","service":"api","pods":3,"prev":2}
{"time":"…","level":"info","msg":"scale_event","service":"compute","pods":3,"prev":2}
```

Config (via ConfigMap → env vars):

| Var | Default | Meaning |
|---|---|---|
| `TARGET_URL` | `http://api-service.demo:3000` | API service address |
| `TARGET_RPS` | `10` | Requests per second |
| `WORKERS` | `5` | Concurrent goroutines |
| `PAYLOAD_N` | `40` | Fibonacci n sent in each request body |

---

### API Service (Next.js) — port 3000

| Endpoint | Method | Purpose |
|---|---|---|
| `/api/health` | GET | Liveness + readiness probe → `{"status":"ok"}` |
| `/api/compute` | POST | Accepts `{"n":N}`, forwards to compute service, returns result |

Logs: one structured JSON line per request (method, path, status, duration_ms) + errors.

---

### Compute Service (Go) — port 8080

| Endpoint | Method | Purpose |
|---|---|---|
| `/health` | GET | Liveness + readiness probe → `{"status":"ok"}` |
| `/compute` | POST | Accepts `{"n":N}`, runs fibonacci(N), returns `{"result":R,"duration_ms":D}` |

Logs: one structured JSON line per request + errors.

> **Why fibonacci?** It's CPU-bound, deterministic, and the load can be tuned by changing `PAYLOAD_N`.
> This is what drives the HPA to scale Compute pods up.

---

## Kubernetes Design

### Namespace
All resources live in: `demo`

### Pod counts

| Service | Starting pods | Min (HPA) | Max (HPA) |
|---|---|---|---|
| api | 2 | 2 | 5 |
| compute | 2 | 2 | 5 |
| loadgen | 1 | — (no HPA) | — |

### Resource limits (POC sizing)

These are intentionally small — this is vanilla Go + vanilla Next.js doing trivial work.
Low CPU requests also make HPA trigger faster and more visibly during the demo.

| Pod | CPU request | CPU limit | Memory request | Memory limit |
|---|---|---|---|---|
| compute | 50m | 200m | 32Mi | 64Mi |
| api | 50m | 200m | 64Mi | 128Mi |
| loadgen | 25m | 100m | 32Mi | 64Mi |

**Total cluster footprint (5 pods): ~275m CPU / ~288Mi RAM**

> `m` = millicores. `50m` = 5% of one CPU core.  
> HPA fires at 50% of the CPU **request** — so at `50m` request, it triggers when a pod
> sustains ~25m CPU. Fibonacci(40) will easily cross that threshold, making scale
> events reliably visible.

### HPA config (both api + compute)

```yaml
minReplicas: 2      # floor — HPA will never scale below this
maxReplicas: 5
metrics:
  - type: Resource
    resource:
      name: cpu
      target:
        type: Utilization
        averageUtilization: 50   # % of CPU request, not limit
```

> `minReplicas: 2` must match the Deployment `replicas: 2`. If `minReplicas` were 1,
> the HPA would scale back down to 1 pod at idle, defeating the 2-pod floor.

### Service exposure (minikube)

| Service | Type | Access |
|---|---|---|
| api-service | NodePort | `minikube service api-service -n demo` → gives local URL |
| compute-service | ClusterIP | Internal only — API pods reach it by DNS name |
| loadgen | none | Initiates traffic only, nothing calls into it |

> LoadBalancer type stays `<pending>` on minikube forever — NodePort is the correct
> choice for local clusters.

### Health probes (api + compute)

```yaml
livenessProbe:
  httpGet:
    path: /health        # /api/health for api service
    port: 8080           # 3000 for api service
  initialDelaySeconds: 5
  periodSeconds: 10

readinessProbe:
  httpGet:
    path: /health
    port: 8080
  initialDelaySeconds: 3
  periodSeconds: 5
```

---

## Local Setup (minikube)

### Prerequisites

| Tool | Install |
|---|---|
| Docker | https://docs.docker.com/get-docker/ |
| minikube | `brew install minikube` (mac) |
| kubectl | `brew install kubectl` (mac) |

### One-time cluster setup

```bash
# POC sizing — 2 CPUs and 2GB RAM is plenty for 5 small pods
minikube start --cpus=2 --memory=2g

# Enable metrics-server — required for HPA to work
minikube addons enable metrics-server

# Point your local Docker CLI at minikube's Docker daemon
# Run this in every terminal session used for building
eval $(minikube docker-env)
```

> `eval $(minikube docker-env)` is the key step. Images built after this command
> are immediately available inside the cluster — no push to any registry needed.
> Manifests use `imagePullPolicy: Never` to match.

---

## Quick Start

```bash
# 1. Clone
git clone https://github.com/you/k8s-autoscale-demo
cd k8s-autoscale-demo

# 2. Point Docker at minikube (if not already done)
eval $(minikube docker-env)

# 3. Build all three images
docker build -t demo/api:latest     ./api
docker build -t demo/compute:latest ./compute
docker build -t demo/loadgen:latest ./loadgen

# 4. Deploy
kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/compute/
kubectl apply -f k8s/api/
kubectl apply -f k8s/loadgen/

# 5. Verify — expect 2+2+1 pods all Running
kubectl get pods -n demo
kubectl get hpa   -n demo

# 6. Watch the demo (single stream — stats + scale events)
kubectl logs -n demo -l app=loadgen --follow

# 7. Watch HPA react in a second terminal
kubectl get hpa -n demo --watch
```

Expected initial state:
```
NAME                       READY   STATUS    RESTARTS
api-xxxxxxxxx-aaaaa        1/1     Running   0
api-xxxxxxxxx-bbbbb        1/1     Running   0
compute-xxxxxxxxx-aaaaa    1/1     Running   0
compute-xxxxxxxxx-bbbbb    1/1     Running   0
loadgen-xxxxxxxxx-aaaaa    1/1     Running   0
```

---

## Controlling Load

### Stop load
```bash
kubectl scale deployment/loadgen -n demo --replicas=0
```

### Resume load
```bash
kubectl scale deployment/loadgen -n demo --replicas=1
```

### Tune load (without rebuilding)
```bash
# Example: double the RPS and increase payload
kubectl patch configmap loadgen-config -n demo --type merge \
  -p '{"data":{"TARGET_RPS":"20","PAYLOAD_N":"35"}}'

# Restart loadgen to pick up the new values
kubectl rollout restart deployment/loadgen -n demo
```

| Var | Default | Effect |
|---|---|---|
| `TARGET_RPS` | `10` | Requests per second |
| `WORKERS` | `5` | Concurrent goroutines |
| `PAYLOAD_N` | `35` | Fibonacci n — higher = more CPU per request |

---

## Monitoring

### Watch live stats (one line per second)
```bash
kubectl logs -n demo -l app=loadgen --follow
```
```json
{"level":"INFO","msg":"stats","sent":7,"ok":7,"errors":0,"p50_ms":500,"p99_ms":1200}
```

### Watch HPA scale in real time
```bash
kubectl get hpa -n demo --watch
```
```
NAME          REFERENCE            TARGETS         MINPODS   MAXPODS   REPLICAS
compute-hpa   Deployment/compute   cpu: 400%/50%   2         5         5
api-hpa       Deployment/api       cpu:  11%/50%   2         5         2
```

**Reading the CPU target column (`108%/50%`):**
- Right value (`50%`) — your threshold: fire when average pod CPU exceeds 50% of its CPU **request** (50m → triggers at 25m)
- Left value (`108%`) — current measurement: pods are using 108% of their request on average right now
- HPA scales to `ceil(replicas × current/target)`, capped at `maxReplicas`

**`m` = millicores.** `1000m` = 1 full CPU core. `50m` = 5% of one core.

### Watch scale events logged by the load generator
```bash
kubectl logs -n demo -l app=loadgen --follow | grep scale_event
```
```json
{"level":"INFO","msg":"scale_event","service":"compute","pods":3,"prev":2}
{"level":"INFO","msg":"scale_event","service":"compute","pods":5,"prev":3}
```

### Check all pod statuses
```bash
kubectl get pods -n demo
```

### Tail logs for a specific service
```bash
kubectl logs -n demo -l app=compute --follow
kubectl logs -n demo -l app=api     --follow
```

### Describe HPA for detailed event history
```bash
kubectl describe hpa compute-hpa -n demo
kubectl describe hpa api-hpa     -n demo
```

---

## Step-by-Step Build Order

```
Step 1 — Compute service (Go)
         └── /health + /compute endpoints
         └── structured JSON logging (log/slog)
         └── Dockerfile
         └── smoke test: go run main.go → curl localhost:8080/health

Step 2 — API service (Next.js)
         └── /api/health + /api/compute endpoints
         └── structured JSON request logging (middleware)
         └── COMPUTE_SERVICE_URL env var
         └── Dockerfile
         └── smoke test: curl localhost:3000/api/health

Step 3 — Load Generator (Go)
         └── goroutine worker pool → TARGET_RPS
         └── per-second stats logging (slog JSON)
         └── k8s pod-count polling → scale event lines
         └── Dockerfile

Step 4 — Kubernetes manifests
         └── namespace.yaml
         └── compute: Deployment (2 replicas, 50m/200m CPU, 32Mi/64Mi RAM) + ClusterIP + HPA (min 2)
         └── api:     Deployment (2 replicas, 50m/200m CPU, 64Mi/128Mi RAM) + NodePort  + HPA (min 2)
         └── loadgen: Deployment (1 replica,  25m/100m CPU, 32Mi/64Mi RAM)  + ConfigMap

Step 5 — Deploy & observe
         └── all 5 pods Running
         └── health probes passing
         └── loadgen stats flowing
         └── kubectl get hpa --watch shows scale events
```

---

## Docs

- [`docs/howto.md`](docs/howto.md) — full walkthrough with every command explained
- [`docs/monitoring.md`](docs/monitoring.md) — reading logs, triggering scale, what to look for

---

## What This Demo Does NOT Cover (intentionally)

- TLS / ingress certificates
- Secrets management
- Persistent storage
- Service mesh / Istio
- Distributed tracing
- CI/CD pipeline
