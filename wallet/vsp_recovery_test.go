package wallet

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

func TestUnminedVSPRecoveryUsesExistingProviderAndTicket(t *testing.T) {
	cfg := basicWalletConfig
	w, teardown := testWallet(t, &cfg)
	defer teardown()
	ctx := context.Background()
	address, err := w.NewExternalAddress(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	addr := address.(stdaddr.StakeAddress)
	host := "https://vsp.example"
	key := bytes.Repeat([]byte{1}, 32)
	var want []chainhash.Hash
	var existing chainhash.Hash
	for i, status := range []int{int(udb.VSPFeeProcessStarted), int(udb.VSPFeeProcessPaid), int(udb.VSPFeeProcessErrored), int(udb.VSPFeeProcessConfirmed), -1, -2, -3} {
		tx, err := makeTicket(cfg.Params, nil, &Input{
			OutPoint: wire.OutPoint{Hash: chainhash.Hash{byte(i + 1)}},
			PrevOut:  wire.TxOut{Value: 100},
		}, addr, addr, 100, nil)
		if err != nil {
			t.Fatal(err)
		}
		rec, err := udb.NewTxRecordFromMsgTx(tx, time.Now())
		if err != nil {
			t.Fatal(err)
		}
		err = walletdb.Update(ctx, w.db, func(dbtx walletdb.ReadWriteTx) error {
			if err := w.txStore.InsertMemPoolTx(dbtx, rec); err != nil {
				return err
			}
			if status == -1 {
				return nil
			}
			record := &udb.VSPTicket{FeeTxStatus: uint32(udb.VSPFeeProcessPaid), Host: host, PubKey: key}
			if status >= 0 {
				record.FeeTxStatus = uint32(status)
			}
			if status == -2 {
				record.Host = "https://other-vsp.example"
			}
			if status == -3 {
				record.PubKey = bytes.Repeat([]byte{2}, 32)
				record.Host = "https://rotated-vsp.example"
			}
			return udb.SetVSPTicket(dbtx, &rec.Hash, record)
		})
		if err != nil {
			t.Fatal(err)
		}
		if i < 3 {
			want = append(want, rec.Hash)
		}
		existing = rec.Hash
	}
	// A saved fee row alone is not a recoverable transaction.
	missing := chainhash.Hash{99}
	err = walletdb.Update(ctx, w.db, func(dbtx walletdb.ReadWriteTx) error {
		return udb.SetVSPTicket(dbtx, &missing, &udb.VSPTicket{FeeTxStatus: uint32(udb.VSPFeeProcessPaid), Host: host, PubKey: key})
	})
	if err != nil {
		t.Fatal(err)
	}
	got, err := w.UnminedVSPTickets(ctx, func(savedHost string, savedKey []byte) bool { return savedHost == host && bytes.Equal(savedKey, key) })
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for _, hash := range want {
		found := false
		for _, actual := range got {
			found = found || actual == hash
		}
		if !found {
			t.Fatalf("unmined registration %v was skipped", hash)
		}
	}
	got, err = w.UnminedVSPTickets(ctx, func(savedHost string, savedKey []byte) bool {
		return savedHost == host && bytes.Equal(savedKey, bytes.Repeat([]byte{3}, 32))
	})
	if err != nil || len(got) != 0 {
		t.Fatalf("changed key recovered tickets: %v, %v", got, err)
	}
	minedCallbacks := 0
	err = w.ForUnspentUnexpiredTickets(ctx, func(*chainhash.Hash) error { minedCallbacks++; return nil })
	if err != nil || minedCallbacks != 0 {
		t.Fatalf("unmanaged scan included unmined tickets: %d, %v", minedCallbacks, err)
	}
	fee := chainhash.Hash{88}
	if err := w.UpdateVspTicketFeeToPaid(ctx, &existing, &fee, host, key); err != nil {
		t.Fatal(err)
	}
	for _, update := range []func() error{
		func() error { return w.UpdateVspTicketFeeToPaid(ctx, &missing, &fee, host, key) },
		func() error { return w.UpdateVspTicketFeeToStarted(ctx, &missing, &fee, host, key) },
		func() error { return w.UpdateVspTicketFeeToErrored(ctx, &missing, host, key) },
		func() error { return w.UpdateVspTicketFeeToConfirmed(ctx, &missing, &fee, host, key) },
	} {
		if err := update(); !errors.Is(err, errors.NotExist) {
			t.Fatalf("missing ticket state update: %v", err)
		}
	}
}
