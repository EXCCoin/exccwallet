// Copyright (c) 2026 The ExchangeCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package udb

import (
	"context"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccwallet/v2/errors"
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

func TestPruneUnminedVSPFee(t *testing.T) {
	for _, mode := range []string{"expired", "price-change", "valid-ticket", "published", "confirmed", "mined", "dependent", "shared", "unassociated"} {
		t.Run(mode, func(t *testing.T) {
			ctx := context.Background()
			db, _, store, _, teardown, err := cloneDB(t.TempDir() + "/vsp-fee.kv")
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
			funding.AddTxOut(wire.NewTxOut(1000, []byte{0x51}))
			spend := chaingen.MakeSpendableOutForTx(funding, 1, 0, 0)
			ticket := g.CreateTicketPurchaseTx(&spend, price, 0)
			if mode != "price-change" && mode != "valid-ticket" {
				ticket.Expiry = 1
			}
			fee := wire.NewMsgTx()
			fee.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: funding.TxHash(), Index: 1}, 1000, nil))
			fee.AddTxOut(wire.NewTxOut(900, []byte{0x51}))
			var records []*TxRecord
			for _, tx := range []*wire.MsgTx{funding, ticket, fee} {
				rec, err := NewTxRecordFromMsgTx(tx, time.Now())
				if err != nil {
					t.Fatal(err)
				}
				records = append(records, rec)
			}
			feeHash := records[2].Hash
			removeFee := mode == "expired" || mode == "price-change"
			var removed []*chainhash.Hash
			err = walletdb.Update(ctx, db, func(dbtx walletdb.ReadWriteTx) error {
				blocks := makeBlockGenerator()
				b := blocks.generate(dcrutil.BlockValid)
				if err := insertMainChainHeaders(store, dbtx, makeHeaderDataSlice(b), emptyFilters(1)); err != nil {
					return err
				}
				for _, rec := range records {
					if err := store.InsertMemPoolTx(dbtx, rec); err != nil {
						return err
					}
				}
				if err := store.SetPublished(dbtx, &feeHash, mode == "published"); err != nil {
					return err
				}
				if mode == "mined" {
					hash := b.BlockHash()
					if err := store.InsertMinedTx(dbtx, records[2], &hash); err != nil {
						return err
					}
				}
				info := &VSPTicket{FeeHash: feeHash, FeeTxStatus: uint32(VSPFeeProcessPaid), Host: "https://vsp.example", PubKey: make([]byte, 32)}
				if mode == "confirmed" {
					info.FeeTxStatus = uint32(VSPFeeProcessConfirmed)
				}
				if mode != "unassociated" {
					if err := SetVSPTicket(dbtx, &records[1].Hash, info); err != nil {
						return err
					}
				}
				if mode == "shared" {
					other := chainhash.Hash{123}
					if err := SetVSPTicket(dbtx, &other, info); err != nil {
						return err
					}
				}
				if mode == "dependent" {
					child := wire.NewMsgTx()
					child.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: feeHash}, 900, nil))
					child.AddTxOut(wire.NewTxOut(800, []byte{0x51}))
					rec, err := NewTxRecordFromMsgTx(child, time.Now())
					if err != nil {
						return err
					}
					if err := store.InsertMemPoolTx(dbtx, rec); err != nil {
						return err
					}
				}
				sdiff := dcrutil.Amount(0)
				if mode == "price-change" {
					sdiff = price + 1
				}
				var err error
				removed, err = store.PruneUnmined(dbtx, sdiff)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			err = walletdb.View(ctx, db, func(dbtx walletdb.ReadTx) error {
				ns := dbtx.ReadBucket(wtxmgrBucketKey)
				_, err := store.TxDetails(ns, &feeHash)
				if removeFee && !errors.Is(err, errors.NotExist) {
					t.Fatalf("fee remains: %v", err)
				}
				if !removeFee && err != nil {
					t.Fatalf("protected fee removed: %v", err)
				}
				if removeFee && existsRawUnminedInput(ns, canonicalOutPoint(&records[0].Hash, 1)) != nil {
					t.Fatal("fee funding remains marked spent")
				}
				_, err = store.TxDetails(ns, &records[1].Hash)
				if mode == "valid-ticket" && err != nil {
					t.Fatalf("valid ticket removed: %v", err)
				}
				if mode != "valid-ticket" && !errors.Is(err, errors.NotExist) {
					t.Fatalf("invalid ticket remains: %v", err)
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}
			feeNotified := false
			for _, hash := range removed {
				if *hash == feeHash {
					feeNotified = true
				}
			}
			if feeNotified != removeFee {
				t.Fatalf("removed fee notification = %v, want %v", feeNotified, removeFee)
			}
			// Repeating pruning after a restart must not remove anything else.
			err = walletdb.Update(ctx, db, func(dbtx walletdb.ReadWriteTx) error {
				again, err := store.PruneUnmined(dbtx, 0)
				if len(again) != 0 {
					t.Fatalf("repeat prune removed transactions: %v", again)
				}
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
		})
	}
}
