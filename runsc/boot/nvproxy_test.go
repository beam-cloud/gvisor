// Copyright 2026 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     https://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package boot

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/state"
)

func TestIdleNvproxyDeviceMetadataIsCollected(t *testing.T) {
	devDir := t.TempDir()
	for _, name := range []string{"nvidia7", "nvidiactl"} {
		if err := os.WriteFile(filepath.Join(devDir, name), nil, 0600); err != nil {
			t.Fatalf("creating fake device %q: %v", name, err)
		}
	}
	goferFD, cleanup, err := startGofer(devDir, testConfig())
	if err != nil {
		t.Fatalf("starting device gofer: %v", err)
	}
	defer cleanup()

	k := &kernel.Kernel{}
	if err := k.AddDevGofer("root", goferFD); err != nil {
		t.Fatalf("adding device gofer: %v", err)
	}
	defer k.RemoveDevGofer("root")

	gpuSpec := &specs.Spec{
		Process: &specs.Process{Env: []string{"NVIDIA_VISIBLE_DEVICES=GPU-old"}},
		Hooks: &specs.Hooks{CreateContainer: []specs.Hook{{
			Path: "/usr/bin/nvidia-cdi-hook",
		}}},
	}
	l := &Loader{
		k:              k,
		root:           containerInfo{conf: testConfig()},
		containerSpecs: map[string]*specs.Spec{"root": gpuSpec},
	}
	if err := l.setNvproxyDeviceRemapMetadata(&state.SaveOpts{Metadata: make(map[string]string)}); err == nil || !strings.Contains(err.Error(), "NV_ESC_CARD_INFO") {
		t.Fatalf("idle GPU metadata collection error = %v, want NV_ESC_CARD_INFO from fake nvidiactl", err)
	}

	l.containerSpecs = map[string]*specs.Spec{"root": {Process: &specs.Process{}}}
	saveOpts := &state.SaveOpts{Metadata: make(map[string]string)}
	if err := l.setNvproxyDeviceRemapMetadata(saveOpts); err != nil {
		t.Fatalf("CPU-only metadata collection failed: %v", err)
	}
	if _, ok := saveOpts.Metadata[nvproxyDeviceRemapIDsKey]; ok {
		t.Fatalf("CPU-only checkpoint contains %q", nvproxyDeviceRemapIDsKey)
	}
}
