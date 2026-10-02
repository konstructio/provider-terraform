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
	// the shape of terraform holding a provider plugin open. Like a real
	// plugin, the sleep has its own stdio rather than the shell's stdout.
	cmd := newCommand(ctx, "/bin/sh", "-c", "sleep 120 >/dev/null 2>&1 & echo $! > "+pidFile+"; wait")

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

// TestCancelInterruptsOnlyTheDirectChild is the regression test for plugins
// being killed mid-operation.
//
// terraform stops its provider plugins itself, over RPC, after they report
// back. If the cancellation signal reaches the plugins directly they die with
// the result of an in-flight create unreported, and the resource is orphaned.
// So when the child handles the interrupt, its own child - standing in for
// terraform-provider-aws - must still be running.
func TestCancelInterruptsOnlyTheDirectChild(t *testing.T) {
	dir := t.TempDir()
	pidFile := filepath.Join(dir, "pid")
	seen := filepath.Join(dir, "seen")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// On SIGINT, record whether the backgrounded "plugin" is still alive,
	// then exit - terraform finishing its graceful stop.
	script := `trap 'if kill -0 "$p" 2>/dev/null; then echo alive; else echo dead; fi > ` + seen + `; exit 0' INT
sleep 120 >/dev/null 2>&1 & p=$!
echo "$p" > ` + pidFile + `
wait`
	cmd := newCommand(ctx, "/bin/sh", "-c", script)

	done := make(chan error, 1)
	go func() {
		_, err := runCommand(ctx, cmd)
		done <- err
	}()

	pid := grandchildPID(t, pidFile)
	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runCommand did not return after the context was cancelled")
	}

	b, err := os.ReadFile(seen)
	switch {
	case err != nil:
		t.Fatalf("the child never ran its SIGINT handler (%v); cancellation must send SIGINT, not SIGTERM or SIGKILL", err)
	case strings.TrimSpace(string(b)) != "alive":
		t.Errorf("the child's child was %q when the child handled SIGINT, want alive; "+
			"the signal reached the whole group, which would kill terraform's provider plugins mid-call",
			strings.TrimSpace(string(b)))
	}

	// Once the child has exited, the leftover is cleaned up.
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("grandchild %d survived after its parent exited; the process group was not killed", pid)
}

// TestCancelForceKillsAfterInterruptGrace checks that a command which does not
// stop on SIGINT is still stopped: after InterruptGrace the child is killed,
// and so is everything in its group.
func TestCancelForceKillsAfterInterruptGrace(t *testing.T) {
	saved := InterruptGrace
	InterruptGrace = 500 * time.Millisecond
	defer func() { InterruptGrace = saved }()

	pidFile := filepath.Join(t.TempDir(), "pid")

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Ignores SIGINT entirely, like a terraform wedged on a provider call.
	cmd := newCommand(ctx, "/bin/sh", "-c", `trap '' INT; sleep 120 >/dev/null 2>&1 & echo $! > `+pidFile+`; wait`)

	done := make(chan error, 1)
	go func() {
		_, err := runCommand(ctx, cmd)
		done <- err
	}()

	pid := grandchildPID(t, pidFile)
	start := time.Now()
	cancel()

	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runCommand did not return; WaitDelay did not bound the graceful stop")
	}
	if waited := time.Since(start); waited < InterruptGrace {
		t.Errorf("runCommand returned after %v, before InterruptGrace (%v); the child was not given its graceful stop",
			waited, InterruptGrace)
	}

	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if !alive(pid) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Errorf("grandchild %d survived the force-kill; the process group was not killed", pid)
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
