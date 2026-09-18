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

package fuse

import (
	"testing"
	"time"

	"gvisor.dev/gvisor/pkg/abi/linux"
	"gvisor.dev/gvisor/pkg/context"
	"gvisor.dev/gvisor/pkg/errors/linuxerr"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/waiter"
)

// testBlocker is a context.Blocker that models only what the FUSE wait
// contract depends on, so that the tests below check the FUSE code rather than
// a second copy of kernel.Task's wait loop.
//
// An ordinary interrupt (sendSignal) is the delivery of a signal the
// application handles or ignores: it makes Block return, and it stays armed so
// that Interrupted reports it, but it must not end a BlockKillable wait. A
// fatal interrupt (sendKill) ends both.
type testBlocker struct {
	mu sync.Mutex

	// armed is set by an ordinary interrupt and reports what the task run
	// loop would observe on the way out of the syscall.
	armed bool

	// interruptCh carries ordinary interrupts to a Block wait.
	interruptCh chan struct{}

	// killCh is closed by a fatal interrupt.
	killCh chan struct{}

	// killed records that killCh has been closed.
	killed bool

	// blockCalls and blockKillableCalls count which wait a caller chose.
	blockCalls         int
	blockKillableCalls int
}

func newTestBlocker() *testBlocker {
	return &testBlocker{
		interruptCh: make(chan struct{}, 1),
		killCh:      make(chan struct{}),
	}
}

// sendSignal delivers an ordinary, non-fatal signal.
func (b *testBlocker) sendSignal() {
	b.mu.Lock()
	b.armed = true
	b.mu.Unlock()
	select {
	case b.interruptCh <- struct{}{}:
	default:
	}
}

// sendKill delivers a fatal signal.
func (b *testBlocker) sendKill() {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.killed {
		return
	}
	b.killed = true
	close(b.killCh)
}

// interrupted reports whether an ordinary interrupt is still pending, i.e.
// whether the signal would be delivered when the syscall returns.
func (b *testBlocker) interrupted() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.armed
}

func (b *testBlocker) counts() (block, killable int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.blockCalls, b.blockKillableCalls
}

// Block implements context.Blocker.Block. Any interrupt ends the wait.
func (b *testBlocker) Block(C <-chan struct{}) error {
	b.mu.Lock()
	b.blockCalls++
	b.mu.Unlock()
	select {
	case <-C:
		return nil
	case <-b.interruptCh:
		return linuxerr.ErrInterrupted
	case <-b.killCh:
		return linuxerr.ErrInterrupted
	}
}

// BlockKillable implements context.Blocker.BlockKillable. Only a fatal
// interrupt ends the wait.
func (b *testBlocker) BlockKillable(C <-chan struct{}) error {
	b.mu.Lock()
	b.blockKillableCalls++
	b.mu.Unlock()
	select {
	case <-C:
		return nil
	case <-b.killCh:
		return linuxerr.ErrInterrupted
	}
}

func (b *testBlocker) Interrupt() { b.sendSignal() }

func (b *testBlocker) Interrupted() bool { return b.interrupted() }

func (b *testBlocker) Killed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.killed
}

func (b *testBlocker) BlockOn(w waiter.Waitable, mask waiter.EventMask) bool {
	e, ch := waiter.NewChannelEntry(mask)
	w.EventRegister(&e)
	defer w.EventUnregister(&e)
	return b.Block(ch) == nil
}

func (b *testBlocker) BlockWithTimeout(C chan struct{}, haveTimeout bool, timeout time.Duration) (time.Duration, error) {
	return timeout, b.Block(C)
}

func (b *testBlocker) BlockWithTimeoutOn(w waiter.Waitable, mask waiter.EventMask, timeout time.Duration) (time.Duration, bool) {
	return timeout, b.BlockOn(w, mask)
}

func (b *testBlocker) UninterruptibleSleepStart() {}

func (b *testBlocker) UninterruptibleSleepFinish() {}

var _ context.Blocker = (*testBlocker)(nil)

// newTestRequest builds a request that is shaped like one the filesystem would
// send, without needing a live connection.
func newTestRequest(id linux.FUSEOpID, opcode linux.FUSEOpcode) *Request {
	return &Request{
		id: id,
		hdr: &linux.FUSEHeaderIn{
			Len:    linux.SizeOfFUSEHeaderIn,
			Opcode: opcode,
			Unique: id,
		},
	}
}

// completeFuture answers a pending request the way connection.sendResponse
// does.
func completeFuture(fut *futureResponse, id linux.FUSEOpID) {
	fut.hdr = &linux.FUSEHeaderOut{
		Len:    linux.SizeOfFUSEHeaderOut,
		Error:  0,
		Unique: id,
	}
	fut.data = fut.buf[:linux.SizeOfFUSEHeaderOut]
	close(fut.ch)
}

// TestResolveSurvivesOrdinarySignal is the regression test for a FUSE-backed
// syscall failing with EINTR because of a signal that was not meant to
// interrupt it (for example git's once-a-second SIGALRM progress timer
// arriving during unlink(2) on a FUSE mount). A request already handed to the
// server must stay in flight, and resolve must return the server's answer.
func TestResolveSurvivesOrdinarySignal(t *testing.T) {
	const id = linux.FUSEOpID(2)
	b := newTestBlocker()
	fut := newFutureResponse(newTestRequest(id, linux.FUSE_UNLINK))

	// Deliver an ordinary signal while the request is in flight, then let the
	// server answer.
	go func() {
		b.sendSignal()
		time.Sleep(10 * time.Millisecond)
		completeFuture(fut, id)
	}()

	res, err := fut.resolve(b)
	if err != nil {
		t.Fatalf("resolve returned %v, want nil: an ordinary signal must not abandon an in-flight FUSE request", err)
	}
	if res == nil {
		t.Fatalf("resolve returned a nil response, want the server's answer")
	}
	if got, want := res.hdr.Unique, id; got != want {
		t.Errorf("resolve returned response for request %d, want %d", got, want)
	}
	if err := res.Error(); err != nil {
		t.Errorf("response carries error %v, want nil", err)
	}

	// The signal is not swallowed: it is still pending when the syscall
	// returns.
	if !b.Interrupted() {
		t.Errorf("Interrupted() = false after resolve, want true: the pending signal must still be delivered")
	}

	// The wait must be the killable one.
	block, killable := b.counts()
	if killable == 0 {
		t.Errorf("resolve never called BlockKillable (Block calls: %d)", block)
	}
	if block != 0 {
		t.Errorf("resolve called Block %d times, want 0: an interruptible wait reintroduces the EINTR bug", block)
	}
}

// TestResolveAbortsOnFatalSignal checks the other half of Linux's
// request_wait_answer(): a fatal signal still ends the wait, so a killed
// process does not hang on an unanswered request.
func TestResolveAbortsOnFatalSignal(t *testing.T) {
	const id = linux.FUSEOpID(4)
	b := newTestBlocker()
	fut := newFutureResponse(newTestRequest(id, linux.FUSE_UNLINK))

	// The server never answers; only the kill ends the wait.
	go func() {
		time.Sleep(10 * time.Millisecond)
		b.sendKill()
	}()

	res, err := fut.resolve(b)
	if err == nil {
		t.Fatalf("resolve returned (%v, nil), want an error: a fatal signal must end the wait", res)
	}
	if !linuxerr.Equals(linuxerr.ErrInterrupted, err) {
		t.Errorf("resolve returned %v, want ErrInterrupted", err)
	}
}

// TestResolveAsyncDoesNotBlock keeps the async short circuit intact.
func TestResolveAsyncDoesNotBlock(t *testing.T) {
	b := newTestBlocker()
	req := newTestRequest(6, linux.FUSE_FORGET)
	req.async = true
	fut := newFutureResponse(req)

	res, err := fut.resolve(b)
	if err != nil || res != nil {
		t.Fatalf("resolve on an async request returned (%v, %v), want (nil, nil)", res, err)
	}
	if block, killable := b.counts(); block != 0 || killable != 0 {
		t.Errorf("resolve on an async request blocked (Block: %d, BlockKillable: %d), want neither", block, killable)
	}
}

// TestCallFutureQueueWaitSurvivesOrdinarySignal covers the second wait a
// request can hit: the one for a free slot when the connection is already at
// maxActiveRequests. Linux waits for a free slot in TASK_KILLABLE
// (fs/fuse/dev.c:fuse_get_req), so an ordinary signal must not fail the
// operation here either.
func TestCallFutureQueueWaitSurvivesOrdinarySignal(t *testing.T) {
	conn := &connection{
		connected:         true,
		fullQueueCh:       make(chan struct{}, 1),
		completions:       make(map[linux.FUSEOpID]*futureResponse),
		maxActiveRequests: 1,
		numActiveRequests: 1,
	}
	b := newTestBlocker()

	go func() {
		b.sendSignal()
		time.Sleep(10 * time.Millisecond)
		// A request completed, freeing a slot.
		conn.mu.Lock()
		conn.numActiveRequests--
		conn.mu.Unlock()
		conn.fullQueueCh <- struct{}{}
	}()

	fut, err := conn.callFuture(b, newTestRequest(8, linux.FUSE_UNLINK))
	if err != nil {
		t.Fatalf("callFuture returned %v, want nil: an ordinary signal must not fail a request waiting for a free slot", err)
	}
	if fut == nil {
		t.Fatalf("callFuture returned a nil future")
	}
	if block, _ := b.counts(); block != 0 {
		t.Errorf("callFuture called Block %d times, want 0", block)
	}
}

// TestCallFutureQueueWaitAbortsOnFatalSignal is the fatal half of the above.
func TestCallFutureQueueWaitAbortsOnFatalSignal(t *testing.T) {
	conn := &connection{
		connected:         true,
		fullQueueCh:       make(chan struct{}, 1),
		completions:       make(map[linux.FUSEOpID]*futureResponse),
		maxActiveRequests: 1,
		numActiveRequests: 1,
	}
	b := newTestBlocker()

	go func() {
		time.Sleep(10 * time.Millisecond)
		b.sendKill()
	}()

	if _, err := conn.callFuture(b, newTestRequest(10, linux.FUSE_UNLINK)); err == nil {
		t.Fatalf("callFuture returned nil error, want an error: a fatal signal must end the wait")
	}
}
