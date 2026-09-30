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
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/crossplane/crossplane-runtime/v2/pkg/test"

	clusterv1beta1 "github.com/upbound/provider-terraform/apis/cluster/v1beta1"
)

func TestMarkMigrationReceived(t *testing.T) {
	now := time.Date(2026, 9, 30, 10, 0, 0, 0, time.UTC)
	stamp := now.Format(time.RFC3339)

	cases := map[string]struct {
		reason      string
		annotations map[string]string
		// wantPatch is whether the Workspace should be patched at all.
		wantPatch bool
		want      map[string]string
	}{
		"NotMigratingIsANoOp": {
			reason:      "The steady state must cost no API call.",
			annotations: map[string]string{"other": "x"},
			want:        map[string]string{"other": "x"},
		},
		"NoAnnotationsIsANoOp": {
			reason: "A Workspace with no annotations at all is not migrating.",
			want:   nil,
		},
		"MigratingIsStamped": {
			reason:      "The new shard's first reconcile records the handover.",
			annotations: map[string]string{MigratingAtAnnotation: "2026-09-30T09:59:00Z"},
			wantPatch:   true,
			want: map[string]string{
				MigratingAtAnnotation:       "2026-09-30T09:59:00Z",
				MigrationReceivedAnnotation: stamp,
			},
		},
		"AlreadyReceivedIsANoOp": {
			reason: "Every later reconcile before the assigner clears it must not re-patch.",
			annotations: map[string]string{
				MigratingAtAnnotation:       "2026-09-30T09:59:00Z",
				MigrationReceivedAnnotation: "2026-09-30T09:59:30Z",
			},
			want: map[string]string{
				MigratingAtAnnotation:       "2026-09-30T09:59:00Z",
				MigrationReceivedAnnotation: "2026-09-30T09:59:30Z",
			},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			ws := &clusterv1beta1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "a", Annotations: tc.annotations}}
			patched := false
			kube := &test.MockClient{
				MockPatch: func(_ context.Context, _ client.Object, _ client.Patch, _ ...client.PatchOption) error {
					patched = true
					return nil
				},
			}

			if err := MarkMigrationReceived(context.Background(), kube, ws, now); err != nil {
				t.Fatalf("MarkMigrationReceived(...): unexpected error: %v\n%s", err, tc.reason)
			}
			if patched != tc.wantPatch {
				t.Errorf("MarkMigrationReceived(...): patched=%v, want %v\n%s", patched, tc.wantPatch, tc.reason)
			}
			if diff := cmp.Diff(tc.want, ws.GetAnnotations()); diff != "" {
				t.Errorf("MarkMigrationReceived(...): -want annotations, +got:\n%s\n%s", diff, tc.reason)
			}
		})
	}
}
