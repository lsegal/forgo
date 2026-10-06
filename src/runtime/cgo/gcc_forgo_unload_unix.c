// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build unix

// Unloading a c-shared library (dlclose). See runtime/forgo_unload.go for
// the whole sequence; this is the C half.
//
// In a c-shared library the runtime calls x_cgo_forgo_lib_init before it
// starts its first thread. From then on every thread the runtime creates is
// joinable and listed here, so the unload can wait until each one has really
// left the library's code, and the library's destructor runs the Go
// teardown when the host unloads it.

#include <errno.h>
#include <signal.h>
#include <pthread.h>
#include <setjmp.h>
#include <stdlib.h>
#include <string.h>
#include <sys/mman.h>
#include <unistd.h>
#include "libcgo.h"
#include "libcgo_unix.h"

// struct forgo_unload_arg is filled in by runtime.forgoUnload.
// Keep in sync with forgoUnloadArg in runtime/forgo_unload.go.
struct forgo_unload_arg {
	uintptr ok;		// teardown finished; unmap the regions
	uintptr *regions;	// address, length pairs to unmap
	uintptr nregions;
	uintptr regionsSize;	// length of the mapping that holds regions
	uintptr tlsKey;		// pthread key holding g, plus one; 0 if none
};

extern void (*x_crosscall2_ptr)(void (*fn)(void *), void *, int, size_t);
extern void _cgo_forgo_delete_keys(void);

static int forgo_lib;
static void (*forgo_unload_fn)(void*);

static pthread_mutex_t forgo_threads_mu = PTHREAD_MUTEX_INITIALIZER;
static pthread_t *forgo_threads;
static size_t forgo_nthreads, forgo_capthreads;
static int forgo_joining;

static sigjmp_buf forgo_m0_jmp;

// Locked by an exit handler when the process exits; see _cgo_forgo_note_loaded.
static pthread_mutex_t *forgo_exit_mu;

// x_cgo_forgo_lib_init enables unloading. fn is the Go function that runs
// the teardown. Called before the runtime creates any thread.
void
x_cgo_forgo_lib_init(void *fn)
{
	forgo_unload_fn = (void (*)(void*))fn;
	forgo_lib = 1;
}

// _cgo_forgo_thread_create creates a runtime thread. In a c-shared library
// the thread is joinable and listed for the unload; otherwise it is
// detached, as before.
int
_cgo_forgo_thread_create(pthread_t *p, pthread_attr_t *attr, void* (*fn)(void*), void *arg)
{
	int err;

	if (!forgo_lib) {
		pthread_attr_setdetachstate(attr, PTHREAD_CREATE_DETACHED);
		return _cgo_try_pthread_create(p, attr, fn, arg);
	}
	pthread_attr_setdetachstate(attr, PTHREAD_CREATE_JOINABLE);
	// Hold the lock until the thread is listed, so that a thread that
	// exits at once cannot miss itself in the list and stay joinable.
	pthread_mutex_lock(&forgo_threads_mu);
	if (forgo_nthreads == forgo_capthreads) {
		size_t cap = forgo_capthreads ? 2*forgo_capthreads : 16;
		pthread_t *n = realloc(forgo_threads, cap * sizeof *n);
		if (n == NULL) {
			fatalf("runtime/cgo: out of memory in thread_start");
		}
		forgo_threads = n;
		forgo_capthreads = cap;
	}
	err = _cgo_try_pthread_create(p, attr, fn, arg);
	if (err == 0) {
		forgo_threads[forgo_nthreads++] = *p;
	}
	pthread_mutex_unlock(&forgo_threads_mu);
	return err;
}

// _cgo_forgo_thread_done is called by a runtime thread just before it
// returns from its start function. Outside an unload the thread detaches
// itself, as it would have been detached from the start; during one the
// unload joins it.
void
_cgo_forgo_thread_done(void)
{
	pthread_t self;
	size_t i;

	if (!forgo_lib) {
		return;
	}
	self = pthread_self();
	pthread_mutex_lock(&forgo_threads_mu);
	if (!forgo_joining) {
		for (i = 0; i < forgo_nthreads; i++) {
			if (pthread_equal(forgo_threads[i], self)) {
				forgo_threads[i] = forgo_threads[--forgo_nthreads];
				break;
			}
		}
		pthread_detach(self);
	}
	pthread_mutex_unlock(&forgo_threads_mu);
}

// x_cgo_forgo_join waits for every runtime thread to exit. The runtime
// calls it once all of its threads have left Go code for good.
void
x_cgo_forgo_join(void *unused __attribute__((unused)))
{
	pthread_t self;
	size_t i;

	self = pthread_self();
	pthread_mutex_lock(&forgo_threads_mu);
	forgo_joining = 1;
	pthread_mutex_unlock(&forgo_threads_mu);
	for (i = 0; i < forgo_nthreads; i++) {
		if (!pthread_equal(forgo_threads[i], self)) {
			pthread_join(forgo_threads[i], NULL);
		}
	}
	free(forgo_threads);
	forgo_threads = NULL;
	forgo_nthreads = forgo_capthreads = 0;
}

// forgo_m0_main runs the runtime's first thread. That thread never returns
// from runtime.rt0_go, so the unload leaves it with x_cgo_forgo_m0_exit.
static void*
forgo_m0_main(void *fn)
{
	if (sigsetjmp(forgo_m0_jmp, 0) == 0) {
		((void* (*)(void*))fn)(NULL);
	}
	_cgo_forgo_thread_done();
	return NULL;
}

// _cgo_forgo_sys_thread_create starts the runtime's first thread in a
// c-shared library. It reports 0 when unloading is not enabled, and the
// caller starts the thread as before.
int
_cgo_forgo_sys_thread_create(void* (*fn)(void*))
{
	pthread_attr_t attr;
	pthread_t p;
	int err;

	if (!forgo_lib) {
		return 0;
	}
	pthread_attr_init(&attr);
	err = _cgo_forgo_thread_create(&p, &attr, forgo_m0_main, (void*)fn);
	if (err != 0) {
		fatalf("pthread_create failed: %s", strerror(err));
	}
	return 1;
}

// x_cgo_forgo_m0_exit returns the runtime's first thread to forgo_m0_main.
// It runs on that thread's system stack, below forgo_m0_main's frame.
void
x_cgo_forgo_m0_exit(void *unused __attribute__((unused)))
{
	siglongjmp(forgo_m0_jmp, 1);
}

// The signal handler stub.
//
// Signal handlers form a chain: a handler installed on top of this
// runtime's (another Go runtime, a crash reporter) saves it and forwards
// signals to it. Once the library is unmapped, such a forward would jump
// into nothing. So the runtime installs its handler through a stub that is
// mapped outside the library and stays mapped after it is gone. While the
// library is loaded the stub jumps to the runtime's handler; at unload the
// runtime points it at whatever was installed before it, so handlers above
// keep working. See runtime/forgo_unload_unix.go.
//
// The stub is two pages: code, then the table it reads. Its entry
// point is the start of the code page, called as handler(sig, info, ctx).
// It looks up table->ent[sig&127] and, by kind:
//	0: jumps to fn;
//	1: returns if info is a Notify signal one forgo runtime handed down to
//	   another (si_errno is "fgo!"; see runtime/forgo_multiruntime_mark.go),
//	   and otherwise jumps to fn;
//	2: returns;
//	3: returns for a marked signal as for 1, and otherwise resets the signal
//	   to its default action and raises it again, so that it takes effect
//	   once the handler that forwarded it returns.
// The code ends with an 8-byte literal holding the table's address.

#if (defined(__linux__) && (defined(__x86_64__) || defined(__aarch64__))) || (defined(__APPLE__) && defined(__aarch64__))

// struct forgo_sigstub_table is filled in by the runtime.
// Keep in sync with forgoSigStubTable in runtime/forgo_unload_unix.go.
struct forgo_sigstub_table {
	uintptr owner;		// the runtime's handler while loaded; 0 after unload
	uintptr signal_fn;	// libc signal and raise, for kind 3
	uintptr raise_fn;
	uintptr pad;
	struct {
		uintptr fn;
		uintptr kind;
	} ent[128];
};

#if defined(__x86_64__)
// endbr64
// movq data(%rip), %rax
// movl %edi, %ecx; andl $127, %ecx; shll $4, %ecx
// leaq 32(%rax,%rcx), %rax
// movq 8(%rax), %rcx
// testq %rcx, %rcx; jz 1f
// testq %rsi, %rsi; jz 3f
// cmpl $0x66676f21, 4(%rsi); je 2f
// 3: cmpq $1, %rcx; je 1f
// cmpq $2, %rcx; je 2f
// movq data(%rip), %rax
// pushq %rdi; xorl %esi, %esi; callq *8(%rax); popq %rdi
// movq data(%rip), %rax; jmpq *16(%rax)
// 1: jmpq *(%rax)
// 2: retq
// nop; data: .quad
static const unsigned char forgo_sigstub_code[] = {
	0xf3, 0x0f, 0x1e, 0xfa, 0x48, 0x8b, 0x05, 0x4d, 0x00, 0x00, 0x00, 0x89,
	0xf9, 0x83, 0xe1, 0x7f, 0xc1, 0xe1, 0x04, 0x48, 0x8d, 0x44, 0x08, 0x20,
	0x48, 0x8b, 0x48, 0x08, 0x48, 0x85, 0xc9, 0x74, 0x32, 0x48, 0x85, 0xf6,
	0x74, 0x09, 0x81, 0x7e, 0x04, 0x21, 0x6f, 0x67, 0x66, 0x74, 0x26, 0x48,
	0x83, 0xf9, 0x01, 0x74, 0x1e, 0x48, 0x83, 0xf9, 0x02, 0x74, 0x1a, 0x48,
	0x8b, 0x05, 0x16, 0x00, 0x00, 0x00, 0x57, 0x31, 0xf6, 0xff, 0x50, 0x08,
	0x5f, 0x48, 0x8b, 0x05, 0x08, 0x00, 0x00, 0x00, 0xff, 0x60, 0x10, 0xff,
	0x20, 0xc3, 0x66, 0x90,
};
#else
// bti c
// ldr x16, data
// and w17, w0, #127
// add x16, x16, #32; add x16, x16, x17, lsl #4
// ldr x17, [x16, #8]
// cbz x17, 1f
// cbz x1, 3f
// ldr w9, [x1, #4]; movz w10, #0x6f21; movk w10, #0x6667, lsl #16
// cmp w9, w10; b.eq 2f
// 3: cmp x17, #1; b.eq 1f
// cmp x17, #2; b.eq 2f
// stp x29, x30, [sp, #-32]!; mov x29, sp; str x0, [sp, #16]
// ldr x16, data; ldr x16, [x16, #8]; mov x1, #0; blr x16
// ldr x0, [sp, #16]; ldp x29, x30, [sp], #32
// ldr x16, data; ldr x16, [x16, #16]; br x16
// 1: ldr x16, [x16]; br x16
// 2: ret
// data: .quad
static const unsigned char forgo_sigstub_code[] = {
	0x5f, 0x24, 0x03, 0xd5, 0xf0, 0x03, 0x00, 0x58, 0x11, 0x18, 0x00, 0x12,
	0x10, 0x82, 0x00, 0x91, 0x10, 0x12, 0x11, 0x8b, 0x11, 0x06, 0x40, 0xf9,
	0xf1, 0x02, 0x00, 0xb4, 0xc1, 0x00, 0x00, 0xb4, 0x29, 0x04, 0x40, 0xb9,
	0x2a, 0xe4, 0x8d, 0x52, 0xea, 0xcc, 0xac, 0x72, 0x3f, 0x01, 0x0a, 0x6b,
	0x60, 0x02, 0x00, 0x54, 0x3f, 0x06, 0x00, 0xf1, 0xe0, 0x01, 0x00, 0x54,
	0x3f, 0x0a, 0x00, 0xf1, 0xe0, 0x01, 0x00, 0x54, 0xfd, 0x7b, 0xbe, 0xa9,
	0xfd, 0x03, 0x00, 0x91, 0xe0, 0x0b, 0x00, 0xf9, 0x90, 0x01, 0x00, 0x58,
	0x10, 0x06, 0x40, 0xf9, 0x01, 0x00, 0x80, 0xd2, 0x00, 0x02, 0x3f, 0xd6,
	0xe0, 0x0b, 0x40, 0xf9, 0xfd, 0x7b, 0xc2, 0xa8, 0xd0, 0x00, 0x00, 0x58,
	0x10, 0x0a, 0x40, 0xf9, 0x00, 0x02, 0x1f, 0xd6, 0x10, 0x02, 0x40, 0xf9,
	0x00, 0x02, 0x1f, 0xd6, 0xc0, 0x03, 0x5f, 0xd6,
};
#endif

// x_cgo_forgo_sigstub_init maps the stub. arg points to five uintptrs:
// the runtime's handler (in), then the stub's entry point, its table, and
// the address and length of its mapping (out). The outputs stay zero when
// the stub cannot be mapped, such as under a hardened runtime that refuses
// executable memory; the runtime then installs its handler directly.
// Called from the runtime without a g, before it installs any handler.
__attribute__((visibility("hidden"))) void
x_cgo_forgo_sigstub_init(void *arg)
{
	uintptr *a = (uintptr*)arg;
	size_t pg = (size_t)sysconf(_SC_PAGESIZE);
	unsigned char *p;
	struct forgo_sigstub_table *t;

	p = mmap(NULL, 2*pg, PROT_READ|PROT_WRITE, MAP_PRIVATE|MAP_ANON, -1, 0);
	if (p == MAP_FAILED) {
		return;
	}
	t = (struct forgo_sigstub_table*)(p + pg);
	t->owner = a[0];
	t->signal_fn = (uintptr)signal;
	t->raise_fn = (uintptr)raise;
	memcpy(p, forgo_sigstub_code, sizeof forgo_sigstub_code);
	*(uintptr*)(p + sizeof forgo_sigstub_code) = (uintptr)t;
	if (mprotect(p, pg, PROT_READ|PROT_EXEC) != 0) {
		munmap(p, 2*pg);
		return;
	}
	__builtin___clear_cache((char*)p, (char*)p + pg);
	a[1] = (uintptr)p;
	a[2] = (uintptr)t;
	a[3] = (uintptr)p;
	a[4] = 2*pg;
}

// _cgo_forgo_sigstub_owner returns the handler of the runtime that installed
// h through its stub while that runtime is loaded, and 0 when h is not a
// stub or its runtime is gone. h must be a signal handler that no loaded
// module holds, so that its first bytes are readable code.
__attribute__((visibility("hidden"))) uintptr
_cgo_forgo_sigstub_owner(uintptr h)
{
	size_t pg = (size_t)sysconf(_SC_PAGESIZE);
	struct forgo_sigstub_table *t;

	if (h == 0 || h%pg != 0 || memcmp((void*)h, forgo_sigstub_code, sizeof forgo_sigstub_code) != 0) {
		return 0;
	}
	t = *(struct forgo_sigstub_table**)(h + sizeof forgo_sigstub_code);
	if ((uintptr)t != h + pg) {
		return 0;
	}
	return __atomic_load_n(&t->owner, __ATOMIC_ACQUIRE);
}

#else

__attribute__((visibility("hidden"))) void
x_cgo_forgo_sigstub_init(void *arg __attribute__((unused)))
{
}

__attribute__((visibility("hidden"))) uintptr
_cgo_forgo_sigstub_owner(uintptr h __attribute__((unused)))
{
	return 0;
}

#endif

#if defined(__linux__) || defined(__APPLE__)

extern int __cxa_atexit(void (*)(void*), void*, void*);

// _cgo_forgo_note_loaded runs on the first call into the library, under
// the runtime init lock. It registers an exit handler so the destructor can
// tell process exit from an unload: exit runs handlers registered without a
// shared object before any destructor, while dlclose runs only the ones
// registered by the library being closed. The handler is pthread_mutex_lock
// on memory that is never freed, so neither runs code from the library or
// touches its memory after it is gone.
void
_cgo_forgo_note_loaded(void)
{
	pthread_mutex_t *m;

	if (!forgo_lib || forgo_exit_mu != NULL) {
		return;
	}
	m = malloc(sizeof *m);
	if (m == NULL) {
		return;
	}
	pthread_mutex_init(m, NULL);
	if (__cxa_atexit((void (*)(void*))pthread_mutex_lock, m, NULL) != 0) {
		free(m);
		return;
	}
	forgo_exit_mu = m;
}

static int
forgo_exiting(void)
{
	if (forgo_exit_mu == NULL) {
		// Never called into; treat it as an unload.
		return 0;
	}
	if (pthread_mutex_trylock(forgo_exit_mu) != 0) {
		return 1;
	}
	pthread_mutex_unlock(forgo_exit_mu);
	return 0;
}

__attribute__((destructor))
static void
forgo_unload(void)
{
	struct forgo_unload_arg a;
	uintptr i;

	if (!forgo_lib || forgo_unload_fn == NULL || x_crosscall2_ptr == NULL) {
		return;
	}
	if (forgo_exiting()) {
		// The process is exiting; leave the runtime running, as it
		// always has.
		return;
	}
	_cgo_wait_runtime_init_done();

	memset(&a, 0, sizeof a);
	x_crosscall2_ptr(forgo_unload_fn, &a, sizeof a, 0);
	if (!a.ok) {
		return;
	}

	_cgo_forgo_delete_keys();
	if (a.tlsKey != 0) {
		pthread_key_delete((pthread_key_t)(a.tlsKey - 1));
	}
	for (i = 0; i < a.nregions; i++) {
		munmap((void*)a.regions[2*i], a.regions[2*i+1]);
	}
	if (a.regions != NULL) {
		munmap(a.regions, a.regionsSize);
	}
}

#else

void
_cgo_forgo_note_loaded(void)
{
}

#endif
