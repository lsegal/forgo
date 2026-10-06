// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vst3

// The C side of the ABI: the interface tables, which point at the
// //export functions in exports.go, and trampolines for calling the
// interfaces of objects that another module owns. No header is included.
// The prototypes below are declared by hand, with int for int32 and
// long long for int64, which holds on every 64-bit target.

/*
extern void free(void*);

extern int forgoVst3QueryInterface(void*, void*, void**);
extern unsigned int forgoVst3AddRef(void*);
extern unsigned int forgoVst3Release(void*);

extern int forgoVst3GetFactoryInfo(void*, void*);
extern int forgoVst3CountClasses(void*);
extern int forgoVst3GetClassInfo(void*, int, void*);
extern int forgoVst3CreateInstance(void*, void*, void*, void**);
extern int forgoVst3GetClassInfo2(void*, int, void*);

extern int forgoVst3Initialize(void*, void*);
extern int forgoVst3Terminate(void*);
extern int forgoVst3GetControllerClassId(void*, void*);
extern int forgoVst3SetIoMode(void*, int);
extern int forgoVst3GetBusCount(void*, int, int);
extern int forgoVst3GetBusInfo(void*, int, int, int, void*);
extern int forgoVst3GetRoutingInfo(void*, void*, void*);
extern int forgoVst3ActivateBus(void*, int, int, int, unsigned char);
extern int forgoVst3SetActive(void*, unsigned char);
extern int forgoVst3SetState(void*, void*);
extern int forgoVst3GetState(void*, void*);

extern int forgoVst3SetBusArrangements(void*, void*, int, void*, int);
extern int forgoVst3GetBusArrangement(void*, int, int, void*);
extern int forgoVst3CanProcessSampleSize(void*, int);
extern unsigned int forgoVst3GetLatencySamples(void*);
extern int forgoVst3SetupProcessing(void*, void*);
extern int forgoVst3SetProcessing(void*, unsigned char);
extern int forgoVst3Process(void*, void*);
extern unsigned int forgoVst3GetTailSamples(void*);

extern int forgoVst3SetComponentState(void*, void*);
extern int forgoVst3ControllerSetState(void*, void*);
extern int forgoVst3ControllerGetState(void*, void*);
extern int forgoVst3GetParameterCount(void*);
extern int forgoVst3GetParameterInfo(void*, int, void*);
extern int forgoVst3GetParamStringByValue(void*, unsigned int, double, void*);
extern int forgoVst3GetParamValueByString(void*, unsigned int, void*, void*);
extern double forgoVst3NormalizedParamToPlain(void*, unsigned int, double);
extern double forgoVst3PlainParamToNormalized(void*, unsigned int, double);
extern double forgoVst3GetParamNormalized(void*, unsigned int);
extern int forgoVst3SetParamNormalized(void*, unsigned int, double);
extern int forgoVst3SetComponentHandler(void*, void*);
extern void* forgoVst3CreateView(void*, void*);

static void* call_p(void* f) { return ((void* (*)(void))f)(); }
static int call_i_p(void* f, void* a) { return ((int (*)(void*))f)(a); }
static void* call_p_pi(void* f, void* a, int i) { return ((void* (*)(void*, int))f)(a, i); }
static int call_i_pp(void* f, void* a, void* b) { return ((int (*)(void*, void*))f)(a, b); }
static int call_i_pip(void* f, void* a, int i, void* b) {
	return ((int (*)(void*, int, void*))f)(a, i, b);
}
static int call_i_ppp(void* f, void* a, void* b, void* c) {
	return ((int (*)(void*, void*, void*))f)(a, b, c);
}
static int call_i_pppp(void* f, void* a, void* b, void* c, void* d) {
	return ((int (*)(void*, void*, void*, void*))f)(a, b, c, d);
}
static int call_i_ppip(void* f, void* a, void* b, int i, void* c) {
	return ((int (*)(void*, void*, int, void*))f)(a, b, i, c);
}
static int call_i_pipp(void* f, void* a, int i, void* b, void* c) {
	return ((int (*)(void*, int, void*, void*))f)(a, i, b, c);
}
*/
import "C"

import "unsafe"

// Interface tables. Each is a C array of function pointers in the
// interface's method order.
var (
	factoryVtbl = vtable(
		C.forgoVst3QueryInterface, C.forgoVst3AddRef, C.forgoVst3Release,
		C.forgoVst3GetFactoryInfo, C.forgoVst3CountClasses, C.forgoVst3GetClassInfo,
		C.forgoVst3CreateInstance, C.forgoVst3GetClassInfo2,
	)
	componentVtbl = vtable(
		C.forgoVst3QueryInterface, C.forgoVst3AddRef, C.forgoVst3Release,
		C.forgoVst3Initialize, C.forgoVst3Terminate,
		C.forgoVst3GetControllerClassId, C.forgoVst3SetIoMode, C.forgoVst3GetBusCount,
		C.forgoVst3GetBusInfo, C.forgoVst3GetRoutingInfo, C.forgoVst3ActivateBus,
		C.forgoVst3SetActive, C.forgoVst3SetState, C.forgoVst3GetState,
	)
	processorVtbl = vtable(
		C.forgoVst3QueryInterface, C.forgoVst3AddRef, C.forgoVst3Release,
		C.forgoVst3SetBusArrangements, C.forgoVst3GetBusArrangement,
		C.forgoVst3CanProcessSampleSize, C.forgoVst3GetLatencySamples,
		C.forgoVst3SetupProcessing, C.forgoVst3SetProcessing, C.forgoVst3Process,
		C.forgoVst3GetTailSamples,
	)
	controllerVtbl = vtable(
		C.forgoVst3QueryInterface, C.forgoVst3AddRef, C.forgoVst3Release,
		C.forgoVst3Initialize, C.forgoVst3Terminate,
		C.forgoVst3SetComponentState, C.forgoVst3ControllerSetState,
		C.forgoVst3ControllerGetState, C.forgoVst3GetParameterCount,
		C.forgoVst3GetParameterInfo, C.forgoVst3GetParamStringByValue,
		C.forgoVst3GetParamValueByString, C.forgoVst3NormalizedParamToPlain,
		C.forgoVst3PlainParamToNormalized, C.forgoVst3GetParamNormalized,
		C.forgoVst3SetParamNormalized, C.forgoVst3SetComponentHandler,
		C.forgoVst3CreateView,
	)
)

// vtable copies function pointers into a C array, which lives as long
// as the module.
func vtable(fns ...unsafe.Pointer) unsafe.Pointer {
	size := unsafe.Sizeof(uintptr(0))
	p := C.malloc(C.size_t(len(fns)) * C.size_t(size))
	for i, fn := range fns {
		*(*unsafe.Pointer)(unsafe.Add(p, uintptr(i)*size)) = fn
	}
	return p
}

// A view is what the host holds for one interface of an object: the
// interface table and the object's handle. It is allocated with malloc,
// so it holds no Go pointer and the host may keep it.
type view struct {
	vtbl   unsafe.Pointer
	handle uint64
}

func newView(vtbl unsafe.Pointer, h uint64) unsafe.Pointer {
	p := C.malloc(C.size_t(unsafe.Sizeof(view{})))
	*(*view)(p) = view{vtbl, h}
	return p
}

func freeView(p unsafe.Pointer) { C.free(p) }

// cnew allocates a zeroed T with malloc, for passing to another module
// without handing it a Go pointer. Free it with cfree.
func cnew[T any]() *T {
	var zero T
	p := (*T)(C.malloc(C.size_t(unsafe.Sizeof(zero))))
	*p = zero
	return p
}

func cfree[T any](p *T) { C.free(unsafe.Pointer(p)) }

func handleOf(self unsafe.Pointer) uint64 { return (*view)(self).handle }

// method returns the function at index i of a foreign object's table.
func method(obj unsafe.Pointer, i int) unsafe.Pointer {
	vtbl := *(*unsafe.Pointer)(obj)
	return *(*unsafe.Pointer)(unsafe.Add(vtbl, uintptr(i)*unsafe.Sizeof(uintptr(0))))
}

// Method indexes shared by every interface.
const (
	mQueryInterface = 0
	mAddRef         = 1
	mRelease        = 2
)

func callP(f unsafe.Pointer) unsafe.Pointer { return C.call_p(f) }

func callIP(obj unsafe.Pointer, m int) int32 {
	return int32(C.call_i_p(method(obj, m), obj))
}

func callPPI(obj unsafe.Pointer, m int, i int32) unsafe.Pointer {
	return C.call_p_pi(method(obj, m), obj, C.int(i))
}

func callIPP(obj unsafe.Pointer, m int, a unsafe.Pointer) Result {
	return Result(C.call_i_pp(method(obj, m), obj, a))
}

func callIPIP(obj unsafe.Pointer, m int, i int32, a unsafe.Pointer) Result {
	return Result(C.call_i_pip(method(obj, m), obj, C.int(i), a))
}

func callIPPP(obj unsafe.Pointer, m int, a, b unsafe.Pointer) Result {
	return Result(C.call_i_ppp(method(obj, m), obj, a, b))
}

func callIPPPP(obj unsafe.Pointer, m int, a, b, c unsafe.Pointer) Result {
	return Result(C.call_i_pppp(method(obj, m), obj, a, b, c))
}

func callIPPIP(obj unsafe.Pointer, m int, a unsafe.Pointer, i int32, b unsafe.Pointer) Result {
	return Result(C.call_i_ppip(method(obj, m), obj, a, C.int(i), b))
}

func callIPIPP(obj unsafe.Pointer, m int, i int32, a, b unsafe.Pointer) Result {
	return Result(C.call_i_pipp(method(obj, m), obj, C.int(i), a, b))
}

// release drops one reference to a foreign object.
func release(obj unsafe.Pointer) { callIP(obj, mRelease) }
