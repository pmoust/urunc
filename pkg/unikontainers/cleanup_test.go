// Copyright (c) 2023-2026, Nubificus LTD
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package unikontainers

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"golang.org/x/sys/unix"
)

// TestCleanupStackUnwindReverseOrder verifies that registered teardowns run in
// reverse (LIFO) order, which is what lets Exec undo partial setup safely.
func TestCleanupStackUnwindReverseOrder(t *testing.T) {
	var order []string
	stack := &cleanupStack{}
	stack.push("first", func() error { order = append(order, "first"); return nil })
	stack.push("second", func() error { order = append(order, "second"); return nil })
	stack.push("third", func() error { order = append(order, "third"); return nil })

	err := stack.unwind()
	assert.NoError(t, err, "unwind should not error when teardowns succeed")
	assert.Equal(t, []string{"third", "second", "first"}, order,
		"teardowns must run in reverse registration order")
}

// TestCleanupStackNilAndEmpty verifies that a nil teardown is ignored and that
// unwinding an empty stack is a no-op.
func TestCleanupStackNilAndEmpty(t *testing.T) {
	stack := &cleanupStack{}
	assert.NoError(t, stack.unwind(), "unwinding an empty stack should be a no-op")

	called := false
	stack.push("nil-teardown", nil)
	stack.push("real", func() error { called = true; return nil })
	assert.NoError(t, stack.unwind())
	assert.True(t, called, "non-nil teardown should run")
	assert.Len(t, stack.steps, 1, "nil teardown should not be registered")
}

// TestCleanupStackContinuesOnError verifies that a failing teardown does not
// prevent the remaining teardowns from running, and that the errors are joined.
func TestCleanupStackContinuesOnError(t *testing.T) {
	var ran []string
	boom := errors.New("boom")
	stack := &cleanupStack{}
	stack.push("a", func() error { ran = append(ran, "a"); return nil })
	stack.push("b", func() error { ran = append(ran, "b"); return boom })
	stack.push("c", func() error { ran = append(ran, "c"); return nil })

	err := stack.unwind()
	assert.Error(t, err, "unwind should report the teardown failure")
	assert.ErrorIs(t, err, boom, "the joined error should wrap the underlying error")
	assert.Equal(t, []string{"c", "b", "a"}, ran,
		"all teardowns must run even if one fails")
}

// spyStep models a setup step whose run() may fail and whose teardown records
// that it was invoked. It mirrors how Exec registers the TAP and monitor-rootfs
// teardowns, using fakes in place of the real network/mount operations.
type spyStep struct {
	name        string
	fail        bool
	ran         bool
	tornDown    bool
	teardownSeq *[]string
}

func newSteps(t *testing.T, specs []*spyStep) []setupStep {
	t.Helper()
	steps := make([]setupStep, 0, len(specs))
	for _, s := range specs {
		s := s
		steps = append(steps, setupStep{
			name: s.name,
			run: func() error {
				s.ran = true
				if s.fail {
					return errors.New(s.name + " failed")
				}
				return nil
			},
			teardown: func() error {
				s.tornDown = true
				*s.teardownSeq = append(*s.teardownSeq, s.name)
				return nil
			},
		})
	}
	return steps
}

// TestRunSetupStepsCleanupPerStage is the core regression test: it drives the
// exact orchestration Exec uses (runSetupSteps) and asserts that when a setup
// stage fails, every earlier successful stage -- and the failing stage itself
// -- is torn down in reverse order, while later stages are never started.
//
// The two stages mirror Exec's real ones: "tap" stands in for the TAP-device
// setup and "mounts" for the monitor-rootfs mounts.
func TestRunSetupStepsCleanupPerStage(t *testing.T) {
	tests := []struct {
		name             string
		failStage        string // which stage's run() fails ("" == none)
		wantErr          bool
		wantTornDown     []string // in the order teardowns should fire
		wantNotTornDown  []string
		wantStagesRanSet []string // stages whose run() executed
	}{
		{
			name:             "all stages succeed -> no teardown",
			failStage:        "",
			wantErr:          false,
			wantTornDown:     nil,
			wantNotTornDown:  []string{"tap", "mounts", "final"},
			wantStagesRanSet: []string{"tap", "mounts", "final"},
		},
		{
			name:      "fail at tap stage -> only tap torn down",
			failStage: "tap",
			wantErr:   true,
			// tap armed its teardown before running, so it is unwound.
			wantTornDown:     []string{"tap"},
			wantNotTornDown:  []string{"mounts", "final"},
			wantStagesRanSet: []string{"tap"},
		},
		{
			name:      "fail at mounts stage -> mounts then tap torn down",
			failStage: "mounts",
			wantErr:   true,
			wantTornDown:     []string{"mounts", "tap"},
			wantNotTornDown:  []string{"final"},
			wantStagesRanSet: []string{"tap", "mounts"},
		},
		{
			name:      "fail at later stage -> mounts then tap torn down",
			failStage: "final",
			wantErr:   true,
			wantTornDown:     []string{"final", "mounts", "tap"},
			wantNotTornDown:  nil,
			wantStagesRanSet: []string{"tap", "mounts", "final"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var teardownSeq []string
			specs := []*spyStep{
				{name: "tap", teardownSeq: &teardownSeq},
				{name: "mounts", teardownSeq: &teardownSeq},
				{name: "final", teardownSeq: &teardownSeq},
			}
			byName := map[string]*spyStep{}
			for _, s := range specs {
				byName[s.name] = s
				if s.name == tt.failStage {
					s.fail = true
				}
			}

			stack, err := runSetupSteps(newSteps(t, specs))

			if tt.wantErr {
				assert.Error(t, err, "expected setup to fail")
				assert.Nil(t, stack, "no stack should be returned on failure")
			} else {
				assert.NoError(t, err, "expected setup to succeed")
				assert.NotNil(t, stack, "successful setup must return the armed stack")
			}

			// On the success path, runSetupSteps must NOT tear anything down;
			// the container is expected to proceed to execution.
			assert.Equal(t, tt.wantTornDown, teardownSeq,
				"teardown order/contents mismatch")

			for _, name := range tt.wantNotTornDown {
				assert.Falsef(t, byName[name].tornDown,
					"stage %q should not have been torn down", name)
			}
			for _, name := range tt.wantStagesRanSet {
				assert.Truef(t, byName[name].ran, "stage %q should have run", name)
			}
		})
	}
}

// TestExecDeferredCleanupPath models Exec's real control flow: the
// resource-acquiring steps (network TAP + monitor-rootfs mounts) succeed and
// runSetupSteps returns the armed stack, then a *later* Exec stage fails and the
// deferred cleanup unwinds the stack. Both the TAP cleanup and the mount
// unmount must run, in reverse order (mounts first, then TAP).
func TestExecDeferredCleanupPath(t *testing.T) {
	var order []string
	tapCleaned := false
	mountsUnmounted := false

	steps := []setupStep{
		{
			name: "network",
			run:  func() error { return nil },
			teardown: func() error {
				tapCleaned = true
				order = append(order, "tap")
				return nil
			},
		},
		{
			name: "monitor-rootfs",
			run:  func() error { return nil },
			teardown: func() error {
				mountsUnmounted = true
				order = append(order, "mounts")
				return nil
			},
		},
	}

	stack, err := runSetupSteps(steps)
	assert.NoError(t, err, "resource setup should succeed")
	assert.NotNil(t, stack, "successful setup returns the armed cleanup stack")
	assert.False(t, tapCleaned, "nothing should be torn down while setup succeeds")
	assert.False(t, mountsUnmounted, "nothing should be torn down while setup succeeds")

	// Simulate a later Exec stage failing (e.g. unikernel.Init, changeRoot):
	// Exec's deferred cleanup unwinds the returned stack.
	assert.NoError(t, stack.unwind())
	assert.True(t, tapCleaned, "TAP device must be cleaned up when a later stage fails")
	assert.True(t, mountsUnmounted, "monitor rootfs must be unmounted when a later stage fails")
	assert.Equal(t, []string{"mounts", "tap"}, order,
		"deferred cleanup must unmount before removing the TAP device")
}

// TestExecInListFailureCleanup models a failure that happens *inside* the
// resource-acquiring sequence itself (e.g. prepareMonRootfs fails): the TAP
// setup already succeeded and must still be undone.
func TestExecInListFailureCleanup(t *testing.T) {
	tapCleaned := false
	mountsUnmounted := false

	steps := []setupStep{
		{
			name:     "network",
			run:      func() error { return nil },
			teardown: func() error { tapCleaned = true; return nil },
		},
		{
			name:     "monitor-rootfs",
			run:      func() error { return errors.New("prepareMonRootfs failed") },
			teardown: func() error { mountsUnmounted = true; return nil },
		},
	}

	stack, err := runSetupSteps(steps)
	assert.Error(t, err, "a failure during setup must be reported")
	assert.Nil(t, stack, "no stack is returned when setup fails")
	assert.True(t, tapCleaned, "TAP device must be cleaned up after a mount-stage failure")
	assert.True(t, mountsUnmounted,
		"the failing mount stage must also be torn down (teardown is armed before run)")
}

// TestMountsUnderFiltersAndSorts verifies mountsUnder only returns mounts at or
// below the given directory and orders them deepest-first, which is required so
// cleanupMonRootfs detaches submounts before their parents.
func TestMountsUnderFiltersAndSorts(t *testing.T) {
	// /proc is always a mount point on Linux; use it as a stable anchor.
	mounts, err := mountsUnder("/proc")
	assert.NoError(t, err, "reading mountinfo should succeed")

	found := false
	for i, mp := range mounts {
		assert.Truef(t, mp == "/proc" || strings.HasPrefix(mp, "/proc/"),
			"mountsUnder(/proc) returned unrelated mount %q", mp)
		if mp == "/proc" {
			found = true
		}
		if i > 0 {
			assert.GreaterOrEqualf(t, len(mounts[i-1]), len(mp),
				"mounts must be sorted deepest-first: %q before %q", mounts[i-1], mp)
		}
	}
	assert.True(t, found, "expected /proc itself to be reported as a mount point")

	// A path that cannot be a mount point should yield nothing.
	none, err := mountsUnder("/nonexistent-path-for-urunc-test")
	assert.NoError(t, err)
	assert.Empty(t, none, "no mounts should be found under a nonexistent path")
}

// TestCleanupMonRootfsUnmounts verifies cleanupMonRootfs detaches a mount that
// was established under the monitor rootfs directory. It needs privileges to
// mount, so it is skipped when not running as root.
func TestCleanupMonRootfsUnmounts(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root to create and unmount a tmpfs")
	}

	monRootfs := t.TempDir()
	sub := filepath.Join(monRootfs, "dev")
	assert.NoError(t, os.MkdirAll(sub, 0o755))

	// Mount a tmpfs under monRootfs to simulate a mount created during setup.
	assert.NoError(t, unix.Mount("tmpfs", sub, "tmpfs", 0, ""),
		"failed to mount tmpfs for test")

	// Sanity: the mount must be visible before cleanup.
	before, err := mountsUnder(monRootfs)
	assert.NoError(t, err)
	assert.Contains(t, before, sub, "tmpfs should be mounted before cleanup")

	// cleanupMonRootfs must detach it.
	assert.NoError(t, cleanupMonRootfs(monRootfs))

	after, err := mountsUnder(monRootfs)
	assert.NoError(t, err)
	assert.NotContains(t, after, sub, "tmpfs should be unmounted after cleanup")
}
