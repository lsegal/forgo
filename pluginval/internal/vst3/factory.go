// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vst3

import (
	"errors"
	"sync"
	"unsafe"
)

// A Factory is what a module's GetPluginFactory hands the host: the
// module's classes and a way to create them.
type Factory interface {
	Info() FactoryInfo
	Classes() []ClassInfo2
	// CreateInstance creates class cid and returns its interface iid
	// holding one reference.
	CreateInstance(cid, iid *TUID) (unsafe.Pointer, Result)
}

var (
	factoryMu  sync.Mutex
	factory    Factory
	factoryObj *object
)

// SetFactory sets the factory that the module's GetPluginFactory
// returns. Call it from an init function.
func SetFactory(f Factory) {
	factoryMu.Lock()
	defer factoryMu.Unlock()
	factory = f
}

type factoryImpl struct{ f Factory }

func (factoryImpl) destroy() {}

// pluginFactory returns the module's IPluginFactory with a new
// reference. The module keeps one reference of its own, so the object
// lives as long as the module.
func pluginFactory() unsafe.Pointer {
	factoryMu.Lock()
	defer factoryMu.Unlock()
	if factory == nil {
		return nil
	}
	if factoryObj == nil {
		factoryObj = newObject(factoryImpl{factory},
			iface{factoryVtbl, []*TUID{&iidIPluginFactory, &iidIPluginFactory2}})
	}
	factoryObj.addRef()
	return factoryObj.views[0]
}

func factoryOf(self unsafe.Pointer) Factory {
	if f, ok := implOf(self).(factoryImpl); ok {
		return f.f
	}
	return nil
}

// NewFactory returns a factory for classes implemented in this module.
func NewFactory(vendor, url string, classes ...*Class) Factory {
	return &goFactory{vendor: vendor, url: url, classes: classes}
}

type goFactory struct {
	vendor, url string
	classes     []*Class
}

func (f *goFactory) Info() FactoryInfo {
	var info FactoryInfo
	setChars(info.Vendor[:], f.vendor)
	setChars(info.URL[:], f.url)
	info.Flags = factoryUnicode
	return info
}

func (f *goFactory) Classes() []ClassInfo2 {
	infos := make([]ClassInfo2, len(f.classes))
	for i, c := range f.classes {
		info := &infos[i]
		info.CID = c.CID
		info.Cardinality = manyInstances
		setChars(info.Category[:], audioEffectClass)
		setChars(info.Name[:], c.Name)
		setChars(info.SubCategories[:], "Fx")
		setChars(info.Vendor[:], f.vendor)
		setChars(info.Version[:], "1.0.0")
		setChars(info.SDKVersion[:], "VST 3.7.0")
	}
	return infos
}

func (f *goFactory) CreateInstance(cid, iid *TUID) (unsafe.Pointer, Result) {
	for _, c := range f.classes {
		if c.CID == *cid {
			return newInstance(c, iid)
		}
	}
	return nil, NoInterface
}

// Method indexes of IPluginFactory and IPluginFactory2.
const (
	mGetFactoryInfo = 3
	mCountClasses   = 4
	mGetClassInfo   = 5
	mCreateInstance = 6
	mGetClassInfo2  = 7
)

// A ModuleFactory is the IPluginFactory of another VST3 module, such as
// a Go plugin library that this module loaded.
type ModuleFactory struct {
	p unsafe.Pointer
	// p2 is the same factory's IPluginFactory2, or nil.
	p2 unsafe.Pointer
}

// OpenFactory calls a module's GetPluginFactory, given its address.
func OpenFactory(getPluginFactory unsafe.Pointer) (*ModuleFactory, error) {
	p := callP(getPluginFactory)
	throw errors.New("GetPluginFactory returned nil") if p == nil
	m := &ModuleFactory{p: p}
	iid, p2 := cnew[TUID](), cnew[unsafe.Pointer]()
	defer cfree(iid)
	defer cfree(p2)
	*iid = iidIPluginFactory2
	if callIPPP(p, mQueryInterface, unsafe.Pointer(iid), unsafe.Pointer(p2)) == ResultOK {
		m.p2 = *p2
	}
	return m, nil
}

// Calls into another module pass it only C memory, so that no Go
// pointer crosses into it.

// Classes lists the module's classes.
func (m *ModuleFactory) Classes() []ClassInfo2 {
	n := callIP(m.p, mCountClasses)
	infos := make([]ClassInfo2, 0, n)
	info := cnew[ClassInfo2]()
	defer cfree(info)
	c := cnew[ClassInfo]()
	defer cfree(c)
	for i := range n {
		if m.p2 != nil {
			*info = ClassInfo2{}
			if callIPIP(m.p2, mGetClassInfo2, i, unsafe.Pointer(info)) != ResultOK {
				continue
			}
			infos = append(infos, *info)
			continue
		}
		*c = ClassInfo{}
		if callIPIP(m.p, mGetClassInfo, i, unsafe.Pointer(c)) != ResultOK {
			continue
		}
		infos = append(infos, ClassInfo2{CID: c.CID, Cardinality: c.Cardinality, Category: c.Category, Name: c.Name})
	}
	return infos
}

// CreateInstance creates one of the module's classes.
func (m *ModuleFactory) CreateInstance(cid, iid *TUID) (unsafe.Pointer, Result) {
	args := cnew[struct {
		cid, iid TUID
		obj      unsafe.Pointer
	}]()
	defer cfree(args)
	args.cid, args.iid = *cid, *iid
	r := callIPPPP(m.p, mCreateInstance, unsafe.Pointer(&args.cid), unsafe.Pointer(&args.iid), unsafe.Pointer(&args.obj))
	return args.obj, r
}
