// Copyright (c) 2018 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"context"
	"os"
	"testing"

	_ "github.com/EXCCoin/exccwallet/v2/wallet/drivers/bdb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
	"github.com/EXCCoin/exccwallet/v2/walletseed"
	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccd/dcrutil/v4"
)

var basicWalletConfig = Config{
	PubPassphrase: []byte(InsecurePubPassphrase),
	GapLimit:      20,
	RelayFee:      dcrutil.Amount(1e5),
	Params:        chaincfg.SimNetParams(),
}

func testWallet(t *testing.T, cfg *Config) (w *Wallet, teardown func()) {
	ctx := context.Background()
	f, err := os.CreateTemp("", "exccwallet.testdb")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	db, err := walletdb.Create("bdb", f.Name())
	if err != nil {
		t.Fatal(err)
	}
	rm := func() {
		db.Close()
		os.Remove(f.Name())
	}
	mnemonic := "piano just arch aim summer bar space hip horse captain sudden glad" +
		" review mushroom salt gather lemon limit humble raccoon copper core vacuum very"
	seed, err := walletseed.DecodeUserInput(mnemonic, "")
	if err != nil {
		t.Fatal(err)
	}
	err = Create(ctx, opaqueDB{db}, []byte(InsecurePubPassphrase), []byte("private"), seed, cfg.Params)
	if err != nil {
		rm()
		t.Fatal(err)
	}
	cfg.DB = opaqueDB{db}
	w, err = Open(ctx, cfg)
	if err != nil {
		rm()
		t.Fatal(err)
	}
	teardown = func() {
		rm()
	}
	return
}
