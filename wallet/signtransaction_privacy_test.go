package wallet

import (
	"context"
	"strings"
	"testing"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/dcrutil/v4"
	"github.com/EXCCoin/exccd/txscript/v4"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/wire"
)

func TestSignTransactionMissingKeyPrivacy(t *testing.T) {
	cfg := basicWalletConfig
	w, closeWallet := testWallet(t, &cfg)
	defer closeWallet()

	// A public script is sufficient to exercise signing's missing-key path.
	pubKeyHash := [20]byte{1}
	addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(pubKeyHash[:], cfg.Params)
	if err != nil {
		t.Fatal(err)
	}
	_, script := addr.PaymentScript()
	prev := wire.OutPoint{Hash: chainhash.Hash{1}, Tree: wire.TxTreeRegular}
	tx := wire.NewMsgTx()
	tx.AddTxIn(wire.NewTxIn(&prev, 100, nil))
	tx.AddTxOut(wire.NewTxOut(90, script))

	const sentinel = "private-map-entry-sentinel"
	sigErrors, err := w.SignTransaction(context.Background(), tx, txscript.SigHashAll,
		map[wire.OutPoint][]byte{prev: script}, map[string]*dcrutil.WIF{sentinel: nil}, nil)
	if err != nil || len(sigErrors) != 1 || sigErrors[0].InputIndex != 0 {
		t.Fatal("expected a signing error for input zero")
	}
	message := sigErrors[0].Error.Error()
	if !strings.Contains(message, addr.String()) || strings.Contains(message, sentinel) {
		t.Fatal("missing-key error must identify the requested public address and omit caller key entries")
	}
}
