// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"context"
	"testing"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
)

func TestChainSwitchPreservesUnprocessedBlock(t *testing.T) {
	for _, reorg := range []bool{false, true} {
		name := "extension"
		if reorg {
			name = "reorganization"
		}
		t.Run(name, func(t *testing.T) {
			cfg := basicWalletConfig
			w, teardown := testWallet(t, &cfg)
			defer teardown()
			g := maketg(t, cfg.Params)
			tw := &tw{t, w}
			forest := new(SidechainForest)
			attach := func(blocks ...*gblock) {
				for _, b := range blocks {
					mustAddBlockNode(t, forest, b.BlockNode)
				}
				tip := blocks[len(blocks)-1].Hash
				tw.chainSwitch(forest, tw.evaluateBestChain(forest, len(blocks), tip))
			}
			assertRescan := func(want *chainhash.Hash) {
				t.Helper()
				got, err := w.RescanPoint(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if got == nil || *got != *want {
					t.Fatalf("rescan point = %v, want first unprocessed block %v", got, want)
				}
			}
			attach(g.createBlockOne("one"))
			if reorg {
				attach(g.nextBlock("two-old", nil, nil))
				g.SetTip("one")
			}
			missing := g.nextBlock("two-missing", nil, nil)
			missing.RelevantTxs = nil
			if reorg {
				// A header-only block at the start of a reorganization leaves the
				// transaction marker on the old branch, whose ancestor is block one.
				attach(missing, g.nextBlock("three-old", nil, nil))
			} else {
				attach(missing)
			}
			assertRescan(missing.Hash)
			if reorg {
				// The next reorganization starts exactly one block after the gap.
				// Its known transactions must not mark the missing block processed.
				g.SetTip("two-missing")
				attach(g.nextBlock("three-new", nil, nil), g.nextBlock("four-new", nil, nil))
			} else {
				attach(g.nextBlock("three-new", nil, nil))
			}
			assertRescan(missing.Hash)
		})
	}
}
