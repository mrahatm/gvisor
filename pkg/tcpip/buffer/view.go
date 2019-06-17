// Copyright 2018 The gVisor Authors.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Package buffer provides the implementation of a buffer view.
package buffer

import (
	"log"
	"sync"
	"sync/atomic"
	"time"
)

// View is a slice of a buffer, with convenience methods.
type View []byte

type counter struct {
	count int64
}

func (c *counter) inc() {
	atomic.AddInt64(&c.count, 1)
}

func (c *counter) get() int64 {
	return atomic.LoadInt64(&c.count)
}

// overall memory allocation metrics, metrics must be accessed
// using atomic operations.
var viewPoolAllocationMetrics = struct {
	// Number of allocations made from one of the pools.
	allocations *counter

	// Number of deallocations where the allocation was returned to a pool.
	deallocations *counter

	// Number of allocations that were not served by a pool.
	largeAllocations *counter

	// Number of large deallocations that were not returned to a pool.
	largeDeallocations *counter

	// Number of small allocations that were not returned to a pool.
	smallDeallocations *counter
}{&counter{}, &counter{}, &counter{}, &counter{}, &counter{}}

type poolInfo struct {
	sz   int
	pool sync.Pool
	// pool metrics must be accessed atomically
	allocations   *counter
	deallocations *counter
}

var viewPools = []poolInfo{
	{64, sync.Pool{New: func() interface{} { return make(View, 64) }}, new(counter), new(counter)},
	{128, sync.Pool{New: func() interface{} { return make(View, 128) }}, new(counter), new(counter)},
	{256, sync.Pool{New: func() interface{} { return make(View, 256) }}, new(counter), new(counter)},
	{512, sync.Pool{New: func() interface{} { return make(View, 512) }}, new(counter), new(counter)},
	{1024, sync.Pool{New: func() interface{} { return make(View, 1024) }}, new(counter), new(counter)},
	{2048, sync.Pool{New: func() interface{} { return make(View, 2048) }}, new(counter), new(counter)},
	{4096, sync.Pool{New: func() interface{} { return make(View, 4096) }}, new(counter), new(counter)},
	{8192, sync.Pool{New: func() interface{} { return make(View, 8192) }}, new(counter), new(counter)},
	{16384, sync.Pool{New: func() interface{} { return make(View, 16384) }}, new(counter), new(counter)},
	{32768, sync.Pool{New: func() interface{} { return make(View, 32768) }}, new(counter), new(counter)},
	{65536, sync.Pool{New: func() interface{} { return make(View, 65536) }}, new(counter), new(counter)},
	{131072, sync.Pool{New: func() interface{} { return make(View, 131072) }}, new(counter), new(counter)},
	{262144, sync.Pool{New: func() interface{} { return make(View, 262144) }}, new(counter), new(counter)},
}

var printerStarted int64

func printPoolStats() {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()
	stats := &viewPoolAllocationMetrics
	for {
		<-t.C
		log.Printf("viewPoolAddress: %p, stats: %p", &viewPoolAllocationMetrics, stats)
		log.Printf("allocations: %d, deallocations: %d, largeAllocations: %d, largeDeallocations: %d, smallDeallocations: %d", stats.allocations.get(), stats.deallocations.get(), stats.largeAllocations.get(), stats.largeDeallocations.get(), stats.smallDeallocations.get())
		for i := 0; i < len(viewPools); i++ {
			log.Printf("Pool SZ: %d, allocations: %d, deallocations: %d", viewPools[i].sz, viewPools[i].allocations.get(), viewPools[i].deallocations.get())
		}
	}
}

// NewView allocates a new buffer and returns an initialized view that covers
// the whole buffer.
func NewView(size int) View {
	if atomic.CompareAndSwapInt64(&printerStarted, 0, 1) {
		go printPoolStats()
	}
	stats := &viewPoolAllocationMetrics
	if size > viewPools[len(viewPools)-1].sz {
		stats.largeAllocations.inc()
		return make(View, size)
	}
	i := 0
	for ; i < len(viewPools); i++ {
		if size <= viewPools[i].sz {
			break
		}
	}
	stats.allocations.inc()
	viewPools[i].allocations.inc()
	v := viewPools[i].pool.Get().(View)
	for i := range v {
		v[i] = 0
	}
	v = v[:size]
	return v
}

// PutView returns a view to the buffer pool.
func PutView(v View) {
	stats := &viewPoolAllocationMetrics
	if len(v) > viewPools[len(viewPools)-1].sz {
		// Let GC handle the large allocation.
		stats.largeAllocations.inc()
		return
	}
	// Return the buffer to the pool which is of a smaller size
	// than the view length but closest to it.
	for i := len(viewPools) - 1; i >= 0; i-- {
		if len(v) >= viewPools[i].sz {
			stats.deallocations.inc()
			viewPools[i].deallocations.inc()
			viewPools[i].pool.Put(v)
			return
		}
	}
	stats.smallDeallocations.inc()
	// If the allocation is now smaller than the smallest pool size
	// just let GC handle it.
}

// NewViewFromBytes allocates a new buffer and copies in the given bytes.
func NewViewFromBytes(b []byte) View {
	return append(View(nil), b...)
}

// TrimFront removes the first "count" bytes from the visible section of the
// buffer.
func (v *View) TrimFront(count int) {
	*v = (*v)[count:]
}

// CapLength irreversibly reduces the length of the visible section of the
// buffer to the value specified.
func (v *View) CapLength(length int) {
	// We also set the slice cap because if we don't, one would be able to
	// expand the view back to include the region just excluded. We want to
	// prevent that to avoid potential data leak if we have uninitialized
	// data in excluded region.
	*v = (*v)[:length:length]
}

// ToVectorisedView returns a VectorisedView containing the receiver.
func (v View) ToVectorisedView() VectorisedView {
	return NewVectorisedView(len(v), []View{v})
}

// VectorisedView is a vectorised version of View using non contigous memory.
// It supports all the convenience methods supported by View.
//
// +stateify savable
type VectorisedView struct {
	views []View
	size  int
}

// NewVectorisedView creates a new vectorised view from an already-allocated slice
// of View and sets its size.
func NewVectorisedView(size int, views []View) VectorisedView {
	return VectorisedView{views: views, size: size}
}

// TrimFront removes the first "count" bytes of the vectorised view.
func (vv *VectorisedView) TrimFront(count int) {
	for count > 0 && len(vv.views) > 0 {
		if count < len(vv.views[0]) {
			vv.size -= count
			vv.views[0].TrimFront(count)
			return
		}
		count -= len(vv.views[0])
		vv.RemoveFirst()
	}
}

// CapLength irreversibly reduces the length of the vectorised view.
func (vv *VectorisedView) CapLength(length int) {
	if length < 0 {
		length = 0
	}
	if vv.size < length {
		return
	}
	vv.size = length
	for i := range vv.views {
		v := &vv.views[i]
		if len(*v) >= length {
			if length == 0 {
				vv.views = vv.views[:i]
			} else {
				v.CapLength(length)
				vv.views = vv.views[:i+1]
			}
			return
		}
		length -= len(*v)
	}
}

// Clone returns a clone of this VectorisedView.
// If the buffer argument is large enough to contain all the Views of this VectorisedView,
// the method will avoid allocations and use the buffer to store the Views of the clone.
func (vv VectorisedView) Clone(buffer []View) VectorisedView {
	return VectorisedView{views: append(buffer[:0], vv.views...), size: vv.size}
}

// First returns the first view of the vectorised view.
func (vv VectorisedView) First() View {
	if len(vv.views) == 0 {
		return nil
	}
	return vv.views[0]
}

// RemoveFirst removes the first view of the vectorised view.
func (vv *VectorisedView) RemoveFirst() {
	if len(vv.views) == 0 {
		return
	}
	vv.size -= len(vv.views[0])
	vv.views = vv.views[1:]
}

// Size returns the size in bytes of the entire content stored in the vectorised view.
func (vv VectorisedView) Size() int {
	return vv.size
}

// ToView returns a single view containing the content of the vectorised view.
//
// If the vectorised view contains a single view, that view will be returned
// directly.
func (vv VectorisedView) ToView() View {
	// if len(vv.views) == 1 {
	// 	return vv.views[0]
	// }
	u := NewView(vv.size)[:0] // make([]byte, 0, vv.size)
	for _, v := range vv.views {
		u = append(u, v...)
	}
	return u
}

// Views returns the slice containing the all views.
func (vv VectorisedView) Views() []View {
	return vv.views
}

// Append appends the views in a vectorised view to this vectorised view.
func (vv *VectorisedView) Append(vv2 VectorisedView) {
	vv.views = append(vv.views, vv2.views...)
	vv.size += vv2.size
}

// Prepend  puts the view in front of the other views in the vectorised view.
func (vv *VectorisedView) Prepend(v View) {
	views := append([]View{}, v)
	vv.views = append(views, vv.views...)
	vv.size += len(v)
}
