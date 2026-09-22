# Workspace shard assigner

`provider-terraform` reconciles each `Workspace` by shelling out to
`terraform init/plan/apply`, which takes seconds to minutes. A single active
controller — the stock active/passive HA model — is therefore a throughput
bottleneck once you have hundreds of Workspaces.

Sharding lets several controller instances run at once, each watching only the
Workspaces labelled with its own shard name. Two halves:

- **The provider consumes the label.** `--shard-name=shard-N` filters its
  Workspace informers, gives it its own leader-election lease, and makes its
  garbage collector reclaim the working directories of Workspaces that belong
  elsewhere.
- **The shard assigner writes the label.** A Workspace with no label is
  reconciled by no shard at all, so something has to own placement.

Both are implemented here. Upstream [PR #288][288] proposes the consuming half
and is still open; this fork does not depend on it.

[288]: https://github.com/crossplane-contrib/provider-terraform/pull/288

## Provider flags

| Flag | Env | Default | Meaning |
| --- | --- | --- | --- |
| `--shard-name` | `SHARD_NAME` | *(empty)* | Reconcile only Workspaces labelled `terraform.crossplane.io/shard=<name>`. Empty reconciles everything, i.e. the stock unsharded behaviour. |
| `--graceful-shutdown-timeout` | | `10m` | How long in-flight reconciles may finish after SIGTERM. |

### Assigner flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--require-shard-offline` | `true` | Refuse to migrate a Workspace while its current shard still has a running pod. |
| `--pod-namespace` | `crossplane-system` | Where the sharded provider Deployments run. |
| `--stale-migration` | `30m` | How long a Workspace that never syncs may block further migrations. |

Setting `--shard-name` changes four things:

1. The Workspace informers are filtered with an equality label selector, so
   cache memory and watch traffic scale with this shard's share rather than the
   whole fleet.
2. The leader-election lease ID gets the shard name appended, so shards are
   active concurrently instead of electing one leader across all of them.
3. The working-directory garbage collector reclaims directories belonging to
   other shards, not just to deleted Workspaces. **Without this every migration
   leaks a directory.**
4. Every provider metric gains a constant `shard` label.

The garbage collector reads through `mgr.GetAPIReader()`, never the manager's
cache. A sharded cache holds only this shard's Workspaces, so a cached list
would make the collector treat every *other* shard's directories as orphaned
and delete them. `GarbageCollector.kube` is a `client.Reader` to keep that
mistake from compiling.

## Graceful shutdown

Set `terminationGracePeriodSeconds` above the p99 apply duration on every shard
Deployment, and keep `--graceful-shutdown-timeout` below it. A `terraform`
killed mid-apply leaves a stale backend lock that needs a manual
`force-unlock` — in practice that damages state far more often than concurrent
writers do.

## How placement is decided

A single controller with a full view of every Workspace, not a hash.

Hashing (`hash(name) % shardCount`, or rendezvous) needs no assigner, but every
controller then caches every Workspace — informer memory and watch traffic scale
with `workspaces × replicas` — placement is invisible to `kubectl`, and plain
modulo reshuffles ~80% of Workspaces on a 4→5 scale. A single writer can place
by least-loaded directly, and placement stays visible:

```sh
kubectl get workspace -L terraform.crossplane.io/shard
```

Three properties worth knowing:

- **`shardCount` is explicit desired state**, never derived from pod liveness.
  If it were, every rolling update, OOM kill, node drain and eviction would look
  like a scale-down and trigger a spurious reshuffle.
- **Labels are sticky.** A Workspace is relabelled only when it is unplaced, or
  when its shard is draining or has fallen out of range. Normal operation never
  relabels.
- **A Workspace is never moved off a shard whose pod is still running.** This
  is the important one, and it is why the drain procedure below looks the way
  it does.

  Shards are separate pods, so separate processes. A shard appearing in
  `draining` says nothing about whether its process is still running — it may
  be mid-`terraform apply` right now. Relabel a Workspace off it and two
  independent `terraform` processes are writing the same remote state. If that
  backend does not lock, nothing stops them: it is not a lock-contention error
  you can retry, it is a silent clobber.

  So before any migration the assigner lists pods carrying
  `terraform.crossplane.io/shard` and refuses to move a Workspace while its
  current shard still has one running. It **fails closed**: any error, or no
  shard-labelled pods at all, and nothing migrates. That last case is what a
  missing pod-template label looks like, and reading it as "everything is
  offline" would disable the check exactly when it matters.

  `--require-shard-offline=false` opts out, and is only reasonable with backend
  locking confirmed: S3 with a DynamoDB lock table or `use_lockfile = true`
  (Terraform ≥ 1.10), GCS, or azurerm — all of which lock natively. Local
  state, or S3 with neither mechanism, is genuinely corruptible under handover.

  The residual overlap the lock still has to cover is informer eviction: when
  the label changes, the old shard's cache is not evicted in an order
  guaranteed against the new shard picking it up. With the old shard's pod
  already gone, there is no process left to act on that stale cache entry.

## Configuration

```yaml
apiVersion: v1
kind: ConfigMap
metadata:
  name: provider-terraform-shards
  namespace: crossplane-system
data:
  shardCount: "4"
  draining: "shard-3"   # comma-separated, optional
```

Active shards are `shard-0 .. shard-{shardCount-1}` minus anything in
`draining`. Those names must match the `--shard-name` values on the provider
Deployments. A change to this ConfigMap re-evaluates every Workspace.

Manifests live in [`examples/shard-assigner/`](../examples/shard-assigner/):
the assigner itself, and
[`provider-shards.yaml`](../examples/shard-assigner/provider-shards.yaml) for
the sharded provider instances.

> A Crossplane `Provider` resource manages exactly one Deployment, and
> `replicas: N` on it is active/passive HA, not sharding. So shard-0 runs as
> the Crossplane-managed instance via a `DeploymentRuntimeConfig` and the rest
> run as ordinary Deployments beside it, reusing the package's ServiceAccount.

## Operational procedures

### Scale up (4 → 5)

1. Add the `shard-4` provider Deployment with `--shard-name=shard-4`.
2. Set `shardCount: "5"`.

New Workspaces start landing on shard-4 via least-loaded. **Existing Workspaces
do not move.** Rebalancing is a separate, deliberate action — there is no
automatic rebalance, by design.

### Scale down (remove shard-3)

1. Add `shard-3` to `draining`. The assigner stops placing new Workspaces
   there, but does **not** move the ones it has yet.
2. **Scale shard-3's Deployment to zero and wait for its pod to go away.**
   Until then the assigner refuses to migrate anything off it, and
   `terraform_shard_drain_blocked{shard="shard-3"}` is 1. Give it at least
   `terminationGracePeriodSeconds` so any in-flight apply finishes cleanly
   rather than being killed into a stale backend lock.
3. With the pod gone, the assigner relabels shard-3's Workspaces **one at a
   time**, waiting for each to reach `SYNCED=True` on its new shard before
   starting the next. Watch `terraform_shard_workspaces_inactive_shard` fall.
4. Once shard-3 owns nothing, drop `shardCount` to 3 and delete the Deployment.

Note step 2 is the opposite of what you would do if the label alone were the
safety mechanism. It is deliberate: shard-3's Workspaces sit unreconciled
between steps 2 and 3, and that is strictly better than two `terraform`
processes writing one state file. The `terraform_shard_drain_blocked` alert
exists so a half-finished drain is visible rather than silent.

Two ways to get this wrong:

- **Do not drop `shardCount` before draining.** Every Workspace on the removed
  shard becomes out-of-range at once. They still will not move while its pod
  runs, but you lose the explicit `draining` marker that says a drain is in
  progress.
- **Do not forget the pod-template label.** Every shard Deployment's pod
  template needs `terraform.crossplane.io/shard: shard-N`. Without it the
  assigner cannot verify liveness and, failing closed, migrates nothing.

A migration that never reaches `SYNCED=True` stops blocking the drain after
`--stale-migration` (default 30m); the assigner logs loudly and moves on, so one
permanently broken Workspace cannot wedge a drain forever.

## Rollout

- **Phase 1 — zero risk, do this first.** Deploy the assigner alongside the
  existing *unsharded* controller. It labels everything; nothing consumes the
  labels. Inspect the distribution with
  `kubectl get workspace -L terraform.crossplane.io/shard`. This is also how you
  find out whether something else owns `metadata.labels` on your Workspaces — if
  they are composed from XRs, a Composition reconcile may revert the label, in
  which case placement has to move into the Composition instead.
- **Phase 2.** Confirm backend locking. Bring up shard-0..3 with `--shard-name`
  (see `provider-shards.yaml`). Scale the unsharded controller to zero.
- **Phase 3.** Exercise a full drain of one shard in a non-production cluster
  before trusting it.

## Metrics

| Metric | Type | Meaning |
| --- | --- | --- |
| `terraform_shard_workspaces{shard}` | gauge | Workspaces owned by each active shard |
| `terraform_shard_workspaces_unlabelled` | gauge | Workspaces no shard reconciles |
| `terraform_shard_workspaces_inactive_shard` | gauge | Workspaces awaiting migration |
| `terraform_shard_workspaces_migrating` | gauge | Migrations in flight |
| `terraform_shard_drain_blocked{shard}` | gauge | 1 while a draining shard still has a running pod |
| `terraform_shard_migrations_started_total{from,to}` | counter | Relabels performed |
| `terraform_shard_migrations_completed_total{shard}` | counter | Migrations that synced |

Every provider-side metric — `terraform_provider_*`, plus Crossplane's managed
resource and state metrics — carries a constant `shard` label on a sharded
instance, so reconcile duration and Workspace counts break out per shard
without any dashboard changes beyond a `by (shard)`.

Alerts are in [`examples/shard-assigner/alerts.yaml`](../examples/shard-assigner/alerts.yaml).
The one that matters most is `TerraformWorkspaceUnsharded`: an unlabelled
Workspace is reconciled by nobody and fails silently otherwise.

## Not built

- **Automatic Deployment management for shards.** Adding a shard means adding a
  Deployment and bumping `shardCount`; nothing reconciles the two together.
- **Cost-weighted placement.** Placement counts objects. `Placer.cost` is the
  seam for weighting by observed reconcile duration — the reason this is an
  assigner rather than a hash — but it returns 1 for everything today.
- **Automatic rebalancing** on scale-up.
- **Structurally eliminating handover overlap.** That needs a per-Workspace
  lease acquired before apply and released after. Unnecessary once backend
  locking is confirmed.
- **A validating webhook** rejecting shard-label edits from anyone but the
  assigner's service account, so nobody hand-places a Workspace onto an
  overloaded shard.
