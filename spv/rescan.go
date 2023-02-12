// Copyright (c) 2013-2014 The btcsuite developers
// Copyright (c) 2018-2021 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package spv

import (
	"github.com/EXCCoin/exccd/blockchain/stake/v4"
	"github.com/EXCCoin/exccd/gcs/v3/blockcf2"
	"github.com/EXCCoin/exccd/txscript/v4/stdscript"
	"github.com/EXCCoin/exccd/wire"
)

// rescanCheckTransaction is a helper function to rescan both stake and regular
// transactions in a block.  It appends transactions that match the filters to
// *matches, while updating the filters to add outpoints for new UTXOs
// controlled by this wallet.  New data added to the Syncer's filters is also
// added to fadded.
//
// This function may only be called with the filter mutex held.
func (s *Syncer) rescanCheckTransactions(matches *[]*wire.MsgTx, fadded *blockcf2.Entries, txs []*wire.MsgTx, tree int8) {
	for i, tx := range txs {
		// Keep track of whether the transaction has already been added
		// to the result.  It shouldn't be added twice.
		added := false

		txty := stake.TxTypeRegular
		if tree == wire.TxTreeStake {
			txty = stake.DetermineTxType(tx, true, false)
		}

		// Coinbases and stakebases are handled specially: all inputs of a
		// coinbase and the first (stakebase) input of a vote are skipped over
		// as they generate coins and do not reference any previous outputs.
		inputs := tx.TxIn
		if i == 0 && txty == stake.TxTypeRegular {
			goto LoopOutputs
		}
		if txty == stake.TxTypeSSGen {
			inputs = inputs[1:]
		}

		for _, input := range inputs {
			if !s.rescanFilter.ExistsUnspentOutPoint(&input.PreviousOutPoint) {
				continue
			}
			if !added {
				*matches = append(*matches, tx)
				added = true
			}
		}

	LoopOutputs:
		for i, output := range tx.TxOut {
			_, addrs := stdscript.ExtractAddrs(output.Version, output.PkScript, s.wallet.ChainParams())
			for _, a := range addrs {
				if !s.rescanFilter.ExistsAddress(a) {
					continue
				}

				op := wire.OutPoint{
					Hash:  tx.TxHash(),
					Index: uint32(i),
					Tree:  tree,
				}
				if !s.rescanFilter.ExistsUnspentOutPoint(&op) {
					s.rescanFilter.AddUnspentOutPoint(&op)
				}

				if !added {
					*matches = append(*matches, tx)
					added = true
				}
			}
		}
	}
}

// rescanBlock rescans a block for any relevant transactions for the passed
// lookup keys.  Returns any discovered transactions and any new data added to
// the filter.
func (s *Syncer) rescanBlock(block *wire.MsgBlock) (matches []*wire.MsgTx, fadded blockcf2.Entries) {
	s.filterMu.Lock()
	s.rescanCheckTransactions(&matches, &fadded, block.STransactions, wire.TxTreeStake)
	s.rescanCheckTransactions(&matches, &fadded, block.Transactions, wire.TxTreeRegular)
	s.filterMu.Unlock()
	return matches, fadded
}

// filterRelevant filters out all transactions considered irrelevant
// without updating filters.
func (s *Syncer) filterRelevant(txs []*wire.MsgTx) []*wire.MsgTx {
	defer s.filterMu.Unlock()
	s.filterMu.Lock()

	matches := txs[:0]
Txs:
	for _, tx := range txs {
		for _, in := range tx.TxIn {
			if s.rescanFilter.ExistsUnspentOutPoint(&in.PreviousOutPoint) {
				matches = append(matches, tx)
				continue Txs
			}
		}
		for _, out := range tx.TxOut {
			_, addrs := stdscript.ExtractAddrs(out.Version, out.PkScript, s.wallet.ChainParams())
			for _, a := range addrs {
				if s.rescanFilter.ExistsAddress(a) {
					matches = append(matches, tx)
					continue Txs
				}
			}
		}
	}

	return matches
}
