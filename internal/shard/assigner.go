/*
Copyright 2026 The Crossplane Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package shard

import (
	"context"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/pkg/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Error strings.
const (
	errGetWorkspace = "cannot get workspace"
	errAssign       = "cannot assign workspace to shard"
	errClear        = "cannot clear migration annotation"
)

// RequeueMigration is how long to wait before re-checking whether a batch slot
// has freed up. A Workspace waiting its turn simply asks again.
const RequeueMigration = 15 * time.Second

// RequeueShardOnline is how long to wait before re-checking whether a shard
// that is being drained has actually stopped. Longer than RequeueMigration:
// this one waits on an operator scaling a Deployment down, not on a reconcile
// finishing.
const RequeueShardOnline = 30 * time.Second

// An Assigner reconciles one Workspace kind, writing the shard label that
// decides which controller instance owns each Workspace.
//
// It is level-triggered and idempotent: the common case is a Workspace already
// sitting on an active shard, which returns without writing anything. A
// Workspace is relabelled only when it is unplaced, or when its current shard
// is draining or has fallen out of range.
type Assigner struct {
	kube   client.Client
	kind   Kind
	placer *Placer
	log    logging.Logger
	now    func() time.Time
}

// NewAssigner returns an Assigner for one Workspace kind. The placer is shared
// across kinds so placement decisions see every Workspace.
func NewAssigner(kube client.Client, k Kind, p *Placer, log logging.Logger) *Assigner {
	return &Assigner{kube: kube, kind: k, placer: p, log: log, now: p.now}
}

// Reconcile places a single Workspace.
func (a *Assigner) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	fleet, err := a.placer.LoadFleet(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}

	ws := a.kind.New()
	if err := a.kube.Get(ctx, req.NamespacedName, ws); err != nil {
		return reconcile.Result{}, errors.Wrap(client.IgnoreNotFound(err), errGetWorkspace)
	}

	// A Workspace being deleted keeps whatever shard it has; taking the label
	// away would strand it with no controller to run its destroy.
	if ws.GetDeletionTimestamp() != nil {
		return reconcile.Result{}, nil
	}

	cur := ws.GetLabels()[ShardLabel]
	if fleet.Active(cur) {
		// Happy path, and where the overwhelming majority of calls end.
		return a.placed(ctx, fleet, ws, cur)
	}

	// Needs placement: unlabelled, or its owner is draining or out of range.
	// The lock covers the whole decision, so the two kind controllers cannot
	// both pass the gates at once.
	defer a.placer.Lock()()

	if cur != "" {
		// Shards are separate pods, so a shard appearing in `draining` says
		// nothing about whether its process is still running. Relabelling a
		// Workspace off a live shard lets two terraform processes write the
		// same remote state - and an unlocked backend will not stop them.
		// Wait for the shard's pod to go away first.
		if a.placer.requireOffline {
			online, err := a.placer.ShardOnline(ctx, cur)
			if err != nil {
				// Fail closed: if we cannot tell, we do not migrate.
				return reconcile.Result{}, err
			}
			if online {
				ShardDrainBlocked.WithLabelValues(cur).Set(1)
				// Every Workspace on the shard re-checks every 30s, so log the
				// wait once per shard rather than once per Workspace per check.
				if a.placer.setDrainBlocked(cur, true) {
					a.log.Info("Not migrating off a shard whose pod is still running; waiting for it to exit",
						"shard", cur)
				}
				a.log.Debug("Not migrating: the current shard still has a running pod",
					"workspace", ws.GetName(), "shard", cur)
				return reconcile.Result{RequeueAfter: RequeueShardOnline}, nil
			}
			ShardDrainBlocked.WithLabelValues(cur).Set(0)
			if a.placer.setDrainBlocked(cur, false) {
				a.log.Info("Shard's pod has exited; migrating its Workspaces", "shard", cur)
			}
		}

		// Cap how many migrate at once. Overlap is not the concern here - the
		// old shard's pod is already gone - but a whole shard's Workspaces
		// arriving as one burst of terraform init would pile up on the
		// receiving shards' plugin-cache lock.
		n, err := a.placer.MigrationsInFlight(ctx)
		if err != nil {
			return reconcile.Result{}, err
		}
		if n >= a.placer.Batch() {
			return reconcile.Result{RequeueAfter: RequeueMigration}, nil
		}
	}

	target, err := a.placer.LeastLoaded(ctx, fleet)
	if err != nil {
		return reconcile.Result{}, err
	}
	return reconcile.Result{}, a.assign(ctx, ws, cur, target)
}

// placed handles a Workspace already on an active shard: it closes a migration
// the new shard has picked up and, with rebalancing on, may move the Workspace
// to a less-loaded shard.
func (a *Assigner) placed(ctx context.Context, fleet Fleet, ws Workspace, cur string) (reconcile.Result, error) {
	if err := a.completeMigration(ctx, ws); err != nil {
		return reconcile.Result{}, err
	}
	if !a.placer.rebalance {
		return reconcile.Result{}, nil
	}
	return a.rebalance(ctx, fleet, ws, cur)
}

// rebalance moves a Workspace off its active shard when that shard carries
// more than the tolerance more load than the least-loaded one - typically
// right after a scale-up, when the new shards are empty. Adding a shard
// Deployment re-evaluates every Workspace, so a scale-up starts this on its
// own; the informer resync keeps it going.
//
// The move goes through the ordinary migration path - migrating-at, the batch
// limit, the new shard's receipt - but off a shard whose pod is still running.
// That is safe only because Setup refuses to enable rebalancing unless the
// Terraform backend is confirmed to lock state: an apply already running on
// the old shard holds the lock, and the new shard's waits for it.
func (a *Assigner) rebalance(ctx context.Context, fleet Fleet, ws Workspace, cur string) (reconcile.Result, error) {
	// Still settling on this shard from a previous move; leave it be.
	if _, ok := ws.GetAnnotations()[MigratingAtAnnotation]; ok {
		return reconcile.Result{}, nil
	}

	defer a.placer.Lock()()

	target, move, err := a.placer.RebalanceTarget(ctx, fleet, cur)
	if err != nil || !move {
		return reconcile.Result{}, err
	}
	n, err := a.placer.MigrationsInFlight(ctx)
	if err != nil {
		return reconcile.Result{}, err
	}
	if n >= a.placer.Batch() {
		return reconcile.Result{RequeueAfter: RequeueMigration}, nil
	}
	a.log.Debug("Rebalancing workspace off an overloaded shard", "workspace", ws.GetName(), "from", cur, "to", target)
	return reconcile.Result{}, a.assign(ctx, ws, cur, target)
}

// assign writes the shard label, and on a migration the migrating-at
// annotation too. The patch carries a resourceVersion precondition, so a stale
// read cannot stomp a label someone else just wrote.
func (a *Assigner) assign(ctx context.Context, ws Workspace, cur, target string) error {
	base, ok := ws.DeepCopyObject().(client.Object)
	if !ok {
		return errors.New(errAssign)
	}
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})

	labels := ws.GetLabels()
	if labels == nil {
		labels = map[string]string{}
	}
	labels[ShardLabel] = target
	ws.SetLabels(labels)

	migration := cur != ""
	if migration {
		annotations := ws.GetAnnotations()
		if annotations == nil {
			annotations = map[string]string{}
		}
		annotations[MigratingAtAnnotation] = a.now().UTC().Format(time.RFC3339)
		// A receipt left from an earlier migration would complete this one
		// before the new shard has seen it.
		delete(annotations, MigrationReceivedAnnotation)
		ws.SetAnnotations(annotations)
	}

	if err := a.kube.Patch(ctx, ws, patch); err != nil {
		return errors.Wrap(err, errAssign)
	}

	if migration {
		MigrationsStarted.WithLabelValues(cur, target).Inc()
		a.log.Info("Migrated workspace to a new shard",
			"kind", a.kind.Name, "workspace", ws.GetName(), "from", cur, "to", target)
		return nil
	}
	a.log.Debug("Assigned workspace to a shard",
		"kind", a.kind.Name, "workspace", ws.GetName(), "shard", target)
	return nil
}

// completeMigration clears the migration annotations once the new shard has
// stamped MigrationReceivedAnnotation, which releases the batch slot for the
// next migration. It is a no-op for a Workspace that is not migrating.
//
// It deliberately does not look at Synced: a Workspace that was Synced=True on
// its old shard still reads Synced=True the instant it is relabelled, so that
// would complete every healthy migration before the new shard had run it.
func (a *Assigner) completeMigration(ctx context.Context, ws Workspace) error {
	annotations := ws.GetAnnotations()
	if _, ok := annotations[MigratingAtAnnotation]; !ok {
		return nil
	}
	if _, ok := annotations[MigrationReceivedAnnotation]; !ok {
		return nil
	}

	base, ok := ws.DeepCopyObject().(client.Object)
	if !ok {
		return errors.New(errClear)
	}
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})

	delete(annotations, MigratingAtAnnotation)
	delete(annotations, MigrationReceivedAnnotation)
	ws.SetAnnotations(annotations)

	if err := a.kube.Patch(ctx, ws, patch); err != nil {
		return errors.Wrap(err, errClear)
	}

	shard := ws.GetLabels()[ShardLabel]
	MigrationsCompleted.WithLabelValues(shard).Inc()
	a.log.Info("Workspace picked up by its new shard; migration complete",
		"kind", a.kind.Name, "workspace", ws.GetName(), "shard", shard)
	return nil
}
