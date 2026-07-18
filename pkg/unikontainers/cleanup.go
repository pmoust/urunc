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
	"bufio"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

// teardownFunc undoes the side effects of a single setup step (e.g. removing a
// TAP device or unmounting the monitor rootfs). It returns an error if the
// teardown could not be completed, but a failing teardown never aborts the
// unwinding of the remaining steps.
type teardownFunc func() error

// cleanupStack records the teardown callbacks of the setup steps that have
// already succeeded (or partially executed). If a later step fails, the stack
// is unwound so the partial setup does not leak resources such as TAP devices
// or mounts. Teardowns are executed in reverse (LIFO) order, mirroring the
// order in which the corresponding setup steps acquired their resources.
type cleanupStack struct {
	steps []namedTeardown
}

type namedTeardown struct {
	name     string
	teardown teardownFunc
}

// push registers a teardown to be run when the stack is unwound. A nil teardown
// is ignored, which lets callers register steps unconditionally.
func (c *cleanupStack) push(name string, teardown teardownFunc) {
	if teardown == nil {
		return
	}
	c.steps = append(c.steps, namedTeardown{name: name, teardown: teardown})
}

// unwind runs every registered teardown in reverse order. It does not stop on
// the first error: each teardown is attempted so that a single failure cannot
// leak the remaining resources. The joined error (if any) is returned and each
// individual failure is also logged.
func (c *cleanupStack) unwind() error {
	var retErr error
	for i := len(c.steps) - 1; i >= 0; i-- {
		step := c.steps[i]
		if err := step.teardown(); err != nil {
			uniklog.WithError(err).Errorf("cleanup step %q failed", step.name)
			retErr = errors.Join(retErr, fmt.Errorf("cleanup step %q: %w", step.name, err))
		}
	}
	return retErr
}

// setupStep is a single resource-acquiring stage of Exec. run performs the
// setup, while teardown (optional) undoes it. runSetupSteps arms teardown
// before invoking run, so that even a partial failure inside run is cleaned up.
type setupStep struct {
	name     string
	run      func() error
	teardown teardownFunc
}

// runSetupSteps executes the given steps in order. Before running each step it
// arms the step's teardown, so any failure -- including a partial failure
// inside the step itself -- unwinds every teardown registered so far in reverse
// order. On success it returns the armed cleanup stack WITHOUT running any
// teardown: the caller keeps it and must unwind it if a later, unrelated Exec
// stage fails, and discard it once the setup is committed (e.g. after the
// process has pivoted into the new rootfs).
func runSetupSteps(steps []setupStep) (*cleanupStack, error) {
	stack := &cleanupStack{}
	for _, step := range steps {
		stack.push(step.name, step.teardown)
		if err := step.run(); err != nil {
			if cerr := stack.unwind(); cerr != nil {
				uniklog.WithError(cerr).Error("errors while cleaning up after failed setup step")
			}
			return nil, fmt.Errorf("setup step %q failed: %w", step.name, err)
		}
	}
	return stack, nil
}

// cleanupMonRootfs unmounts every mount that was established under monRootfs
// (its self-bind from prepareRoot, /proc, the /dev tmpfs, /dev/pts, the
// bind-mounted monitor binary and libraries, vsock sockets, etc.), so that a
// failed Exec does not leak them. Mounts are detached deepest-first with
// MNT_DETACH, matching the lazy-unmount idiom already used in pivotRootfs.
//
// It intentionally does not remove the directory tree: monRootfs may be the
// container's own rootfs (in the initrd and shared-fs cases MonRootfs equals
// the container rootfs), and directory removal is handled separately by Delete.
func cleanupMonRootfs(monRootfs string) error {
	mountPoints, err := mountsUnder(monRootfs)
	if err != nil {
		return err
	}

	var retErr error
	for _, mp := range mountPoints {
		err := unix.Unmount(mp, unix.MNT_DETACH)
		// EINVAL means it was not a mount point (already gone / never mounted)
		// and ENOENT means the path no longer exists; both are fine to ignore.
		if err != nil && !errors.Is(err, unix.EINVAL) && !errors.Is(err, unix.ENOENT) {
			uniklog.WithError(err).Errorf("failed to unmount %s", mp)
			retErr = errors.Join(retErr, fmt.Errorf("failed to unmount %s: %w", mp, err))
		}
	}
	return retErr
}

// mountsUnder returns the mount points found in /proc/self/mountinfo that are
// equal to dir or located under dir, sorted deepest-first so that submounts are
// unmounted before their parents. The parsing mirrors getMountInfo, keeping a
// single convention for reading mountinfo in this package.
func mountsUnder(dir string) ([]string, error) {
	const selfProcMountInfo = "/proc/self/mountinfo"

	file, err := os.Open(selfProcMountInfo)
	if err != nil {
		return nil, fmt.Errorf("failed to open mountinfo: %w", err)
	}
	defer file.Close()

	var mountPoints []string
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		// The mount point is the 5th space-separated field of the part before
		// the " - " separator (see proc(5) / mountinfo format).
		fields := strings.Fields(scanner.Text())
		if len(fields) < 5 {
			continue
		}
		mountPoint := unescapeMountInfo(fields[4])
		if mountPoint == dir || strings.HasPrefix(mountPoint, dir+"/") {
			mountPoints = append(mountPoints, mountPoint)
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("failed to read mountinfo: %w", err)
	}

	// Unmount the deepest paths first.
	sort.Slice(mountPoints, func(i, j int) bool {
		return len(mountPoints[i]) > len(mountPoints[j])
	})
	return mountPoints, nil
}

// unescapeMountInfo decodes the octal escape sequences that the kernel uses in
// /proc/self/mountinfo for space (\040), tab (\011), newline (\012) and
// backslash (\134) inside path fields.
func unescapeMountInfo(field string) string {
	if !strings.Contains(field, `\`) {
		return field
	}
	replacer := strings.NewReplacer(
		`\040`, " ",
		`\011`, "\t",
		`\012`, "\n",
		`\134`, `\`,
	)
	return replacer.Replace(field)
}
