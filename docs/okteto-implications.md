# Running Floci on Okteto: what it costs, and the fix

Notes for whoever owns the cluster policy. Everything here was verified against
Floci 1.7.0 by reading its source and running the full stack.

## Why a privileged container is needed at all

Floci splits its services into two classes:

* **In-process** — S3, SQS, SNS, DynamoDB, IAM, KMS, Secrets Manager, SSM and
  most of the rest. Pure Java, no Docker, runs anywhere.
* **Container-backed** — RDS, ElastiCache, OpenSearch, Lambda, Neptune,
  DocumentDB, MSK, ECS, EC2, EKS, CodeBuild, Managed Flink. Floci drives a
  Docker daemon and starts real engine containers.

The three services this demo needs are all in the second class. Kubernetes has no
Docker socket to hand over, so the daemon has to be brought along — hence
`docker:28-dind` with `privileged: true`.

There is no configuration escape. Floci has no "bring your own endpoint" option
for these services: you cannot point its RDS implementation at an existing
PostgreSQL. RDS and OpenSearch have a `mock` mode that returns control-plane
metadata with no engine behind it; **ElastiCache has no mock mode at all**, so
without Docker its API is simply unavailable.

## Compose cannot express it either

Worth stating separately from the guardrail, because it is a different wall.

Okteto's compose support parses `privileged` only to emit a warning and then
discards it:

```go
// pkg/model/stack_serializer.go
107:  Privileged *WarningType `yaml:"privileged,omitempty"`
1496: notSupported = append(notSupported, "services[%s].privileged")
```

The same is true of `security_opt`, `sysctls` and `userns_mode`. The only
security levers compose honours are `cap_add`/`cap_drop` and `user`, and
`cap_add: [SYS_ADMIN]` alone will not rescue rootful `dockerd`: a non-privileged
container gets `/sys/fs/cgroup` read-only, and compose cannot set the AppArmor or
seccomp overrides the daemon needs.

That is why this repo has exactly one Kubernetes manifest. Everything else -
api, web, ports, environment, resources, probes - comes from
`docker-compose.yml`, with the `floci` service filtered out of cluster deploys
via `deploy.compose.services`.

## The guardrail

Okteto blocks privileged containers in its own mutating webhook —
`pod.webhook.okteto.com`, one per namespace, `path: /mutate/pod`. It is an
application-level guardrail, not an infrastructure one, so an admin can relax it.

Two properties matter when testing:

1. **It mutates rather than denies.** A privileged pod is admitted with the flag
   stripped. The pod then starts and `dockerd` fails later with a cgroup or
   bridge error, which reads like a broken image rather than a policy decision.
2. **`sideEffects: NoneOnDryRun`** means the webhook runs on
   `--dry-run=server` too. A dry-run that reports "created" proves nothing unless
   you inspect the returned object:

   ```bash
   kubectl apply --dry-run=server -f probe.yaml \
     -o jsonpath='{.spec.containers[0].securityContext}'
   ```

`bootstrap.sh` prints a pointer to this file if `dockerd` never becomes ready,
because the symptom is so far from the cause.

## What the exception is worth

A privileged container is effectively root on the node. Recommended handling:

* Scope it to one namespace, not the installation.
* Write down an owner and a revert path.
* Treat it as a demo affordance. It is not a pattern to recommend to customers.

## Other Okteto constraints this design had to fit

| Constraint | Consequence |
|---|---|
| LimitRange caps a container at 2 CPU / 8 GiB | Fits: measured idle use is ~1.6 GiB, dominated by OpenSearch at ~1.45 GiB |
| Services cannot express port ranges | Floci's proxy ranges narrowed to 3 ports each and enumerated |
| `convertLoadBalancedServices` turns LB/NodePort into ClusterIP + HTTP ingress | PostgreSQL, Valkey and OpenSearch ports are not externally exposable; port-forward only |
| Namespaces scale to zero | Image cache and engine data on a PVC, or every wake-up re-pulls 2.3 GB |
| Kyverno caps the *number* of Services | The design uses one Service for all ten Floci ports |

Constraints were measured on a different cluster (`product_okteto_dev`) than the
one this is deployed to; re-check before relying on the exact numbers.

## Restarts do not reconcile

The finding with the most operational weight, and the reason this repo runs Floci
in memory mode.

Floci restores resource *metadata* across a restart but does not reconcile the
engine *containers* against it. Observed directly, after a plain restart with
`FLOCI_STORAGE_MODE=hybrid`:

| Service | Control plane said | Reality |
|---|---|---|
| RDS | `available` | container recreated, but empty - schema gone |
| ElastiCache | `available` | Valkey container **removed**, proxy had no backend |
| OpenSearch | `active` | container in `Exited(137)`, never restarted |

The control plane advertised three healthy endpoints with nothing usable behind
two of them, and the app failed with connection refused.

This matters on Okteto specifically because namespaces scale to zero, so a
restart is a normal weekly event rather than an incident. Two mitigations are
baked into this repo:

1. `FLOCI_STORAGE_MODE=memory`, so the init hooks rebuild all three resources on
   every boot and a restart is deterministic.
2. The API supervises its connections, checking every 10 seconds that its schema
   and index still exist rather than that the sockets are open. Reachability is
   not evidence of correctness when the engine behind an address can be swapped.

A third upstream mitigation would be for Floci to reconcile container state
against persisted metadata on startup, or to report a resource as
`creating`/`failed` when its container is gone.

### Open: why was the RDS volume not re-adopted?

Deferred rather than answered, and tracked as P6 in the plan. The evidence is
ambiguous: across a restart the RDS container kept its name, which argues the
volume binding *was* preserved, yet its uptime suggested it had been recreated
and the schema was gone either way. The decisive observation was not captured -
comparing `docker inspect -f '{{json .Mounts}}'` and `docker volume ls` before
and after - so three outcomes are still open:

| Observation | Meaning |
|---|---|
| same container name, same volume | binding is fine; the fault is elsewhere in the restore path |
| same container name, **new** volume | volume name regenerated, old data orphaned (leading hypothesis) |
| container never recreated | the restore path does not run at all |

The code to read once it is known: `RdsContainerManager`
(`containerStorageResourceId`, `dockerVolumeName`, `claimStorage`,
`addPersistenceMounts`) and `ContainerStorageHelper.isNamedVolumeMode`. Also
worth checking whether ElastiCache and OpenSearch have any restore path at all -
the Valkey container was removed outright, which suggests not.

Enabling persistence itself is three lines (`FLOCI_STORAGE_MODE=hybrid`,
`FLOCI_STORAGE_PERSISTENT_PATH=/app/data`, and `-v floci-data:/app/data` on the
inner container; no new PVC, since Docker named volumes already sit on the
existing one). The work is the app-side consequence: with persistence, `Ensure`
cannot stay "create if absent", because a resource whose container is gone still
reports `available`, so `Ensure` skips it and the app hangs. It would need to
verify the resource works and delete-and-recreate when it does not - destructive,
so worth gating behind an env var.

## The upstream fix

Floci already ships a Kubernetes executor — but **only for Lambda**:
`FLOCI_SERVICES_LAMBDA_EXECUTOR=kubernetes` runs functions as pods and, in its
own source comment, "never touches docker.sock". Nothing equivalent exists for
the container-backed data services.

Two changes upstream would remove the need for privileged entirely. The first is
small:

1. **Make OpenSearch report a reachable address.** `OpenSearchDomainManager`
   polls readiness by Docker container name and advertises that same name,
   ignoring `FLOCI_HOSTNAME`:

   ```java
   // OpenSearchDomainManager.java:110
   String url = "http://" + containerName + ":" + OPENSEARCH_PORT + "/_cluster/health";
   ```

   RDS and ElastiCache already resolve the container **IP** instead
   (`EndpointInfo` returns it in Docker mode) and are fronted by a proxy inside
   Floci. Bringing OpenSearch in line would make the much simpler
   Floci-plus-DinD-sidecar topology work for all three, since IP routing works
   across a shared pod network namespace while Docker name resolution does not.

2. **Extend the Kubernetes executor to the data services**, so RDS, ElastiCache
   and OpenSearch engines run as pods. This is the change that would make Floci a
   first-class citizen on any Kubernetes development platform, with no privileged
   container anywhere.

## The alternative we did not take

A non-privileged variant is possible today and worth knowing about: run
PostgreSQL, Valkey and OpenSearch as ordinary Okteto services, set
`FLOCI_SERVICES_RDS_MOCK=true` and `FLOCI_SERVICES_OPENSEARCH_MOCK=true` for the
control-plane API shape, and point the app's data plane at the real services.
Floci then needs no Docker at all — it boots fine, since its Docker client is
created lazily and only container-backed calls fail.

The cost is that ElastiCache's AWS API disappears entirely (no mock mode), and
the RDS and OpenSearch APIs become shape-only. For a demo whose purpose is
showing Floci emulating these three services, that trade was the wrong way round
— but for a customer-facing pattern it is the right one.
