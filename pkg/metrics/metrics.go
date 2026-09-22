package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
)

// labelWorkspace is the metric label carrying the Workspace name.
const labelWorkspace = "workspace"

var (
	// GitHub API metrics
	GitHubAPIRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "terraform_provider_github_api_requests_total",
			Help: "Total number of GitHub API requests made by the provider",
		},
		[]string{labelWorkspace, "operation", "status"},
	)

	GitHubAPIRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "terraform_provider_github_api_request_duration_seconds",
			Help:    "Duration of GitHub API requests in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{labelWorkspace, "operation"},
	)

	// Module fetch metrics (go-getter)
	ModuleFetchTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "terraform_provider_module_fetch_total",
			Help: "Total number of remote module fetch operations",
		},
		[]string{labelWorkspace, "module_source", "status"},
	)

	ModuleFetchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "terraform_provider_module_fetch_duration_seconds",
			Help:    "Duration of remote module fetch operations in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{labelWorkspace, "module_source"},
	)

	// Terraform CLI operation metrics
	TerraformOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "terraform_provider_terraform_operations_total",
			Help: "Total number of Terraform CLI operations performed",
		},
		[]string{labelWorkspace, "operation", "status"},
	)

	TerraformOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "terraform_provider_terraform_operation_duration_seconds",
			Help:    "Duration of Terraform CLI operations in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{labelWorkspace, "operation"},
	)
)

// Register registers the provider's metrics with reg.
//
// Callers pass a registerer that may be wrapped with constant labels - the
// shard name, for instance - so every series is broken out without changing
// any metric definition here.
func Register(reg prometheus.Registerer) {
	reg.MustRegister(
		GitHubAPIRequestsTotal,
		GitHubAPIRequestDuration,
		ModuleFetchTotal,
		ModuleFetchDuration,
		TerraformOperationsTotal,
		TerraformOperationDuration,
	)
}
