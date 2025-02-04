//
//  Copyright 2023 PayPal Inc.
//
//  Licensed to the Apache Software Foundation (ASF) under one or more
//  contributor license agreements.  See the NOTICE file distributed with
//  this work for additional information regarding copyright ownership.
//  The ASF licenses this file to You under the Apache License, Version 2.0
//  (the "License"); you may not use this file except in compliance with
//  the License.  You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
//  Unless required by applicable law or agreed to in writing, software
//  distributed under the License is distributed on an "AS IS" BASIS,
//  WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
//  See the License for the specific language governing permissions and
//  limitations under the License.
//

package main

import (
	"encoding/binary"
	"fmt"
	"math"
	"math/rand"
	"sync"
	"time"

	"juno/pkg/client"

	"juno/third_party/forked/golang/glog"

	uuid "github.com/satori/go.uuid"
)

const (
	kRequestTypeCreate RequestType = iota
	kRequestTypeGet
	kRequestTypeUpdate
	kRequestTypeSet
	kRequestTypeDestroy
	kNumRequestTypes
)

const MaxDeletes = 10000

type (
	RequestType uint8
	Record      struct {
		key []byte
		ctx client.IContext
	}

	RecordStore struct {
		records []Record
		// Used for preloaded keys
		numKeys    int
		currGet    int
		nextDelete int
		offsetDel  int
		offsetGet  int
		LastDelete bool
	}

	TestEngine struct {
		rdgen       *RandomGen
		recStore    RecordStore
		reqSequence RequestSequence
		//chDone      chan bool
		invokeFuncs     []InvokeFunc
		client          client.IClient
		stats           *Statistics
		movingStats     *Statistics
		numReqPerSecond int
	}
	InvokeFunc func() error
)

func newTestKey() []byte {
	uuid := uuid.NewV1()
	key := uuid.Bytes()
	key[11] |= 0x2 // to distinguish with NewRandomKey
	return key
}

func NewRandomKey(s int) []byte {
	key := make([]byte, 16)
	r := uint32(((int64(s+1)*25214903917 + 11) >> 5) & 0x7fffffff)
	binary.BigEndian.PutUint32(key[0:], r)
	binary.BigEndian.PutUint32(key[4:], uint32(s))
	// key[11] == 0
	binary.BigEndian.PutUint32(key[12:], 0xff)
	return key
}

func expRand(n int) int {
	len := float64(n + 1)
	m := float64(rand.Intn(n) + 1)
	x := n - 1 - int(float64(n)*math.Log(m)/math.Log(len))
	return x
}

func (t RequestType) String() (str string) {
	switch t {
	case kRequestTypeCreate:
		str = "Create"
	case kRequestTypeGet:
		str = "Get"
	case kRequestTypeUpdate:
		str = "Update"
	case kRequestTypeSet:
		str = "Set"
	case kRequestTypeDestroy:
		str = "Destroy"
	default:
		str = "Unsupported"
	}
	return
}

func (r *Record) isExpired() bool {
	if r.ctx == nil {
		return false
	}
	return int64(r.ctx.GetCreationTime()+r.ctx.GetTimeToLive()) < time.Now().Unix()
	//??time_t now = time(NULL) + 5;
}

func (s *RecordStore) Add(rec Record) {
	if s.numKeys > 0 {
		return
	}
	s.records = append(s.records, rec)
}

func (s *RecordStore) display() {
	glog.Infof("numKeys=%d currGet=%d nextDelete=%d offsetDel=%d offsetGet=%d",
		s.numKeys, s.currGet, s.nextDelete, s.offsetDel, s.offsetGet)
}

func (s *RecordStore) takeRecord() (rec Record, err error) {
	if s.numKeys > 0 { // preloaded keys
		rec = Record{
			key: NewRandomKey(s.offsetDel + s.nextDelete),
		}
		if s.endOfDelete() {
			err = fmt.Errorf("no more record for destroy")
			return
		}
		s.nextDelete++
		return
	}

	sz := len(s.records)
	if sz == 0 {
		err = fmt.Errorf("no record in the store")
		return
	}
	index := rand.Intn(sz)
	rec = s.records[index]
	s.records = append(s.records[:index], s.records[index+1:]...)
	return
}

func (s *RecordStore) getRecord() (rec Record, err error) {
	if s.numKeys > 0 { // preloaded keys
		count := s.numKeys
		if s.numKeys >= MaxDeletes {
			count = s.numKeys >> 2
		}
		k := expRand(count)
		s.currGet = k

		rec = Record{
			key: NewRandomKey(s.offsetGet + k),
		}
		return
	}

	sz := len(s.records)
	if sz == 0 {
		err = fmt.Errorf("no record in the store")
		return
	}
	index := rand.Intn(sz)
	rec = s.records[index]
	if rec.isExpired() {
		s.records = append(s.records[:index], s.records[index+1:]...)
		err = fmt.Errorf("expired")
	}
	return
}

func (s *RecordStore) empty() bool {
	return len(s.records) == 0 && s.numKeys == 0
}

func (s *RecordStore) endOfDelete() bool {
	return s.numKeys > 0 &&
		(s.nextDelete >= s.numKeys || s.nextDelete >= MaxDeletes)
}

func (s *RecordStore) Get() (rec Record, err error) {
	for !s.empty() {
		rec, err = s.getRecord()
		if err == nil {
			return
		}
	}
	err = fmt.Errorf("no record")
	return
}

func (s *RecordStore) Take() (rec Record, err error) {
	for !s.empty() && !s.endOfDelete() {
		rec, err = s.takeRecord()
		if err == nil && !rec.isExpired() {
			if s.endOfDelete() {
				s.LastDelete = true
			}
			return
		}
	}
	err = fmt.Errorf("no unexpired record")
	return
}

func (e *TestEngine) Init() {
	e.invokeFuncs = make([]InvokeFunc, kNumRequestTypes)
	e.invokeFuncs[kRequestTypeCreate] = e.invokeCreate
	e.invokeFuncs[kRequestTypeGet] = e.invokeGet
	e.invokeFuncs[kRequestTypeUpdate] = e.invokeUpdate
	e.invokeFuncs[kRequestTypeSet] = e.invokeSet
	e.invokeFuncs[kRequestTypeDestroy] = e.invokeDestroy
}

func (e *TestEngine) restoreData() {
	if e.recStore.numKeys <= 0 || e.recStore.nextDelete <= 0 {
		return
	}

	count := e.recStore.nextDelete

	// Add back deleted keys
	glog.Infof("Add back deleted keys: count=%d", count)
	for i := 0; i < count; i++ {

		now := time.Now()

		key := NewRandomKey(e.recStore.offsetDel + i)
		_, err := e.client.Create(key, e.rdgen.createPayload())
		tm := time.Since(now)

		e.stats.Put(kRequestTypeCreate, tm, err)
		e.movingStats.Put(kRequestTypeCreate, tm, err)
		if err != nil {
			glog.Errorf("%s error: %s", kRequestTypeCreate.String(), err)
			e.recStore.display()
		}
	}
}

func (e *TestEngine) Run(wg *sync.WaitGroup, chDone <-chan bool) {
	defer wg.Done()
	startTime := time.Now()
	var numreq int = 0
	errCount := 0
	for {
		for _, item := range e.reqSequence.items {
			if e.recStore.numKeys > 0 &&
				item.reqType == kRequestTypeCreate {
				continue
			}
			for i := 0; i < item.numRequests; i++ {
				select {
				case <-chDone:
					e.restoreData()
					return
				default:
					now := time.Now()
					err := e.invoke(item.reqType)
					tm := time.Since(now)
					if item.reqType == kRequestTypeDestroy &&
						e.recStore.endOfDelete() {
						if e.recStore.LastDelete {
							e.recStore.LastDelete = false
						} else {
							continue
						}
					}
					e.stats.Put(item.reqType, tm, err)
					e.movingStats.Put(item.reqType, tm, err)
					if err != nil {
						glog.Errorf("%s error: %s", item.reqType.String(), err)
						if e.recStore.numKeys > 0 {
							e.recStore.display()
							errCount++
							if errCount > 100 {
								return
							}
						}
					}
					diff := now.Sub(startTime)
					if e.rdgen.isVariable && diff > (12*time.Second) {
						e.numReqPerSecond = e.rdgen.getThroughPut()
						startTime = time.Now()
						numreq = 0
					}
					numreq++
					if e.rdgen.isVariable {
						e.checkSpeedForVariableTp(now, numreq, startTime)
					} else {
						e.checkSpeedDelayIfNeeded(now)
					}
				}
			}
		}
	}
}

func (e *TestEngine) checkSpeedDelayIfNeeded(now time.Time) {
	num := e.stats.GetNumRequests()
	if num < 10 {
		return
	}
	expectedDur := 1 * time.Second / time.Duration(e.numReqPerSecond)
	expectedDur *= time.Duration(num)

	//	glog.Infof("expected to take %s", expectedDur.String())
	dur := now.Sub(e.stats.tmStart)
	//	glog.Infof("actual time taken %s", dur.String())
	delta := expectedDur - dur
	if delta > 0 {
		//	glog.Infof("sleeping ... %s", delta.String())
		time.Sleep(delta)
	}
}

func (e *TestEngine) checkSpeedForVariableTp(now time.Time, numReq int, startTime time.Time) {
	if numReq < 10 {
		return
	}
	expectedDur := 1 * time.Second / time.Duration(e.numReqPerSecond)
	expectedDur *= time.Duration(numReq)

	//      glog.Infof("expected to take %s", expectedDur.String())
	dur := now.Sub(startTime)
	//      glog.Infof("actual time taken %s", dur.String())
	delta := expectedDur - dur
	if delta > 0 {
		//      glog.Infof("sleeping ... %s", delta.String())
		time.Sleep(delta)
	}
}

func (e *TestEngine) invokeCreate() (err error) {
	if e.recStore.numKeys > 0 {
		return
	}
	key := newTestKey()
	var ctx client.IContext

	if ctx, err = e.client.Create(key, e.rdgen.createPayload(), client.WithTTL(e.rdgen.getTTL())); err == nil {
		rec := Record{
			key: key,
			ctx: ctx,
		}
		e.recStore.Add(rec)
	}
	return
}

func (e *TestEngine) invokeGet() (err error) {
	var rec Record

	if rec, err = e.recStore.Get(); err == nil {
		ttl := uint32(0)
		if !e.rdgen.disableGetTTL {
			ttl = e.rdgen.getTTL()
		}
		_, _, err = e.client.Get(rec.key, client.WithTTL(ttl))
	}
	return
}

func (e *TestEngine) invokeUpdate() (err error) {
	var rec Record

	if rec, err = e.recStore.Get(); err == nil {
		_, err = e.client.Update(rec.key, e.rdgen.createPayload(), client.WithTTL(e.rdgen.getTTL()))
	}
	return
}

func (e *TestEngine) invokeSet() (err error) {
	var rec Record

	if rec, err = e.recStore.Get(); err == nil {
		_, err = e.client.Set(rec.key, e.rdgen.createPayload(), client.WithTTL(e.rdgen.getTTL()))
	}
	return
}

func (e *TestEngine) invokeDestroy() (err error) {
	var rec Record

	if e.recStore.endOfDelete() {
		return nil
	}
	if rec, err = e.recStore.Take(); err == nil {
		err = e.client.Destroy(rec.key)
	}

	return
}

func (e *TestEngine) invoke(t RequestType) (err error) {
	if t > kNumRequestTypes {
		glog.Exitf("not supported request type : %d", t)
	}
	f := e.invokeFuncs[t]
	if f != nil {
		err = f()
	} else {
		glog.Errorf("test engine not properly initalized")
	}
	return
}
