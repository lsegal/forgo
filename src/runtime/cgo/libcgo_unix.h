// Copyright 2016 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

#include <pthread.h>

/*
 * Call pthread_create, retrying on EAGAIN.
 */
extern int _cgo_try_pthread_create(pthread_t*, const pthread_attr_t*, void* (*)(void*), void*);

extern void* threadentry(void*);

/*
 * Unloading a c-shared library (forgo); see gcc_forgo_unload_unix.c.
 */
extern int _cgo_forgo_thread_create(pthread_t*, pthread_attr_t*, void* (*)(void*), void*);
extern int _cgo_forgo_sys_thread_create(void* (*)(void*));
extern void _cgo_forgo_thread_done(void);
extern void _cgo_forgo_note_loaded(void);
