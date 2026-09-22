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
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// WorkspacesPerShard is how many Workspaces each active shard owns. A
	// lopsided distribution shows up here.
	WorkspacesPerShard = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "terraform_shard_workspaces",
			Help: "Number of Workspaces assigned to each active shard",
		},
		[]string{"shard"},
	)

	// WorkspacesUnlabelled is how many Workspaces carry no shard label.
	// Nothing reconciles these, so a sustained non-zero value means the
	// assigner is wedged - the failure is otherwise silent.
	WorkspacesUnlabelled = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "terraform_shard_workspaces_unlabelled",
			Help: "Number of Workspaces with no shard label, which no shard reconciles",
		},
	)

	// WorkspacesInactiveShard is how many Workspaces are labelled to a shard
	// that is draining or out of range - migrations that have not run yet.
	WorkspacesInactiveShard = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "terraform_shard_workspaces_inactive_shard",
			Help: "Number of Workspaces labelled to a draining or out-of-range shard",
		},
	)

	// WorkspacesMigrating is how many Workspaces carry the migrating-at
	// annotation.
	WorkspacesMigrating = prometheus.NewGauge(
		prometheus.GaugeOpts{
			Name: "terraform_shard_workspaces_migrating",
			Help: "Number of Workspaces relabelled onto a new shard but not yet synced there",
		},
	)

	// ShardDrainBlocked is 1 while a shard that should be drained still has a
	// running pod, so its Workspaces cannot be migrated safely. It is the
	// signal that an operator needs to scale that Deployment to zero.
	ShardDrainBlocked = prometheus.NewGaugeVec(
		prometheus.GaugeOpts{
			Name: "terraform_shard_drain_blocked",
			Help: "1 while a draining shard still has a running pod, blocking migration of its Workspaces",
		},
		[]string{"shard"},
	)

	// MigrationsStarted counts relabels from one shard onto another.
	MigrationsStarted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "terraform_shard_migrations_started_total",
			Help: "Total Workspace migrations started, by source and target shard",
		},
		[]string{"from", "to"},
	)

	// MigrationsCompleted counts migrations that reached Synced=True on the
	// target shard.
	MigrationsCompleted = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "terraform_shard_migrations_completed_total",
			Help: "Total Workspace migrations that synced on their target shard",
		},
		[]string{"shard"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		WorkspacesPerShard,
		WorkspacesUnlabelled,
		WorkspacesInactiveShard,
		WorkspacesMigrating,
		ShardDrainBlocked,
		MigrationsStarted,
		MigrationsCompleted,
	)
}

// record publishes a Census to the gauges.
func record(c Census) {
	// Reset so a shard that has gone out of range stops reporting a stale
	// series rather than freezing at its last count.
	WorkspacesPerShard.Reset()
	for s, n := range c.PerShard {
		WorkspacesPerShard.WithLabelValues(s).Set(float64(n))
	}
	WorkspacesUnlabelled.Set(float64(c.Unlabelled))
	WorkspacesInactiveShard.Set(float64(c.Inactive))
	WorkspacesMigrating.Set(float64(c.Migrating))
}
