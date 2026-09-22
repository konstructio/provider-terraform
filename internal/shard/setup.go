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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// DefaultCensusInterval is how often placement gauges are refreshed.
const DefaultCensusInterval = 30 * time.Second

// SetupOptions configures Setup.
type SetupOptions struct {
	// ConfigRef names the ConfigMap holding shardCount and draining.
	ConfigRef types.NamespacedName

	// StaleMigration bounds how long a stalled migration blocks a drain.
	// Zero selects DefaultStaleMigration.
	StaleMigration time.Duration

	// CensusInterval is how often the placement gauges are refreshed. Zero
	// selects DefaultCensusInterval.
	CensusInterval time.Duration
}

// Setup registers an assigner for every Workspace kind, plus the periodic
// census that drives the placement gauges.
//
// The manager's cache must NOT be shard filtered: the assigner needs to see
// every Workspace to balance across shards.
func Setup(mgr ctrl.Manager, log logging.Logger, o SetupOptions) error {
	if o.StaleMigration == 0 {
		o.StaleMigration = DefaultStaleMigration
	}
	if o.CensusInterval == 0 {
		o.CensusInterval = DefaultCensusInterval
	}

	placer := NewPlacer(mgr.GetClient(), o.ConfigRef, log, WithStaleMigration(o.StaleMigration))

	for _, k := range Kinds() {
		a := NewAssigner(mgr.GetClient(), k, placer, log.WithValues("kind", k.Name))

		if err := ctrl.NewControllerManagedBy(mgr).
			Named("shard-assigner-"+k.Name).
			For(k.New()).
			Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(enqueueAll(mgr.GetClient(), k, o.ConfigRef, log))).
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

// enqueueAll maps a change to the config ConfigMap onto every Workspace of one
// kind, so a shardCount or draining edit is re-evaluated everywhere.
func enqueueAll(kube client.Client, k Kind, cfgRef types.NamespacedName, log logging.Logger) handler.MapFunc {
	return func(ctx context.Context, o client.Object) []reconcile.Request {
		if o.GetNamespace() != cfgRef.Namespace || o.GetName() != cfgRef.Name {
			return nil
		}
		l := k.NewList()
		if err := kube.List(ctx, l); err != nil {
			log.Info("Cannot list workspaces after a shard config change", "kind", k.Name, "error", err)
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
		log.Debug("Shard config changed; re-evaluating every workspace", "kind", k.Name, "count", len(reqs))
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
	cfg, err := c.placer.LoadConfig(ctx)
	if err != nil {
		c.log.Info("Cannot load shard config for census", "error", err)
		return
	}
	got, err := c.placer.Census(ctx, cfg)
	if err != nil {
		c.log.Info("Cannot take workspace census", "error", err)
		return
	}
	record(got)
}

// NeedLeaderElection keeps the census on the active replica only.
func (c *census) NeedLeaderElection() bool { return true }
