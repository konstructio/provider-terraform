package metrics

import (
	"net/url"
	"strings"
	"time"
)

// ModuleFetchTimer helps with timing module fetch operations.
type ModuleFetchTimer struct {
	Workspace    string
	ModuleSource string
	StartTime    time.Time
}

// NewModuleFetchTimer creates a new timer for a module fetch operation.
func NewModuleFetchTimer(workspace, moduleSource string) *ModuleFetchTimer {
	if workspace == "" {
		workspace = "unknown"
	}
	return &ModuleFetchTimer{
		Workspace:    workspace,
		ModuleSource: ExtractModuleSource(moduleSource),
		StartTime:    time.Now(),
	}
}

// RecordSuccess records a successful module fetch.
func (t *ModuleFetchTimer) RecordSuccess() {
	t.record("success")
}

// RecordFailure records a failed module fetch.
func (t *ModuleFetchTimer) RecordFailure() {
	t.record("failure")
}

func (t *ModuleFetchTimer) record(status string) {
	duration := time.Since(t.StartTime)
	ModuleFetchTotal.WithLabelValues(t.Workspace, t.ModuleSource, status).Inc()
	ModuleFetchDuration.WithLabelValues(t.Workspace, t.ModuleSource).Observe(duration.Seconds())
}

// ExtractModuleSource extracts a clean label from a go-getter source URL.
// For git URLs: "git::https://github.com/owner/repo.git//subdir" -> "owner/repo"
// For other URLs: returns the host + path without query params.
func ExtractModuleSource(src string) string {
	if src == "" {
		return "unknown"
	}

	// go-getter uses "type::url" format
	cleaned := src
	if idx := strings.Index(src, "::"); idx >= 0 {
		cleaned = src[idx+2:]
	}

	// Remove subdirectory specifier (//subdir)
	if idx := strings.Index(cleaned, "//"); idx >= 0 {
		cleaned = cleaned[:idx]
	}

	// Remove .git suffix
	cleaned = strings.TrimSuffix(cleaned, ".git")

	// Try to parse as URL and extract host/path
	if u, err := url.Parse(cleaned); err == nil && u.Host != "" {
		path := strings.Trim(u.Path, "/")
		parts := strings.Split(path, "/")
		if len(parts) >= 2 {
			return parts[0] + "/" + parts[1]
		}
		return u.Host + "/" + path
	}

	return cleaned
}
