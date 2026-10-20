// Copyright 2018 The gVisor Authors.
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

package kernel

import (
	"testing"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/sentry/kernel/sched"
	"gvisor.dev/gvisor/pkg/sentry/platform"
)

type oomTestContext struct{ platform.Context }

func (*oomTestContext) Interrupt() {}

func newOOMTestTask() *Task {
	tg := &ThreadGroup{signalHandlers: NewSignalHandlers()}
	task := &Task{k: &Kernel{}, p: &oomTestContext{}, interruptChan: make(chan struct{}, 1)}
	task.tg = tg
	tg.tasks.PushBack(task)
	prefix := "OOM test: "
	task.logPrefix.Store(&prefix)
	return task
}

func TestConcurrentMemoryOOMKillsCountOneVictim(t *testing.T) {
	for attempt := 0; attempt < 100; attempt++ {
		victim := newOOMTestTask()
		start := make(chan struct{})
		results := make(chan bool, 32)
		for i := 0; i < cap(results); i++ {
			go func() {
				<-start
				sent, err := victim.sendMemoryOOMKill()
				if err != nil {
					t.Errorf("sendMemoryOOMKill: %v", err)
				}
				results <- sent
			}()
		}
		close(start)
		var sent int
		for i := 0; i < cap(results); i++ {
			if <-results {
				sent++
			}
		}
		if got := victim.k.GuestOOMKills(); got != 1 || sent != 1 {
			t.Fatalf("attempt %d: OOM kills=%d, new signals=%d; want one victim", attempt, got, sent)
		}
	}
}

func TestMemoryOOMKillDoesNotCountEarlierSIGKILL(t *testing.T) {
	victim := newOOMTestTask()
	signals := victim.tg.signalLock()
	result := make(chan bool, 1)
	go func() {
		sent, err := victim.sendMemoryOOMKill()
		if err != nil {
			t.Errorf("sendMemoryOOMKill: %v", err)
		}
		result <- sent
	}()
	// An ordinary kill wins while the OOM path is waiting for the signal lock.
	err := victim.sendSignalLocked(SignalInfoPriv(linux.SIGKILL), true /* group */)
	signals.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	if sent := <-result; sent || victim.k.GuestOOMKills() != 0 {
		t.Fatalf("ordinary SIGKILL was counted as an OOM: sent=%v, kills=%d", sent, victim.k.GuestOOMKills())
	}
}

func TestTaskCPU(t *testing.T) {
	for _, test := range []struct {
		mask sched.CPUSet
		tid  ThreadID
		cpu  int32
	}{
		{
			mask: []byte{0xff},
			tid:  1,
			cpu:  1,
		},
		{
			mask: []byte{0xff},
			tid:  10,
			cpu:  2,
		},
		{
			// more than 8 cpus.
			mask: []byte{0xff, 0xff},
			tid:  10,
			cpu:  10,
		},
		{
			// missing the first cpu.
			mask: []byte{0xfe},
			tid:  1,
			cpu:  2,
		},
		{
			mask: []byte{0xfe},
			tid:  10,
			cpu:  4,
		},
		{
			// missing the fifth cpu.
			mask: []byte{0xef},
			tid:  10,
			cpu:  3,
		},
		{
			// only the fifth cpu.
			mask: []byte{0x10},
			tid:  10,
			cpu:  4,
		},
	} {
		assigned := assignCPU(test.mask, test.tid)
		if test.cpu != assigned {
			t.Errorf("assignCPU(%v, %v) got %v, want %v", test.mask, test.tid, assigned, test.cpu)
		}
	}

}
