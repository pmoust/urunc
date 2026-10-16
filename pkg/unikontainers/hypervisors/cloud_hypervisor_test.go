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

package hypervisors

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/urunc-dev/urunc/pkg/unikontainers/types"
)

const testCloudHypervisorBinary = "/usr/bin/cloud-hypervisor"

func TestCloudHypervisorBuildExecCmd(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name           string
		args           types.ExecArgs
		unikernel      types.Unikernel
		mustContain    []string
		mustNotContain []string
	}{
		{
			name: "no block device renders no --disk",
			args: types.ExecArgs{
				UnikernelPath: testKernelPath,
				Command:       testCommand,
			},
			unikernel:      &fakeUnikernel{},
			mustContain:    []string{"--kernel " + testKernelPath},
			mustNotContain: []string{"--disk"},
		},
		{
			name: "a block device names its image type",
			args: types.ExecArgs{
				UnikernelPath: testKernelPath,
				Command:       testCommand,
			},
			unikernel: &fakeUnikernel{
				blockCli: []types.MonitorBlockArgs{
					{ID: "rootfs", Path: "/dev/mapper/urunc-rootfs"},
				},
			},
			mustContain: []string{
				"--disk path=/dev/mapper/urunc-rootfs,id=rootfs,image_type=raw",
			},
		},
		{
			name: "a block device without an id still names its image type",
			args: types.ExecArgs{
				UnikernelPath: testKernelPath,
				Command:       testCommand,
			},
			unikernel: &fakeUnikernel{
				blockCli: []types.MonitorBlockArgs{{Path: "/tmp/rootfs.img"}},
			},
			mustContain: []string{"--disk path=/tmp/rootfs.img,image_type=raw"},
		},
		{
			name: "exact block arguments pass through untouched",
			args: types.ExecArgs{
				UnikernelPath: testKernelPath,
				Command:       testCommand,
			},
			unikernel: &fakeUnikernel{
				blockCli: []types.MonitorBlockArgs{
					{ExactArgs: []string{"--disk", "path=/tmp/custom.img,readonly=on"}},
				},
			},
			mustContain:    []string{"--disk path=/tmp/custom.img,readonly=on"},
			mustNotContain: []string{"image_type"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			ch := &CloudHypervisor{binaryPath: testCloudHypervisorBinary, binary: CloudHypervisorBinary}
			cmd, err := ch.BuildExecCmd(tt.args, tt.unikernel)
			assert.NoError(t, err)
			joined := strings.Join(cmd, " ")
			for _, want := range tt.mustContain {
				assert.Contains(t, joined, want)
			}
			for _, unwanted := range tt.mustNotContain {
				assert.NotContains(t, joined, unwanted)
			}
		})
	}
}
