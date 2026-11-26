// Package gitcred scopes the git credentials directory to a single Workspace.
//
// The container image's /.gitconfig configures
//
//	[credential]
//		helper = store --file=$GIT_CRED_DIR/.git-credentials
//
// so every git process reads its credentials from whatever GIT_CRED_DIR is in
// its environment. Each Workspace writes its own .git-credentials under
// /tmp/tf/<uid>, and different Workspaces can hold tokens for different GitHub
// orgs (one per GitHub App installation).
//
// GIT_CRED_DIR must therefore never be shared process state while git runs
// concurrently. Terraform commands get it per command through Env. go-getter
// spawns git with the process environment and offers no per-call env, so
// remote module downloads go through WithDir, which sets the variable and runs
// the download under a process-wide lock.
package gitcred

import (
	"fmt"
	"os"
	"sync"
)

// EnvVar is the environment variable the image's git credential helper reads.
const EnvVar = "GIT_CRED_DIR"

var mu sync.Mutex

// Env returns the EnvVar entry pointing git at dir, for a command's Env.
func Env(dir string) string {
	return EnvVar + "=" + dir
}

// WithDir runs fn with EnvVar set to dir in the process environment, holding a
// process-wide lock so no other WithDir call can change it while fn runs. An
// empty dir runs fn with EnvVar unset, so a download without credentials never
// picks up another Workspace's. EnvVar is unset again before WithDir returns.
//
// fn must not call WithDir.
func WithDir(dir string, fn func() error) error {
	mu.Lock()
	defer mu.Unlock()
	defer os.Unsetenv(EnvVar) //nolint:errcheck // Unsetenv only fails on an invalid key, and EnvVar is valid

	if dir == "" {
		if err := os.Unsetenv(EnvVar); err != nil {
			return fmt.Errorf("cannot unset %s: %w", EnvVar, err)
		}
		return fn()
	}
	if err := os.Setenv(EnvVar, dir); err != nil {
		return fmt.Errorf("cannot set %s: %w", EnvVar, err)
	}
	return fn()
}
