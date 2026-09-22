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
		return reconcile.Result{}, a.completeMigration(ctx, ws)
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
				a.log.Debug("Not migrating: the current shard still has a running pod",
					"workspace", ws.GetName(), "shard", cur)
				return reconcile.Result{RequeueAfter: RequeueShardOnline}, nil
			}
			ShardDrainBlocked.WithLabelValues(cur).Set(0)
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

// completeMigration clears the migrating-at annotation once the Workspace has
// reported Synced=True on its new shard, which releases the gate for the next
// migration. It is a no-op for a Workspace that is not migrating.
func (a *Assigner) completeMigration(ctx context.Context, ws Workspace) error {
	if _, ok := ws.GetAnnotations()[MigratingAtAnnotation]; !ok {
		return nil
	}
	if !Synced(ws) {
		return nil
	}

	base, ok := ws.DeepCopyObject().(client.Object)
	if !ok {
		return errors.New(errClear)
	}
	patch := client.MergeFromWithOptions(base, client.MergeFromWithOptimisticLock{})

	annotations := ws.GetAnnotations()
	delete(annotations, MigratingAtAnnotation)
	ws.SetAnnotations(annotations)

	if err := a.kube.Patch(ctx, ws, patch); err != nil {
		return errors.Wrap(err, errClear)
	}

	shard := ws.GetLabels()[ShardLabel]
	MigrationsCompleted.WithLabelValues(shard).Inc()
	a.log.Info("Workspace synced on its new shard; migration complete",
		"kind", a.kind.Name, "workspace", ws.GetName(), "shard", shard)
	return nil
}
