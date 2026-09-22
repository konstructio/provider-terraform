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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// Error strings.
const (
	errGetConfig      = "cannot get shard config"
	errParseConfig    = "cannot parse shard config"
	errListWorkspaces = "cannot list workspaces"
	errNoActiveShards = "no active shards: every shard is draining or shardCount is unset"
	errListShardPods  = "cannot list shard pods"

	errFmtNoShardPods = "cannot verify which shards are running: no pod in namespace %q carries the %s label. " +
		"Label every shard Deployment's pod template with it, or set --require-shard-offline=false if the " +
		"Terraform backend is confirmed to lock state"
)

// DefaultStaleMigration is how long a migrating-at annotation may sit on a
// Workspace that never reaches Synced=True before the assigner stops counting
// it. Without a cutoff a single permanently broken Workspace would block a
// drain forever.
const DefaultStaleMigration = 30 * time.Minute

// A Placer answers placement questions across every Workspace kind. One Placer
// is shared by the per-kind reconcilers, so load balancing and the
// one-at-a-time migration gate see all Workspaces rather than only those of
// whichever kind happens to be reconciling.
type Placer struct {
	kube   client.Reader
	kinds  []Kind
	cfgRef types.NamespacedName
	log    logging.Logger

	// pods answers "is this shard's process still running". It must be a
	// live, uncached reader: a stale cache here decides whether two terraform
	// processes are allowed to touch the same state file. It also avoids
	// caching every Pod in the cluster.
	pods client.Reader

	// podNamespace is where the sharded provider Deployments run.
	podNamespace string

	// requireOffline refuses to migrate a Workspace while its current shard
	// still has a running pod. Shards are separate processes, so a shard being
	// listed in draining says nothing about whether it is still mid-apply.
	requireOffline bool

	// staleAfter bounds how long a stalled migration blocks the drain.
	staleAfter time.Duration

	// cost weights a Workspace when choosing the least-loaded shard. It
	// returns 1 for every Workspace today, which makes placement a plain
	// object count. It is the seam for weighting by observed reconcile
	// duration later - the reason this is an assigner rather than a hash.
	cost func(Workspace) float64

	now func() time.Time

	// mu serialises the read-decide-write sequence in placement. Both kind
	// controllers share one Placer and each runs its own worker, so without
	// this two Workspaces can clear the one-at-a-time migration gate
	// concurrently and migrate together.
	mu sync.Mutex
}

// Lock serialises a placement decision. The caller must call the returned
// function once the decision has been written. The happy path - a Workspace
// already on an active shard - does not take this lock.
func (p *Placer) Lock() func() {
	p.mu.Lock()
	return p.mu.Unlock
}

// A PlacerOption configures a Placer.
type PlacerOption func(*Placer)

// WithStaleMigration sets how long a stalled migration blocks further ones.
func WithStaleMigration(d time.Duration) PlacerOption {
	return func(p *Placer) { p.staleAfter = d }
}

// WithCost sets the weighting function used to pick the least-loaded shard.
func WithCost(f func(Workspace) float64) PlacerOption {
	return func(p *Placer) { p.cost = f }
}

// WithClock sets the clock, for tests.
func WithClock(f func() time.Time) PlacerOption {
	return func(p *Placer) { p.now = f }
}

// WithPodReader sets the live reader and namespace used to check whether a
// shard still has a running pod.
func WithPodReader(r client.Reader, namespace string) PlacerOption {
	return func(p *Placer) { p.pods, p.podNamespace = r, namespace }
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

// NewPlacer returns a Placer reading desired state from the named ConfigMap.
// kube must be an uncached reader, or a reader whose cache is not shard
// filtered: a Placer that cannot see other shards' Workspaces will pile every
// new Workspace onto one shard.
func NewPlacer(kube client.Reader, cfgRef types.NamespacedName, log logging.Logger, o ...PlacerOption) *Placer {
	p := &Placer{
		kube:           kube,
		kinds:          Kinds(),
		cfgRef:         cfgRef,
		log:            log,
		staleAfter:     DefaultStaleMigration,
		cost:           func(Workspace) float64 { return 1 },
		now:            time.Now,
		requireOffline: true,
		podNamespace:   "crossplane-system",
	}
	for _, fn := range o {
		fn(p)
	}
	return p
}

// LoadConfig reads the desired sharding state.
func (p *Placer) LoadConfig(ctx context.Context) (Config, error) {
	cm := &corev1.ConfigMap{}
	if err := p.kube.Get(ctx, p.cfgRef, cm); err != nil {
		return Config{}, errors.Wrap(err, errGetConfig)
	}
	cfg, err := ParseConfig(cm.Data)
	return cfg, errors.Wrap(err, errParseConfig)
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
func (p *Placer) LeastLoaded(ctx context.Context, cfg Config) (string, error) {
	active := cfg.ActiveShards()
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
// counted, so it cannot wedge a drain.
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
			p.log.Info("Migration is stale and no longer blocking the drain; the workspace has not synced on its new shard",
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

	// Inactive counts Workspaces labelled to a shard that is draining or out
	// of range - a migration that has not happened yet.
	Inactive int

	// Migrating counts Workspaces carrying the migrating-at annotation.
	Migrating int
}

// Census counts Workspaces by placement state.
func (p *Placer) Census(ctx context.Context, cfg Config) (Census, error) {
	c := Census{PerShard: map[string]int{}}
	for _, s := range cfg.ActiveShards() {
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
		case cfg.Active(s):
			c.PerShard[s]++
		default:
			c.Inactive++
		}
	})
	return c, err
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
		client.InNamespace(p.podNamespace),
		client.HasLabels{ShardLabel},
	); err != nil {
		return nil, errors.Wrap(err, errListShardPods)
	}
	if len(pods.Items) == 0 {
		return nil, errors.Errorf(errFmtNoShardPods, p.podNamespace, ShardLabel)
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
