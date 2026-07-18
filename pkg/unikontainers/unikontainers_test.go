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
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/opencontainers/runtime-spec/specs-go"
	"github.com/stretchr/testify/assert"
)

// TestGetContainerIDPathTraversal ensures that a path-traversal containerID
// cannot escape the root directory when locating the container state file.
// securejoin.SecureJoin confines the constructed path within rootDir, unlike
// filepath.Join which would allow the "../" component to reach a sibling
// directory. Before the fix, Get would read the escaped state.json and return
// ErrNotUnikernel; after the fix, the lookup stays inside rootDir and fails
// with a not-exist error.
func TestGetContainerIDPathTraversal(t *testing.T) {
	t.Parallel()

	parent := t.TempDir()
	rootDir := filepath.Join(parent, "root")
	err := os.MkdirAll(rootDir, 0o750)
	assert.NoError(t, err)

	// Place a state.json OUTSIDE rootDir that a traversal containerID could
	// reach through filepath.Join(rootDir, "../outside").
	outsideDir := filepath.Join(parent, "outside")
	err = os.MkdirAll(outsideDir, 0o750)
	assert.NoError(t, err)

	state := specs.State{
		Version: "1.0.0",
		ID:      "outside",
	}
	data, err := json.Marshal(state)
	assert.NoError(t, err)
	err = os.WriteFile(filepath.Join(outsideDir, stateFilename), data, 0o600)
	assert.NoError(t, err)

	// The traversal must be contained within rootDir, so the state file is
	// not found and the escaped file is never read.
	_, err = Get("../outside", rootDir)
	assert.Error(t, err, "path-traversal containerID must not escape rootDir")
	assert.False(t, errors.Is(err, ErrNotUnikernel),
		"escaped state file must not be read")
	assert.True(t, os.IsNotExist(err),
		"state file lookup must be confined within rootDir")
}
