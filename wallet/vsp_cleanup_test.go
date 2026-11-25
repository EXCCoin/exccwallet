package wallet

import (
	"context"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
	"testing"
	"time"
)

func TestPrunedVSPTicketRetainsOutstandingFeeAssociation(t *testing.T) {
	ctx := context.Background()
	w, teardown := testWallet(t, &basicWalletConfig)
	defer teardown()
	ticket := chainhash.Hash{1}
	fee := wire.NewMsgTx()
	fee.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{2}}, 1000, nil))
	fee.AddTxOut(wire.NewTxOut(900, []byte{0x51}))
	rec, err := udb.NewTxRecordFromMsgTx(fee, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	var pruned []chainhash.Hash
	w.SetVSPTicketPruner(func(hash chainhash.Hash) { pruned = append(pruned, hash) })
	err = walletdb.Update(ctx, w.db, func(tx walletdb.ReadWriteTx) error {
		if err := w.txStore.InsertMemPoolTx(tx, rec); err != nil {
			return err
		}
		if err := udb.SetVSPTicket(tx, &ticket, &udb.VSPTicket{FeeHash: rec.Hash, Host: "https://vsp.example", PubKey: make([]byte, 32)}); err != nil {
			return err
		}
		return w.pruneVSPTickets(tx, []*chainhash.Hash{&ticket})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = walletdb.View(ctx, w.db, func(tx walletdb.ReadTx) error {
		info, err := udb.GetVSPTicket(tx, ticket)
		if err != nil {
			return err
		}
		if info.FeeHash != rec.Hash {
			t.Fatal("outstanding fee association lost")
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(pruned) != 1 || pruned[0] != ticket {
		t.Fatalf("pruned jobs: %v", pruned)
	}
	err = walletdb.Update(ctx, w.db, func(tx walletdb.ReadWriteTx) error {
		if err := w.txStore.RemoveUnconfirmed(tx.ReadWriteBucket(wtxmgrNamespaceKey), fee, &rec.Hash); err != nil {
			return err
		}
		return w.pruneVSPTickets(tx, []*chainhash.Hash{&ticket})
	})
	if err != nil {
		t.Fatal(err)
	}
	err = walletdb.View(ctx, w.db, func(tx walletdb.ReadTx) error {
		if _, err := udb.GetVSPTicket(tx, ticket); !errors.Is(err, errors.NotExist) {
			t.Fatalf("settled association remains: %v", err)
		}
		if _, err := udb.GetVSPPubKey(tx, []byte("https://vsp.example")); err != nil {
			t.Fatalf("provider identity removed: %v", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}
