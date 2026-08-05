// Copyright (c) 2026 The ExchangeCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package udb

import (
	"context"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/blockchain/v4/chaingen"
	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccd/dcrutil/v4"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

func TestPruneUnminedUnknownStakeDifficultyKeepsTickets(t *testing.T) {
	ctx := context.Background()
	db, _, store, _, teardown, err := cloneDB(t.TempDir() + "/prune-unmined.kv")
	if err != nil {
		t.Fatal(err)
	}
	defer teardown()

	params := chaincfg.TestNet3Params()
	g, err := chaingen.MakeGenerator(params, nil)
	if err != nil {
		t.Fatal(err)
	}
	price := dcrutil.Amount(params.MinimumStakeDiff)
	funding := wire.NewMsgTx()
	funding.AddTxOut(wire.NewTxOut(int64(price), []byte{0x51}))
	spend := chaingen.MakeSpendableOutForTx(funding, 1, 0, 0)
	ticket := g.CreateTicketPurchaseTx(&spend, price, 0)
	rec, err := NewTxRecordFromMsgTx(ticket, time.Now())
	if err != nil {
		t.Fatal(err)
	}

	err = walletdb.Update(ctx, db, func(dbtx walletdb.ReadWriteTx) error {
		if err := store.InsertMemPoolTx(dbtx, rec); err != nil {
			return err
		}
		removed, err := store.PruneUnmined(dbtx, 0)
		if err != nil {
			return err
		}
		if len(removed) != 0 {
			t.Fatalf("removed tickets with unknown stake difficulty: %v", removed)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	err = walletdb.Update(ctx, db, func(dbtx walletdb.ReadWriteTx) error {
		removed, err := store.PruneUnmined(dbtx, price+1)
		if err != nil {
			return err
		}
		if len(removed) != 1 || *removed[0] != rec.Hash {
			t.Fatalf("removed tickets = %v, want %v", removed, rec.Hash)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
