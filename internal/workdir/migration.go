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

package workdir

import (
	"context"
	"time"

	"github.com/pkg/errors"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// MigratingAtAnnotation records when the shard assigner relabelled a
	// Workspace onto a new shard, RFC3339. It marks a migration in flight,
	// which is what the migration batch limit counts.
	MigratingAtAnnotation = "terraform.crossplane.io/migrating-at"

	// MigrationReceivedAnnotation records when the new shard first set the
	// Workspace up, RFC3339, whether or not that succeeded. It is the
	// handover signal: the Workspace's status cannot be, because one that was
	// Synced=True on its old shard reports the same thing on its new one, so
	// status alone cannot tell a migration that has landed from one nobody
	// has picked up yet. The assigner removes both annotations once it sees
	// this one.
	MigrationReceivedAnnotation = "terraform.crossplane.io/migration-received"
)

const errMarkReceived = "cannot mark migration received"

// MarkMigrationReceived stamps MigrationReceivedAnnotation on a Workspace that
// is mid-migration and has not been stamped yet. It is a no-op otherwise, so
// the steady state costs no API call.
//
// obj is patched in place, so the caller's copy carries the new
// resourceVersion and a later status update does not conflict.
func MarkMigrationReceived(ctx context.Context, kube client.Client, obj client.Object, now time.Time) error {
	a := obj.GetAnnotations()
	if _, ok := a[MigratingAtAnnotation]; !ok {
		return nil
	}
	if _, ok := a[MigrationReceivedAnnotation]; ok {
		return nil
	}
	base, ok := obj.DeepCopyObject().(client.Object)
	if !ok {
		return errors.New(errMarkReceived)
	}
	a[MigrationReceivedAnnotation] = now.UTC().Format(time.RFC3339)
	obj.SetAnnotations(a)
	return errors.Wrap(kube.Patch(ctx, obj, client.MergeFrom(base)), errMarkReceived)
}
