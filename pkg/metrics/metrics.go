package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
)

var (
	// GitHub API metrics
	GitHubAPIRequestsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "terraform_provider_github_api_requests_total",
			Help: "Total number of GitHub API requests made by the provider",
		},
		[]string{"workspace", "operation", "status"},
	)

	GitHubAPIRequestDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "terraform_provider_github_api_request_duration_seconds",
			Help:    "Duration of GitHub API requests in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"workspace", "operation"},
	)

	// Module fetch metrics (go-getter)
	ModuleFetchTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "terraform_provider_module_fetch_total",
			Help: "Total number of remote module fetch operations",
		},
		[]string{"workspace", "module_source", "status"},
	)

	ModuleFetchDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "terraform_provider_module_fetch_duration_seconds",
			Help:    "Duration of remote module fetch operations in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"workspace", "module_source"},
	)

	// Terraform CLI operation metrics
	TerraformOperationsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "terraform_provider_terraform_operations_total",
			Help: "Total number of Terraform CLI operations performed",
		},
		[]string{"workspace", "operation", "status"},
	)

	TerraformOperationDuration = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "terraform_provider_terraform_operation_duration_seconds",
			Help:    "Duration of Terraform CLI operations in seconds",
			Buckets: prometheus.DefBuckets,
		},
		[]string{"workspace", "operation"},
	)
)

func init() {
	metrics.Registry.MustRegister(
		GitHubAPIRequestsTotal,
		GitHubAPIRequestDuration,
		ModuleFetchTotal,
		ModuleFetchDuration,
		TerraformOperationsTotal,
		TerraformOperationDuration,
	)
}
