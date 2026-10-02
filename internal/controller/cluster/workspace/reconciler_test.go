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

package workspace

import (
	"context"
	"testing"
	"time"

	"github.com/crossplane/crossplane-runtime/v2/pkg/meta"
	"github.com/crossplane/crossplane-runtime/v2/pkg/reconciler/managed"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource"
	"github.com/crossplane/crossplane-runtime/v2/pkg/resource/fake"
	"github.com/crossplane/crossplane-runtime/v2/pkg/test"
	"github.com/pkg/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/upbound/provider-terraform/apis/cluster/v1beta1"
)

// TestReconcilerDefaultsRetryIncompleteCreate checks that a Workspace whose
// last create never recorded a result - external-create-pending newer than
// external-create-failed, as a shard pod stopping mid-create leaves it - is
// reconciled again rather than parked for good.
//
// It drives crossplane-runtime's real reconciler with the options Setup uses,
// and treats reaching Connect as "the reconcile carried on". The control case
// without them shows the same Workspace is otherwise stopped before Connect.
func TestReconcilerDefaultsRetryIncompleteCreate(t *testing.T) {
	cases := map[string]struct {
		reason      string
		opts        []managed.ReconcilerOption
		wantConnect bool
	}{
		"WithReconcilerDefaults": {
			reason:      "Terraform state makes a retried create safe, so an incomplete one must not stop reconciliation.",
			opts:        reconcilerDefaults,
			wantConnect: true,
		},
		"WithoutReconcilerDefaults": {
			reason:      "crossplane-runtime's default parks an incomplete create; this is what Workspaces did before.",
			opts:        nil,
			wantConnect: false,
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			now := time.Now()
			ws := &v1beta1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: "s3-dev-011"}}
			meta.SetExternalName(ws, "s3-dev-011")
			meta.SetExternalCreateFailed(ws, now.Add(-20*time.Minute))
			meta.SetExternalCreatePending(ws, now.Add(-10*time.Minute))
			if !meta.ExternalCreateIncomplete(ws) {
				t.Fatal("fixture: the Workspace must read as create-incomplete")
			}

			kube := &test.MockClient{
				MockGet: func(_ context.Context, _ client.ObjectKey, obj client.Object) error {
					ws.DeepCopyInto(obj.(*v1beta1.Workspace))
					return nil
				},
				MockUpdate:       test.NewMockUpdateFn(nil),
				MockPatch:        test.NewMockPatchFn(nil),
				MockStatusUpdate: test.NewMockSubResourceUpdateFn(nil),
				MockStatusPatch:  test.NewMockSubResourcePatchFn(nil),
			}
			s := runtime.NewScheme()
			if err := v1beta1.SchemeBuilder.AddToScheme(s); err != nil {
				t.Fatalf("cannot build scheme: %v", err)
			}
			mgr := &fake.Manager{Client: kube, Scheme: s}

			connected := false
			connector := managed.ExternalConnectorFn(func(context.Context, resource.Managed) (managed.ExternalClient, error) {
				connected = true
				return nil, errors.New("stop here")
			})

			opts := append([]managed.ReconcilerOption{managed.WithExternalConnector(connector)}, tc.opts...)
			r := managed.NewReconciler(mgr, resource.ManagedKind(v1beta1.WorkspaceGroupVersionKind), opts...)

			_, _ = r.Reconcile(context.Background(), reconcile.Request{NamespacedName: types.NamespacedName{Name: ws.Name}})

			if connected != tc.wantConnect {
				t.Errorf("Reconcile(...) reached Connect = %v, want %v\n%s", connected, tc.wantConnect, tc.reason)
			}
		})
	}
}
