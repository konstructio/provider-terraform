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
// Desired state lives in a ConfigMap:
//
//	data:
//	  shardCount: "4"
//	  draining: "shard-3"
//
// Active shards are shard-0 .. shard-{shardCount-1} minus anything draining.
// A Workspace's label is sticky: it is rewritten only when its current shard
// is draining or has fallen out of range.
package shard

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/pkg/errors"
)

const (
	// ShardLabel names the controller shard that owns a Workspace. Upstream
	// PR #288 consumes this label from the provider side as
	// workdir.ShardLabel; the two strings must stay identical.
	ShardLabel = "terraform.crossplane.io/shard"

	// MigratingAtAnnotation records when a Workspace was relabelled onto a
	// new shard, RFC3339. It is present only between the relabel and the
	// Workspace reporting Synced=True on its new owner, which is what makes
	// "one migration at a time" expressible level-triggered.
	MigratingAtAnnotation = "terraform.crossplane.io/migrating-at"
)

// ConfigMap keys.
const (
	KeyShardCount = "shardCount"
	KeyDraining   = "draining"
)

// Error strings.
const (
	errNoShardCount      = "config is missing the " + KeyShardCount + " key"
	errFmtBadShardCount  = "cannot parse " + KeyShardCount + " %q"
	errFmtNonPositive    = "shardCount must be at least 1, got %d"
	errFmtUnknownDrainer = "draining lists %q, which is not a valid shard name"
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

// A Config is the desired sharding state.
type Config struct {
	// Count is the number of shards that exist, shard-0 .. shard-{Count-1}.
	Count int

	// Draining holds shards that still own Workspaces but must not be given
	// any more.
	Draining map[string]bool
}

// ParseConfig reads a Config out of ConfigMap data.
func ParseConfig(data map[string]string) (Config, error) {
	raw, ok := data[KeyShardCount]
	if !ok {
		return Config{}, errors.New(errNoShardCount)
	}

	n, err := strconv.Atoi(strings.TrimSpace(raw))
	if err != nil {
		return Config{}, errors.Errorf(errFmtBadShardCount, raw)
	}
	if n < 1 {
		return Config{}, errors.Errorf(errFmtNonPositive, n)
	}

	cfg := Config{Count: n, Draining: map[string]bool{}}
	for _, d := range strings.Split(data[KeyDraining], ",") {
		d = strings.TrimSpace(d)
		if d == "" {
			continue
		}
		if _, ok := shardIndex(d); !ok {
			return Config{}, errors.Errorf(errFmtUnknownDrainer, d)
		}
		cfg.Draining[d] = true
	}
	return cfg, nil
}

// Active reports whether shard is one this Config still places Workspaces on.
// The empty string is never active, so an unlabelled Workspace always needs
// placement.
func (c Config) Active(shard string) bool {
	i, ok := shardIndex(shard)
	if !ok || i >= c.Count {
		return false
	}
	return !c.Draining[shard]
}

// ActiveShards lists the placeable shards in index order. Callers rely on that
// ordering to break ties deterministically.
func (c Config) ActiveShards() []string {
	out := make([]string, 0, c.Count)
	for i := range c.Count {
		if s := ShardName(i); !c.Draining[s] {
			out = append(out, s)
		}
	}
	return out
}
