# app-with-floci

A sample application that uses [Floci](https://github.com/floci-io/floci) to
stand in for three AWS services — **RDS**, **ElastiCache** and **OpenSearch** —
and runs on [Okteto](https://okteto.com).

**Floci Flix** is a small movie catalogue. Each service does the job it actually
exists for:

| Service | Backed by | Used for |
|---|---|---|
| RDS | PostgreSQL 16 | The authoritative `movies` table. Writes are transactional. |
| OpenSearch | OpenSearch 2.11 | Full-text and fuzzy search, with genre facets. |
| ElastiCache | Valkey 8 | Cache-aside on the detail read path, plus a sorted set of view counts. |

There is a fourth page in the UI, **Emulator**, which shows what each AWS
control-plane call returned and where the app actually connected. That page is
the interesting one — see [The one wrinkle](#the-one-wrinkle) below.

---

## The thing to know first

Floci implements these three services by starting **real engine containers**
through a Docker daemon. It is not mocking them.

That is fine on a laptop, where Floci can use the host's Docker socket. It is
the whole problem on Kubernetes, which has no socket to share — so in the
cluster Floci runs inside a **privileged Docker-in-Docker pod**.

> **This deployment needs Okteto's privileged-container guardrail relaxed for the
> target namespace.** That guardrail lives in Okteto's own mutating webhook, and
> it *strips* the flag rather than rejecting the pod — so without the exception
> the pod starts and `dockerd` fails later with a confusing cgroup error.
> Details and the long-term fix: [docs/okteto-implications.md](docs/okteto-implications.md).

---

## Deploy it

```bash
okteto context use <your-context>
okteto namespace use <your-namespace>
okteto deploy
```

That applies `k8s/floci.yaml`, then hands `docker-compose.yml` to Okteto, which
turns it into the api and web Deployments and Services. `okteto endpoints` prints
the public URL.

First deploy is slow: the Floci pod pulls ~2.3 GB of engine images into its PVC
before it will answer. Subsequent deploys reuse the cache.

There is deliberately no local `docker compose up` path. Floci needs a
privileged container in the cluster and the host Docker socket on a laptop —
two different topologies — and maintaining both meant two copies of the same
`FLOCI_*` configuration. The dev loop is `okteto up` instead:

```bash
okteto up api     # go run, rebuilt on save
okteto up web     # vite dev server
```

Reaching PostgreSQL, Valkey or OpenSearch from your laptop needs a port-forward.
Okteto only exposes HTTP through its ingress, and these are TCP:

```bash
kubectl port-forward -n <ns> svc/floci 7001:7001 6379:6379 9400:9400
redis-cli -h localhost -p 6379 ping
curl localhost:9400/_cluster/health
```

The database credentials come from `APP_DB_USER`, `APP_DB_PASSWORD` and
`APP_DB_NAME`. `docker-compose.yml` supplies defaults, but an Okteto admin
variable or `okteto deploy --var APP_DB_PASSWORD=...` overrides them — so read
the effective values from the pod rather than assuming the defaults. Note the
label: Okteto tags compose services `stack.okteto.com/service`, not `app`.

```bash
kubectl get pod -n <ns> -l stack.okteto.com/service=api \
  -o jsonpath='{range .spec.containers[0].env[*]}{.name}={.value}{"\n"}{end}' \
  | grep APP_DB
psql "postgresql://<user>:<password>@localhost:7001/<db>"
```

Those values land in the pod spec as plaintext env, readable by anyone with
namespace read access. Acceptable for an in-memory emulator database that is
recreated on every Floci restart and never reachable from outside the namespace —
but do not point these variables at anything real.


## How a request flows

The API has no public endpoint. Only `web` does, and nginx proxies to the API
server-side inside the cluster:

```
browser -> https://web-<ns>.<cluster>/api/movies     public endpoint, web pod
             nginx: location /api/ -> proxy_pass http://api:8080
               ClusterIP Service "api", private to the namespace
                 api -> floci:7001  PostgreSQL  (through Floci's proxy)
                 api -> floci:6379  Valkey      (through Floci's proxy)
                 api -> floci:9400  OpenSearch  (direct, published port)
```

The frontend only ever issues relative URLs, so the browser sees a single origin
and there is no CORS involved. Under `okteto up web`, Vite's dev proxy takes
nginx's place via `API_PROXY`.

In compose terms: `api` declares a bare `"8080"`, which stays on the cluster's
private network, while `web` declares `"8080:8080"` — the mapping is what makes
Okteto publish an endpoint.

## How the app finds its dependencies

At startup the API calls Floci's control plane with the ordinary AWS SDK —
`DescribeDBInstances`, `DescribeReplicationGroups`, `DescribeDomain` — and uses
the endpoints it gets back. That is the point of running an emulator: the SDK
path is real.

```
DescribeDBInstances        -> floci:7001   ->  PostgreSQL  (proxied by Floci)
DescribeReplicationGroups  -> floci:6379   ->  Valkey      (proxied by Floci)
DescribeDomain             -> floci:9400   ->  OpenSearch  (direct, see below)
```

`FLOCI_HOSTNAME=floci` is what makes the first two resolve: Floci advertises
itself by that name, and it is both the compose service name and the Kubernetes
Service name.

On connect it also applies `internal/store/schema.sql`, loads
`internal/store/seed.sql` when the table is empty, and rebuilds the OpenSearch
index from whatever PostgreSQL holds. OpenSearch is never seeded directly, so a
single path covers both a first run and a Floci restart that replaced the search
node while leaving the database intact.

The API also **creates** those three resources if they are missing, through the
same SDK. That is why there are no Floci init hooks: hooks would have to exist
twice — mounted from the repo locally, inlined into the manifest in the cluster —
and Floci's hook runner is fail-fast, so a slow `CreateDomain` can shut the
emulator down rather than merely being slow.

### The one wrinkle

OpenSearch is the exception, and the app resolves it without configuration.

Floci advertises the OpenSearch endpoint as a **Docker container name**,
`http://floci-opensearch-flociflix-search:9200`, and ignores `FLOCI_HOSTNAME`
when doing so. That name resolves only from inside Floci's Docker network — true
when the caller is a container on that network, false when it is a Kubernetes
pod.

So the API simply checks whether the name resolves. If it does, it uses it. If it
does not, it falls back to the Floci service on port 9400, where the OpenSearch
container publishes on the pod IP. Nothing to set per environment;
`OPENSEARCH_ENDPOINT_OVERRIDE` exists only to force a specific address.

The Emulator page shows the advertised value, the effective one, and which rule
was applied.

The underlying reason is that RDS and ElastiCache are reached *through* Floci's
own TCP proxies, while OpenSearch publishes its own port and Floci talks to it by
name. Internally that is an IP-versus-name lookup difference.

---

## Ports

| Port | Bound by | Carries |
|---|---|---|
| 4566 | Floci | Every AWS API call |
| 7001–7003 | Floci proxy | PostgreSQL wire protocol + IAM auth |
| 6379–6381 | Floci proxy | Valkey RESP + IAM auth |
| 9400–9402 | OpenSearch container | OpenSearch REST, direct |

Kubernetes Services cannot express port ranges, so the ranges are narrowed to
three ports each in the `FLOCI_SERVICES_*_PROXY_*_PORT` env vars and enumerated
in the Service. Both live in `k8s/floci.yaml` — widen them together.

Two ranges must **not** be published: `5100–5199` (Floci starts an ECR registry
sidecar that binds them itself) and `9200–9299` (Lambda Runtime API, internal).

---

## Layout

```
docker-compose.yml   The application. Okteto turns this into Deployments and
                     Services directly — no manifests, no ConfigMaps. Its
                     `floci` service is the laptop variant (host Docker socket)
                     and is filtered out of cluster deploys.
okteto.yaml          Two things only: apply the Floci manifest, then the
                     compose file. Plus `dev` for hot reload.
k8s/floci.yaml       The one manifest — StatefulSet + Service for privileged
                     Docker-in-Docker. Self-contained; nothing to template.
api/                 Go: provisioning, discovery, the three data paths, the
                     inspector endpoint. internal/store/schema.sql is the DDL,
                     internal/store/seed.sql the starter catalogue (30 movies),
                     both embedded and applied on connect.
web/                 React + Vite + TypeScript: search, detail, add, emulator
docs/                Okteto implications and the guardrail decision

.oktetoignore        What gets uploaded when deploy/destroy run with remote
                     execution. An allow-list: exclude everything, then name
                     what the commands need. Sectioned, so `destroy` uploads
                     3 files instead of 36.
api/.stignore        What `okteto up` syncs into each dev container. Separate
web/.stignore        mechanism from .oktetoignore — this is Syncthing, and it
                     keeps node_modules and build output out of the sync.
```

### Why there is one manifest and not zero

Okteto's compose support parses `privileged` only to warn about it
(`pkg/model/stack_serializer.go`), along with `security_opt`, `sysctls` and
`userns_mode` — `cap_add` and `user` are the only security levers it honours.
Docker-in-Docker needs `privileged`, so Floci cannot come from compose. Every
other service does.

## API

| Route | Exercises | Notes |
|---|---|---|
| `GET /api/movies?q=&genre=` | OpenSearch | Fuzzy multi-field query, genre facets |
| `GET /api/movies/{id}` | Valkey → PostgreSQL | Cache-aside; returns `X-Cache: HIT\|MISS` and the lookup time |
| `POST /api/movies` | all three | PostgreSQL tx → index → invalidate |
| `GET /api/trending` | Valkey | `ZINCRBY` view counters |
| `GET /api/aws/status` | Floci control plane | Advertised vs effective endpoints |
| `GET /api/health`, `/api/ready` | — | Health answers immediately; ready waits for all three |

## What a restart does

Worth knowing before you rely on this, because Okteto namespaces scale to zero
and restarts are routine rather than exceptional.

When Floci restarts it provisions **brand new engine containers behind the same
proxy addresses**. Nothing about the connection looks broken: `floci:7001` still
accepts connections, it is simply a different, empty PostgreSQL. Measured with a
persistent storage mode, it was worse — `DescribeReplicationGroups` and
`DescribeDomain` both still reported `available` while the Valkey container had
been removed and OpenSearch sat in `Exited(137)`.

Two things follow, both deliberate:

* **Floci runs in memory mode.** The init hooks recreate all three resources on
  every boot, so a restart is deterministic. Data does not survive it — the right
  way round for an environment that sleeps, where a consistent empty state beats
  an inconsistent populated one.
* **The API supervises rather than connects once.** Every 10 seconds it checks
  that its schema and search index are still there, not just that the sockets are
  open, and reconnects and re-seeds when they are not. Recovery takes one
  interval:

  ```
  15:52:37  kubectl delete pod floci-0
  15:52:58  WARN  backing services were replaced underneath us; reconnecting
  15:54:02  INFO  connected - all three backing services ready
  ```

  Measured on Okteto: 21s to detect, 85s to fully recover, most of it waiting for
  OpenSearch to reach Active. The engine images came from the PVC cache
  (`[boot] cached: ...` rather than `pulling`), so none of the 2.3GB was
  re-downloaded.

The PVC still matters: it caches the ~2.3 GB of engine images, which is the
expensive part of a cold start.

## Caveats

This is a demo, and a few choices reflect that:

* One pod carries Floci and every engine. No HA, and a restart interrupts everything.
* The API provisions its own AWS resources at startup. Fine against an emulator;
  not a pattern for anything with real infrastructure behind it.
* The privileged container is a real security trade-off, scoped to one namespace.
* Data is intentionally not durable across restarts; see above.
