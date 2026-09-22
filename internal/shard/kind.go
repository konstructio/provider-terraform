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
	xpv2 "github.com/crossplane/crossplane/apis/v2/core/v2"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	clusterv1beta1 "github.com/upbound/provider-terraform/apis/cluster/v1beta1"
	namespacedv1beta1 "github.com/upbound/provider-terraform/apis/namespaced/v1beta1"
)

// A Workspace is the part of the two generated Workspace types this package
// needs: object metadata, plus the Synced condition that tells us a migration
// has landed on its new shard.
type Workspace interface {
	client.Object
	GetCondition(xpv2.ConditionType) xpv2.Condition
}

// Synced reports whether ws has reached Synced=True.
func Synced(ws Workspace) bool {
	return ws.GetCondition(xpv2.TypeSynced).Status == corev1.ConditionTrue
}

// A Kind adapts one of the two Workspace API types to the assigner. The
// provider serves a cluster-scoped Workspace (tf.upbound.io) and a namespaced
// one (tf.m.upbound.io); a shard reconciles both, so placement has to consider
// both.
type Kind struct {
	// Name identifies the kind in logs and metrics.
	Name string

	// New returns an empty Workspace to Get into.
	New func() Workspace

	// NewList returns an empty list to List into.
	NewList func() client.ObjectList

	// Items extracts the Workspaces from a list returned by NewList.
	Items func(client.ObjectList) []Workspace
}

// ClusterKind adapts the cluster-scoped tf.upbound.io Workspace.
func ClusterKind() Kind {
	return Kind{
		Name:    "cluster",
		New:     func() Workspace { return &clusterv1beta1.Workspace{} },
		NewList: func() client.ObjectList { return &clusterv1beta1.WorkspaceList{} },
		Items: func(l client.ObjectList) []Workspace {
			wl, ok := l.(*clusterv1beta1.WorkspaceList)
			if !ok {
				return nil
			}
			out := make([]Workspace, 0, len(wl.Items))
			for i := range wl.Items {
				out = append(out, &wl.Items[i])
			}
			return out
		},
	}
}

// NamespacedKind adapts the namespaced tf.m.upbound.io Workspace.
func NamespacedKind() Kind {
	return Kind{
		Name:    "namespaced",
		New:     func() Workspace { return &namespacedv1beta1.Workspace{} },
		NewList: func() client.ObjectList { return &namespacedv1beta1.WorkspaceList{} },
		Items: func(l client.ObjectList) []Workspace {
			wl, ok := l.(*namespacedv1beta1.WorkspaceList)
			if !ok {
				return nil
			}
			out := make([]Workspace, 0, len(wl.Items))
			for i := range wl.Items {
				out = append(out, &wl.Items[i])
			}
			return out
		},
	}
}

// Kinds returns every Workspace kind the assigner places.
func Kinds() []Kind { return []Kind{ClusterKind(), NamespacedKind()} }
