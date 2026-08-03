// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package spv

import (
	"sync/atomic"
	"testing"
)

func TestLastPeerDisconnectMarksWalletUnsynced(t *testing.T) {
	var synced []bool
	s := &Syncer{notifications: &Notifications{
		Synced: func(v bool) { synced = append(synced, v) },
	}}
	atomic.StoreUint32(&s.atomicWalletSynced, 1)

	s.peerDisconnected(0, "peer")

	if s.Synced() {
		t.Fatal("wallet remained synced after its last peer disconnected")
	}
	if len(synced) != 1 || synced[0] {
		t.Fatalf("synced notifications = %v, want [false]", synced)
	}
}
