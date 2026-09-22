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
	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/apiutil"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// DefaultCensusInterval is how often placement gauges are refreshed.
const DefaultCensusInterval = 30 * time.Second

// errNoWorkspaceAPI reports that the cluster serves no Workspace API at all,
// which leaves the assigner nothing to place.
const errNoWorkspaceAPI = "no Workspace API is installed; nothing to place"

// installedKinds returns the Workspace kinds the cluster serves. A kind whose
// CRD is absent is logged and dropped rather than treated as an error: a
// cluster may legitimately serve only one of the two.
func installedKinds(mapper meta.RESTMapper, s *runtime.Scheme, log logging.Logger) ([]Kind, error) {
	all := Kinds()
	out := make([]Kind, 0, len(all))
	for _, k := range all {
		gvk, err := apiutil.GVKForObject(k.New(), s)
		if err != nil {
			return nil, errors.Wrap(err, "cannot resolve Workspace GroupVersionKind")
		}
		switch _, err := mapper.RESTMapping(gvk.GroupKind(), gvk.Version); {
		case err == nil:
			out = append(out, k)
		case meta.IsNoMatchError(err):
			log.Info("Workspace API not installed; the assigner will not place it. Restart it if the CRD is installed later.",
				"kind", k.Name, "gvk", gvk.String())
		default:
			return nil, errors.Wrapf(err, "cannot look up a REST mapping for %s", gvk)
		}
	}
	return out, nil
}

// SetupOptions configures Setup.
type SetupOptions struct {
	// Namespace is where the shard Deployments and their pods live.
	Namespace string

	// StaleMigration bounds how long a stalled migration holds a batch slot.
	// Zero selects DefaultStaleMigration.
	StaleMigration time.Duration

	// MigrationBatch is how many Workspaces may migrate at once. Zero selects
	// DefaultMigrationBatch.
	MigrationBatch int

	// CensusInterval is how often the placement gauges are refreshed. Zero
	// selects DefaultCensusInterval.
	CensusInterval time.Duration

	// RequireShardOffline refuses to migrate a Workspace while its current
	// shard still has a running pod. Keep it on unless the Terraform backend
	// is confirmed to lock state.
	RequireShardOffline bool
}

// Setup registers an assigner for every Workspace kind, plus the periodic
// census that drives the placement gauges.
//
// The manager's cache must NOT be shard filtered: the assigner balances across
// shards, so it must see every Workspace.
func Setup(mgr ctrl.Manager, log logging.Logger, o SetupOptions) error {
	if o.StaleMigration == 0 {
		o.StaleMigration = DefaultStaleMigration
	}
	if o.CensusInterval == 0 {
		o.CensusInterval = DefaultCensusInterval
	}
	if o.MigrationBatch == 0 {
		o.MigrationBatch = DefaultMigrationBatch
	}

	// Only place the Workspace kinds the cluster actually serves. A cluster
	// running a provider package that predates the namespaced Workspace API
	// has only the cluster-scoped one, and naming the other would make every
	// list fail - so LeastLoaded, the migration gate and the census would all
	// error, and the controller for it would log "if kind is a CRD, it should
	// be installed before calling Start" forever.
	kinds, err := installedKinds(mgr.GetRESTMapper(), mgr.GetScheme(), log)
	if err != nil {
		return err
	}
	if len(kinds) == 0 {
		return errors.New(errNoWorkspaceAPI)
	}

	placer := NewPlacer(mgr.GetClient(), log,
		WithKinds(kinds),
		WithNamespace(o.Namespace),
		WithStaleMigration(o.StaleMigration),
		WithMigrationBatch(o.MigrationBatch),
		WithRequireShardOffline(o.RequireShardOffline),
		// The API reader, not the cache: shard liveness decides whether two
		// terraform processes may touch the same state, so it must not be
		// answered from a possibly stale cache - and caching every Pod in the
		// cluster would be a serious memory cost for one boolean.
		WithPodReader(mgr.GetAPIReader()),
	)

	for _, k := range kinds {
		a := NewAssigner(mgr.GetClient(), k, placer, log.WithValues("kind", k.Name))

		if err := ctrl.NewControllerManagedBy(mgr).
			Named("shard-assigner-"+k.Name).
			For(k.New()).
			// Shard Deployments are the desired state. Argo scaling one to
			// zero, or pruning it, arrives here as an update or delete and
			// re-evaluates every Workspace - the same job the old ConfigMap
			// watch did, without a second object to drift from this one.
			Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(enqueueAll(mgr.GetClient(), k, o.Namespace, log))).
			// One worker per kind. Placement is a low-rate control loop and
			// the decision is serialised anyway; extra workers would only
			// add contention on the placement lock.
			WithOptions(controller.Options{MaxConcurrentReconciles: 1}).
			Complete(a); err != nil {
			return err
		}
	}

	return mgr.Add(&census{placer: placer, interval: o.CensusInterval, log: log})
}

// enqueueAll maps a change to a shard Deployment onto every Workspace of one
// kind, so a scale-to-zero or a prune is re-evaluated everywhere.
func enqueueAll(kube client.Client, k Kind, namespace string, log logging.Logger) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		if o.GetNamespace() != namespace {
			return nil
		}
		if _, ok := o.GetLabels()[ShardLabel]; !ok {
			return nil
		}
		l := k.NewList()
		if err := kube.List(ctx, l); err != nil {
			log.Info("Cannot list workspaces after a shard deployment change", "kind", k.Name, "error", err)
			return nil
		}
		items := k.Items(l)
		reqs := make([]reconcile.Request, 0, len(items))
		for _, ws := range items {
			reqs = append(reqs, reconcile.Request{NamespacedName: types.NamespacedName{
				Namespace: ws.GetNamespace(),
				Name:      ws.GetName(),
			}})
		}
		log.Debug("Shard deployment changed; re-evaluating every workspace",
			"kind", k.Name, "deployment", o.GetName(), "count", len(reqs))
		return reqs
	}
}

// census refreshes the placement gauges on an interval. Reconcile-driven
// updates would miss the states that matter most here - an unlabelled
// Workspace nothing reconciles, or a drain that has stalled.
type census struct {
	placer   *Placer
	interval time.Duration
	log      logging.Logger
}

// Start runs until ctx is cancelled.
func (c *census) Start(ctx context.Context) error {
	t := time.NewTicker(c.interval)
	defer t.Stop()
	for {
		c.run(ctx)
		select {
		case <-ctx.Done():
			return nil
		case <-t.C:
		}
	}
}

func (c *census) run(ctx context.Context) {
	fleet, err := c.placer.LoadFleet(ctx)
	if err != nil {
		c.log.Info("Cannot load the shard fleet for census", "error", err)
		return
	}
	got, err := c.placer.Census(ctx, fleet)
	if err != nil {
		c.log.Info("Cannot take workspace census", "error", err)
		return
	}
	record(got)

	online, err := c.placer.OnlineShards(ctx)
	if err != nil {
		c.log.Info("Cannot determine which shards are running", "error", err)
		return
	}

	// An active shard with no running pod is desired-state-says-alive,
	// reality-says-dead: crash-looping, unschedulable, or stuck. Nothing else
	// catches it, because its Workspaces do carry a label and that label does
	// point at an active shard. Report it and let a human decide - migrating
	// automatically would make every rolling restart look like a dead shard.
	for _, s := range fleet.ActiveShards() {
		missing := 0.0
		if !online[s] {
			missing = 1
		}
		ShardWithoutPods.WithLabelValues(s).Set(missing)
	}
}

// NeedLeaderElection keeps the census on the active replica only.
func (c *census) NeedLeaderElection() bool { return true }
