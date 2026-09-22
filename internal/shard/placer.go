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
	"sync"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/pkg/errors"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Error strings.
const (
	errListWorkspaces  = "cannot list workspaces"
	errListShardDeploy = "cannot list shard deployments"
	errListShardPods   = "cannot list shard pods"
	errNoActiveShards  = "no active shards: every shard deployment is scaled to zero or being deleted"

	errFmtNoShardDeployments = "no deployment in namespace %q carries the %s label. Label every shard " +
		"Deployment with it - that label is how the assigner learns which shards exist"

	errFmtNoShardPods = "cannot verify which shards are running: no pod in namespace %q carries the %s label. " +
		"Label every shard Deployment's pod template with it, or set --require-shard-offline=false if the " +
		"Terraform backend is confirmed to lock state"
)

// DefaultStaleMigration is how long a migrating-at annotation may sit on a
// Workspace that never reaches Synced=True before the assigner stops counting
// it. Without a cutoff a single permanently broken Workspace would block a
// drain forever.
const DefaultStaleMigration = 30 * time.Minute

// DefaultMigrationBatch is how many Workspaces may be migrating at once.
//
// Handover overlap is not why this is limited: a migration only happens once
// the old shard's pod is gone, so there is no second writer. It is limited so
// a drained shard's Workspaces do not arrive as one burst of terraform init on
// the receiving shards, which serialise on the provider's plugin-cache lock.
const DefaultMigrationBatch = 5

// A Placer answers placement questions across every Workspace kind. One Placer
// is shared by the per-kind reconcilers, so load balancing and the migration
// batch limit see all Workspaces rather than only those of whichever kind
// happens to be reconciling.
type Placer struct {
	kube  client.Reader
	kinds []Kind
	log   logging.Logger

	// pods answers "is this shard's process still running". It must be a
	// live, uncached reader: a stale cache here decides whether two terraform
	// processes are allowed to touch the same state file. It also avoids
	// caching every Pod in the cluster.
	pods client.Reader

	// namespace is where the shard Deployments and their pods live.
	namespace string

	// requireOffline refuses to migrate a Workspace while its current shard
	// still has a running pod. Shards are separate processes, so a Deployment
	// scaled to zero says nothing about whether its pod is still mid-apply.
	requireOffline bool

	// batch is the most Workspaces that may be migrating at once.
	batch int

	// staleAfter bounds how long a stalled migration occupies a batch slot.
	staleAfter time.Duration

	// cost weights a Workspace when choosing the least-loaded shard. It
	// returns 1 for every Workspace today, which makes placement a plain
	// object count. It is the seam for weighting by observed reconcile
	// duration later - the reason this is an assigner rather than a hash.
	cost func(Workspace) float64

	now func() time.Time

	// mu serialises the read-decide-write sequence in placement. Both kind
	// controllers share one Placer and each runs its own worker, so without
	// this the batch limit can be exceeded by concurrent decisions.
	mu sync.Mutex
}

// A PlacerOption configures a Placer.
type PlacerOption func(*Placer)

// WithStaleMigration sets how long a stalled migration occupies a batch slot.
func WithStaleMigration(d time.Duration) PlacerOption {
	return func(p *Placer) { p.staleAfter = d }
}

// WithMigrationBatch sets how many Workspaces may migrate at once.
func WithMigrationBatch(n int) PlacerOption {
	return func(p *Placer) { p.batch = n }
}

// WithCost sets the weighting function used to pick the least-loaded shard.
func WithCost(f func(Workspace) float64) PlacerOption {
	return func(p *Placer) { p.cost = f }
}

// WithClock sets the clock, for tests.
func WithClock(f func() time.Time) PlacerOption {
	return func(p *Placer) { p.now = f }
}

// WithPodReader sets the live reader used to check whether a shard still has a
// running pod.
func WithPodReader(r client.Reader) PlacerOption {
	return func(p *Placer) { p.pods = r }
}

// WithNamespace sets where the shard Deployments and their pods live.
func WithNamespace(ns string) PlacerOption {
	return func(p *Placer) { p.namespace = ns }
}

// WithKinds sets the Workspace kinds to place. The default is every kind this
// package knows about, which is only correct on a cluster that serves them
// all - see installedKinds.
func WithKinds(k []Kind) PlacerOption {
	return func(p *Placer) { p.kinds = k }
}

// WithRequireShardOffline sets whether a Workspace may be migrated while its
// current shard still has a running pod.
//
// Keep this on unless the Terraform backend is confirmed to lock state. Shards
// are separate processes: relabelling a Workspace off a live shard lets two
// terraform processes write the same remote state, and an unlocked backend -
// S3 with neither a DynamoDB table nor use_lockfile, or local state - will not
// stop them.
func WithRequireShardOffline(v bool) PlacerOption {
	return func(p *Placer) { p.requireOffline = v }
}

// Lock serialises a placement decision. The caller must call the returned
// function once the decision has been written. The happy path - a Workspace
// already on an active shard - does not take this lock.
func (p *Placer) Lock() func() {
	p.mu.Lock()
	return p.mu.Unlock
}

// NewPlacer returns a Placer. kube reads Workspaces and shard Deployments and
// may be cached; the pod reader set by WithPodReader must not be.
func NewPlacer(kube client.Reader, log logging.Logger, o ...PlacerOption) *Placer {
	p := &Placer{
		kube:           kube,
		kinds:          Kinds(),
		log:            log,
		namespace:      "crossplane-system",
		requireOffline: true,
		batch:          DefaultMigrationBatch,
		staleAfter:     DefaultStaleMigration,
		cost:           func(Workspace) float64 { return 1 },
		now:            time.Now,
	}
	for _, fn := range o {
		fn(p)
	}
	return p
}

// Batch returns the migration batch limit.
func (p *Placer) Batch() int { return p.batch }

// LoadFleet reads the declared shards straight off the shard Deployments.
// There is no ConfigMap: spec.replicas is already the desired state, and
// mirroring it into a second object would only create something to drift.
//
// A Deployment that is being deleted does not count - Argo's prune leaves it
// around briefly while its pods drain, and it is not placeable in that window.
func (p *Placer) LoadFleet(ctx context.Context) (Fleet, error) {
	list := &appsv1.DeploymentList{}
	if err := p.kube.List(ctx, list,
		client.InNamespace(p.namespace),
		client.HasLabels{ShardLabel},
	); err != nil {
		return Fleet{}, errors.Wrap(err, errListShardDeploy)
	}
	if len(list.Items) == 0 {
		return Fleet{}, errors.Errorf(errFmtNoShardDeployments, p.namespace, ShardLabel)
	}

	active := make([]string, 0, len(list.Items))
	for _, d := range list.Items {
		name := d.Labels[ShardLabel]
		if _, ok := shardIndex(name); !ok {
			p.log.Debug("Ignoring deployment with a non-canonical shard label",
				"deployment", d.Name, ShardLabel, name)
			continue
		}
		if d.DeletionTimestamp != nil {
			continue
		}
		if d.Spec.Replicas == nil || *d.Spec.Replicas < 1 {
			continue
		}
		active = append(active, name)
	}
	return NewFleet(active), nil
}

// OnlineShards returns the set of shards that still have a running pod.
//
// It fails closed: any error and the caller must not migrate anything.
//
// Finding no shard-labelled pods at all is treated as an error rather than as
// "everything is offline". That is what a missing pod-template label looks
// like, and silently reading it as "safe to migrate" would defeat the check
// exactly when it matters.
func (p *Placer) OnlineShards(ctx context.Context) (map[string]bool, error) {
	pods := &corev1.PodList{}
	if err := p.pods.List(ctx, pods,
		client.InNamespace(p.namespace),
		client.HasLabels{ShardLabel},
	); err != nil {
		return nil, errors.Wrap(err, errListShardPods)
	}
	if len(pods.Items) == 0 {
		return nil, errors.Errorf(errFmtNoShardPods, p.namespace, ShardLabel)
	}

	online := map[string]bool{}
	for _, pod := range pods.Items {
		// Succeeded and Failed mean every container has terminated, so no
		// terraform is running. Anything else - including a pod still
		// terminating inside its grace period - counts as online, because
		// that is exactly when an apply may still be in flight.
		if pod.Status.Phase == corev1.PodSucceeded || pod.Status.Phase == corev1.PodFailed {
			continue
		}
		online[pod.Labels[ShardLabel]] = true
	}
	return online, nil
}

// ShardOnline reports whether shard still has a pod running. It is the gate on
// every migration: shards are separate processes, and relabelling a Workspace
// off a shard whose pod is alive lets two terraform processes write the same
// remote state. With an unlocked backend that is silent corruption, not a lock
// contention error.
func (p *Placer) ShardOnline(ctx context.Context, shard string) (bool, error) {
	online, err := p.OnlineShards(ctx)
	if err != nil {
		return false, err
	}
	return online[shard], nil
}

// forEach calls fn for every Workspace of every kind.
func (p *Placer) forEach(ctx context.Context, fn func(Workspace)) error {
	for _, k := range p.kinds {
		l := k.NewList()
		if err := p.kube.List(ctx, l); err != nil {
			return errors.Wrap(err, errListWorkspaces)
		}
		for _, ws := range k.Items(l) {
			fn(ws)
		}
	}
	return nil
}

// LeastLoaded returns the active shard carrying the least load. Ties break
// toward the lowest shard index, so the choice is deterministic.
func (p *Placer) LeastLoaded(ctx context.Context, f Fleet) (string, error) {
	active := f.ActiveShards()
	if len(active) == 0 {
		return "", errors.New(errNoActiveShards)
	}

	load := make(map[string]float64, len(active))
	for _, s := range active {
		load[s] = 0
	}
	if err := p.forEach(ctx, func(ws Workspace) {
		s := ws.GetLabels()[ShardLabel]
		if _, ok := load[s]; ok {
			load[s] += p.cost(ws)
		}
	}); err != nil {
		return "", err
	}

	// ActiveShards is index-ordered, so keeping the first strict minimum
	// gives the lowest-index winner.
	best := active[0]
	for _, s := range active[1:] {
		if load[s] < load[best] {
			best = s
		}
	}
	return best, nil
}

// MigrationsInFlight counts Workspaces that have been relabelled onto a new
// shard but have not yet reported Synced=True there. A migration whose
// annotation is older than staleAfter, or unparseable, is logged and not
// counted, so it cannot hold a batch slot forever.
func (p *Placer) MigrationsInFlight(ctx context.Context) (int, error) {
	n := 0
	err := p.forEach(ctx, func(ws Workspace) {
		at, ok := ws.GetAnnotations()[MigratingAtAnnotation]
		if !ok || Synced(ws) {
			return
		}
		started, err := time.Parse(time.RFC3339, at)
		if err != nil {
			p.log.Info("Ignoring unparseable migration annotation",
				"workspace", ws.GetName(), MigratingAtAnnotation, at, "error", err)
			return
		}
		if age := p.now().Sub(started); age > p.staleAfter {
			p.log.Info("Migration is stale and no longer holds a batch slot; the workspace has not synced on its new shard",
				"workspace", ws.GetName(), "shard", ws.GetLabels()[ShardLabel],
				"age", age.String(), "cutoff", p.staleAfter.String())
			return
		}
		n++
	})
	return n, err
}

// A Census is a point-in-time count of where Workspaces sit.
type Census struct {
	// PerShard counts Workspaces on each active shard.
	PerShard map[string]int

	// Unlabelled counts Workspaces with no shard label. Nothing reconciles
	// these, so any non-zero value is a wedged assigner.
	Unlabelled int

	// Inactive counts Workspaces labelled to a shard that is no longer
	// active - a migration that has not happened yet.
	Inactive int

	// Migrating counts Workspaces carrying the migrating-at annotation.
	Migrating int
}

// Census counts Workspaces by placement state.
func (p *Placer) Census(ctx context.Context, f Fleet) (Census, error) {
	c := Census{PerShard: map[string]int{}}
	for _, s := range f.ActiveShards() {
		c.PerShard[s] = 0
	}
	err := p.forEach(ctx, func(ws Workspace) {
		if _, ok := ws.GetAnnotations()[MigratingAtAnnotation]; ok {
			c.Migrating++
		}
		s := ws.GetLabels()[ShardLabel]
		switch {
		case s == "":
			c.Unlabelled++
		case f.Active(s):
			c.PerShard[s]++
		default:
			c.Inactive++
		}
	})
	return c, err
}
