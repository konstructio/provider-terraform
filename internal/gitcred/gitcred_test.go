package gitcred

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"
)

// TestWithDirConcurrent reproduces the multi-org race: many Workspaces
// downloading at once, each with its own credentials directory. Every fn must
// see its own directory for its whole run, never another Workspace's.
func TestWithDirConcurrent(t *testing.T) {
	const workers = 20

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			want := fmt.Sprintf("/tmp/tf/workspace-%d", i)
			errs <- WithDir(want, func() error {
				// Read the variable before and after yielding, as git would
				// when go-getter forks it.
				if got := os.Getenv(EnvVar); got != want {
					return fmt.Errorf("before yield: %s = %q, want %q", EnvVar, got, want)
				}
				time.Sleep(time.Millisecond)
				if got := os.Getenv(EnvVar); got != want {
					return fmt.Errorf("after yield: %s = %q, want %q", EnvVar, got, want)
				}
				return nil
			})
		}()
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		if err != nil {
			t.Error(err)
		}
	}
	if v, ok := os.LookupEnv(EnvVar); ok {
		t.Errorf("%s left set to %q after all WithDir calls returned", EnvVar, v)
	}
}

func TestWithDirEmptyUnsets(t *testing.T) {
	t.Setenv(EnvVar, "/tmp/tf/other-workspace")

	err := WithDir("", func() error {
		if v, ok := os.LookupEnv(EnvVar); ok {
			return fmt.Errorf("%s = %q during a download without credentials, want unset", EnvVar, v)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestWithDirReturnsFnError(t *testing.T) {
	errBoom := errors.New("boom")

	err := WithDir("/tmp/tf/workspace", func() error { return errBoom })
	if !errors.Is(err, errBoom) {
		t.Fatalf("WithDir(...) = %v, want %v", err, errBoom)
	}
	if v, ok := os.LookupEnv(EnvVar); ok {
		t.Errorf("%s left set to %q after fn failed", EnvVar, v)
	}
}

func TestEnv(t *testing.T) {
	if got, want := Env("/tmp/tf/abc"), "GIT_CRED_DIR=/tmp/tf/abc"; got != want {
		t.Errorf("Env(...) = %q, want %q", got, want)
	}
}
