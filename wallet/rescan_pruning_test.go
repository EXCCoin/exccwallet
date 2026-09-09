// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"context"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/blockchain/v4/chaingen"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/dcrutil/v4"
	gcs2 "github.com/EXCCoin/exccd/gcs/v3"
	"github.com/EXCCoin/exccd/gcs/v3/blockcf2"
	"github.com/EXCCoin/exccd/txscript/v4"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

func TestPruningWaitsForCompleteRescan(t *testing.T) {
	for _, mode := range []string{"mined", "empty", "failed", "cancelled"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			w, teardown := testWallet(t, &basicWalletConfig)
			defer teardown()

			params := w.chainParams
			emptyFilter, err := gcs2.FromBytesV2(blockcf2.B, blockcf2.M, nil)
			if err != nil {
				t.Fatal(err)
			}
			prevHash := params.GenesisHash
			err = walletdb.Update(ctx, w.db, func(dbtx walletdb.ReadWriteTx) error {
				ns := dbtx.ReadWriteBucket(wtxmgrNamespaceKey)
				for height := uint32(1); height < 23; height++ {
					header := wire.BlockHeader{
						PrevBlock: prevHash,
						Height:    height,
						Bits:      params.PowLimitBits,
						SBits:     params.MinimumStakeDiff,
					}
					if height == 15 {
						header.PoolSize = 1
					}
					if err := w.txStore.ExtendMainChain(ns, &header, emptyFilter); err != nil {
						return err
					}
					prevHash = header.BlockHash()
				}
				return w.txStore.UpdateProcessedTxsBlockMarker(dbtx, &prevHash)
			})
			if err != nil {
				t.Fatal(err)
			}

			header := &wire.BlockHeader{
				PrevBlock: prevHash,
				Height:    23,
				Bits:      params.PowLimitBits,
				SBits:     params.MinimumStakeDiff,
				PoolSize:  400,
			}
			var nextStakeDiff dcrutil.Amount
			err = walletdb.View(ctx, w.db, func(dbtx walletdb.ReadTx) error {
				var err error
				nextStakeDiff, err = w.nextRequiredDCP0001PoSDifficulty(dbtx, header, nil)
				return err
			})
			if err != nil {
				t.Fatal(err)
			}
			if int64(nextStakeDiff) == header.SBits {
				t.Fatal("test setup did not change stake difficulty")
			}

			funding := wire.NewMsgTx()
			funding.AddTxOut(wire.NewTxOut(int64(nextStakeDiff)+1, []byte{txscript.OP_TRUE}))
			spend := chaingen.MakeSpendableOutForTx(funding, 1, 0, 0)
			ticket := maketg(t, params).CreateTicketPurchaseTx(&spend, nextStakeDiff, 1)
			rec, err := udb.NewTxRecordFromMsgTx(ticket, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			funding.LockTime++
			funding.CachedHash = nil
			badSpend := chaingen.MakeSpendableOutForTx(funding, 1, 0, 0)
			badTicket := maketg(t, params).CreateTicketPurchaseTx(&badSpend, dcrutil.Amount(header.SBits), 1)
			badRec, err := udb.NewTxRecordFromMsgTx(badTicket, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			funding.LockTime++
			funding.CachedHash = nil
			obsoleteSpend := chaingen.MakeSpendableOutForTx(funding, 1, 0, 0)
			obsoleteTicket := maketg(t, params).CreateTicketPurchaseTx(&obsoleteSpend, dcrutil.Amount(header.SBits), 1)
			obsoleteRec, err := udb.NewTxRecordFromMsgTx(obsoleteTicket, time.Time{})
			if err != nil {
				t.Fatal(err)
			}
			const host = "https://vsp.example"
			pruned := make([]chainhash.Hash, 0, 1)
			w.SetVSPTicketPruner(func(hash chainhash.Hash) { pruned = append(pruned, hash) })
			err = walletdb.Update(ctx, w.db, func(dbtx walletdb.ReadWriteTx) error {
				for _, r := range []*udb.TxRecord{rec, badRec, obsoleteRec} {
					if err := w.txStore.InsertMemPoolTx(dbtx, r); err != nil {
						return err
					}
					if err := udb.SetVSPTicket(dbtx, &r.Hash, &udb.VSPTicket{Host: host, PubKey: make([]byte, 32)}); err != nil {
						return err
					}
				}
				return nil
			})
			if err != nil {
				t.Fatal(err)
			}

			hash := header.BlockHash()
			node := NewBlockNode(header, &hash, emptyFilter, nil)
			forest := new(SidechainForest)
			mustAddBlockNode(t, forest, node)
			if _, err := w.ChainSwitch(ctx, forest, []*BlockNode{node}); err != nil {
				t.Fatal(err)
			}

			assertRecord := func(record *udb.TxRecord, want bool) {
				t.Helper()
				err := walletdb.View(context.Background(), w.db, func(dbtx walletdb.ReadTx) error {
					_, err := udb.GetVSPTicket(dbtx, record.Hash)
					if want && err != nil {
						t.Fatalf("lost saved VSP record for %v: %v", record.Hash, err)
					}
					if !want && !errors.Is(err, errors.NotExist) {
						t.Fatalf("obsolete record retained: %v", err)
					}
					return nil
				})
				if err != nil {
					t.Fatal(err)
				}
			}
			for _, r := range []*udb.TxRecord{rec, badRec, obsoleteRec} {
				assertRecord(r, true)
			}
			if len(pruned) != 0 {
				t.Fatalf("jobs pruned before rescan: %v", pruned)
			}
			backend := pruningRescanNetwork{rescan: func(ctx context.Context, blocks []chainhash.Hash, save func(*chainhash.Hash, []*wire.MsgTx) error) error {
				if len(blocks) != 1 || blocks[0] != hash {
					t.Fatalf("rescan blocks = %v", blocks)
				}
				switch mode {
				case "failed":
					return context.DeadlineExceeded
				case "cancelled":
					cancel()
					return nil
				case "mined":
					return save(&hash, []*wire.MsgTx{badTicket})
				default:
					return nil
				}
			}}
			err = w.RescanFromHeight(ctx, backend, 23)
			if mode == "failed" || mode == "cancelled" {
				if err == nil {
					t.Fatal("incomplete scan succeeded")
				}
				for _, r := range []*udb.TxRecord{rec, badRec, obsoleteRec} {
					assertRecord(r, true)
				}
				if len(pruned) != 0 {
					t.Fatalf("incomplete scan pruned jobs: %v", pruned)
				}
				rp, err := w.RescanPoint(context.Background())
				if err != nil || rp == nil || *rp != hash {
					t.Fatalf("incomplete rescan point = %v, %v", rp, err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			assertRecord(rec, true)
			assertRecord(badRec, mode == "mined")
			assertRecord(obsoleteRec, false)
			wantPruned := 2
			if mode == "mined" {
				wantPruned = 1
			}
			if len(pruned) != wantPruned {
				t.Fatalf("pruned jobs = %v, want %d", pruned, wantPruned)
			}
			for _, h := range pruned {
				if h != obsoleteRec.Hash && !(mode == "empty" && h == badRec.Hash) {
					t.Fatalf("unexpected pruned ticket: %v", h)
				}
			}
			rp, err := w.RescanPoint(ctx)
			if err != nil || rp != nil {
				t.Fatalf("completed rescan point = %v, %v", rp, err)
			}
		})
	}
}

type pruningRescanNetwork struct {
	mockNetwork
	rescan func(context.Context, []chainhash.Hash, func(*chainhash.Hash, []*wire.MsgTx) error) error
}

func (n pruningRescanNetwork) Rescan(ctx context.Context, blocks []chainhash.Hash, save func(*chainhash.Hash, []*wire.MsgTx) error) error {
	return n.rescan(ctx, blocks, save)
}
