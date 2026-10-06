// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Stopping the world when a goroutine enters a system call or returns to C
// at the moment the stop begins.
//
// stopTheWorldWithSema takes the Ps of goroutines in system calls once, right
// after it sets sched.gcwaiting, and then waits for the remaining Ps to stop
// themselves. A goroutine that is entering a system call gives its P up when
// it sees gcwaiting, but it reads gcwaiting before it switches to _Gsyscall,
// so one that read it just before the stop began is still _Grunning when the
// stop looks and _Gsyscall right after, with its P. The stop then has to
// wait until the call returns, and sysmon, which would otherwise take the P,
// sleeps while the world is stopping.
//
// That wait is unbounded when the goroutine is a callback from C on an extra
// M: cgocallback enters a system call on its way back to C, and the C thread
// may never call into this runtime again. A plugin host's worker threads do
// exactly that: they call into a library a few times and exit, so a stop
// that started during their last call never ended (lsegal/forgo#46).
//
// forgoStopSyscallPs takes those Ps on every round of the wait instead.

package runtime

// forgoStopSyscallPs stops the Ps whose goroutines entered a system call, or
// went back to C, after the world began to stop. It is called by
// stopTheWorldWithSema each time it wakes up to preempt the remaining Ps.
func forgoStopSyscallPs() {
	lock(&sched.lock)
	stopped := false
	for _, pp := range allp {
		if thread, ok := setBlockOnExitSyscall(pp); ok {
			thread.gcstopP()
			thread.resume()
			stopped = true
		}
	}
	if stopped && sched.stopwait == 0 {
		notewakeup(&sched.stopnote)
	}
	unlock(&sched.lock)
}
