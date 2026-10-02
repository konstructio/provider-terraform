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

package gc

import (
	"path/filepath"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	"github.com/pkg/errors"
	"github.com/spf13/afero"
	ctrl "sigs.k8s.io/controller-runtime"

	"github.com/upbound/provider-terraform/internal/workdir"
)

// Error strings.
const errFmtNegativeInterval = "gc interval must be zero (disabled) or positive, got %s"

// An Option configures Setup. Using functional options here (rather than
// positional parameters) lets independent Setup features - e.g. a scan
// interval, a shard filter - be added without colliding on the function
// signature.
type Option func(*options)

type options struct {
	// interval is nil when the caller supplied no WithInterval option, in
	// which case the workdir package's own default (one hour) applies.
	interval *time.Duration

	// shardName is this instance's shard, empty when unsharded.
	shardName string
}

// WithInterval sets how often each garbage collector scans its directory.
// Zero disables the garbage collectors entirely - no directories are ever
// removed. A negative value is rejected by Setup. If this option isn't
// supplied at all, the workdir package's default of one hour applies.
func WithInterval(d time.Duration) Option {
	return func(o *options) { o.interval = &d }
}

// WithShardName sets the shard these garbage collectors run for. When set they
// also reclaim the working directories of Workspaces that have migrated to
// another shard; without it a drain leaks one directory per Workspace moved.
func WithShardName(name string) Option {
	return func(o *options) { o.shardName = name }
}

// Setup initializes and registers the garbage collectors with the manager.
//
// Two GC instances are created:
// - One for the main workspace directory containing workspace roots
// - One for /tmp directory containing temporary workspace files
//
// Each GC queries both cluster-scoped and namespaced workspaces to determine
// which directories can be safely deleted.
func Setup(mgr ctrl.Manager, tfDir string, logger logging.Logger, opts ...Option) error {
	o := &options{}
	for _, fn := range opts {
		fn(o)
	}

	if o.interval != nil && *o.interval == 0 {
		logger.Debug("Workspace directory garbage collection disabled (gc interval is zero)")
		return nil
	}

	if o.interval != nil && *o.interval < 0 {
		return errors.Errorf(errFmtNegativeInterval, o.interval.String())
	}

	fs := afero.Afero{Fs: afero.NewOsFs()}
	gcOpts := []workdir.GarbageCollectorOption{
		workdir.WithFs(fs),
		workdir.WithLogger(logger),
	}
	if o.interval != nil {
		gcOpts = append(gcOpts, workdir.WithInterval(*o.interval))
	}
	if o.shardName != "" {
		gcOpts = append(gcOpts, workdir.WithShardName(o.shardName))
	}

	// The API reader, not the manager's client: a sharded manager's cache only
	// holds this shard's Workspaces, and a garbage collector reading from it
	// would delete every other shard's working directories.
	reader := mgr.GetAPIReader()

	// GC for main workspace directory
	gcWorkspace := workdir.NewGarbageCollector(reader, tfDir, gcOpts...)
	if err := mgr.Add(gcWorkspace); err != nil {
		return err
	}

	// GC for temporary workspace directory
	gcTmp := workdir.NewGarbageCollector(reader, filepath.Join("/tmp", tfDir), gcOpts...)
	if err := mgr.Add(gcTmp); err != nil {
		return err
	}

	logger.Debug("Workspace garbage collectors initialized successfully", "interval", o.interval, "shard", o.shardName)

	return nil
}
