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
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	specs "github.com/opencontainers/runtime-spec/specs-go"
	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/abi/nvgpu"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/fspath"
	"gvisor.dev/gvisor/pkg/memutil"
	"gvisor.dev/gvisor/pkg/sentry/devices/nvproxy"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/dev"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/tmpfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/ktime"
	"gvisor.dev/gvisor/pkg/sentry/pgalloc"
	"gvisor.dev/gvisor/pkg/sentry/state"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
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

type remappedNvproxyDeviceTest struct {
	ctx           context.Context
	l             *Loader
	vfsObj        *vfs.VirtualFilesystem
	namespaceRoot vfs.VirtualDentry
	devRoots      []vfs.VirtualDentry
}

func setupRemappedNvproxyDeviceTest(t *testing.T) *remappedNvproxyDeviceTest {
	t.Helper()
	memFD, err := memutil.CreateMemFD("nvproxy-device-test", 0)
	if err != nil {
		t.Fatalf("creating memory file: %v", err)
	}
	mf, err := pgalloc.NewMemoryFile(os.NewFile(uintptr(memFD), "nvproxy-device-test"), pgalloc.MemoryFileOpts{
		DelayedEviction:         pgalloc.DelayedEvictionDisabled,
		DisableMemoryAccounting: true,
		DisableIMAWorkAround:    true,
	})
	if err != nil {
		t.Fatalf("initializing memory file: %v", err)
	}
	ctx := context.WithValue(context.Background(), pgalloc.CtxMemoryFile, mf)
	ctx = context.WithValue(ctx, ktime.CtxRealtimeClock, &ktime.SyntheticClock{})
	ctx = auth.ContextWithCredentials(ctx, auth.NewRootCredentials(auth.NewRootUserNamespace()))
	k := &kernel.Kernel{}
	vfsObj := k.VFS()
	if err := vfsObj.Init(ctx); err != nil {
		t.Fatalf("VFS init: %v", err)
	}
	vfsObj.MustRegisterFilesystemType(tmpfs.Name, tmpfs.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{AllowUserMount: true})
	vfsObj.MustRegisterFilesystemType(dev.Name, &dev.FilesystemType{}, &vfs.RegisterFilesystemTypeOptions{AllowUserMount: true})
	mntns, err := vfsObj.NewMountNamespace(ctx, auth.CredentialsFromContext(ctx), "root", tmpfs.Name, &vfs.MountOptions{}, nil)
	if err != nil {
		t.Fatalf("creating root mount namespace: %v", err)
	}
	test := &remappedNvproxyDeviceTest{
		ctx:           ctx,
		l:             &Loader{k: k},
		vfsObj:        vfsObj,
		namespaceRoot: mntns.Root(ctx),
	}
	t.Cleanup(func() {
		for _, root := range test.devRoots {
			root.DecRef(ctx)
		}
		test.namespaceRoot.DecRef(ctx)
		mntns.DecRef(ctx)
		vfsObj.Release(ctx)
		mf.Destroy()
	})
	return test
}

func (test *remappedNvproxyDeviceTest) mountDev(t *testing.T) vfs.VirtualDentry {
	t.Helper()
	name := fmt.Sprintf("dev%d", len(test.devRoots))
	pop := vfs.PathOperation{
		Root:  test.namespaceRoot,
		Start: test.namespaceRoot,
		Path:  fspath.Parse(name),
	}
	creds := auth.CredentialsFromContext(test.ctx)
	if err := test.vfsObj.MkdirAt(test.ctx, creds, &pop, &vfs.MkdirOptions{Mode: 0755}); err != nil {
		t.Fatalf("creating %s mountpoint: %v", name, err)
	}
	if _, err := test.vfsObj.MountAt(test.ctx, creds, "dev", &pop, dev.Name, &vfs.MountOptions{}); err != nil {
		t.Fatalf("mounting %s: %v", name, err)
	}
	root, err := test.vfsObj.GetDentryAt(test.ctx, creds, &pop, &vfs.GetDentryOptions{})
	if err != nil {
		t.Fatalf("getting %s root: %v", name, err)
	}
	test.devRoots = append(test.devRoots, root)
	return root
}

func mknodNvproxyTestDevice(t *testing.T, ctx context.Context, vfsObj *vfs.VirtualFilesystem, root vfs.VirtualDentry, name string, minor uint32) {
	t.Helper()
	if err := vfsObj.MknodAt(ctx, auth.CredentialsFromContext(ctx), &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(name),
	}, &vfs.MknodOptions{
		Mode:     linux.FileMode(linux.S_IFCHR | 0o666),
		DevMajor: nvgpu.NV_MAJOR_DEVICE_NUMBER,
		DevMinor: minor,
	}); err != nil {
		t.Fatalf("creating %s: %v", name, err)
	}
}

func nvproxyRemapContext(ctx context.Context, oldMinor, newMinor uint32) context.Context {
	return nvproxyRemapContextForMinors(ctx, map[uint32]uint32{oldMinor: newMinor})
}

func nvproxyRemapContextForMinors(ctx context.Context, newMinorByOld map[uint32]uint32) context.Context {
	newDeviceByOld := make(map[*nvproxy.DeviceRemapID]*nvproxy.DeviceRemapID)
	oldDeviceByMinor := make(map[uint32]*nvproxy.DeviceRemapID)
	for oldMinor, newMinor := range newMinorByOld {
		oldID := &nvproxy.DeviceRemapID{Minor: oldMinor}
		newID := &nvproxy.DeviceRemapID{Minor: newMinor}
		newDeviceByOld[oldID] = newID
		oldDeviceByMinor[oldMinor] = oldID
	}
	return context.WithValue(ctx, nvproxy.CtxDeviceRemapping, &nvproxy.DeviceRemapping{
		NewDeviceByOld:            newDeviceByOld,
		OldDeviceByMinor:          oldDeviceByMinor,
		OldDeviceByDeviceInstance: make(map[uint32]*nvproxy.DeviceRemapID),
	})
}

func assertNvproxyTestDevice(t *testing.T, ctx context.Context, vfsObj *vfs.VirtualFilesystem, root vfs.VirtualDentry, name string, minor uint32) {
	t.Helper()
	stat, err := vfsObj.StatAt(ctx, auth.CredentialsFromContext(ctx), &vfs.PathOperation{
		Root:  root,
		Start: root,
		Path:  fspath.Parse(name),
	}, &vfs.StatOptions{Mask: linux.STATX_TYPE})
	if err != nil {
		t.Fatalf("statting %s: %v", name, err)
	}
	if ftype := stat.Mode & linux.S_IFMT; ftype != linux.S_IFCHR || stat.RdevMajor != nvgpu.NV_MAJOR_DEVICE_NUMBER || stat.RdevMinor != minor {
		t.Fatalf("%s has type %v and rdev (%d, %d), want character device (%d, %d)", name, ftype, stat.RdevMajor, stat.RdevMinor, nvgpu.NV_MAJOR_DEVICE_NUMBER, minor)
	}
}

func TestCreateRemappedNvproxyDeviceFilesOldZeroToNonzero(t *testing.T) {
	test := setupRemappedNvproxyDeviceTest(t)
	root := test.mountDev(t)
	mknodNvproxyTestDevice(t, test.ctx, test.vfsObj, root, "nvidia0", 0)
	ctx := nvproxyRemapContext(test.ctx, 0, 2)

	if err := test.l.createRemappedNvproxyDeviceFiles(ctx); err != nil {
		t.Fatalf("createRemappedNvproxyDeviceFiles: %v", err)
	}
	assertNvproxyTestDevice(t, ctx, test.vfsObj, root, "nvidia2", 2)
}

func TestCreateRemappedNvproxyDeviceFilesFailsWithoutSource(t *testing.T) {
	test := setupRemappedNvproxyDeviceTest(t)
	test.mountDev(t)
	ctx := nvproxyRemapContext(test.ctx, 0, 2)

	err := test.l.createRemappedNvproxyDeviceFiles(ctx)
	if err == nil || !strings.Contains(err.Error(), "no restored NVIDIA source device file") {
		t.Fatalf("createRemappedNvproxyDeviceFiles error = %v, want missing source error", err)
	}
}

func TestCreateRemappedNvproxyDeviceFilesValidatesExistingTarget(t *testing.T) {
	test := setupRemappedNvproxyDeviceTest(t)
	root := test.mountDev(t)
	mknodNvproxyTestDevice(t, test.ctx, test.vfsObj, root, "nvidia0", 0)
	mknodNvproxyTestDevice(t, test.ctx, test.vfsObj, root, "nvidia2", 3)
	ctx := nvproxyRemapContext(test.ctx, 0, 2)

	err := test.l.createRemappedNvproxyDeviceFiles(ctx)
	if err == nil || !strings.Contains(err.Error(), "want character device") {
		t.Fatalf("createRemappedNvproxyDeviceFiles error = %v, want invalid target error", err)
	}
}

func TestCreateRemappedNvproxyDeviceFilesAcrossSeparateDevFilesystems(t *testing.T) {
	test := setupRemappedNvproxyDeviceTest(t)
	root0 := test.mountDev(t)
	root1 := test.mountDev(t)
	mknodNvproxyTestDevice(t, test.ctx, test.vfsObj, root0, "nvidia0", 0)
	mknodNvproxyTestDevice(t, test.ctx, test.vfsObj, root1, "nvidia1", 1)
	ctx := nvproxyRemapContextForMinors(test.ctx, map[uint32]uint32{0: 1, 1: 0})

	if err := test.l.createRemappedNvproxyDeviceFiles(ctx); err != nil {
		t.Fatalf("createRemappedNvproxyDeviceFiles: %v", err)
	}
	assertNvproxyTestDevice(t, ctx, test.vfsObj, root0, "nvidia1", 1)
	assertNvproxyTestDevice(t, ctx, test.vfsObj, root1, "nvidia0", 0)
}

func TestCreateRemappedNvproxyDeviceFilesValidatesNoopSource(t *testing.T) {
	test := setupRemappedNvproxyDeviceTest(t)
	root := test.mountDev(t)
	mknodNvproxyTestDevice(t, test.ctx, test.vfsObj, root, "nvidia0", 3)
	ctx := nvproxyRemapContext(test.ctx, 0, 0)

	err := test.l.createRemappedNvproxyDeviceFiles(ctx)
	if err == nil || !strings.Contains(err.Error(), "no restored NVIDIA source device file") {
		t.Fatalf("createRemappedNvproxyDeviceFiles error = %v, want invalid no-op source error", err)
	}
}

func TestCreateRemappedNvproxyDeviceFilesDoesNotReuseCreatedTargetAsSource(t *testing.T) {
	test := setupRemappedNvproxyDeviceTest(t)
	root := test.mountDev(t)
	mknodNvproxyTestDevice(t, test.ctx, test.vfsObj, root, "nvidia0", 0)
	ctx := nvproxyRemapContextForMinors(test.ctx, map[uint32]uint32{0: 1, 1: 2})

	err := test.l.createRemappedNvproxyDeviceFiles(ctx)
	if err == nil || !strings.Contains(err.Error(), "old minor(s) [1]") {
		t.Fatalf("createRemappedNvproxyDeviceFiles error = %v, want missing original minor 1 error", err)
	}
}
