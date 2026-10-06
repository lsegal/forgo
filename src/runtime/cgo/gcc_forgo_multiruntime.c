// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build linux || darwin || freebsd

// Lets a forgo runtime in a c-shared library tell another forgo runtime's
// signal handler from a C one, so that os/signal.Notify signals reach every
// Go runtime in the process that asked for them while an earlier C handler
// still loses them. See runtime/forgo_multiruntime_unix.go.

#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdint.h>

#include "libcgo.h"

// The dynamic loader functions are weak outside macOS. A static glibc
// program then links without dlopen's static-linking warning, and an older
// glibc that keeps them in libdl needs no -ldl. When they are missing, every
// earlier handler is treated as a C handler, which is upstream's behavior.
#ifndef __APPLE__
#pragma weak dladdr
#pragma weak dlopen
#pragma weak dlsym
#pragma weak dlclose
#endif

// forgo_sighandler is the signal handler this module's Go runtime installs
// for os/signal.Notify, or 0 before it has installed one.
static uintptr_t forgo_sighandler;

// _forgo_cgo_sighandler is how another runtime reads forgo_sighandler. It
// is the only symbol this file exports, and it is only ever called through
// dlsym on this module's own handle.
uintptr_t
_forgo_cgo_sighandler(void)
{
	return __atomic_load_n(&forgo_sighandler, __ATOMIC_ACQUIRE);
}

// x_cgo_forgo_setsighandler records the handler this module's runtime
// installed. It is hidden so that a host loading several libraries with
// RTLD_GLOBAL cannot bind one runtime's call to another runtime's copy.
__attribute__((visibility("hidden"))) void
x_cgo_forgo_setsighandler(uintptr_t *fn)
{
	__atomic_store_n(&forgo_sighandler, *fn, __ATOMIC_RELEASE);
}

// x_cgo_forgo_isgosighandler sets arg[1] to 1 when the signal handler arg[0]
// is the one another forgo runtime's library installed for Notify, and to 0
// for anything else, such as a C handler or an upstream Go runtime.
__attribute__((visibility("hidden"))) void
x_cgo_forgo_isgosighandler(uintptr_t *arg)
{
	Dl_info info, symInfo;
	void *h;
	uintptr_t (*get)(void);

	arg[1] = 0;
#ifndef __APPLE__
	if (&dladdr == NULL || &dlopen == NULL || &dlsym == NULL || &dlclose == NULL) {
		return;
	}
#endif
	_cgo_tsan_acquire();
	if (dladdr((void*)arg[0], &info) == 0 || info.dli_fname == NULL) {
		goto out;
	}
	h = dlopen(info.dli_fname, RTLD_LAZY | RTLD_NOLOAD);
	if (h == NULL) {
		goto out;
	}
	get = (uintptr_t (*)(void))dlsym(h, "_forgo_cgo_sighandler");
	// The answer must come from the module that holds the handler, not one
	// that the loader found by name or among its dependencies.
	if (get != NULL && dladdr((void*)get, &symInfo) != 0 && symInfo.dli_fbase == info.dli_fbase && get() == arg[0]) {
		arg[1] = 1;
	}
	dlclose(h);
out:
	_cgo_tsan_release();
}
