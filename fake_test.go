package main

import (
	"context"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2026, 10, 19, 12, 30, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Add(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type entry struct {
	v        []byte
	deadline time.Time
}

// fakeKV follows godis semantics: Set and a CAS swap clear the deadline, Incr keeps it, an expired key is missing
type fakeKV struct {
	mu  sync.Mutex
	now func() time.Time
	m   map[string]entry

	expireErr func(key string) error
	failAll   error
}

func newFakeKV(now func() time.Time) *fakeKV {
	return &fakeKV{now: now, m: map[string]entry{}}
}

func (f *fakeKV) live(key string) (entry, bool) {
	e, ok := f.m[key]
	if ok && !e.deadline.IsZero() && !e.deadline.After(f.now()) {
		delete(f.m, key)
		return entry{}, false
	}
	return e, ok
}

func (f *fakeKV) keys(prefix string) []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []string
	for k := range f.m {
		if _, ok := f.live(k); ok && strings.HasPrefix(k, prefix) {
			out = append(out, k)
		}
	}
	return out
}

func (f *fakeKV) put(key string, v []byte) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[key] = entry{v: append([]byte(nil), v...)}
}

func (f *fakeKV) Ping(context.Context) error { return f.failAll }

func (f *fakeKV) Get(_ context.Context, key string) ([]byte, bool, error) {
	f.mu.Lock()
	if f.failAll != nil {
		f.mu.Unlock()
		return nil, false, f.failAll
	}
	e, ok := f.live(key)
	f.mu.Unlock()
	runtime.Gosched()
	return append([]byte(nil), e.v...), ok, nil
}

func (f *fakeKV) Del(_ context.Context, key string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return f.failAll
	}
	delete(f.m, key)
	return nil
}

func (f *fakeKV) CAS(_ context.Context, key string, expected []byte, expectFound bool, value []byte) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return false, f.failAll
	}
	e, ok := f.live(key)
	if ok != expectFound || (ok && string(e.v) != string(expected)) {
		return false, nil
	}
	f.m[key] = entry{v: append([]byte(nil), value...)}
	return true, nil
}

func (f *fakeKV) SetTTL(_ context.Context, key string, value []byte, ttl time.Duration) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return f.failAll
	}
	f.m[key] = entry{v: append([]byte(nil), value...), deadline: f.now().Add(ttl)}
	return nil
}

func (f *fakeKV) Expire(_ context.Context, key string, ttl time.Duration) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return false, f.failAll
	}
	if f.expireErr != nil {
		if err := f.expireErr(key); err != nil {
			return false, err
		}
	}
	e, ok := f.live(key)
	if !ok {
		return false, nil
	}
	if ttl <= 0 {
		delete(f.m, key)
		return true, nil
	}
	e.deadline = f.now().Add(ttl)
	f.m[key] = e
	return true, nil
}

func (f *fakeKV) TTL(_ context.Context, key string) (time.Duration, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return 0, false, f.failAll
	}
	e, ok := f.live(key)
	if !ok {
		return 0, false, nil
	}
	if e.deadline.IsZero() {
		return 0, true, nil
	}
	return e.deadline.Sub(f.now()), true, nil
}

func (f *fakeKV) Incr(_ context.Context, key string, delta int64) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.failAll != nil {
		return 0, f.failAll
	}
	e, _ := f.live(key)
	var n int64
	if e.v != nil {
		var err error
		if n, err = strconv.ParseInt(string(e.v), 10, 64); err != nil {
			return 0, fmt.Errorf("value at %s is not an integer", key)
		}
	}
	n += delta
	e.v = []byte(strconv.FormatInt(n, 10))
	f.m[key] = e
	return n, nil
}
