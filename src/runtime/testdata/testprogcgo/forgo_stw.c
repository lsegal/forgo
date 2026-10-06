// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build !plan9 && !windows

#include <pthread.h>
#include "_cgo_export.h"

#define STWTHREADS 16

static void* forgoSTWThread(void* arg) {
	ForgoSTWCallback();
	return NULL;
}

// ForgoSTWThreads starts threads that each call into Go once and then
// exit, and waits for them.
void ForgoSTWThreads(void) {
	int i;
	pthread_t t[STWTHREADS];

	for (i = 0; i < STWTHREADS; i++) {
		pthread_create(&t[i], NULL, forgoSTWThread, NULL);
	}
	for (i = 0; i < STWTHREADS; i++) {
		pthread_join(t[i], NULL);
	}
}
