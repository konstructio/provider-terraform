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
	"testing"
	"time"

	"fmt"

	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	"github.com/google/go-cmp/cmp"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	"github.com/crossplane/crossplane-runtime/v2/pkg/test"

	clusterv1beta1 "github.com/upbound/provider-terraform/apis/cluster/v1beta1"
	namespacedv1beta1 "github.com/upbound/provider-terraform/apis/namespaced/v1beta1"
)

var (
	testConfigRef = types.NamespacedName{Namespace: "crossplane-system", Name: "provider-terraform-shards"}
	testNow       = time.Date(2026, 9, 22, 12, 0, 0, 0, time.UTC)
)

// cws builds a cluster-scoped Workspace with the given shard label.
func cws(name, shard string, opts ...func(client.Object)) clusterv1beta1.Workspace {
	w := clusterv1beta1.Workspace{ObjectMeta: metav1.ObjectMeta{Name: name, ResourceVersion: "1"}}
	if shard != "" {
		w.Labels = map[string]string{ShardLabel: shard}
	}
	for _, fn := range opts {
		fn(&w)
	}
	return w
}

// nws builds a namespaced Workspace with the given shard label.
func nws(namespace, name, shard string, opts ...func(client.Object)) namespacedv1beta1.Workspace {
	w := namespacedv1beta1.Workspace{ObjectMeta: metav1.ObjectMeta{Namespace: namespace, Name: name, ResourceVersion: "1"}}
	if shard != "" {
		w.Labels = map[string]string{ShardLabel: shard}
	}
	for _, fn := range opts {
		fn(&w)
	}
	return w
}

func migrating(at time.Time) func(client.Object) {
	return func(o client.Object) {
		o.SetAnnotations(map[string]string{MigratingAtAnnotation: at.UTC().Format(time.RFC3339)})
	}
}

func migratingRaw(v string) func(client.Object) {
	return func(o client.Object) { o.SetAnnotations(map[string]string{MigratingAtAnnotation: v}) }
}

func synced() func(client.Object) {
	return func(o client.Object) {
		c := xpv2.Condition{Type: xpv2.TypeSynced, Status: corev1.ConditionTrue}
		switch w := o.(type) {
		case *clusterv1beta1.Workspace:
			w.SetConditions(c)
		case *namespacedv1beta1.Workspace:
			w.SetConditions(c)
		}
	}
}

func deleting() func(client.Object) {
	return func(o client.Object) {
		t := metav1.NewTime(testNow)
		o.SetDeletionTimestamp(&t)
		o.SetFinalizers([]string{"test"})
	}
}

// shardPods returns one running pod per named shard, labelled the way the
// shard Deployments' pod templates must be.
func shardPods(shards ...string) []corev1.Pod {
	out := make([]corev1.Pod, 0, len(shards))
	for i, s := range shards {
		out = append(out, corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Namespace: "crossplane-system",
				Name:      fmt.Sprintf("provider-terraform-%s-%d", s, i),
				Labels:    map[string]string{ShardLabel: s},
			},
			Status: corev1.PodStatus{Phase: corev1.PodRunning},
		})
	}
	return out
}

// fixture is the simulated cluster the mock client serves.
type fixture struct {
	config     map[string]string
	cluster    []clusterv1beta1.Workspace
	namespaced []namespacedv1beta1.Workspace

	// pods is the set of shard pods. Nil means "the default": a running pod
	// for every active shard, so a draining or out-of-range shard reads as
	// offline and its Workspaces may migrate.
	pods []corev1.Pod
}

// kube returns a mock client over the fixture, recording patched objects.
func (f fixture) kube(patched *[]client.Object) *test.MockClient {
	return &test.MockClient{
		MockGet: func(_ context.Context, key client.ObjectKey, obj client.Object) error {
			switch o := obj.(type) {
			case *corev1.ConfigMap:
				if f.config == nil {
					return apierrors.NewNotFound(schema.GroupResource{Resource: "configmaps"}, key.Name)
				}
				o.Data = f.config
				return nil
			case *clusterv1beta1.Workspace:
				for i := range f.cluster {
					if f.cluster[i].Name == key.Name {
						f.cluster[i].DeepCopyInto(o)
						return nil
					}
				}
			case *namespacedv1beta1.Workspace:
				for i := range f.namespaced {
					if f.namespaced[i].Name == key.Name && f.namespaced[i].Namespace == key.Namespace {
						f.namespaced[i].DeepCopyInto(o)
						return nil
					}
				}
			}
			return apierrors.NewNotFound(schema.GroupResource{Resource: "workspaces"}, key.Name)
		},
		MockList: func(_ context.Context, list client.ObjectList, _ ...client.ListOption) error {
			switch l := list.(type) {
			case *clusterv1beta1.WorkspaceList:
				l.Items = append([]clusterv1beta1.Workspace(nil), f.cluster...)
			case *namespacedv1beta1.WorkspaceList:
				l.Items = append([]namespacedv1beta1.Workspace(nil), f.namespaced...)
			case *corev1.PodList:
				l.Items = append([]corev1.Pod(nil), f.pods...)
			}
			return nil
		},
		MockPatch: func(_ context.Context, obj client.Object, _ client.Patch, _ ...client.PatchOption) error {
			if patched != nil {
				*patched = append(*patched, obj.DeepCopyObject().(client.Object))
			}
			return nil
		},
	}
}

func testAssigner(f fixture, k Kind, patched *[]client.Object, o ...PlacerOption) *Assigner {
	kube := f.kube(patched)
	o = append([]PlacerOption{
		WithClock(func() time.Time { return testNow }),
		WithPodReader(kube, "crossplane-system"),
	}, o...)
	p := NewPlacer(kube, testConfigRef, logging.NewNopLogger(), o...)
	return NewAssigner(kube, k, p, logging.NewNopLogger())
}

func TestReconcile(t *testing.T) {
	type want struct {
		result reconcile.Result
		// shard is the label on the patched object, "" when nothing was
		// patched.
		shard string
		// migratingSet is whether the patch carried a migrating-at annotation.
		migratingSet bool
		// patches is how many Patch calls were made.
		patches int
		wantErr bool
	}
	cases := map[string]struct {
		reason     string
		fixture    fixture
		placerOpts []PlacerOption
		req        reconcile.Request
		want       want
	}{
		"UnlabelledGetsLeastLoaded": {
			reason: "An unplaced Workspace lands on the emptiest active shard.",
			fixture: fixture{
				config: map[string]string{KeyShardCount: "3"},
				cluster: []clusterv1beta1.Workspace{
					cws("a", "shard-0"), cws("b", "shard-0"), cws("c", "shard-1"), cws("new", ""),
				},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "new"}},
			want: want{shard: "shard-2", patches: 1},
		},
		"AlreadyActiveIsNoOp": {
			reason: "The happy path writes nothing at all - placement is sticky.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "3"},
				cluster: []clusterv1beta1.Workspace{cws("a", "shard-2")},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want: want{patches: 0},
		},
		"DrainingShardMigrates": {
			reason: "A Workspace on a draining shard moves, and is stamped migrating-at.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
				cluster: []clusterv1beta1.Workspace{cws("a", "shard-1")},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want: want{shard: "shard-0", migratingSet: true, patches: 1},
		},
		"OutOfRangeShardMigrates": {
			reason: "Dropping shardCount moves Workspaces off the shards that no longer exist.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2"},
				cluster: []clusterv1beta1.Workspace{cws("a", "shard-5")},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want: want{shard: "shard-0", migratingSet: true, patches: 1},
		},
		"MigrationInFlightBlocksAnother": {
			reason: "Migrations are serialised, so a second one waits rather than doubling the overlap.",
			fixture: fixture{
				config: map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
				cluster: []clusterv1beta1.Workspace{
					cws("busy", "shard-0", migrating(testNow.Add(-1*time.Minute))),
					cws("waiting", "shard-1"),
				},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "waiting"}},
			want: want{result: reconcile.Result{RequeueAfter: RequeueMigration}, patches: 0},
		},
		"MigrationInFlightDoesNotBlockFirstPlacement": {
			reason: "An unlabelled Workspace is reconciled by nobody, so it is placed even mid-drain.",
			fixture: fixture{
				config: map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
				cluster: []clusterv1beta1.Workspace{
					cws("busy", "shard-0", migrating(testNow.Add(-1*time.Minute))),
					cws("new", ""),
				},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "new"}},
			want: want{shard: "shard-0", patches: 1},
		},
		"StaleMigrationStopsBlocking": {
			reason: "A migration that never syncs must not wedge the drain forever.",
			fixture: fixture{
				config: map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
				cluster: []clusterv1beta1.Workspace{
					cws("stuck", "shard-0", migrating(testNow.Add(-31*time.Minute))),
					cws("waiting", "shard-1"),
				},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "waiting"}},
			want: want{shard: "shard-0", migratingSet: true, patches: 1},
		},
		"UnparseableMigrationStopsBlocking": {
			reason: "A corrupt annotation must not wedge the drain either.",
			fixture: fixture{
				config: map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
				cluster: []clusterv1beta1.Workspace{
					cws("corrupt", "shard-0", migratingRaw("not-a-timestamp")),
					cws("waiting", "shard-1"),
				},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "waiting"}},
			want: want{shard: "shard-0", migratingSet: true, patches: 1},
		},
		"SyncedMigrationClearsAnnotation": {
			reason: "Reaching Synced=True on the new shard completes the migration and releases the gate.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2"},
				cluster: []clusterv1beta1.Workspace{cws("a", "shard-0", migrating(testNow.Add(-1*time.Minute)), synced())},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want: want{shard: "shard-0", patches: 1},
		},
		"UnsyncedMigrationKeepsAnnotation": {
			reason: "The annotation stays until the Workspace actually syncs on its new shard.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2"},
				cluster: []clusterv1beta1.Workspace{cws("a", "shard-0", migrating(testNow.Add(-1*time.Minute)))},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want: want{patches: 0},
		},
		"DeletingIsLeftAlone": {
			reason: "Taking the label off a deleting Workspace would strand its destroy with no owner.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
				cluster: []clusterv1beta1.Workspace{cws("a", "shard-1", deleting())},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want: want{patches: 0},
		},
		"GoneIsNotAnError": {
			reason: "A Workspace deleted between the event and the Get is not an error.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2"},
				cluster: []clusterv1beta1.Workspace{},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "missing"}},
			want: want{patches: 0},
		},
		"MissingConfigIsAnError": {
			reason: "Without desired state the assigner must not guess a placement.",
			fixture: fixture{
				config:  nil,
				cluster: []clusterv1beta1.Workspace{cws("a", "")},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want: want{wantErr: true},
		},
		"EverythingDrainingIsAnError": {
			reason: "With nowhere to place, the assigner errors rather than picking a draining shard.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2", KeyDraining: "shard-0,shard-1"},
				cluster: []clusterv1beta1.Workspace{cws("a", "")},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want: want{wantErr: true},
		},
		"SourceShardStillRunningBlocksMigration": {
			reason: "Shards are separate processes. Moving a Workspace off a shard whose pod is alive lets two terraform processes write the same remote state, which an unlocked backend will not stop.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
				cluster: []clusterv1beta1.Workspace{cws("a", "shard-1")},
				pods:    shardPods("shard-0", "shard-1"),
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want: want{result: reconcile.Result{RequeueAfter: RequeueShardOnline}, patches: 0},
		},
		"SourceShardScaledToZeroMigrates": {
			reason: "Once the draining shard's pod is gone there is no second writer, so the migration is safe to make.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
				cluster: []clusterv1beta1.Workspace{cws("a", "shard-1")},
				pods:    shardPods("shard-0"),
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want: want{shard: "shard-0", migratingSet: true, patches: 1},
		},
		"TerminatedShardPodCountsAsOffline": {
			reason: "A pod whose containers have all terminated is running no terraform, so it must not block the drain forever.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
				cluster: []clusterv1beta1.Workspace{cws("a", "shard-1")},
				pods: append(shardPods("shard-0"), corev1.Pod{
					ObjectMeta: metav1.ObjectMeta{
						Namespace: "crossplane-system", Name: "old-shard-1",
						Labels: map[string]string{ShardLabel: "shard-1"},
					},
					Status: corev1.PodStatus{Phase: corev1.PodSucceeded},
				}),
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want: want{shard: "shard-0", migratingSet: true, patches: 1},
		},
		"UnlabelledShardPodsFailClosed": {
			reason: "No shard-labelled pods is what a missing pod-template label looks like; reading it as 'everything is offline' would defeat the check exactly when it matters.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
				cluster: []clusterv1beta1.Workspace{cws("a", "shard-1")},
				pods:    []corev1.Pod{},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want: want{wantErr: true},
		},
		"RequireOfflineDisabledMigratesWhileRunning": {
			reason: "With backend locking confirmed, an operator may opt back into migrating off a live shard.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
				cluster: []clusterv1beta1.Workspace{cws("a", "shard-1")},
				pods:    shardPods("shard-0", "shard-1"),
			},
			placerOpts: []PlacerOption{WithRequireShardOffline(false)},
			req:        reconcile.Request{NamespacedName: types.NamespacedName{Name: "a"}},
			want:       want{shard: "shard-0", migratingSet: true, patches: 1},
		},
		"FirstPlacementIgnoresShardLiveness": {
			reason: "An unplaced Workspace has no current shard, so there is no second writer to wait for.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
				cluster: []clusterv1beta1.Workspace{cws("new", "")},
				pods:    shardPods("shard-0", "shard-1"),
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "new"}},
			want: want{shard: "shard-0", patches: 1},
		},
		"BalancesAcrossBothKinds": {
			reason: "A shard reconciles both Workspace kinds, so load must be counted across both.",
			fixture: fixture{
				config:  map[string]string{KeyShardCount: "2"},
				cluster: []clusterv1beta1.Workspace{cws("new", "")},
				namespaced: []namespacedv1beta1.Workspace{
					nws("ns", "a", "shard-0"), nws("ns", "b", "shard-0"), nws("ns", "c", "shard-1"),
				},
			},
			req:  reconcile.Request{NamespacedName: types.NamespacedName{Name: "new"}},
			want: want{shard: "shard-1", patches: 1},
		},
	}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			f := tc.fixture
			if f.pods == nil {
				// Default: a pod for every shard that is still active, so a
				// draining or out-of-range shard reads as offline.
				cfg, err := ParseConfig(f.config)
				if err == nil {
					f.pods = shardPods(cfg.ActiveShards()...)
				} else {
					f.pods = shardPods("shard-0")
				}
			}
			var patched []client.Object
			a := testAssigner(f, ClusterKind(), &patched, tc.placerOpts...)

			got, err := a.Reconcile(context.Background(), tc.req)
			if tc.want.wantErr {
				if err == nil {
					t.Errorf("Reconcile(...): want error, got none\n%s", tc.reason)
				}
				return
			}
			if err != nil {
				t.Fatalf("Reconcile(...): unexpected error: %v\n%s", err, tc.reason)
			}
			if diff := cmp.Diff(tc.want.result, got); diff != "" {
				t.Errorf("Reconcile(...): -want result, +got:\n%s\n%s", diff, tc.reason)
			}
			if len(patched) != tc.want.patches {
				t.Fatalf("Reconcile(...): want %d patches, got %d\n%s", tc.want.patches, len(patched), tc.reason)
			}
			if tc.want.patches == 0 {
				return
			}
			p := patched[0]
			if diff := cmp.Diff(tc.want.shard, p.GetLabels()[ShardLabel]); diff != "" {
				t.Errorf("Reconcile(...): -want shard, +got:\n%s\n%s", diff, tc.reason)
			}
			_, gotMigrating := p.GetAnnotations()[MigratingAtAnnotation]
			if gotMigrating != tc.want.migratingSet {
				t.Errorf("Reconcile(...): want migrating-at set=%v, got %v\n%s", tc.want.migratingSet, gotMigrating, tc.reason)
			}
		})
	}
}

func TestReconcileNamespacedKind(t *testing.T) {
	f := fixture{
		config: map[string]string{KeyShardCount: "2"},
		namespaced: []namespacedv1beta1.Workspace{
			nws("ns", "a", "shard-0"), nws("ns", "new", ""),
		},
	}
	f.pods = shardPods("shard-0", "shard-1")
	var patched []client.Object
	a := testAssigner(f, NamespacedKind(), &patched)

	if _, err := a.Reconcile(context.Background(), reconcile.Request{
		NamespacedName: types.NamespacedName{Namespace: "ns", Name: "new"},
	}); err != nil {
		t.Fatalf("Reconcile(...): unexpected error: %v", err)
	}
	if len(patched) != 1 {
		t.Fatalf("Reconcile(...): want 1 patch, got %d", len(patched))
	}
	if got := patched[0].GetLabels()[ShardLabel]; got != "shard-1" {
		t.Errorf("Reconcile(...): want shard-1, got %q; the namespaced kind must place like the cluster one", got)
	}
}

func TestLeastLoadedTieBreak(t *testing.T) {
	f := fixture{config: map[string]string{KeyShardCount: "3"}}
	kube := f.kube(nil)
	p := NewPlacer(kube, testConfigRef, logging.NewNopLogger(),
		WithClock(func() time.Time { return testNow }), WithPodReader(kube, "crossplane-system"))

	cfg, err := p.LoadConfig(context.Background())
	if err != nil {
		t.Fatalf("LoadConfig(...): %v", err)
	}
	got, err := p.LeastLoaded(context.Background(), cfg)
	if err != nil {
		t.Fatalf("LeastLoaded(...): %v", err)
	}
	if got != "shard-0" {
		t.Errorf("LeastLoaded(...): want shard-0 on an empty cluster, got %q; ties must break to the lowest index for determinism", got)
	}
}

func TestCensus(t *testing.T) {
	f := fixture{
		config: map[string]string{KeyShardCount: "2", KeyDraining: "shard-1"},
		cluster: []clusterv1beta1.Workspace{
			cws("a", "shard-0"),
			cws("b", "shard-1"),
			cws("c", ""),
			cws("d", "shard-9"),
			cws("e", "shard-0", migrating(testNow)),
		},
	}
	kube := f.kube(nil)
	p := NewPlacer(kube, testConfigRef, logging.NewNopLogger(),
		WithClock(func() time.Time { return testNow }), WithPodReader(kube, "crossplane-system"))

	cfg, err := p.LoadConfig(context.Background())
	if err != nil {
		t.Fatalf("LoadConfig(...): %v", err)
	}
	got, err := p.Census(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Census(...): %v", err)
	}

	want := Census{
		PerShard:   map[string]int{"shard-0": 2},
		Unlabelled: 1,
		Inactive:   2, // shard-1 is draining, shard-9 is out of range
		Migrating:  1,
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("Census(...): -want, +got:\n%s", diff)
	}
}
