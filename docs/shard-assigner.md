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
  when its shard stops being active. Normal operation never relabels.
- **A Workspace is never moved off a shard whose pod is still running.** This
  is the important one, and it is why the drain procedure below looks the way
  it does.

  Shards are separate pods, so separate processes. A Deployment scaled to zero
  or pruned says nothing about whether its pod is still running — it may be
  mid-`terraform apply` right now. Relabel a Workspace off it and two
  independent `terraform` processes are writing the same remote state. If that
  backend does not lock, nothing stops them: it is not a lock-contention error
  you can retry, it is a silent clobber.

  So before any migration the assigner lists pods carrying
  `terraform.crossplane.io/shard` and refuses to move a Workspace while its
  current shard still has one running. It **fails closed**: any error, or no
  shard-labelled pods at all, and nothing migrates. That last case is what a
  missing pod-template label looks like, and reading it as "everything is
  offline" would disable the check exactly when it matters.

  You cannot check this per-Workspace. A Crossplane managed resource has no
  "reconcile in progress" condition — `Synced=True` reports the last result,
  not whether one is running right now. "No process is acting as shard-3" is
  knowable; "no process is acting on W1" is not. Which is why the gate is at
  the shard level, and why the shard's process must fully drain *before* it
  disappears.

  `--require-shard-offline=false` opts out, and is only reasonable with backend
  locking confirmed: S3 with a DynamoDB lock table or `use_lockfile = true`
  (Terraform ≥ 1.10), GCS, or azurerm — all of which lock natively. Local
  state, or S3 with neither mechanism, is genuinely corruptible under handover.

  The residual overlap the lock still has to cover is informer eviction: when
  the label changes, the old shard's cache is not evicted in an order
  guaranteed against the new shard picking it up. With the old shard's pod
  already gone, there is no process left to act on that stale cache entry.

## Configuration

There is no ConfigMap. The shard **Deployments** are the desired state:

| State | Assigner reads it as |
| --- | --- |
| Deployment exists, `replicas > 0` | active, placeable |
| Deployment exists, `replicas: 0` | draining — migrate its Workspaces away |
| Deployment absent | removed — migrate its Workspaces away |

`spec.replicas` is already desired state; mirroring it into a second object
would only create something to drift from it. Expressing "draining" as
`replicas: 0` also works for any shard, not just the highest-numbered one — a
count can only remove from the top.

The shard label appears in three places, doing three different jobs:

- on a **Workspace** — the assignment itself
- on a shard **Deployment's own labels** — how the assigner discovers the shard
  exists and reads its replicas
- on that Deployment's **pod template** — how a migration confirms the old
  shard's process is gone

Miss the second and the assigner sees no fleet. Miss the third and it cannot
verify liveness and, failing closed, migrates nothing.

[`cluster/shard-assigner/chart/`](../cluster/shard-assigner/chart/) renders
all of it from one value:

```yaml
shardCount: 4
draining: []          # e.g. [shard-2] to drain one in the middle
```

Argo needs an ignore rule on `/spec/replicas`, since the assigner patches it to
drive a drain it started itself. Same thing you would do for an HPA.

### Assigner flags

| Flag | Default | Meaning |
| --- | --- | --- |
| `--namespace` | `crossplane-system` | Where the shard Deployments and their pods live. |
| `--require-shard-offline` | `true` | Refuse to migrate a Workspace while its current shard still has a running pod. |
| `--migration-batch` | `5` | How many Workspaces may migrate at once. |
| `--stale-migration` | `30m` | How long a Workspace that never syncs may hold a batch slot. |

## Operational procedures

### Scale up (4 → 5)

Raise `shardCount`. A new Deployment appears; **existing Workspaces do not
move**. Placement is least-loaded, so new Workspaces land on the empty shard.
Rebalancing existing ones is a separate, deliberate action.

### Scale down (5 → 4)

Lower `shardCount` — one commit, and the sequence runs itself:

1. Argo prunes shard-4's Deployment. Its pod gets SIGTERM.
2. The assigner sees shard-4 is no longer declared, so its Workspaces need
   placement — but the liveness gate finds shard-4's pod still terminating and
   **writes nothing**. `terraform_shard_drain_blocked{shard="shard-4"}` is 1.
3. The pod drains: in-flight applies finish inside
   `--graceful-shutdown-timeout`, bounded by `terminationGracePeriodSeconds`.
   The pod goes away.
4. The assigner migrates shard-4's Workspaces onto the survivors, `--migration-batch`
   at a time, least-loaded.

You do not have to sequence anything. The ordering that matters — pod gone
*before* relabel — is enforced by the gate, not by a runbook.

### Draining one shard in the middle

Add it to `draining` so it renders with `replicas: 0`. Same flow from step 2.

### Why the batch is 5 and not 1

Migrations only start once the old shard's pod is gone, so there is no handover
overlap to serialise against. The batch exists purely so a drained shard's
Workspaces do not arrive as one burst of `terraform init` on the receiving
shards, which serialise on the provider's plugin-cache lock. Strictly one at a
time would make a 40-Workspace drain take over an hour for no safety benefit.

A migration that never reaches `SYNCED=True` stops holding its slot after
`--stale-migration` (default 30m); the assigner logs loudly and moves on, so one
permanently broken Workspace cannot wedge a drain.

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
| `terraform_shard_without_pods{shard}` | gauge | 1 while an active shard has no running pod |
| `terraform_shard_migrations_started_total{from,to}` | counter | Relabels performed |
| `terraform_shard_migrations_completed_total{shard}` | counter | Migrations that synced |

Every provider-side metric — `terraform_provider_*`, plus Crossplane's managed
resource and state metrics — carries a constant `shard` label on a sharded
instance, so reconcile duration and Workspace counts break out per shard
without any dashboard changes beyond a `by (shard)`.

Alerts are in [`cluster/shard-assigner/alerts.yaml`](../cluster/shard-assigner/alerts.yaml).
The one that matters most is `TerraformWorkspaceUnsharded`: an unlabelled
Workspace is reconciled by nobody and fails silently otherwise.

## Not built

- **Creating or deleting shard Deployments.** The assigner only ever patches
  `spec.replicas` on a drain it started. Existence stays in GitOps, so image
  upgrades and `git revert` keep working normally. If shard count ever needs to
  autoscale on load, that is the piece to build.
- **Automatic migration off a shard that is up but has no pods.** Reported via
  `terraform_shard_without_pods` and alerted; doing it automatically would make
  every rolling restart look like a dead shard.
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
