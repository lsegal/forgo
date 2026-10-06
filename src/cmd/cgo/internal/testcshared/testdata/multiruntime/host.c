// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// A C host that loads two c-shared libraries, each with its own Go
// runtime, into one process, the way a plugin host (a DAW loading VST or
// CLAP plugins, Python loading extensions) does.
//
// Usage: host MODE LIBA LIBB
//
// Prints PASS on success.

#include <stdio.h>
#include <stdlib.h>
#include <string.h>

#ifdef _WIN32
#include <windows.h>
#else
#include <dlfcn.h>
#include <pthread.h>
#include <setjmp.h>
#include <signal.h>
#endif

typedef int (*work_fn)(int);
typedef void (*setpeer_fn)(void*);
typedef int (*bounce_fn)(int);
typedef int (*int_fn)(void);
typedef void (*void_fn)(void);
typedef int (*work2_fn)(int, int);

struct lib {
	const char* path;
	work_fn work;
	setpeer_fn setpeer;
	bounce_fn bounce;
	int_fn fault;
	void_fn startspin;
	int_fn gc;
	int_fn newinstance;
	work2_fn process;
	work_fn destroyinstance;
	int_fn liveinstances;
};

static struct lib libs[2];

static void fail(const char* msg, const char* detail) {
	fprintf(stderr, "FAIL: %s%s%s\n", msg, detail ? ": " : "", detail ? detail : "");
	exit(1);
}

#ifdef _WIN32

static void* lookup(void* h, const char* name) {
	void* p = (void*)GetProcAddress((HMODULE)h, name);
	if (p == NULL) {
		fail("GetProcAddress", name);
	}
	return p;
}

static void load(struct lib* l) {
	HMODULE h = LoadLibraryA(l->path);
	if (h == NULL) {
		fail("LoadLibrary", l->path);
	}
#define LOOKUP(field, name) l->field = lookup(h, name)
	LOOKUP(work, "Work");
	LOOKUP(setpeer, "SetPeer");
	LOOKUP(bounce, "Bounce");
	LOOKUP(fault, "Fault");
	LOOKUP(startspin, "StartSpin");
	LOOKUP(gc, "CollectGarbage");
	LOOKUP(newinstance, "NewInstance");
	LOOKUP(process, "Process");
	LOOKUP(destroyinstance, "DestroyInstance");
	LOOKUP(liveinstances, "LiveInstances");
#undef LOOKUP
}

// open_ref opens another reference to an already loaded library, as a
// host does for every plugin instance; close_ref drops it.
static void* open_ref(const char* path) {
	HMODULE h = LoadLibraryA(path);
	if (h == NULL) {
		fail("LoadLibrary", path);
	}
	return h;
}

static void close_ref(void* h) {
	FreeLibrary((HMODULE)h);
}

typedef HANDLE thread_t;

static DWORD WINAPI thread_start(LPVOID arg) {
	((void (*)(long))((void**)arg)[0])((long)(size_t)((void**)arg)[1]);
	free(arg);
	return 0;
}

static void start_thread(thread_t* t, void (*fn)(long), long arg) {
	void** a = malloc(2 * sizeof(void*));
	a[0] = (void*)fn;
	a[1] = (void*)(size_t)arg;
	*t = CreateThread(NULL, 0, thread_start, a, 0, NULL);
	if (*t == NULL) {
		fail("CreateThread", NULL);
	}
}

static void join_thread(thread_t t) {
	WaitForSingleObject(t, INFINITE);
	CloseHandle(t);
}

#else

static void* lookup(void* h, const char* name) {
	void* p = dlsym(h, name);
	if (p == NULL) {
		fail("dlsym", name);
	}
	return p;
}

static void load(struct lib* l) {
	// Plugin hosts normally use RTLD_LOCAL, so each plugin's symbols
	// stay out of the global namespace; some use RTLD_GLOBAL.
	int global = getenv("MULTIRUNTIME_RTLD_GLOBAL") != NULL;
	void* h = dlopen(l->path, RTLD_NOW | (global ? RTLD_GLOBAL : RTLD_LOCAL));
	if (h == NULL) {
		fail("dlopen", dlerror());
	}
#define LOOKUP(field, name) l->field = lookup(h, name)
	LOOKUP(work, "Work");
	LOOKUP(setpeer, "SetPeer");
	LOOKUP(bounce, "Bounce");
	LOOKUP(fault, "Fault");
	LOOKUP(startspin, "StartSpin");
	LOOKUP(gc, "CollectGarbage");
	LOOKUP(newinstance, "NewInstance");
	LOOKUP(process, "Process");
	LOOKUP(destroyinstance, "DestroyInstance");
	LOOKUP(liveinstances, "LiveInstances");
#undef LOOKUP
}

// open_ref opens another reference to an already loaded library, as a
// host does for every plugin instance; close_ref drops it.
static void* open_ref(const char* path) {
	void* h = dlopen(path, RTLD_NOW | RTLD_LOCAL);
	if (h == NULL) {
		fail("dlopen", dlerror());
	}
	return h;
}

static void close_ref(void* h) {
	dlclose(h);
}

typedef pthread_t thread_t;

static void* thread_start(void* arg) {
	((void (*)(long))((void**)arg)[0])((long)(size_t)((void**)arg)[1]);
	free(arg);
	return NULL;
}

static void start_thread(thread_t* t, void (*fn)(long), long arg) {
	void** a = malloc(2 * sizeof(void*));
	a[0] = (void*)fn;
	a[1] = (void*)(size_t)arg;
	if (pthread_create(t, NULL, thread_start, a) != 0) {
		fail("pthread_create", NULL);
	}
}

static void join_thread(thread_t t) {
	pthread_join(t, NULL);
}

#endif

#define NTHREADS 8

// stress: every thread calls into both runtimes in turn, and each call
// allocates and forces collections in the runtime it lands in.
static void stress_thread(long id) {
	int i;

	for (i = 0; i < 1000; i++) {
		libs[(i + id) % 2].work(i % 64 + 1);
	}
}

// nested: host -> A -> C -> B -> C -> A ..., with a panic recovered in
// every frame, on several threads at once.
static void nested_thread(long id) {
	int i, depth = 6;

	for (i = 0; i < 200; i++) {
		int got = libs[(i + id) % 2].bounce(depth);
		if (got != depth + 1) {
			fprintf(stderr, "FAIL: Bounce(%d) recovered %d panics, want %d\n", depth, got, depth + 1);
			exit(1);
		}
	}
}

// instances: many instances of each plugin, each opened through its own
// reference to the library, driven from every thread in turn (a host may
// move an instance between audio threads, but never runs one instance on
// two threads at once), then destroyed.
#define NINSTANCES 24
#define NROUNDS 100

struct instance {
	struct lib* lib;
	void* ref;
	int id;
	int calls;
};

static struct instance insts[2 * NINSTANCES];
static int round_no;

static void instances_thread(long id) {
	int i;

	for (i = 0; i < 2 * NINSTANCES; i++) {
		struct instance* in = &insts[i];
		int got;

		if ((i + round_no) % NTHREADS != id) {
			continue;
		}
		got = in->lib->process(in->id, 64 + i);
		if (got != ++in->calls) {
			fprintf(stderr, "FAIL: instance %d of %s processed %d blocks, want %d\n", in->id, in->lib->path, got, in->calls);
			exit(1);
		}
	}
}

static void run_threads(void (*fn)(long));

static void run_instances(void) {
	int i, w;

	for (i = 0; i < 2 * NINSTANCES; i++) {
		struct instance* in = &insts[i];

		in->lib = &libs[i % 2];
		in->ref = open_ref(in->lib->path);
		in->id = in->lib->newinstance();
	}
	for (round_no = 0; round_no < NROUNDS; round_no++) {
		run_threads(instances_thread);
	}
	for (i = 0; i < 2 * NINSTANCES; i++) {
		if (insts[i].lib->destroyinstance(insts[i].id) != 1) {
			fail("DestroyInstance of a live instance failed", NULL);
		}
		close_ref(insts[i].ref);
	}
	for (w = 0; w < 2; w++) {
		if (libs[w].liveinstances() != 0) {
			fail("instances left alive after destroying all of them", libs[w].path);
		}
	}
}

static volatile int busy_stop;

static void busy_thread(long which) {
	while (!busy_stop) {
		libs[which].work(32);
	}
}

static void run_threads(void (*fn)(long)) {
	thread_t t[NTHREADS];
	long i;

	for (i = 0; i < NTHREADS; i++) {
		start_thread(&t[i], fn, i);
	}
	for (i = 0; i < NTHREADS; i++) {
		join_thread(t[i]);
	}
}

#ifndef _WIN32
static sigjmp_buf hostjmp;
static volatile sig_atomic_t hostfaults;

static void host_segv(int sig, siginfo_t* info, void* ctx) {
	hostfaults++;
	siglongjmp(hostjmp, 1);
}

static void install_host_handler(void) {
	struct sigaction sa;

	memset(&sa, 0, sizeof sa);
	sa.sa_sigaction = host_segv;
	sa.sa_flags = SA_SIGINFO | SA_ONSTACK;
	sigemptyset(&sa.sa_mask);
	if (sigaction(SIGSEGV, &sa, NULL) != 0 || sigaction(SIGBUS, &sa, NULL) != 0) {
		fail("sigaction", NULL);
	}
}

// host_fault faults in C code, which the host's own handler must see.
static void host_fault(void) {
	if (sigsetjmp(hostjmp, 1) == 0) {
		*(volatile int*)(size_t)8 = 1;
		fail("C fault did not fault", NULL);
	}
}
#endif

int main(int argc, char** argv) {
	const char* mode;
	int i, w;

	if (argc != 4) {
		fprintf(stderr, "usage: %s MODE LIBA LIBB\n", argv[0]);
		return 2;
	}
	mode = argv[1];
	libs[0].path = argv[2];
	libs[1].path = argv[3];

#ifndef _WIN32
	if (strcmp(mode, "hostsig") == 0) {
		install_host_handler();
	}
#endif

	load(&libs[0]);
	if (strcmp(mode, "upstream") == 0) {
		// golang/go#65050: call the first library while the second
		// runtime is still starting up.
		libs[0].work(1);
		load(&libs[1]);
		libs[1].work(1);
		printf("PASS\n");
		return 0;
	}
	load(&libs[1]);
	libs[0].setpeer((void*)libs[1].bounce);
	libs[1].setpeer((void*)libs[0].bounce);

	if (strcmp(mode, "stress") == 0) {
		run_threads(stress_thread);
	} else if (strcmp(mode, "instances") == 0) {
		run_instances();
	} else if (strcmp(mode, "nested") == 0) {
		run_threads(nested_thread);
	} else if (strcmp(mode, "fault") == 0) {
		// A nil dereference in one runtime is recovered there while the
		// other runtime is busy on another thread, in both directions.
		for (w = 0; w < 2; w++) {
			thread_t t;

			busy_stop = 0;
			start_thread(&t, busy_thread, 1 - w);
			for (i = 0; i < 200; i++) {
				if (libs[w].fault() != 1) {
					fail("nil dereference was not recovered as a runtime error", NULL);
				}
			}
			busy_stop = 1;
			join_thread(t);
		}
	} else if (strcmp(mode, "preempt") == 0) {
		// Library B was loaded last, so its signal handler runs first;
		// A's preemption requests must still reach A, or a collection
		// that has to stop A's spinning goroutine never finishes.
		for (w = 0; w < 2; w++) {
			libs[w].startspin();
			for (i = 0; i < 5; i++) {
				libs[w].gc();
			}
		}
#ifndef _WIN32
	} else if (strcmp(mode, "hostsig") == 0) {
		// A SIGSEGV handler the host installed before loading the
		// libraries still gets faults in C code, from a plain host
		// thread and from a thread that has called into both runtimes,
		// while Go faults still go to the Go runtimes.
		host_fault();
		for (i = 0; i < 20; i++) {
			libs[i % 2].work(8);
			if (libs[i % 2].fault() != 1) {
				fail("nil dereference was not recovered as a runtime error", NULL);
			}
			host_fault();
		}
		if (hostfaults != 21) {
			fprintf(stderr, "FAIL: host handler saw %d faults, want 21\n", (int)hostfaults);
			return 1;
		}
#endif
	} else {
		fail("unknown mode", mode);
	}
	printf("PASS\n");
	return 0;
}
