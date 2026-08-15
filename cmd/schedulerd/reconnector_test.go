package main

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// Regression tests for GAP-048 (2026-08-14): the background reconnector must
// re-ping the gateway even when gwConnected==false (fallback-start), so a
// daemon that started with the gateway down can re-engage HTTP without a
// restart. The original code had `if !gwConnected { continue }`, which meant
// a fallback-start daemon never attempted reconnection.

// fakePinger is a test gatewayPinger whose Ping result can be flipped at
// runtime to simulate a gateway coming back up.
type fakePinger struct {
	failCount int // remaining failures before Ping succeeds
	failErr   error
}

func (f *fakePinger) Ping(ctx context.Context) error {
	if f.failCount > 0 {
		f.failCount--
		return f.failErr
	}
	return nil
}

// TestReconnector_FallbackStart_ReconnectsWhenGatewayReturns proves the core
// GAP-048 fix: a reconnector with gwConnected=false (the fallback-start state)
// pings the gateway and, when it succeeds, calls onConnect and sets
// gwConnected=true. Before the fix, the reconnector skipped pinging entirely
// when gwConnected was false.
func TestReconnector_FallbackStart_ReconnectsWhenGatewayReturns(t *testing.T) {
	pinger := &fakePinger{failCount: 0, failErr: errors.New("connection refused")}
	var gwConnected atomic.Bool
	// Simulate fallback-start: gateway was never connected at startup.
	gwConnected.Store(false)

	connectCount := atomic.Int32{}
	onConnect := func() {
		connectCount.Add(1)
	}

	rc := &gatewayReconnector{
		client:    pinger,
		onConnect: onConnect,
		connected: &gwConnected,
		interval:  50 * time.Millisecond, // fast cycle for testing
		url:       "http://test:8642",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	go rc.run(ctx)

	// Wait for the reconnector to ping + connect.
	deadline := time.Now().Add(2 * time.Second)
	for !gwConnected.Load() || connectCount.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("reconnector never reconnected: gwConnected=%v, connectCount=%d",
				gwConnected.Load(), connectCount.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if connectCount.Load() != 1 {
		t.Errorf("onConnect called %d times, want exactly 1", connectCount.Load())
	}
}

// TestReconnector_AlreadyConnected_StaysConnected proves that when
// gwConnected==true and the ping succeeds, the reconnector does NOT call
// onConnect again (no spurious reconnects on a healthy gateway).
func TestReconnector_AlreadyConnected_StaysConnected(t *testing.T) {
	pinger := &fakePinger{failCount: 0}
	var gwConnected atomic.Bool
	gwConnected.Store(true)

	connectCount := atomic.Int32{}
	onConnect := func() {
		connectCount.Add(1)
	}

	rc := &gatewayReconnector{
		client:    pinger,
		onConnect: onConnect,
		connected: &gwConnected,
		interval:  50 * time.Millisecond,
		url:       "http://test:8642",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go rc.run(ctx)

	// Let it run through a couple of ping cycles.
	time.Sleep(200 * time.Millisecond)

	if !gwConnected.Load() {
		t.Error("gwConnected became false despite successful pings")
	}
	if connectCount.Load() != 0 {
		t.Errorf("onConnect called %d times on healthy gateway, want 0", connectCount.Load())
	}
}

// TestReconnector_GatewayDrops_ReconnectsWithBackoff proves the "gateway
// dropped" path: when gwConnected==true and ping fails, the reconnector enters
// the inner backoff retry loop and, on success, calls onConnect and keeps
// gwConnected=true.
func TestReconnector_GatewayDrops_ReconnectsWithBackoff(t *testing.T) {
	// failCount=1: the outer ping (drop detection) fails, then the first
	// backoff retry succeeds. Backoff wait for attempt 0 is 2s, so total
	// reconnection time is ~2s — well within the 5s context timeout.
	pinger := &fakePinger{failCount: 1, failErr: errors.New("connection reset")}
	var gwConnected atomic.Bool
	gwConnected.Store(true)

	connectCount := atomic.Int32{}
	onConnect := func() {
		connectCount.Add(1)
	}

	rc := &gatewayReconnector{
		client:    pinger,
		onConnect: onConnect,
		connected: &gwConnected,
		interval:  50 * time.Millisecond,
		url:       "http://test:8642",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	go rc.run(ctx)

	// Wait for reconnection.
	deadline := time.Now().Add(5 * time.Second)
	for connectCount.Load() < 1 {
		if time.Now().After(deadline) {
			t.Fatalf("reconnector never reconnected after drop: gwConnected=%v, connectCount=%d",
				gwConnected.Load(), connectCount.Load())
		}
		time.Sleep(10 * time.Millisecond)
	}

	if !gwConnected.Load() {
		t.Error("gwConnected should be true after successful reconnection")
	}
}

// TestReconnector_FallbackStart_AllRetriesFail_StaysDisconnected proves that
// when the gateway stays down, the reconnector does not set gwConnected=true
// or call onConnect — it keeps trying every interval.
func TestReconnector_FallbackStart_AllRetriesFail_StaysDisconnected(t *testing.T) {
	// Always fail: set failCount high enough that no ping succeeds during the test.
	pinger := &fakePinger{failCount: 1000, failErr: errors.New("connection refused")}
	var gwConnected atomic.Bool
	gwConnected.Store(false)

	connectCount := atomic.Int32{}
	onConnect := func() {
		connectCount.Add(1)
	}

	rc := &gatewayReconnector{
		client:    pinger,
		onConnect: onConnect,
		connected: &gwConnected,
		interval:  50 * time.Millisecond,
		url:       "http://test:8642",
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	go rc.run(ctx)

	// Let it run through a few failed cycles.
	time.Sleep(200 * time.Millisecond)

	if gwConnected.Load() {
		t.Error("gwConnected became true despite all pings failing")
	}
	if connectCount.Load() != 0 {
		t.Errorf("onConnect called %d times on failing gateway, want 0", connectCount.Load())
	}
}
