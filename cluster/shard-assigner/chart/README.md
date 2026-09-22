# provider-terraform shard chart

Renders the sharded provider-terraform Deployments. `shardCount` is the only
knob you normally touch.

There is deliberately **no ConfigMap**. `spec.replicas` on these Deployments is
already the desired state, and mirroring it into a second object would only
create something to drift. The assigner reads them directly:

| State | Assigner reads it as |
| --- | --- |
| Deployment exists, `replicas > 0` | active, placeable |
| Deployment exists, `replicas: 0` | draining — migrate its Workspaces away |
| Deployment absent | removed — migrate its Workspaces away |

## Argo

`spec.replicas` needs an ignore rule, because the assigner patches it to drive
a drain it started itself:

```yaml
ignoreDifferences:
  - group: apps
    kind: Deployment
    name: provider-terraform-shard-*
    jsonPointers:
      - /spec/replicas
```

## Scaling

**Up:** raise `shardCount`. New Deployments appear; existing Workspaces do not
move. Placement is least-loaded, so new Workspaces land on the empty shards.

**Down (5 → 4):** lower `shardCount`. Argo prunes shard-4's Deployment, its pod
drains gracefully, and only once that pod is gone does the assigner migrate its
Workspaces — in batches of `--migration-batch` (default 5). Watch
`terraform_shard_drain_blocked{shard="shard-4"}`: it sits at 1 until the pod is
actually gone.

**Draining a shard in the middle:** add its index to `draining`. It renders with
`replicas: 0`, which the assigner treats the same way.
