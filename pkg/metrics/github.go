package metrics

import (
	"time"
)

// RecordGitHubAPICall records a GitHub API call with its outcome.
func RecordGitHubAPICall(workspace, operation, status string, duration time.Duration) {
	if workspace == "" {
		workspace = "unknown"
	}

	GitHubAPIRequestsTotal.WithLabelValues(workspace, operation, status).Inc()
	GitHubAPIRequestDuration.WithLabelValues(workspace, operation).Observe(duration.Seconds())
}
