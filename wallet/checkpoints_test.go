// Copyright (c) 2026 The EXCC developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"testing"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/wire"
)

func TestMainNetRecoveryCheckpoint(t *testing.T) {
	want := mustParseHash("0000193e7b4c05dd28f56bc7c9180364dab3357b5a4a23f2a776fc44d7b61901")
	got := CheckpointHash(wire.MainNet, 1726905)
	if got == nil || *got != want {
		t.Fatalf("CheckpointHash(MainNet, 1726905) = %v, want %v", got, want)
	}
	if BadCheckpoint(wire.MainNet, &want, 1726905) {
		t.Fatal("BadCheckpoint rejected canonical block 1726905")
	}
	wrong := chainhash.Hash{0x01}
	if !BadCheckpoint(wire.MainNet, &wrong, 1726905) {
		t.Fatal("BadCheckpoint accepted wrong block hash at 1726905")
	}
	if CheckpointHash(wire.TestNet3, 1726905) != nil {
		t.Fatal("mainnet checkpoint unexpectedly active on TestNet3")
	}
}
