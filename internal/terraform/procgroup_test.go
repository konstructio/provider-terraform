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

package terraform

import (
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// grandchildPID waits for the shell under test to report the PID of the child
// it backgrounded.
func grandchildPID(t *testing.T, path string) int {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil && pid > 0 {
				return pid
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("the backgrounded grandchild never reported its PID")
	return 0
}

// alive reports whether pid still exists. Signal 0 performs the permission and
// existence checks without actually signalling.
func alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

// TestCancelKillsGrandchildren is the regression test for the PID leak, and
// for the assumption shard handover rests on.
//
// terraform spawns terraform-provider-* plugins; /bin/sh spawns the checksum
// pipeline. Signalling only the direct child leaves those grandchildren
// running: they reparent to PID 1 - this provider, which has no init to reap
// them - and become zombies that cgroup v2 keeps charging against
// pids.current, invisibly, until the container cannot fork.
//
// It is also what makes "the shard's pod is gone" mean "no terraform of its is
// still running", which is the precondition for migrating a Workspace to
// another shard.
func TestCancelKillsGrandchildren(t *testing.T) {
	pidFile := filepath.Join(t.TempDir(), "pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// A shell that backgrounds a long sleep, reports its PID, and waits -
	// the shape of terraform holding a provider plugin open.
	cmd := newCommand(ctx, "/bin/sh", "-c", "sleep 120 & echo $! > "+pidFile+"; wait")

	done := make(chan error, 1)
	go func() {
		_, err := runCommand(ctx, cmd)
		done <- err
	}()

	pid := grandchildPID(t, pidFile)
	if !alive(pid) {
		t.Fatalf("grandchild %d was not running before cancellation", pid)
	}

	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runCommand did not return after the context was cancelled")
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("grandchild %d survived cancellation of its parent; the process group was not signalled, "+
		"so it would orphan to PID 1 and leak a PID", pid)
}

// TestCancelledCommandRunsInItsOwnGroup checks the property the kill relies on:
// the child leads a process group of its own, so signalling that group cannot
// reach the provider itself.
func TestCancelledCommandRunsInItsOwnGroup(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	cmd := newCommand(ctx, "/bin/sh", "-c", "sleep 5")
	if err := cmd.Start(); err != nil {
		t.Fatalf("cannot start command: %v", err)
	}
	defer func() { _ = cmd.Wait() }()

	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		t.Fatalf("cannot read the child's process group: %v", err)
	}
	if pgid != cmd.Process.Pid {
		t.Errorf("child pgid is %d, want its own pid %d; without Setpgid a group kill would signal the provider too",
			pgid, cmd.Process.Pid)
	}
	if pgid == syscall.Getpgrp() {
		t.Errorf("child shares the provider's process group (%d); a group kill would take the provider down with it", pgid)
	}
}
