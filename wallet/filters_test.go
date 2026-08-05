// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"context"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

type filterRecordingNetwork struct {
	mockNetwork
	reloaded  bool
	outpoints map[wire.OutPoint]struct{}
}

func (n *filterRecordingNetwork) LoadTxFilter(_ context.Context, reload bool,
	_ []stdaddr.Address, outpoints []wire.OutPoint) error {

	n.reloaded = n.reloaded || reload
	for _, outpoint := range outpoints {
		n.outpoints[outpoint] = struct{}{}
	}
	return nil
}

type contextRecordingNetwork struct {
	mockNetwork
	loadCalls       int
	canceledContext bool
}

func (n *contextRecordingNetwork) LoadTxFilter(ctx context.Context, _ bool,
	_ []stdaddr.Address, _ []wire.OutPoint) error {

	n.loadCalls++
	n.canceledContext = n.canceledContext || ctx.Err() != nil
	return nil
}

func TestPurchaseTicketsWatchesAfterRequestCancellation(t *testing.T) {
	w, teardown := testWallet(t, &basicWalletConfig)
	defer teardown()

	ctx, cancel := context.WithCancel(context.Background())
	n := new(contextRecordingNetwork)
	req := &PurchaseTicketsRequest{
		VSPFeeProcess: func(context.Context) (float64, error) {
			cancel()
			return 0, context.Canceled
		},
		VSPFeePaymentProcess: func(context.Context, *chainhash.Hash, *wire.MsgTx) error {
			return nil
		},
	}
	_, err := w.purchaseTickets(ctx, "test", n, req)
	if err == nil {
		t.Fatal("purchaseTickets succeeded after request cancellation")
	}
	if n.loadCalls == 0 {
		t.Fatal("transaction filters were not updated after request cancellation")
	}
	if n.canceledContext {
		t.Fatal("transaction filters were updated with the canceled request context")
	}
}

func TestLoadActiveDataFiltersWatchesUnminedInputs(t *testing.T) {
	w, teardown := testWallet(t, &basicWalletConfig)
	defer teardown()

	prevOut := wire.OutPoint{
		Hash:  chainhash.Hash{1},
		Index: 2,
		Tree:  wire.TxTreeRegular,
	}
	tx := wire.NewMsgTx()
	tx.AddTxIn(wire.NewTxIn(&prevOut, 1, nil))
	tx.AddTxOut(wire.NewTxOut(1, nil))
	rec, err := udb.NewTxRecordFromMsgTx(tx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	err = walletdb.Update(context.Background(), w.db, func(dbtx walletdb.ReadWriteTx) error {
		return w.txStore.InsertMemPoolTx(dbtx, rec)
	})
	if err != nil {
		t.Fatal(err)
	}

	n := &filterRecordingNetwork{outpoints: make(map[wire.OutPoint]struct{})}
	err = w.LoadActiveDataFilters(context.Background(), n, true)
	if err != nil {
		t.Fatal(err)
	}
	if !n.reloaded {
		t.Fatal("transaction filter was not reloaded")
	}
	if _, ok := n.outpoints[prevOut]; !ok {
		t.Fatalf("unmined input %v was not watched", prevOut)
	}
}
