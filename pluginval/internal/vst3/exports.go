// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vst3

// The host calls these through the interface tables in cabi.go. Each
// receives the view it was called through as self.

import "C"

import (
	"math"
	"unsafe"
)

// Module entry points. A host calls the one for its platform, if any,
// before GetPluginFactory. There is nothing to set up.

//export GetPluginFactory
func GetPluginFactory() unsafe.Pointer { return pluginFactory() }

//export bundleEntry
func bundleEntry(unsafe.Pointer) uint8 { return 1 }

//export bundleExit
func bundleExit() uint8 { return 1 }

//export ModuleEntry
func ModuleEntry(unsafe.Pointer) uint8 { return 1 }

//export ModuleExit
func ModuleExit() uint8 { return 1 }

//export InitDll
func InitDll() uint8 { return 1 }

//export ExitDll
func ExitDll() uint8 { return 1 }

// FUnknown

//export forgoVst3QueryInterface
func forgoVst3QueryInterface(self, iid, out unsafe.Pointer) int32 {
	o := lookup(self)
	if o == nil {
		if out != nil {
			*(*unsafe.Pointer)(out) = nil
		}
		return int32(NoInterface)
	}
	return int32(o.queryInterface(iid, (*unsafe.Pointer)(out)))
}

//export forgoVst3AddRef
func forgoVst3AddRef(self unsafe.Pointer) uint32 {
	if o := lookup(self); o != nil {
		return o.addRef()
	}
	return 0
}

//export forgoVst3Release
func forgoVst3Release(self unsafe.Pointer) uint32 {
	if o := lookup(self); o != nil {
		return o.release()
	}
	return 0
}

// IPluginFactory, IPluginFactory2

//export forgoVst3GetFactoryInfo
func forgoVst3GetFactoryInfo(self, info unsafe.Pointer) int32 {
	f := factoryOf(self)
	if f == nil || info == nil {
		return int32(InvalidArgument)
	}
	*(*FactoryInfo)(info) = f.Info()
	return int32(ResultOK)
}

//export forgoVst3CountClasses
func forgoVst3CountClasses(self unsafe.Pointer) int32 {
	if f := factoryOf(self); f != nil {
		return int32(len(f.Classes()))
	}
	return 0
}

func classInfo2(self unsafe.Pointer, index int32) (ClassInfo2, bool) {
	f := factoryOf(self)
	if f == nil {
		return ClassInfo2{}, false
	}
	classes := f.Classes()
	if index < 0 || int(index) >= len(classes) {
		return ClassInfo2{}, false
	}
	return classes[index], true
}

//export forgoVst3GetClassInfo
func forgoVst3GetClassInfo(self unsafe.Pointer, index int32, info unsafe.Pointer) int32 {
	c, ok := classInfo2(self, index)
	if !ok || info == nil {
		return int32(InvalidArgument)
	}
	*(*ClassInfo)(info) = c.classInfo()
	return int32(ResultOK)
}

//export forgoVst3GetClassInfo2
func forgoVst3GetClassInfo2(self unsafe.Pointer, index int32, info unsafe.Pointer) int32 {
	c, ok := classInfo2(self, index)
	if !ok || info == nil {
		return int32(InvalidArgument)
	}
	*(*ClassInfo2)(info) = c
	return int32(ResultOK)
}

//export forgoVst3CreateInstance
func forgoVst3CreateInstance(self, cid, iid, out unsafe.Pointer) int32 {
	if out == nil {
		return int32(InvalidArgument)
	}
	*(*unsafe.Pointer)(out) = nil
	f := factoryOf(self)
	if f == nil || cid == nil || iid == nil {
		return int32(InvalidArgument)
	}
	obj, r := f.CreateInstance((*TUID)(cid), (*TUID)(iid))
	*(*unsafe.Pointer)(out) = obj
	return int32(r)
}

// IPluginBase, IComponent

//export forgoVst3Initialize
func forgoVst3Initialize(self, context unsafe.Pointer) int32 {
	if in := instanceOf(self); in != nil {
		return int32(in.initialize())
	}
	return int32(InvalidArgument)
}

//export forgoVst3Terminate
func forgoVst3Terminate(self unsafe.Pointer) int32 {
	if in := instanceOf(self); in != nil {
		return int32(in.terminate())
	}
	return int32(InvalidArgument)
}

// forgoVst3GetControllerClassId reports that there is no separate
// controller class: the host queries the component for IEditController.
//
//export forgoVst3GetControllerClassId
func forgoVst3GetControllerClassId(self, cid unsafe.Pointer) int32 {
	return int32(NotImplemented)
}

//export forgoVst3SetIoMode
func forgoVst3SetIoMode(self unsafe.Pointer, mode int32) int32 { return int32(NotImplemented) }

//export forgoVst3GetBusCount
func forgoVst3GetBusCount(self unsafe.Pointer, media, dir int32) int32 {
	if in := instanceOf(self); in != nil {
		return in.busCount(media, dir)
	}
	return 0
}

//export forgoVst3GetBusInfo
func forgoVst3GetBusInfo(self unsafe.Pointer, media, dir, index int32, info unsafe.Pointer) int32 {
	if in := instanceOf(self); in != nil {
		return int32(in.busInfo(media, dir, index, (*busInfo)(info)))
	}
	return int32(InvalidArgument)
}

//export forgoVst3GetRoutingInfo
func forgoVst3GetRoutingInfo(self, inInfo, outInfo unsafe.Pointer) int32 {
	return int32(NotImplemented)
}

//export forgoVst3ActivateBus
func forgoVst3ActivateBus(self unsafe.Pointer, media, dir, index int32, state uint8) int32 {
	if in := instanceOf(self); in != nil {
		return int32(in.activateBus(media, dir, index))
	}
	return int32(InvalidArgument)
}

//export forgoVst3SetActive
func forgoVst3SetActive(self unsafe.Pointer, state uint8) int32 { return int32(ResultOK) }

//export forgoVst3SetState
func forgoVst3SetState(self, stream unsafe.Pointer) int32 {
	if in := instanceOf(self); in != nil {
		return int32(in.setState(stream))
	}
	return int32(InvalidArgument)
}

//export forgoVst3GetState
func forgoVst3GetState(self, stream unsafe.Pointer) int32 {
	if in := instanceOf(self); in != nil {
		return int32(in.getState(stream))
	}
	return int32(InvalidArgument)
}

// IAudioProcessor

//export forgoVst3SetBusArrangements
func forgoVst3SetBusArrangements(self, ins unsafe.Pointer, nins int32, outs unsafe.Pointer, nouts int32) int32 {
	in := instanceOf(self)
	if in == nil || nins < 0 || nouts < 0 || (nins > 0 && ins == nil) || (nouts > 0 && outs == nil) {
		return int32(InvalidArgument)
	}
	return int32(in.setBusArrangements(unsafe.Slice((*uint64)(ins), nins), unsafe.Slice((*uint64)(outs), nouts)))
}

//export forgoVst3GetBusArrangement
func forgoVst3GetBusArrangement(self unsafe.Pointer, dir, index int32, arr unsafe.Pointer) int32 {
	if in := instanceOf(self); in != nil {
		return int32(in.busArrangement(dir, index, (*uint64)(arr)))
	}
	return int32(InvalidArgument)
}

//export forgoVst3CanProcessSampleSize
func forgoVst3CanProcessSampleSize(self unsafe.Pointer, size int32) int32 {
	if size == sample32 {
		return int32(ResultTrue)
	}
	return int32(ResultFalse)
}

//export forgoVst3GetLatencySamples
func forgoVst3GetLatencySamples(self unsafe.Pointer) uint32 { return 0 }

//export forgoVst3SetupProcessing
func forgoVst3SetupProcessing(self, setup unsafe.Pointer) int32 {
	if setup == nil {
		return int32(InvalidArgument)
	}
	if (*processSetup)(setup).SymbolicSampleSize != sample32 {
		return int32(ResultFalse)
	}
	return int32(ResultOK)
}

//export forgoVst3SetProcessing
func forgoVst3SetProcessing(self unsafe.Pointer, state uint8) int32 { return int32(ResultOK) }

//export forgoVst3Process
func forgoVst3Process(self, data unsafe.Pointer) int32 {
	if in := instanceOf(self); in != nil {
		return int32(in.process((*processData)(data)))
	}
	return int32(InvalidArgument)
}

//export forgoVst3GetTailSamples
func forgoVst3GetTailSamples(self unsafe.Pointer) uint32 { return 0 }

// IEditController

//export forgoVst3SetComponentState
func forgoVst3SetComponentState(self, stream unsafe.Pointer) int32 {
	if in := instanceOf(self); in != nil {
		return int32(in.setState(stream))
	}
	return int32(InvalidArgument)
}

//export forgoVst3ControllerSetState
func forgoVst3ControllerSetState(self, stream unsafe.Pointer) int32 { return int32(ResultOK) }

//export forgoVst3ControllerGetState
func forgoVst3ControllerGetState(self, stream unsafe.Pointer) int32 { return int32(ResultOK) }

//export forgoVst3GetParameterCount
func forgoVst3GetParameterCount(self unsafe.Pointer) int32 { return 1 }

//export forgoVst3GetParameterInfo
func forgoVst3GetParameterInfo(self unsafe.Pointer, index int32, info unsafe.Pointer) int32 {
	if in := instanceOf(self); in != nil {
		return int32(in.parameterInfo(index, (*parameterInfo)(info)))
	}
	return int32(InvalidArgument)
}

//export forgoVst3GetParamStringByValue
func forgoVst3GetParamStringByValue(self unsafe.Pointer, id uint32, value float64, str unsafe.Pointer) int32 {
	if id != paramID || str == nil {
		return int32(InvalidArgument)
	}
	setString128((*String128)(str), formatParam(value))
	return int32(ResultOK)
}

//export forgoVst3GetParamValueByString
func forgoVst3GetParamValueByString(self unsafe.Pointer, id uint32, str, value unsafe.Pointer) int32 {
	if id != paramID || str == nil || value == nil {
		return int32(InvalidArgument)
	}
	v, ok := parseParam((*String128)(str).String())
	if !ok {
		return int32(ResultFalse)
	}
	*(*float64)(value) = v
	return int32(ResultOK)
}

// The parameter's plain value is its normalized value.

//export forgoVst3NormalizedParamToPlain
func forgoVst3NormalizedParamToPlain(self unsafe.Pointer, id uint32, value float64) float64 {
	return value
}

//export forgoVst3PlainParamToNormalized
func forgoVst3PlainParamToNormalized(self unsafe.Pointer, id uint32, value float64) float64 {
	return value
}

//export forgoVst3GetParamNormalized
func forgoVst3GetParamNormalized(self unsafe.Pointer, id uint32) float64 {
	if in := instanceOf(self); in != nil && id == paramID {
		return in.paramValue()
	}
	return 0
}

//export forgoVst3SetParamNormalized
func forgoVst3SetParamNormalized(self unsafe.Pointer, id uint32, value float64) int32 {
	in := instanceOf(self)
	if in == nil || id != paramID || math.IsNaN(value) {
		return int32(InvalidArgument)
	}
	in.setParam(value)
	return int32(ResultOK)
}

// forgoVst3SetComponentHandler accepts the host's handler without
// keeping it: the plugin never edits its parameter itself.
//
//export forgoVst3SetComponentHandler
func forgoVst3SetComponentHandler(self, handler unsafe.Pointer) int32 { return int32(ResultOK) }

// forgoVst3CreateView reports that there is no editor.
//
//export forgoVst3CreateView
func forgoVst3CreateView(self, name unsafe.Pointer) unsafe.Pointer { return nil }
