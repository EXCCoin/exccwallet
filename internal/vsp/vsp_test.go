// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package vsp

import (
	"context"
	"testing"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/wire"
)

func TestBindPolicyIsRequestLocal(t *testing.T) {
	want := []Policy{
		{MaxFee: 5, FeeAcct: 7, ChangeAcct: 9},
		{MaxFee: 6, FeeAcct: 8, ChangeAcct: 10},
	}
	var got []Policy
	process := func(_ context.Context, _ *chainhash.Hash, _ *wire.MsgTx,
		policy Policy) error {

		got = append(got, policy)
		return nil
	}

	for _, policy := range want {
		if err := BindPolicy(process, policy)(context.Background(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("wrong policy: got %+v, want %+v", got, want)
	}
}
