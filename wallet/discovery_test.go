// Copyright (c) 2020 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"context"
	"testing"

	blockchain "github.com/EXCCoin/exccd/blockchain/standalone/v2"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	gcs2 "github.com/EXCCoin/exccd/gcs/v3"
	"github.com/EXCCoin/exccd/gcs/v3/blockcf2"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

func TestAddrFinderIncludesTipFilter(t *testing.T) {
	ctx := context.Background()
	w, teardown := testWallet(t, &basicWalletConfig)
	defer teardown()

	w.addressBuffersMu.Lock()
	xpub := w.addressBuffers[0].albExternal.branchXpub
	w.addressBuffersMu.Unlock()
	addr, err := deriveChildAddress(xpub, 0, w.chainParams)
	if err != nil {
		t.Fatal(err)
	}
	_, script := addr.PaymentScript()

	tx := wire.NewMsgTx()
	tx.AddTxOut(wire.NewTxOut(1, script))
	header := wire.BlockHeader{
		PrevBlock:  w.chainParams.GenesisHash,
		MerkleRoot: blockchain.CalcTxTreeMerkleRoot([]*wire.MsgTx{tx}),
		Height:     1,
	}
	block := wire.NewMsgBlock(&header)
	block.AddTransaction(tx)
	key := blockcf2.Key(&header.MerkleRoot)
	filter, err := gcs2.NewFilterV2(blockcf2.B, blockcf2.M, key, [][]byte{script})
	if err != nil {
		t.Fatal(err)
	}
	err = walletdb.Update(ctx, w.db, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadWriteBucket(wtxmgrNamespaceKey)
		return w.txStore.ExtendMainChain(ns, &header, filter)
	})
	if err != nil {
		t.Fatal(err)
	}

	finder, err := newAddrFinder(ctx, w, w.GapLimit())
	if err != nil {
		t.Fatal(err)
	}
	var blockRequests int
	peer := &peerFuncs{blocks: func(_ context.Context, hashes []*chainhash.Hash) ([]*wire.MsgBlock, error) {
		blockRequests++
		if len(hashes) != 1 || *hashes[0] != header.BlockHash() {
			t.Fatalf("requested blocks = %v, want %v", hashes, header.BlockHash())
		}
		return []*wire.MsgBlock{block}, nil
	}}
	hash := header.BlockHash()
	if err := finder.find(ctx, &hash, peer); err != nil {
		t.Fatal(err)
	}
	if blockRequests != 1 {
		t.Fatalf("block requests = %d, want 1", blockRequests)
	}
	if got := finder.usage[0].extLastUsed; got != 0 {
		t.Fatalf("last used external index = %d, want 0", got)
	}
}

// TestDiscoveryCursorPos tests that the account cursor index is not reset
// during address discovery such that an address could be reused.
func TestDiscoveryCursorPos(t *testing.T) {
	ctx := context.Background()

	cfg := basicWalletConfig
	// normally would just do the upgrade, but the buffers record
	// off-by-ones after the upgrade.  will be fixed in a later commit.
	cfg.DisableCoinTypeUpgrades = true

	w, teardown := testWallet(t, &cfg)
	defer teardown()

	/*
		// Upgrade the cointype before proceeding.  The test is invalid if a
		// cointype upgrade occurs during discovery.
		err := w.UpgradeToSLIP0044CoinType(ctx)
		if err != nil {
			t.Fatal(err)
		}
	*/

	// Advance the cursor within the gap limit but without recording the
	// returned addresses in the database (these may be persisted during a
	// later update).
	w.addressBuffersMu.Lock()
	xpub := w.addressBuffers[0].albExternal.branchXpub
	w.addressBuffers[0].albExternal.cursor = 9 // 0-9 have been returned
	w.addressBuffersMu.Unlock()

	// Perform address discovery
	// All peer funcs may be left unimplemented; wallet only records the genesis block.
	peer := &peerFuncs{}
	err := w.DiscoverActiveAddresses(ctx, peer, &w.chainParams.GenesisHash, false, w.GapLimit())
	if err != nil {
		t.Fatal(err)
	}

	w.addressBuffersMu.Lock()
	lastUsed := w.addressBuffers[0].albExternal.lastUsed
	cursor := w.addressBuffers[0].albExternal.cursor
	w.addressBuffersMu.Unlock()
	wasLastUsed := ^uint32(0)
	wasCursor := uint32(9)
	if lastUsed != wasLastUsed || cursor != wasCursor {
		t.Errorf("cursor was reset: lastUsed=%d (want %d) cursor=%d (want %d)",
			lastUsed, wasLastUsed, cursor, wasCursor)
	}

	// Manually mark an address between the lastUsed and cursor as used, and
	// addresses through the cursor as returned, then perform discovery
	// again.  The cursor should be reduced such that the next returned
	// address would be the same as before, without introducing a backwards
	// reset or wasted addresses.
	addr4, err := deriveChildAddress(xpub, 4, w.chainParams)
	if err != nil {
		t.Fatal(err)
	}
	err = walletdb.Update(ctx, w.db, func(dbtx walletdb.ReadWriteTx) error {
		ns := dbtx.ReadBucket(waddrmgrNamespaceKey)
		err = w.manager.MarkReturnedChildIndex(dbtx, 0, 0, 9) // 0-9 have been returned
		if err != nil {
			return err
		}
		maddr4, err := w.manager.Address(ns, addr4)
		if err != nil {
			return err
		}
		return w.markUsedAddress("", dbtx, maddr4)
	})
	if err != nil {
		t.Fatal(err)
	}
	err = w.DiscoverActiveAddresses(ctx, peer, &w.chainParams.GenesisHash, false, w.GapLimit())
	if err != nil {
		t.Fatal(err)
	}

	w.addressBuffersMu.Lock()
	lastUsed = w.addressBuffers[0].albExternal.lastUsed
	cursor = w.addressBuffers[0].albExternal.cursor
	w.addressBuffersMu.Unlock()
	wasLastUsed += 5
	wasCursor -= 5
	if lastUsed != wasLastUsed || cursor != wasCursor {
		t.Errorf("cursor was reset: lastUsed=%d (want %d) cursor=%d (want %d)",
			lastUsed, wasLastUsed, cursor, wasCursor)
	}
}
