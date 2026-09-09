// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"context"
	"testing"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
)

func TestChainSwitchDetachedBlockPreservesScanGap(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		name := "known_empty"
		if unknown {
			name = "header_only"
		}
		t.Run(name, func(t *testing.T) {
			cfg := basicWalletConfig
			w, teardown := testWallet(t, &cfg)
			defer teardown()
			g := maketg(t, cfg.Params)
			tw := &tw{t, w}
			forest := new(SidechainForest)
			attach := func(branchLen int, blocks ...*gblock) {
				t.Helper()
				for _, b := range blocks {
					mustAddBlockNode(t, forest, b.BlockNode)
				}
				tip := blocks[len(blocks)-1].Hash
				tw.chainSwitch(forest, tw.evaluateBestChain(forest, branchLen, tip))
			}
			assertRescan := func(want *chainhash.Hash) {
				t.Helper()
				got, err := w.RescanPoint(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				if (got == nil) != (want == nil) || (got != nil && *got != *want) {
					t.Fatalf("rescan point = %v, want %v", got, want)
				}
			}
			attach(1, g.createBlockOne("one"))
			oldTwo := g.nextBlock("two-old", nil, nil)
			oldThree := g.nextBlock("three-old", nil, nil)
			var gap *chainhash.Hash
			if unknown {
				oldTwo.RelevantTxs = nil
				gap = oldTwo.Hash
			}
			attach(2, oldTwo, oldThree)
			assertRescan(gap)
			oldFour := g.nextBlock("four-old", nil, nil)
			oldFive := g.nextBlock("five-old", nil, nil)

			// Detach the branch containing the original scan gap.
			g.SetTip("one")
			attach(3, g.nextBlock("two-new", nil, nil),
				g.nextBlock("three-new", nil, nil), g.nextBlock("four-new", nil, nil))
			assertRescan(nil)
			for _, tree := range forest.trees {
				if *tree.root.Hash == *oldTwo.Hash && (tree.root.RelevantTxs == nil) != unknown {
					t.Errorf("detached original block lost its known/unknown scan status")
				}
			}

			// Reuse the saved detached nodes. No new transaction information for
			// the original gap arrived, so returning to it must require a rescan.
			attach(4, oldFour, oldFive)
			assertRescan(gap)
		})
	}
}
