// Copyright 2026 The forgo Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package vst3

import "unsafe"

// Calls into objects that the host owns.

// Method indexes of IBStream, IParameterChanges, and IParamValueQueue.
const (
	mStreamRead  = 3
	mStreamWrite = 4

	mChangesCount = 3
	mChangesData  = 4

	mQueueParamID    = 3
	mQueuePointCount = 4
	mQueuePoint      = 5
)

// readStream fills b from an IBStream and returns how much it read.
func readStream(stream unsafe.Pointer, b []byte) int {
	if stream == nil {
		return 0
	}
	n := 0
	for n < len(b) {
		var got int32
		r := callIPPIP(stream, mStreamRead, unsafe.Pointer(&b[n]), int32(len(b)-n), unsafe.Pointer(&got))
		if r != ResultOK || got <= 0 {
			break
		}
		n += int(got)
	}
	return n
}

// writeStream writes b to an IBStream and returns how much it wrote.
func writeStream(stream unsafe.Pointer, b []byte) int {
	if stream == nil {
		return 0
	}
	n := 0
	for n < len(b) {
		var put int32
		r := callIPPIP(stream, mStreamWrite, unsafe.Pointer(&b[n]), int32(len(b)-n), unsafe.Pointer(&put))
		if r != ResultOK || put <= 0 {
			break
		}
		n += int(put)
	}
	return n
}

// lastParamValue returns the last value that an IParameterChanges holds
// for parameter id.
func lastParamValue(changes unsafe.Pointer, id uint32) (v float64, ok bool) {
	for i := range callIP(changes, mChangesCount) {
		queue := callPPI(changes, mChangesData, i)
		if queue == nil || uint32(callIP(queue, mQueueParamID)) != id {
			continue
		}
		n := callIP(queue, mQueuePointCount)
		if n <= 0 {
			continue
		}
		var offset int32
		var value float64
		if callIPIPP(queue, mQueuePoint, n-1, unsafe.Pointer(&offset), unsafe.Pointer(&value)) == ResultOK {
			v, ok = value, true
		}
	}
	return
}
