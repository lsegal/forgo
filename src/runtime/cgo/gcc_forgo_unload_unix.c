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
