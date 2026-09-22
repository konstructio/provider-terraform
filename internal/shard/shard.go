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

// Package shard assigns Workspaces to controller shards.
//
// provider-terraform can run as several controller instances, each watching
// only the Workspaces carrying its own shard name in the ShardLabel. Nothing
// in the provider writes that label; this package owns placement.
//
// Desired state is the shard Deployments themselves - there is no separate
// ConfigMap to drift from them:
//
//	Deployment exists, replicas > 0   -> active, placeable
//	Deployment exists, replicas == 0  -> draining, migrate away
//	Deployment absent                 -> removed, migrate away
//
// Expressing "draining" as replicas: 0 means it works for any shard, not only
// the highest-numbered one, and it is something a GitOps repo already knows
// how to say.
//
// A Workspace's label is sticky: it is rewritten only when its shard stops
// being active.
package shard

import (
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/upbound/provider-terraform/internal/workdir"
)

const (
	// ShardLabel names the controller shard that owns a Workspace. It is an
	// alias rather than a copy: the provider reads the same constant to filter
	// its informers, and a drift between writer and reader would silently
	// leave every Workspace unreconciled.
	//
	// It appears in three places, doing three jobs:
	//   - on a Workspace, it is the assignment
	//   - on a shard Deployment's own labels, it declares the shard exists
	//   - on that Deployment's pod template, it makes the shard's pods
	//     findable, which is how a migration knows the old process is gone
	ShardLabel = workdir.ShardLabel

	// MigratingAtAnnotation records when a Workspace was relabelled onto a
	// new shard, RFC3339. It is present only between the relabel and the
	// Workspace reporting Synced=True on its new owner, which is what makes
	// the migration batch limit expressible level-triggered.
	MigratingAtAnnotation = "terraform.crossplane.io/migrating-at"
)

// ShardName returns the canonical name of shard i.
func ShardName(i int) string { return fmt.Sprintf("shard-%d", i) }

// shardIndex parses a canonical shard name. It reports false for anything that
// is not exactly "shard-<non-negative integer>", so a hand-written label like
// "shard-3-old" is treated as unplaced rather than silently accepted.
func shardIndex(name string) (int, bool) {
	rest, ok := strings.CutPrefix(name, "shard-")
	if !ok || rest == "" {
		return 0, false
	}
	i, err := strconv.Atoi(rest)
	if err != nil || i < 0 || strconv.Itoa(i) != rest {
		return 0, false
	}
	return i, true
}

// A Fleet is the set of shards that are currently placeable, as declared by
// the shard Deployments.
type Fleet struct {
	active  map[string]bool
	ordered []string
}

// NewFleet returns a Fleet over the given active shard names. Names that are
// not canonical are dropped. The result is ordered by shard index so that
// placement ties break deterministically toward the lowest one.
func NewFleet(active []string) Fleet {
	f := Fleet{active: make(map[string]bool, len(active)), ordered: make([]string, 0, len(active))}
	for _, s := range active {
		if _, ok := shardIndex(s); !ok || f.active[s] {
			continue
		}
		f.active[s] = true
		f.ordered = append(f.ordered, s)
	}
	sort.Slice(f.ordered, func(i, j int) bool {
		a, _ := shardIndex(f.ordered[i])
		b, _ := shardIndex(f.ordered[j])
		return a < b
	})
	return f
}

// Active reports whether shard is one this Fleet still places Workspaces on.
// The empty string is never active, so an unlabelled Workspace always needs
// placement.
func (f Fleet) Active(shard string) bool { return f.active[shard] }

// ActiveShards lists the placeable shards in index order.
func (f Fleet) ActiveShards() []string { return f.ordered }
