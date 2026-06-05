# Learn Kubernetes & Docker — Conversation Guide

This document captures a guided walkthrough of the **k8s-autoscale-demo** project: what we built, how it works internally, and the reasoning behind each design choice.

Use it as a self-paced tutorial. Each section ends with questions you can answer yourself before moving on.

---

## Table of contents

### Foundations (original demo)
1. [Big picture](#1-big-picture)
2. [Docker: images vs containers](#2-docker-images-vs-containers)
3. [Your Dockerfiles](#3-your-dockerfiles)
4. [Building for minikube](#4-building-for-minikube)
5. [Services & DNS](#5-services--dns)
6. [HPA, requests, and limits](#6-hpa-requests-and-limits)
7. [Liveness vs readiness probes](#7-liveness-vs-readiness-probes)
8. [ConfigMaps](#8-configmaps)
9. [Pod lifecycle](#9-pod-lifecycle)
10. [Loadgen RBAC](#10-loadgen-rbac)

### Extended topics (implemented)
11. [Ingress](#11-ingress)
12. [Secrets](#12-secrets)
13. [Persistent volumes](#13-persistent-volumes)

---

## 1. Big picture

Three services run in the `demo` namespace:

```
[ loadgen ]  ──POST /api/compute──▶  [ api ]  ──POST /compute──▶  [ compute ]
  1 pod (fixed)                       2–5 pods (HPA)              2–5 pods (HPA)
  generates traffic                   Node.js gateway             Go + fibonacci
```

| Service | Role | Scales? |
|---|---|---|
| **loadgen** | Sends configurable RPS to the API | No (fixed 1 replica) |
| **api** | Health check + forwards compute requests | Yes (HPA 2–5) |
| **compute** | CPU-heavy fibonacci work | Yes (HPA 2–5) |

The load generator makes the demo self-contained — no external tools (`hey`, `k6`) required.

---

## 2. Docker: images vs containers

### Key distinction

```
Dockerfile  →  docker build  →  IMAGE (demo/compute:latest)
                                      ↓
                               docker run / Kubernetes pod
                                      ↓
                                 CONTAINER (running process)
```

- **`docker build`** creates an **image** — a frozen snapshot (filesystem + startup command).
- A **container** is a **running instance** of that image.

### Naming: `demo/compute:latest`

| Part | Meaning |
|---|---|
| `demo` | Repository / project prefix (convention) |
| `compute` | Image name for this service |
| `latest` | Tag (version label; `latest` is just a default name) |

### Build context: `./compute`

The path after `-t` is the **build context** — Docker sends that directory to the builder. The `Dockerfile` inside defines the steps.

---

## 3. Your Dockerfiles

### Compute (multi-stage build)

```dockerfile
FROM golang:1.22-alpine AS builder
# compile main.go → binary "compute"

FROM alpine:3.19
# copy ONLY the binary — no Go compiler in final image
ENTRYPOINT ["./compute"]
```

| Stage | Purpose |
|---|---|
| **builder** | Fat image with compiler; produces the binary |
| **final** | Tiny Alpine image (~10–20 MB vs ~300 MB) |

`ENTRYPOINT` is the process that runs when the container starts. If it exits, the container dies.

`EXPOSE 8080` is documentation only — it does not open a port. Kubernetes opens ports via the Deployment manifest.

### API (single-stage)

```dockerfile
FROM node:22-alpine
COPY package.json main.js ./
ENTRYPOINT ["node", "main.js"]
```

Simpler service — no compile step needed in a separate stage.

---

## 4. Building for minikube

```bash
eval $(minikube docker-env)
docker build -t demo/compute:latest ./compute
```

`minikube docker-env` points your local Docker CLI at **minikube's Docker daemon**. Images built after this land **inside the cluster** — no registry push needed.

That is why Deployments use:

```yaml
image: demo/compute:latest
imagePullPolicy: Never
```

`Never` = "use the local image; do not try to pull from Docker Hub."

---

## 5. Services & DNS

### Internal DNS (CoreDNS)

When the API pod calls `http://compute-service.demo:8080`:

```
compute-service  .  demo  .  svc  .  cluster  .  local
     │               │
     │               └── namespace
     └── Service name (from service.yaml)
```

Short form `compute-service.demo` works inside the cluster. No public internet involved.

### Service vs Pod DNS

| Thing | Stable DNS? | Who calls it? |
|---|---|---|
| **Service** (`compute-service`) | ✅ Yes | API, loadgen — normal app traffic |
| **Pod** (`compute-abc123-xyz`) | Exists but ephemeral | Debugging, not typical service-to-service calls |

Pod IPs and names change on restart/scale. **Services** provide a stable front door.

### How a Service routes traffic

```yaml
# k8s/compute/service.yaml
selector:
  app: compute
```

```
API calls compute-service.demo:8080
        │
        ▼
CoreDNS → Service virtual IP
        │
        ▼
kube-proxy load-balances to ONE of:
  compute-pod-aaaa
  compute-pod-bbbb
  compute-pod-cccc   ← new pod after HPA; no config change needed
```

### Service types in this demo

| Service | Type | Why |
|---|---|---|
| `api-service` | ClusterIP | Internal; external access via **Ingress** |
| `compute-service` | ClusterIP | Internal only — API pods reach it by DNS |

> **Note:** The API Service was originally NodePort for minikube convenience. It is now ClusterIP with Ingress handling external HTTP access (see [§11](#11-ingress)).

---

## 6. HPA, requests, and limits

### Resource block (compute Deployment)

```yaml
resources:
  requests:
    cpu: 50m      # scheduling + HPA denominator
    memory: 32Mi
  limits:
    cpu: 200m      # hard cap (throttle, not HPA input)
    memory: 64Mi
```

### What is a request?

**You choose it** in YAML. It tells Kubernetes:

| Consumer | Uses request for |
|---|---|
| **Scheduler** | "Only place pod on a node with this much free capacity" |
| **HPA** | "% CPU used" = actual usage ÷ request |

`50m` = 50 millicores = 5% of one CPU core. Chosen **small on purpose** in this demo so HPA triggers quickly and scale events are visible on a 2-core minikube cluster.

### What is a limit?

Maximum the container may use. Enforced by Linux cgroups:

| Resource | At limit |
|---|---|
| CPU | Throttled (slowed), not killed |
| Memory | OOMKilled (container restarted) |

HPA **ignores limits** for `averageUtilization` metrics — only **requests** matter.

### Reading `kubectl get hpa`

```
compute-hpa   cpu: 400%/50%   2   5   5
              ─────┬─────     │   │   │
                   │          │   │   └── REPLICAS (current)
                   │          │   └── MAXPODS
                   │          └── MINPODS
                   └── TARGETS
```

| Value | Meaning |
|---|---|
| **400%** | Pods average 400% of their CPU **request** right now |
| **50%** | HPA target: keep average at 50% of request |
| **2** | `minReplicas` |
| **5** | `maxReplicas` |
| **5** (last) | Currently running pods |

### HPA formula

```
desired pods = ceil(current pods × (current% / target%))
```

Example at 5 pods, 400%/50%:

```
ceil(5 × (400/50)) = ceil(40) = 40  →  capped at maxReplicas 5
```

### Scenario A: raise maxReplicas 5 → 10

HPA scales **5 → 10**. Fixed RPS from loadgen spreads across more pods → CPU per pod drops → utilization may fall below 400%.

### Scenario B: raise request 50m → 200m (limit stays 200m)

Same physical CPU (~200m), but reported % drops:

```
Before: 200m / 50m  = 400%
After:  200m / 200m = 100%
```

HPA scales up **less aggressively** — you moved the goalposts. Compare Config 1 (request=50m, max=5 → 5 pods) vs Config 2 (request=200m, max=10 → 10 pods): **maxReplicas** often matters more than the reported percentage.

---

## 7. Liveness vs readiness probes

Both probes HTTP-check your app. Defined in each Deployment:

```yaml
livenessProbe:
  httpGet:
    path: /health
    port: 8080
  periodSeconds: 10

readinessProbe:
  httpGet:
    path: /health
    port: 8080
  periodSeconds: 5
```

| Probe | Question | On failure |
|---|---|---|
| **Readiness** | Can this pod take traffic **right now**? | Removed from Service endpoints — no new requests |
| **Liveness** | Is the process **still alive**? | Container **killed and restarted** |

```
Readiness = "step aside"  (reversible, no restart)
Liveness  = "you're dead" (restart)
```

### Why `/health` not `/compute`?

Probes must be **fast and cheap**. Hitting fibonacci on liveness under load causes timeouts → restarts → **death spiral**.

### Failure thresholds

Default `failureThreshold: 3` — must fail 3 checks in a row before action (~15s with `periodSeconds: 5`).

### Example: readiness fails, liveness passes

- Pod stays `Running`, `RESTARTS` stays 0
- Service stops sending traffic for ~30s
- When `/health` returns 200 again → traffic resumes
- Other healthy pods handle requests meanwhile

---

## 8. ConfigMaps

ConfigMaps store **non-sensitive** key-value config separately from images.

```yaml
# k8s/loadgen/configmap.yaml
data:
  TARGET_RPS: "10"
  PAYLOAD_N: "40"
```

Injected via:

```yaml
envFrom:
  - configMapRef:
      name: loadgen-config
```

App reads at startup: `os.Getenv("TARGET_RPS")`.

### Changing config live

```bash
kubectl patch configmap loadgen-config -n demo --type merge \
  -p '{"data":{"TARGET_RPS":"20"}}'

kubectl rollout restart deployment/loadgen -n demo
```

**Patch alone does not update running pods.** Env vars are set at container start. You need a restart (or pod deletion) for new values.

Kubernetes does not auto-restart on ConfigMap change by design — you control blast radius.

| Knob | Effect |
|---|---|
| `TARGET_RPS` | More requests per second |
| `PAYLOAD_N` | Heavier fib per request → more CPU per request |
| `WORKERS` | More concurrent goroutines |

---

## 9. Pod lifecycle

From `kubectl apply` to `1/1 Running`:

```
YOU: kubectl apply
  ▼
API server → etcd (desired state stored)
  ▼
Deployment controller → ReplicaSet → Pod objects (Pending)
  ▼
Scheduler assigns pod to node (uses CPU/memory requests)
  ▼
kubelet on node:
  1. Pull image (skipped with imagePullPolicy: Never)
  2. Create container (same runtime as Docker)
  3. Assign pod IP
  4. Apply cgroup limits
  5. Start ENTRYPOINT process
  ▼
ContainerCreating → Running (may show 0/1 Ready)
  ▼
Readiness probe passes → 1/1 Ready → Service adds to endpoints
```

### Controllers reconcile constantly

```
Deployment ──▶ ReplicaSet ──▶ N pods
     ▲                            │
     └──── "always N replicas" ◀──┘
```

Delete a pod manually → ReplicaSet creates a replacement.

### Useful commands

```bash
kubectl get pods -n demo --watch
kubectl describe pod -n demo -l app=compute
kubectl get events -n demo --sort-by='.lastTimestamp'
```

---

## 10. Loadgen RBAC

Loadgen polls the Kubernetes API to log `scale_event` lines when api/compute pod counts change.

```yaml
serviceAccountName: loadgen-sa   # in loadgen Deployment
```

```yaml
# k8s/loadgen/rbac.yaml
Role:        list pods in namespace demo
RoleBinding: grants Role to loadgen-sa
```

Inside the pod, loadgen reads the auto-mounted token at:

```
/var/run/secrets/kubernetes.io/serviceaccount/token
```

And calls:

```
GET https://kubernetes.default.svc/api/v1/namespaces/demo/pods?labelSelector=app=compute
```

Without RBAC, the API server would return 403 and scale events would be disabled (loadgen logs `k8s_poller_disabled`).

---

## 11. Ingress

### What Ingress adds

Before Ingress, external access used NodePort (`minikube service api-service`). **Ingress** provides HTTP routing from outside the cluster to Services — closer to production.

```
Internet / laptop
      │
      ▼
Ingress Controller (nginx on minikube)
      │  host: api.demo.local
      │  path: /
      ▼
api-service:3000 (ClusterIP)
      ▼
api pods
```

### Files

| File | Purpose |
|---|---|
| `k8s/api/ingress.yaml` | Routes `api.demo.local` → `api-service:3000` |
| `k8s/api/tls-secret.yaml` | TLS cert for HTTPS (generated by script) |
| `scripts/generate-tls-secret.sh` | Creates self-signed cert for local demo |

### One-time setup

```bash
minikube addons enable ingress

# Generate TLS secret (self-signed for local demo)
./scripts/generate-tls-secret.sh

kubectl apply -f k8s/api/ingress.yaml
```

Add to `/etc/hosts`:

```
$(minikube ip)  api.demo.local
```

### Access the API

```bash
# HTTP (redirects to HTTPS if ssl-redirect enabled)
curl http://api.demo.local/api/health

# HTTPS (self-signed — use -k)
curl -k https://api.demo.local/api/health
```

### Internal vs external paths

| Caller | URL | Path |
|---|---|---|
| **loadgen** (inside cluster) | `http://api-service.demo:3000` | Direct to Service — bypasses Ingress |
| **You** (from laptop) | `https://api.demo.local` | Through Ingress |

This mirrors production: microservices talk via ClusterIP; users hit Ingress.

### Ingress vs NodePort vs LoadBalancer

| Type | Local minikube | Production |
|---|---|---|
| NodePort | Works, awkward port | Rarely exposed directly |
| LoadBalancer | Stays `<pending>` | Cloud LB provisions public IP |
| **Ingress** | nginx addon + /etc/hosts | Standard HTTP(S) entry point |

---

## 12. Secrets

### What Secrets are for

| | ConfigMap | Secret |
|---|---|---|
| Data | Non-sensitive config | Passwords, tokens, TLS certs |
| Encoding | Plaintext in etcd | Base64 by default (not encryption at rest by default) |

### Service-to-service auth in this demo

The API forwards requests to compute with a shared token:

```
api pod  ──POST /compute──▶  compute pod
           Header: X-Internal-Token: <from Secret>
```

| File | Purpose |
|---|---|
| `k8s/secrets/compute-auth.yaml` | `token` key shared by api + compute |
| api Deployment | `COMPUTE_AUTH_TOKEN` from secret |
| compute Deployment | `AUTH_TOKEN` from secret |

```yaml
# Inject one key from a Secret
env:
  - name: COMPUTE_AUTH_TOKEN
    valueFrom:
      secretKeyRef:
        name: compute-auth
        key: token
```

### Why not put the token in ConfigMap?

ConfigMaps are for config you would commit to git openly. Tokens belong in Secrets — and in production, consider **Sealed Secrets**, **External Secrets Operator**, or a vault.

### Rotating a secret

```bash
kubectl patch secret compute-auth -n demo --type merge \
  -p '{"stringData":{"token":"new-token-value"}}'

kubectl rollout restart deployment/api deployment/compute -n demo
```

Same rule as ConfigMap: **running pods keep old env until restarted**.

### TLS Secret (Ingress)

`api-tls` Secret holds `tls.crt` and `tls.key` for HTTPS termination at the Ingress controller — separate from the compute-auth token.

---

## 13. Persistent volumes

### The problem PVs solve

Containers are ephemeral. When a pod dies, its filesystem dies with it. **PersistentVolumeClaims (PVCs)** request durable storage that survives pod restarts.

### Why loadgen holds the PVC (not compute)

Compute runs **2–5 replicas** (HPA). A standard `ReadWriteOnce` PVC can only mount on **one pod at a time**. Sharing one disk across scaled compute pods would not work without ReadWriteMany storage or an external database.

Loadgen is **fixed at 1 replica** — ideal for demonstrating PVC.

```
loadgen pod
  └── volumeMount: /data
        └── PVC: loadgen-data (100Mi, ReadWriteOnce)
              └── totals.json  (cumulative stats survive restart)
```

### Files

| File | Purpose |
|---|---|
| `k8s/storage/loadgen-pvc.yaml` | PVC definition |
| loadgen Deployment | `volumeMount` + `volumes` referencing PVC |
| loadgen `main.go` | Reads/writes `/data/totals.json` |

### What persists

```json
{"total_sent": 45230, "total_ok": 45100, "total_errors": 130}
```

After `kubectl rollout restart deployment/loadgen`, the new pod mounts the same PVC and continues counting from the saved totals.

### Verify persistence

```bash
kubectl logs -n demo -l app=loadgen --tail=3
# note cumulative totals in "totals_persisted" log lines

kubectl rollout restart deployment/loadgen -n demo
kubectl logs -n demo -l app=loadgen --tail=5
# totals should continue from previous values, not reset to 0
```

### PV / PVC / Pod relationship

```
PersistentVolume (PV)     ← cluster storage resource (minikube provisions automatically)
        ▲
        │ bound
PersistentVolumeClaim     ← "I need 100Mi ReadWriteOnce"
        ▲
        │ mounted
Pod (loadgen)             ← sees /data inside container
```

On minikube, the `standard` StorageClass dynamically provisions PVs when you create a PVC.

### Multi-replica + persistence (production pattern)

| Approach | When |
|---|---|
| ReadWriteMany PVC | Shared files (uncommon, limited storage classes) |
| External database | Most stateful multi-replica apps |
| StatefulSet + per-pod PVC | One disk per replica (e.g. Kafka, etcd) |

---

## Quick reference — the full stack

```
DOCKER                          KUBERNETES
──────                          ──────────
Dockerfile          →  image    Deployment → desired state
docker build                    ReplicaSet → N identical pods
image vs container              Scheduler → picks node
ENTRYPOINT          →  process kubelet → starts container
                                Service + DNS → stable names
                                Ingress → external HTTP(S)
                                ConfigMap → non-secret config
                                Secret → tokens, TLS keys
                                PVC → durable disk
                                HPA → scale on CPU % of request
                                readiness → traffic gate
                                liveness → restart if dead
                                RBAC → API permissions for loadgen
```

---

## Practice questions (with answers)

<details>
<summary>What does docker build create — an image or a container?</summary>

An **image**. Containers are created when the image is run (via `docker run` or kubelet).
</details>

<details>
<summary>Readiness fails 3 times, liveness passes. Is the pod restarted?</summary>

**No.** It is removed from Service endpoints until readiness passes again.
</details>

<details>
<summary>patch ConfigMap but forget rollout restart. What values does loadgen use?</summary>

**Old values.** The running pod's env was set at start. Only a new pod picks up the new ConfigMap.
</details>

<details>
<summary>request=50m, actual CPU=200m. What % does HPA see?</summary>

**400%** (200m ÷ 50m). HPA uses request, not limit, as the denominator.
</details>

<details>
<summary>Why does loadgen use a PVC but compute does not?</summary>

Loadgen has **1 replica** — ReadWriteOnce works. Compute scales to **multiple pods** — one RWO disk cannot be shared; production would use a database or StatefulSet.
</details>

---

## Deploy order (full stack)

```bash
eval $(minikube docker-env)
docker build -t demo/api:latest     ./api
docker build -t demo/compute:latest ./compute
docker build -t demo/loadgen:latest ./loadgen

minikube addons enable ingress
minikube addons enable metrics-server

./scripts/generate-tls-secret.sh

kubectl apply -f k8s/namespace.yaml
kubectl apply -f k8s/secrets/
kubectl apply -f k8s/storage/
kubectl apply -f k8s/compute/
kubectl apply -f k8s/api/
kubectl apply -f k8s/loadgen/

kubectl get pods,ingress,pvc -n demo
```
