// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"context"
	"runtime"
	"testing"
	"time"
)

func TestWalletStateNotificationsFollowUnlockAndTimeout(t *testing.T) {
	cfg := basicWalletConfig
	w, teardown := testWallet(t, &cfg)
	defer teardown()
	changed, done := w.NtfnServer.WalletStateNotifications()
	defer done()
	timer := time.NewTimer(time.Hour)
	defer timer.Stop()
	if err := w.Unlock(context.Background(), testPrivPass, timer.C); err != nil {
		t.Fatal(err)
	}
	select {
	case <-changed:
		if w.Locked() {
			t.Fatal("unlock notification preceded the state change")
		}
	case <-time.After(time.Second):
		t.Fatal("unlock did not notify subscribers")
	}
	timer.Reset(10 * time.Millisecond)
	select {
	case <-changed:
		if !w.Locked() {
			t.Fatal("timeout notification preceded the lock")
		}
	case <-time.After(time.Second):
		t.Fatal("automatic timer lock did not notify subscribers")
	}
	done()
	done()
	if err := w.Unlock(context.Background(), testPrivPass, nil); err != nil {
		t.Fatal(err)
	}
	w.Lock()
	select {
	case <-changed:
		t.Fatal("removed subscriber still received events")
	default:
	}
}

func TestWalletStateNotificationsCoalesceWithoutBlocking(t *testing.T) {
	s := &NotificationServer{}
	changed, done := s.WalletStateNotifications()
	defer done()
	for i := 0; i < 100; i++ {
		s.notifyWalletState()
	}
	select {
	case <-changed:
	default:
		t.Fatal("no state update queued")
	}
	select {
	case <-changed:
		t.Fatal("state changes were not coalesced")
	default:
	}
}

func TestWalletStateNotificationsBypassBlockedTipSubscriber(t *testing.T) {
	s := &NotificationServer{}
	tips := s.MainTipChangedNotifications()
	changed, done := s.WalletStateNotifications()
	defer done()

	tipSent := make(chan struct{})
	go func() {
		s.notifyMainChainTipChanged(&MainTipChangedNotification{})
		close(tipSent)
	}()
	// The unbuffered tip delivery holds the notification mutex until its
	// subscriber either receives the tip or starts draining on shutdown.
	deadline := time.Now().Add(time.Second)
	for s.mu.TryLock() {
		s.mu.Unlock()
		if time.Now().After(deadline) {
			t.Fatal("tip sender did not acquire the notification mutex")
		}
		runtime.Gosched()
	}
	workerFinished := make(chan struct{})
	go func() {
		s.notifyWalletState()
		done()
		close(workerFinished)
	}()
	for _, completed := range []<-chan struct{}{workerFinished, changed} {
		select {
		case <-completed:
		case <-time.After(time.Second):
			t.Fatal("state notification or unsubscribe blocked behind a tip subscriber")
		}
	}

	// Teardown still drains the pending tip to release legacy notifications.
	tips.Done()
	select {
	case <-tipSent:
	case <-time.After(time.Second):
		t.Fatal("stopping the tip subscriber did not release notification delivery")
	}
}
