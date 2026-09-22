# Workspace shard assigner

`provider-terraform` reconciles each `Workspace` by shelling out to
`terraform init/plan/apply`, which takes seconds to minutes. A single active
controller — the stock active/passive HA model — is therefore a throughput
bottleneck once you have hundreds of Workspaces.

Upstream [PR #288][288] lets several controller instances run at once, each
watching only the Workspaces labelled with its own shard name. **It only
consumes the label; nothing assigns it**, and a Workspace with no label is
reconciled by no shard at all. This assigner fills that gap: it owns placement.

[288]: https://github.com/crossplane-contrib/provider-terraform/pull/288

> **Status.** PR #288 is still open upstream and is not in this fork. The
> assigner is useful and safe to run without it (see Phase 1 below), but the
> labels it writes are inert until #288 lands and the provider is configured
> with `--shard-name`.

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
- **The label does not guarantee a single writer, and is not meant to.** When a
  Workspace moves from shard-3 to shard-0, shard-3 may be mid-apply; informer
  eviction is not ordered against shard-0 picking it up. The same overlap exists
  on any rolling restart, sharded or not. What protects Terraform state is the
  **backend lock**. The assigner's job is to keep the window narrow — one
  Workspace at a time — not to eliminate it.

  **Confirm backend locking before enabling sharding at all.** S3 needs a
  DynamoDB lock table or `use_lockfile = true` (Terraform ≥ 1.10). GCS and
  azurerm lock natively. An HTTP backend must implement `LOCK`/`UNLOCK`. Local
  state, or S3 with neither mechanism, is genuinely corruptible under handover.

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

Manifests live in [`examples/shard-assigner/`](../examples/shard-assigner/).

## Operational procedures

### Scale up (4 → 5)

1. Add the `shard-4` provider Deployment with `--shard-name=shard-4`.
2. Set `shardCount: "5"`.

New Workspaces start landing on shard-4 via least-loaded. **Existing Workspaces
do not move.** Rebalancing is a separate, deliberate action — there is no
automatic rebalance, by design.

### Scale down (remove shard-3)

1. Add `shard-3` to `draining`. The assigner stops placing new Workspaces there.
2. It relabels shard-3's Workspaces **one at a time**, waiting for each to reach
   `SYNCED=True` on its new shard before starting the next. Watch progress with
   `terraform_shard_workspaces_inactive_shard`.
3. Once shard-3 owns nothing, scale its Deployment to zero and drop
   `shardCount` to 3.

Two ways to get this wrong:

- **Do not delete the pod first.** Its Workspaces sit unreconciled until the
  assigner moves them, and nothing alerts on a shard that merely has no pod.
- **Do not drop `shardCount` before draining.** Every Workspace on the removed
  shard becomes out-of-range at once and the relabels all fire together, which
  is exactly the wide overlap the one-at-a-time gate exists to avoid.

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
- **Phase 2.** Confirm backend locking. Bring up shard-0..3 with `--shard-name`.
  Scale the unsharded controller to zero.
- **Phase 3.** Exercise a full drain of one shard in a non-production cluster
  before trusting it.

## Metrics

| Metric | Type | Meaning |
| --- | --- | --- |
| `terraform_shard_workspaces{shard}` | gauge | Workspaces owned by each active shard |
| `terraform_shard_workspaces_unlabelled` | gauge | Workspaces no shard reconciles |
| `terraform_shard_workspaces_inactive_shard` | gauge | Workspaces awaiting migration |
| `terraform_shard_workspaces_migrating` | gauge | Migrations in flight |
| `terraform_shard_migrations_started_total{from,to}` | counter | Relabels performed |
| `terraform_shard_migrations_completed_total{shard}` | counter | Migrations that synced |

Alerts are in [`examples/shard-assigner/alerts.yaml`](../examples/shard-assigner/alerts.yaml).
The one that matters most is `TerraformWorkspaceUnsharded`: an unlabelled
Workspace is reconciled by nobody and fails silently otherwise.

## Not built

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
