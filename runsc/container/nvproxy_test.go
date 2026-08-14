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

package container

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func TestRemoveStaleNvidiaRegularDeviceFilesBeforeReinjection(t *testing.T) {
	root := t.TempDir()
	devDir := filepath.Join(root, "dev")
	if err := os.Mkdir(devDir, 0755); err != nil {
		t.Fatalf("creating /dev: %v", err)
	}
	for _, name := range []string{"nvidia0", "nvidiactl", "nvidia-uvm", "nvidia0.keep", "dri"} {
		if err := os.WriteFile(filepath.Join(devDir, name), nil, 0600); err != nil {
			t.Fatalf("creating /dev/%s: %v", name, err)
		}
	}

	if err := removeStaleNvidiaRegularDeviceFiles(root); err != nil {
		t.Fatalf("removeStaleNvidiaRegularDeviceFiles: %v", err)
	}
	// Stand in for nvidia-container-cli injecting the newly assigned non-zero
	// minor after a restored old minor 0 has been removed.
	if err := os.WriteFile(filepath.Join(devDir, "nvidia2"), nil, 0600); err != nil {
		t.Fatalf("injecting /dev/nvidia2: %v", err)
	}

	entries, err := os.ReadDir(devDir)
	if err != nil {
		t.Fatalf("reading /dev: %v", err)
	}
	var got []string
	for _, entry := range entries {
		got = append(got, entry.Name())
	}
	slices.Sort(got)
	want := []string{"dri", "nvidia-uvm", "nvidia0.keep", "nvidia2", "nvidiactl"}
	if !slices.Equal(got, want) {
		t.Fatalf("/dev entries after stale old0 cleanup and current minor2 injection = %v, want %v", got, want)
	}
}

func TestRemoveStaleNvidiaRegularDeviceFilesDoesNotFollowDevSymlink(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	outsideDevice := filepath.Join(outside, "nvidia0")
	if err := os.WriteFile(outsideDevice, nil, 0600); err != nil {
		t.Fatalf("creating outside device: %v", err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "dev")); err != nil {
		t.Fatalf("creating /dev symlink: %v", err)
	}

	if err := removeStaleNvidiaRegularDeviceFiles(root); err == nil {
		t.Fatal("removeStaleNvidiaRegularDeviceFiles succeeded through /dev symlink")
	}
	if _, err := os.Stat(outsideDevice); err != nil {
		t.Fatalf("outside device was modified: %v", err)
	}
}

func TestRemoveStaleNvidiaRegularDeviceFilesRejectsNonPlaceholder(t *testing.T) {
	root := t.TempDir()
	devDir := filepath.Join(root, "dev")
	if err := os.Mkdir(devDir, 0755); err != nil {
		t.Fatalf("creating /dev: %v", err)
	}
	device := filepath.Join(devDir, "nvidia0")
	if err := os.WriteFile(device, []byte("user data"), 0600); err != nil {
		t.Fatalf("creating non-placeholder device: %v", err)
	}

	if err := removeStaleNvidiaRegularDeviceFiles(root); err == nil {
		t.Fatal("removeStaleNvidiaRegularDeviceFiles removed non-placeholder")
	}
	if got, err := os.ReadFile(device); err != nil || string(got) != "user data" {
		t.Fatalf("non-placeholder contents = %q, %v; want preserved", got, err)
	}
}

func TestRemoveStaleNvidiaRegularDeviceFilesDoesNotFollowDeviceSymlink(t *testing.T) {
	root := t.TempDir()
	devDir := filepath.Join(root, "dev")
	if err := os.Mkdir(devDir, 0755); err != nil {
		t.Fatalf("creating /dev: %v", err)
	}
	outside := filepath.Join(t.TempDir(), "outside")
	if err := os.WriteFile(outside, nil, 0600); err != nil {
		t.Fatalf("creating outside file: %v", err)
	}
	device := filepath.Join(devDir, "nvidia0")
	if err := os.Symlink(outside, device); err != nil {
		t.Fatalf("creating device symlink: %v", err)
	}

	if err := removeStaleNvidiaRegularDeviceFiles(root); err == nil {
		t.Fatal("removeStaleNvidiaRegularDeviceFiles removed device symlink")
	}
	if info, err := os.Lstat(device); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("device symlink was modified: info=%v, err=%v", info, err)
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatalf("outside file was modified: %v", err)
	}
}
