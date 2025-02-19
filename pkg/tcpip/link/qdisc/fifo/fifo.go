// Copyright 2020 The gVisor Authors.
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

// Package fifo provides the implementation of FIFO queuing discipline that
// queues all outbound packets and asynchronously dispatches them to the
// lower link endpoint in the order that they were queued.
package fifo

import (
	"sync/atomic"

	"gvisor.dev/gvisor/pkg/sleep"
	"gvisor.dev/gvisor/pkg/sync"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

var _ stack.QueueingDiscipline = (*discipline)(nil)

const (
	// BatchSize represents the number of packets written to the
	// lower link endpoint during calls to WritePackets.
	BatchSize   = 32
	qDiscClosed = 1
)

// discipline represents a QueueingDiscipline which implements a FIFO queue for
// all outgoing packets. discipline can have 1 or more underlying
// queueDispatchers. All outgoing packets are consistenly hashed to a single
// underlying queue using the PacketBuffer.Hash if set, otherwise all packets
// are queued to the first queue to avoid reordering in case of missing hash.
type discipline struct {
	wg          sync.WaitGroup
	dispatchers []queueDispatcher

	// +checkatomic
	closed int32
}

// queueDispatcher is responsible for dispatching all outbound packets in its
// queue. It will also smartly batch packets when possible and write them
// through the lower LinkWriter.
type queueDispatcher struct {
	lower stack.LinkWriter
	limit int

	mu sync.Mutex
	// +checklocks:mu
	queue stack.PacketBufferList
	// +checklocks:mu
	used int

	newPacketWaker sleep.Waker
	closeWaker     sleep.Waker
}

// New creates a new fifo queuing discipline  with the n queues with maximum
// capacity of queueLen.
func New(lower stack.LinkWriter, n int, queueLen int) stack.QueueingDiscipline {
	d := &discipline{
		dispatchers: make([]queueDispatcher, n),
	}
	// Create the required dispatchers
	for i := range d.dispatchers {
		qd := &d.dispatchers[i]
		qd.lower = lower
		qd.limit = queueLen

		d.wg.Add(1)
		go func() {
			defer d.wg.Done()
			qd.dispatchLoop()
		}()
	}
	return d
}

func (qd *queueDispatcher) dispatchLoop() {
	s := sleep.Sleeper{}
	s.AddWaker(&qd.newPacketWaker)
	s.AddWaker(&qd.closeWaker)
	defer s.Done()

	var batch stack.PacketBufferList
	for {
		switch w := s.Fetch(true); w {
		case &qd.newPacketWaker:
		case &qd.closeWaker:
			qd.mu.Lock()
			for p := qd.queue.Front(); p != nil; p = qd.queue.Front() {
				qd.queue.Remove(p)
				p.DecRef()
				qd.used--
			}
			qd.queue.DecRef()
			qd.mu.Unlock()
			return
		default:
			panic("unknown waker")
		}
		qd.mu.Lock()
		for pkt := qd.queue.Front(); pkt != nil; pkt = qd.queue.Front() {
			qd.queue.Remove(pkt)
			qd.used--
			batch.PushBack(pkt)
			if batch.Len() < BatchSize && qd.used != 0 {
				continue
			}
			qd.mu.Unlock()
			_, _ = qd.lower.WritePackets(batch)
			batch.DecRef()
			batch.Reset()
			qd.mu.Lock()
		}
		qd.mu.Unlock()
	}
}

// WritePacket implements stack.QueueingDiscipline.WritePacket.
//
// The packet must have the following fields populated:
//  - pkt.EgressRoute
//  - pkt.GSOOptions
//  - pkt.NetworkProtocolNumber
func (d *discipline) WritePacket(pkt *stack.PacketBuffer) tcpip.Error {
	if atomic.LoadInt32(&d.closed) == qDiscClosed {
		return &tcpip.ErrClosedForSend{}
	}
	qd := &d.dispatchers[int(pkt.Hash)%len(d.dispatchers)]
	qd.mu.Lock()
	haveSpace := qd.used < qd.limit
	if haveSpace {
		pkt.IncRef()
		qd.queue.PushBack(pkt)
		qd.used++
	}
	qd.mu.Unlock()
	if !haveSpace {
		return &tcpip.ErrNoBufferSpace{}
	}
	qd.newPacketWaker.Assert()
	return nil
}

func (d *discipline) Close() {
	atomic.StoreInt32(&d.closed, qDiscClosed)
	for i := range d.dispatchers {
		d.dispatchers[i].closeWaker.Assert()
	}
	d.wg.Wait()
}
