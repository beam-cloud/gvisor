// Copyright 2026 The gVisor Authors.
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

package cgroupfs

import (
	"bytes"
	"fmt"
	"strings"

	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sentry/fsimpl/kernfs"
	"gvisor.dev/gvisor/pkg/sentry/kernel"
	"gvisor.dev/gvisor/pkg/sentry/kernel/auth"
	"gvisor.dev/gvisor/pkg/sentry/vfs"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/usermem"
)

// +stateify savable
type freezerController struct {
	controllerCommon
	controllerStateless
	controllerNoResource

	isRoot bool

	mu sync.Mutex `state:"nosave"`

	selfFreezing bool
}

var _ controller = (*freezerController)(nil)

func newRootFreezerController(fs *filesystem) *freezerController {
	c := &freezerController{
		isRoot: true,
	}
	c.controllerCommon.init(kernel.CgroupControllerFreezer, fs)
	return c
}

// Clone implements controller.Clone.
func (c *freezerController) Clone() controller {
	c.mu.Lock()
	defer c.mu.Unlock()
	new := &freezerController{
		isRoot:       false,
		selfFreezing: false,
	}
	new.controllerCommon.cloneFromParent(c)
	return new
}

// AddControlFiles implements controller.AddControlFiles.
func (c *freezerController) AddControlFiles(ctx context.Context, creds *auth.Credentials, _ *cgroupInode, contents map[string]kernfs.Inode) {
	if !c.isRoot {
		contents["freezer.state"] = c.fs.newControllerWritableFile(ctx, creds, &freezerStateData{c: c}, true)
		contents["freezer.self_freezing"] = c.fs.newControllerFile(ctx, creds, &freezerSelfFreezingData{c: c}, true)
		contents["freezer.parent_freezing"] = c.fs.newControllerFile(ctx, creds, &freezerParentFreezingData{c: c}, true)
	}
}

func (c *freezerController) isParentFreezing() bool {
	for p := c.parent; p != nil; {
		pf, ok := p.(*freezerController)
		if !ok || pf == nil {
			break
		}
		pf.mu.Lock()
		frozen := pf.selfFreezing
		pf.mu.Unlock()
		if frozen {
			return true
		}
		p = pf.parent
	}
	return false
}

// +stateify savable
type freezerStateData struct {
	c *freezerController
}

// Generate implements vfs.DynamicBytesSource.Generate.
func (d *freezerStateData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	d.c.mu.Lock()
	self := d.c.selfFreezing
	d.c.mu.Unlock()

	parent := d.c.isParentFreezing()

	if self || parent {
		fmt.Fprintf(buf, "FROZEN\n")
	} else {
		fmt.Fprintf(buf, "THAWED\n")
	}
	return nil
}

// Write implements vfs.WritableDynamicBytesSource.Write.
func (d *freezerStateData) Write(ctx context.Context, fd *vfs.FileDescription, src usermem.IOSequence, offset int64) (int64, error) {
	const maxLen = 32
	buf := copyScratchBufferFromContext(ctx, maxLen)
	n, err := src.CopyIn(ctx, buf)
	if err != nil {
		return int64(n), err
	}
	str := strings.TrimSpace(string(buf[:n]))

	d.c.mu.Lock()
	defer d.c.mu.Unlock()

	switch str {
	case "FROZEN":
		d.c.selfFreezing = true
	case "THAWED":
		d.c.selfFreezing = false
	default:
		return int64(n), linuxerr.EINVAL
	}
	return int64(n), nil
}

// +stateify savable
type freezerSelfFreezingData struct {
	c *freezerController
}

// Generate implements vfs.DynamicBytesSource.Generate.
func (d *freezerSelfFreezingData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	d.c.mu.Lock()
	self := d.c.selfFreezing
	d.c.mu.Unlock()

	if self {
		fmt.Fprintf(buf, "1\n")
	} else {
		fmt.Fprintf(buf, "0\n")
	}
	return nil
}

// +stateify savable
type freezerParentFreezingData struct {
	c *freezerController
}

// Generate implements vfs.DynamicBytesSource.Generate.
func (d *freezerParentFreezingData) Generate(ctx context.Context, buf *bytes.Buffer) error {
	if d.c.isParentFreezing() {
		fmt.Fprintf(buf, "1\n")
	} else {
		fmt.Fprintf(buf, "0\n")
	}
	return nil
}
