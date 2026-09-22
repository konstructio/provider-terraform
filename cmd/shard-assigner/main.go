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

// Command shard-assigner writes the terraform.crossplane.io/shard label onto
// every Workspace, deciding which provider-terraform controller instance owns
// it, and migrates Workspaces off shards that are draining.
//
// It is deliberately a separate binary from the provider: the provider's own
// cache is shard filtered, whereas placement needs a full view of every
// Workspace.
package main

import (
	"os"
	"path/filepath"
	"time"

	"github.com/alecthomas/kingpin/v2"
	"github.com/crossplane/crossplane-runtime/v2/pkg/logging"
	zapuber "go.uber.org/zap"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	apiscluster "github.com/upbound/provider-terraform/apis/cluster"
	apisnamespaced "github.com/upbound/provider-terraform/apis/namespaced"
	"github.com/upbound/provider-terraform/internal/shard"
)

func main() {
	var (
		app   = kingpin.New(filepath.Base(os.Args[0]), "Assigns Crossplane Terraform Workspaces to controller shards.").DefaultEnvars()
		debug = app.Flag("debug", "Run with debug logging.").Short('d').Bool()

		leaderElection = app.Flag("leader-election", "Use leader election for the assigner.").Short('l').Default("true").OverrideDefaultFromEnvar("LEADER_ELECTION").Bool()

		configNamespace = app.Flag("config-namespace", "Namespace of the ConfigMap holding shardCount and draining.").Default("crossplane-system").String()
		configName      = app.Flag("config-name", "Name of the ConfigMap holding shardCount and draining.").Default("provider-terraform-shards").String()

		syncInterval   = app.Flag("sync-interval", "How often the informer cache is resynced, which re-evaluates placement for every Workspace.").Default("10m").Duration()
		staleMigration = app.Flag("stale-migration", "How long a Workspace that never syncs on its new shard may block further migrations.").Default("30m").Duration()
		censusInterval = app.Flag("census-interval", "How often the placement gauges are refreshed.").Default("30s").Duration()

		metricsBind = app.Flag("metrics-bind-address", "Address the metrics endpoint binds to.").Default(":8080").String()
	)
	kingpin.MustParse(app.Parse(os.Args[1:]))

	zl := zap.New(zap.UseDevMode(*debug))
	log := logging.NewLogrLogger(zl.WithName("shard-assigner"))
	if *debug {
		// The controller-runtime runs a k8s client, which will log a lot of
		// messages at debug level if we don't set this.
		ctrl.SetLogger(zl)
	} else {
		ctrl.SetLogger(zap.New(zap.UseDevMode(false), zap.Level(zapuber.NewAtomicLevelAt(zapuber.ErrorLevel))))
	}

	cfg, err := ctrl.GetConfig()
	kingpin.FatalIfError(err, "Cannot get API server rest config")

	scheme := runtime.NewScheme()
	kingpin.FatalIfError(clientgoscheme.AddToScheme(scheme), "Cannot add clientgo scheme")
	kingpin.FatalIfError(apiscluster.AddToScheme(scheme), "Cannot add cluster-scoped terraform APIs to scheme")
	kingpin.FatalIfError(apisnamespaced.AddToScheme(scheme), "Cannot add namespaced terraform APIs to scheme")

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,

		// No ByObject label selector here, unlike the provider: the assigner
		// balances across shards, so it must see every Workspace.
		Cache: cache.Options{SyncPeriod: syncInterval},

		Metrics: metricsserver.Options{BindAddress: *metricsBind},

		// A second assigner writing labels concurrently would defeat the
		// single-writer premise the design rests on.
		LeaderElection:             *leaderElection,
		LeaderElectionID:           "crossplane-leader-election-provider-terraform-shard-assigner",
		LeaderElectionResourceLock: resourcelock.LeasesResourceLock,
		LeaseDuration:              durationPtr(60 * time.Second),
		RenewDeadline:              durationPtr(50 * time.Second),
	})
	kingpin.FatalIfError(err, "Cannot create controller manager")

	kingpin.FatalIfError(shard.Setup(mgr, log, shard.SetupOptions{
		ConfigRef:      types.NamespacedName{Namespace: *configNamespace, Name: *configName},
		StaleMigration: *staleMigration,
		CensusInterval: *censusInterval,
	}), "Cannot setup shard assigner")

	log.Info("Starting shard assigner",
		"config", *configNamespace+"/"+*configName,
		"sync-interval", syncInterval.String(),
		"stale-migration", staleMigration.String())

	kingpin.FatalIfError(mgr.Start(ctrl.SetupSignalHandler()), "Cannot start controller manager")
}

func durationPtr(d time.Duration) *time.Duration { return &d }
