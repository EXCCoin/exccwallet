// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"testing"

	"github.com/EXCCoin/exccd/wire"
)

// A duplicate header notification may carry relevant transactions which were
// not known when the forest first learned about that block.
func TestDuplicateSidechainKeepsRelevantTransactions(t *testing.T) {
	first := &wire.MsgTx{TxOut: []*wire.TxOut{{Value: 1}}}
	second := &wire.MsgTx{
		TxIn:  []*wire.TxIn{{PreviousOutPoint: wire.OutPoint{Hash: first.TxHash()}}},
		TxOut: []*wire.TxOut{{Value: 2}},
	}
	for _, tc := range []struct {
		name          string
		old, incoming []*wire.MsgTx
		want          int
		wantKnown     bool
	}{
		{"header_only_then_known", nil, []*wire.MsgTx{first}, 1, true},
		{"empty_then_known", []*wire.MsgTx{}, []*wire.MsgTx{first}, 1, true},
		{"known_then_header_only", []*wire.MsgTx{first}, nil, 1, true},
		{"additional_relevant_tx", []*wire.MsgTx{first}, []*wire.MsgTx{first, second}, 2, true},
		{"parent_discovered_after_child", []*wire.MsgTx{second}, []*wire.MsgTx{first, second}, 2, true},
		{"retain_old_only_child", []*wire.MsgTx{second}, []*wire.MsgTx{first}, 2, true},
		{"header_only_then_empty", nil, []*wire.MsgTx{}, 0, true},
		{"empty_then_header_only", []*wire.MsgTx{}, nil, 0, true},
		{"both_header_only", nil, nil, 0, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			header := &wire.BlockHeader{Height: 1, Bits: 0x207fffff}
			hash := header.BlockHash()
			var forest SidechainForest
			old := NewBlockNode(header, &hash, nil, tc.old)
			if !forest.AddBlockNode(old) {
				t.Fatal("initial block not added")
			}
			fresh := NewBlockNode(header, &hash, nil, tc.incoming)
			if forest.AddBlockNode(fresh) {
				t.Fatal("duplicate should retain original node identity")
			}
			if got := len(old.RelevantTxs); got != tc.want {
				t.Fatalf("retained %d relevant transactions, want %d", got, tc.want)
			}
			if (old.RelevantTxs != nil) != tc.wantKnown {
				t.Fatalf("known transaction list = %v, want %v", old.RelevantTxs != nil, tc.wantKnown)
			}
			if tc.want == 2 && (old.RelevantTxs[0].TxHash() != first.TxHash() ||
				old.RelevantTxs[1].TxHash() != second.TxHash()) {
				t.Fatal("merged transactions must retain parent before child")
			}
		})
	}
}
