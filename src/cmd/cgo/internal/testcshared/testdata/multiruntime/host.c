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
#include <tlhelp32.h>
#else
#include <dirent.h>
#include <dlfcn.h>
#include <pthread.h>
#include <setjmp.h>
#include <signal.h>
#include <sys/resource.h>
#include <time.h>
#include <unistd.h>
#endif
#ifdef __APPLE__
#include <mach/mach.h>
#endif

typedef int (*work_fn)(int);
typedef void (*setpeer_fn)(void*);
typedef int (*bounce_fn)(int);
typedef int (*int_fn)(void);
typedef void (*void_fn)(void);
typedef int (*work2_fn)(int, int);

struct lib {
	const char* path;
	void* handle;
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
	int_fn generation;
	void_fn startbackground;
	void_fn blockinc;
	work_fn notifysignal;
	work_fn resetsignal;
	int_fn signalcount;
	int_fn writeclosedpipe;
	void_fn crash;
	void_fn crashfault;
	void_fn park;
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
	l->handle = h;
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
	LOOKUP(generation, "Generation");
	LOOKUP(startbackground, "StartBackground");
	LOOKUP(blockinc, "BlockInC");
	LOOKUP(notifysignal, "NotifySignal");
	LOOKUP(resetsignal, "ResetSignal");
	LOOKUP(signalcount, "SignalCount");
	LOOKUP(writeclosedpipe, "WriteClosedPipe");
	LOOKUP(crash, "Crash");
	LOOKUP(crashfault, "CrashFault");
	LOOKUP(park, "Park");
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

typedef HANDLE hthread_t;

static DWORD WINAPI thread_start(LPVOID arg) {
	((void (*)(long))((void**)arg)[0])((long)(size_t)((void**)arg)[1]);
	free(arg);
	return 0;
}

static void start_thread(hthread_t* t, void (*fn)(long), long arg) {
	void** a = malloc(2 * sizeof(void*));
	a[0] = (void*)fn;
	a[1] = (void*)(size_t)arg;
	*t = CreateThread(NULL, 0, thread_start, a, 0, NULL);
	if (*t == NULL) {
		fail("CreateThread", NULL);
	}
}

static void join_thread(hthread_t t) {
	WaitForSingleObject(t, INFINITE);
	CloseHandle(t);
}

static void unload(struct lib* l) {
	if (!FreeLibrary((HMODULE)l->handle)) {
		fail("FreeLibrary", l->path);
	}
	l->handle = NULL;
}

static int is_loaded(const char* path) {
	return GetModuleHandleA(path) != NULL;
}

static int thread_count(void) {
	HANDLE snap = CreateToolhelp32Snapshot(TH32CS_SNAPTHREAD, 0);
	THREADENTRY32 te;
	DWORD pid = GetCurrentProcessId();
	int n = 0;

	te.dwSize = sizeof te;
	if (snap != INVALID_HANDLE_VALUE && Thread32First(snap, &te)) {
		do {
			if (te.th32OwnerProcessID == pid) {
				n++;
			}
		} while (Thread32Next(snap, &te));
	}
	CloseHandle(snap);
	return n;
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
	l->handle = h;
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
	LOOKUP(generation, "Generation");
	LOOKUP(startbackground, "StartBackground");
	LOOKUP(blockinc, "BlockInC");
	LOOKUP(notifysignal, "NotifySignal");
	LOOKUP(resetsignal, "ResetSignal");
	LOOKUP(signalcount, "SignalCount");
	LOOKUP(writeclosedpipe, "WriteClosedPipe");
	LOOKUP(crash, "Crash");
	LOOKUP(crashfault, "CrashFault");
	LOOKUP(park, "Park");
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

typedef pthread_t hthread_t;

static void* thread_start(void* arg) {
	((void (*)(long))((void**)arg)[0])((long)(size_t)((void**)arg)[1]);
	free(arg);
	return NULL;
}

static void start_thread(hthread_t* t, void (*fn)(long), long arg) {
	void** a = malloc(2 * sizeof(void*));
	a[0] = (void*)fn;
	a[1] = (void*)(size_t)arg;
	if (pthread_create(t, NULL, thread_start, a) != 0) {
		fail("pthread_create", NULL);
	}
}

static void join_thread(hthread_t t) {
	pthread_join(t, NULL);
}

static void unload(struct lib* l) {
	if (dlclose(l->handle) != 0) {
		fail("dlclose", dlerror());
	}
	l->handle = NULL;
}

static int is_loaded(const char* path) {
	void* h = dlopen(path, RTLD_NOW | RTLD_NOLOAD);

	if (h == NULL) {
		return 0;
	}
	dlclose(h);
	return 1;
}

static int thread_count(void) {
#if defined(__APPLE__)
	thread_act_array_t threads;
	mach_msg_type_number_t n;

	if (task_threads(mach_task_self(), &threads, &n) != KERN_SUCCESS) {
		fail("task_threads", NULL);
	}
	vm_deallocate(mach_task_self(), (vm_address_t)threads, n * sizeof threads[0]);
	return (int)n;
#else
	DIR* d = opendir("/proc/self/task");
	struct dirent* e;
	int n = 0;

	if (d == NULL) {
		return -1;
	}
	while ((e = readdir(d)) != NULL) {
		if (e->d_name[0] != '.') {
			n++;
		}
	}
	closedir(d);
	return n;
#endif
}

#endif

#define NTHREADS 8

// mapping_count reports how many memory mappings the process has, or -1
// where that is not easy to find out.
static int mapping_count(void) {
#if defined(__linux__)
	FILE* f = fopen("/proc/self/maps", "r");
	int c, n = 0;

	if (f == NULL) {
		return -1;
	}
	while ((c = fgetc(f)) != EOF) {
		if (c == '\n') {
			n++;
		}
	}
	fclose(f);
	return n;
#else
	return -1;
#endif
}

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

// unload_thread uses both runtimes while one of them is about to be
// unloaded.
static void unload_thread(long id) {
	int i;

	for (i = 0; i < 50; i++) {
		libs[(i + id) % 2].work(i % 32 + 1);
	}
}

#define NRELOADS 100

// run_unload loads, uses and unloads library A NRELOADS times while library
// B stays loaded and busy, then checks that nothing piled up and that B
// still works. B is loaded first, so A's signal handler is the top of the
// chain whenever A is unloaded.
static void run_unload(void) {
	hthread_t busy, t[4];
	int i, j, threads0 = 0, maps0 = 0, n;

	load(&libs[1]);
	libs[1].work(1);
	busy_stop = 0;
	start_thread(&busy, busy_thread, 1);
	for (i = 0; i < NRELOADS; i++) {
		load(&libs[0]);
		if ((n = libs[0].generation()) != 1) {
			fprintf(stderr, "FAIL: reload %d: library kept its state across an unload (generation %d)\n", i, n);
			exit(1);
		}
		libs[0].startbackground();
		for (j = 0; j < 4; j++) {
			start_thread(&t[j], unload_thread, j);
		}
		for (j = 0; j < 4; j++) {
			join_thread(t[j]);
		}
		if (libs[0].fault() != 1) {
			fail("nil dereference was not recovered as a runtime error", NULL);
		}
		unload(&libs[0]);
		if (is_loaded(libs[0].path)) {
			fail("library is still loaded after unloading it", libs[0].path);
		}
		if (i == 0) {
			threads0 = thread_count();
			maps0 = mapping_count();
		}
	}
	busy_stop = 1;
	join_thread(busy);

	// Each load starts several threads and maps a heap. Without the
	// teardown they would pile up; allow for B growing a little.
	n = thread_count();
	if (n > threads0 + 16) {
		fprintf(stderr, "FAIL: %d threads after %d reloads, %d after the first\n", n, NRELOADS, threads0);
		exit(1);
	}
	n = mapping_count();
	if (maps0 > 0 && n > maps0 + 64) {
		fprintf(stderr, "FAIL: %d mappings after %d reloads, %d after the first\n", n, NRELOADS, maps0);
		exit(1);
	}

	// B still handles its own faults and preempts its own goroutines.
	if (libs[1].fault() != 1) {
		fail("nil dereference was not recovered as a runtime error", libs[1].path);
	}
	libs[1].startspin();
	for (i = 0; i < 5; i++) {
		libs[1].gc();
	}
}

static void run_threads(void (*fn)(long)) {
	hthread_t t[NTHREADS];
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

// run_unload_below loads A, then B, and unloads A while B's signal handler
// sits on top of A's and forwards to it, as a plugin host does when it
// unloads plugins in any order. B must keep recovering its own faults and
// preempting its own goroutines, and the host's SIGSEGV handler, installed
// before A, must still get faults in C code through the chain. Unloading B
// afterwards puts back the handler it found, A's stub, which still leads
// to the host's handler. The host does this a few times over.
static void run_unload_below(void) {
	int round, i;

	install_host_handler();
	for (round = 0; round < 3; round++) {
		hostfaults = 0;
		load(&libs[0]);
		libs[0].work(1);
		load(&libs[1]);
		libs[1].work(1);
		unload(&libs[0]);
		if (is_loaded(libs[0].path)) {
			fail("library is still loaded after unloading it", libs[0].path);
		}
		host_fault();
		for (i = 0; i < 20; i++) {
			libs[1].work(8);
			if (libs[1].fault() != 1) {
				fail("nil dereference was not recovered as a runtime error", libs[1].path);
			}
			host_fault();
		}
		libs[1].startspin();
		for (i = 0; i < 5; i++) {
			libs[1].gc();
		}
		unload(&libs[1]);
		host_fault();
		if (hostfaults != 22) {
			fprintf(stderr, "FAIL: round %d: host handler saw %d faults, want 22\n", round, (int)hostfaults);
			exit(1);
		}
	}
}

static void sleep_ms(int ms) {
	struct timespec ts = {ms / 1000, (ms % 1000) * 1000000L};

	nanosleep(&ts, NULL);
}

// notify: both runtimes call os/signal.Notify for SIGUSR1, and every
// runtime that asked gets every signal, however their handlers are chained
// and whichever of them calls Reset. A runtime that has called Reset no
// longer gets it, and neither does the host while either runtime is still
// listening. With withhost, the host installed its own SIGUSR1 handler
// first, which, as with a single runtime, loses the signal to Notify;
// otherwise the default action, which kills the process, sits below both
// runtimes. Once both runtimes have called Reset, the host's original
// handling is back.
static volatile sig_atomic_t hostusr1;
static int want[3];

static void host_usr1(int sig) {
	hostusr1++;
}

static int usr1_counts(int i) {
	return i < 2 ? libs[i].signalcount() : (int)hostusr1;
}

// usr1_round sends SIGUSR1 to the process and waits until exactly the
// listeners marked in got (A, B, host) have seen it.
static void usr1_round(int gotA, int gotB, int gotHost) {
	int i, ms;

	want[0] += gotA;
	want[1] += gotB;
	want[2] += gotHost;
	kill(getpid(), SIGUSR1);
	for (ms = 0; ms < 10000; ms++) {
		if (usr1_counts(0) >= want[0] && usr1_counts(1) >= want[1] && usr1_counts(2) >= want[2]) {
			break;
		}
		sleep_ms(1);
	}
	// Give a delivery that should not happen time to show up.
	sleep_ms(20);
	for (i = 0; i < 3; i++) {
		if (usr1_counts(i) != want[i]) {
			fprintf(stderr, "FAIL: SIGUSR1 seen by A, B, host = %d, %d, %d; want %d, %d, %d\n",
				usr1_counts(0), usr1_counts(1), usr1_counts(2), want[0], want[1], want[2]);
			exit(1);
		}
	}
}

static void usr1_rounds(int gotA, int gotB) {
	int i;

	for (i = 0; i < 5; i++) {
		usr1_round(gotA, gotB, 0);
	}
}

static void run_notify(int withhost) {
	struct sigaction sa;
	int i;

	for (i = 0; i < 2; i++) {
		libs[i].notifysignal(SIGUSR1);
	}
	// B's handler sits on top of A's and hands the signal down to it.
	usr1_rounds(1, 1);
	// A's Reset must not uninstall B's handler, and A must not pass what
	// B hands down on to the host or to the default action.
	libs[0].resetsignal(SIGUSR1);
	usr1_rounds(0, 1);
	// A calling Notify again must not chain A's handler to itself.
	libs[0].notifysignal(SIGUSR1);
	usr1_rounds(1, 1);
	// B's Reset hands the signal back to A alone.
	libs[1].resetsignal(SIGUSR1);
	usr1_rounds(1, 0);
	// Now A's handler is the one below.
	libs[1].notifysignal(SIGUSR1);
	usr1_rounds(1, 1);
	libs[1].resetsignal(SIGUSR1);
	usr1_rounds(1, 0);
	// A's Reset restores the host's original handling.
	libs[0].resetsignal(SIGUSR1);
	if (sigaction(SIGUSR1, NULL, &sa) != 0) {
		fail("sigaction", NULL);
	}
	if (sa.sa_handler != (withhost ? host_usr1 : SIG_DFL)) {
		fail("SIGUSR1 handler not restored after both runtimes reset it", NULL);
	}
	if (withhost) {
		usr1_round(0, 0, 1);
	}
}

static void install_host_usr1(void) {
	struct sigaction sa;

	memset(&sa, 0, sizeof sa);
	sa.sa_handler = host_usr1;
	sigemptyset(&sa.sa_mask);
	if (sigaction(SIGUSR1, &sa, NULL) != 0) {
		fail("sigaction", NULL);
	}
}

// sigpipe: a Go write to a closed pipe fails with EPIPE in whichever
// runtime makes it, though the SIGPIPE it raises reaches the handler of
// the library loaded last first.
static void sigpipe_thread(long id) {
	int i;

	for (i = 0; i < 100; i++) {
		if (libs[(i + id) % 2].writeclosedpipe() != 1) {
			fail("write to a closed pipe did not fail with EPIPE", libs[(i + id) % 2].path);
		}
	}
}

// rlimit_before lowers the soft open-file limit so that a runtime raising
// it would show; rlimit_after checks that loading the libraries left it
// alone.
static struct rlimit rlim_before;

static void rlimit_before(void) {
	if (getrlimit(RLIMIT_NOFILE, &rlim_before) != 0) {
		fail("getrlimit", NULL);
	}
	if (rlim_before.rlim_max != RLIM_INFINITY && rlim_before.rlim_max < 64) {
		fail("hard open-file limit too low to test", NULL);
	}
	rlim_before.rlim_cur = 64;
	if (setrlimit(RLIMIT_NOFILE, &rlim_before) != 0) {
		fail("setrlimit", NULL);
	}
}

static void rlimit_after(void) {
	struct rlimit lim;

	if (getrlimit(RLIMIT_NOFILE, &lim) != 0) {
		fail("getrlimit", NULL);
	}
	if (lim.rlim_cur != rlim_before.rlim_cur || lim.rlim_max != rlim_before.rlim_max) {
		fprintf(stderr, "FAIL: open-file limit changed from %llu/%llu to %llu/%llu\n",
			(unsigned long long)rlim_before.rlim_cur, (unsigned long long)rlim_before.rlim_max,
			(unsigned long long)lim.rlim_cur, (unsigned long long)lim.rlim_max);
		exit(1);
	}
}
#endif

#ifdef _WIN32
// ctrlbreak: both runtimes call os/signal.Notify for os.Interrupt, and
// every runtime that asked gets every Ctrl+Break console event, whichever
// handler Windows calls first. A console control handler the host installed
// first loses the event while either runtime is listening, and gets it back
// once both have called Reset. The test starts the host in a new process
// group, so the event reaches only this process.
#define GO_SIGINT 2

static volatile LONG hostbreaks;
static int wantbreak[3];

static BOOL WINAPI host_ctrl(DWORD type) {
	if (type != CTRL_BREAK_EVENT) {
		return FALSE;
	}
	InterlockedIncrement(&hostbreaks);
	return TRUE;
}

static int break_counts(int i) {
	return i < 2 ? libs[i].signalcount() : (int)hostbreaks;
}

static void break_round(int gotA, int gotB, int gotHost) {
	int i, ms;

	wantbreak[0] += gotA;
	wantbreak[1] += gotB;
	wantbreak[2] += gotHost;
	if (!GenerateConsoleCtrlEvent(CTRL_BREAK_EVENT, GetCurrentProcessId())) {
		printf("SKIP: GenerateConsoleCtrlEvent failed: %lu\n", GetLastError());
		exit(0);
	}
	for (ms = 0; ms < 10000; ms++) {
		if (break_counts(0) >= wantbreak[0] && break_counts(1) >= wantbreak[1] && break_counts(2) >= wantbreak[2]) {
			break;
		}
		Sleep(1);
	}
	// Give a delivery that should not happen time to show up.
	Sleep(50);
	for (i = 0; i < 3; i++) {
		if (break_counts(i) != wantbreak[i]) {
			fprintf(stderr, "FAIL: Ctrl+Break seen by A, B, host = %d, %d, %d; want %d, %d, %d\n",
				break_counts(0), break_counts(1), break_counts(2), wantbreak[0], wantbreak[1], wantbreak[2]);
			exit(1);
		}
	}
}

static void run_ctrlbreak(void) {
	int i;

	for (i = 0; i < 2; i++) {
		libs[i].notifysignal(GO_SIGINT);
	}
	break_round(1, 1, 0);
	libs[0].resetsignal(GO_SIGINT);
	break_round(0, 1, 0);
	libs[0].notifysignal(GO_SIGINT);
	break_round(1, 1, 0);
	libs[1].resetsignal(GO_SIGINT);
	break_round(1, 0, 0);
	libs[0].resetsignal(GO_SIGINT);
	break_round(0, 0, 1);
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
	if (strcmp(mode, "notifyhost") == 0) {
		install_host_usr1();
	}
	if (strcmp(mode, "rlimit") == 0) {
		rlimit_before();
	}
#else
	if (strcmp(mode, "ctrlbreak") == 0 && !SetConsoleCtrlHandler(host_ctrl, TRUE)) {
		fail("SetConsoleCtrlHandler", NULL);
	}
#endif

	if (strcmp(mode, "unload") == 0) {
		run_unload();
		printf("PASS\n");
		return 0;
	}
#ifndef _WIN32
	if (strcmp(mode, "unloadbelow") == 0) {
		run_unload_below();
		printf("PASS\n");
		return 0;
	}
#endif
	if (strcmp(mode, "unloadblocked") == 0) {
		// A goroutine that never returns from C makes unloading
		// impossible; the runtime must refuse with a fatal error rather
		// than let the library disappear under it.
		load(&libs[0]);
		libs[0].blockinc();
		unload(&libs[0]);
		fail("unloading with a goroutine blocked in C succeeded", NULL);
	}
	if (strcmp(mode, "exitblocked") == 0) {
		// Exiting is not unloading: the same goroutine must not stop
		// the process from exiting normally.
		load(&libs[0]);
		libs[0].startbackground();
		libs[0].blockinc();
		printf("PASS\n");
		fflush(stdout);
		exit(0);
	}

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
			hthread_t t;

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
	} else if (strcmp(mode, "crash") == 0 || strcmp(mode, "crashfault") == 0) {
		// An unrecovered panic or fault in A, while B is busy on another
		// thread, ends the process with a crash report from A alone. The
		// test checks the report.
		hthread_t t;

		libs[0].park();
		libs[1].park();
		busy_stop = 0;
		start_thread(&t, busy_thread, 1);
		libs[1].work(64);
		if (strcmp(mode, "crash") == 0) {
			libs[0].crash();
		} else {
			libs[0].crashfault();
		}
		fail("library did not crash", NULL);
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
	} else if (strcmp(mode, "notify") == 0 || strcmp(mode, "notifyhost") == 0) {
		run_notify(strcmp(mode, "notifyhost") == 0);
	} else if (strcmp(mode, "sigpipe") == 0) {
		run_threads(sigpipe_thread);
	} else if (strcmp(mode, "rlimit") == 0) {
		for (i = 0; i < 2; i++) {
			libs[i].work(8);
		}
		rlimit_after();
#else
	} else if (strcmp(mode, "ctrlbreak") == 0) {
		run_ctrlbreak();
#endif
	} else {
		fail("unknown mode", mode);
	}
	printf("PASS\n");
	return 0;
}
