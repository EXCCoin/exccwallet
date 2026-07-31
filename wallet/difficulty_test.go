// Copyright (c) 2026 The ExchangeCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"context"
	"testing"

	blockchain "github.com/EXCCoin/exccd/blockchain/standalone/v2"
)

func TestValidateHeaderChainDifficultiesPreAgendaEquihash(t *testing.T) {
	t.Parallel()

	cfg := basicWalletConfig
	w, teardown := testWallet(t, &cfg)
	defer teardown()

	tg := maketg(t, cfg.Params)
	blockOne := tg.createBlockOne("block-one")
	header := blockOne.MsgBlock.Header
	header.Bits = cfg.Params.GenesisBlock.Header.Bits
	header.EquihashSolution[0] ^= 1
	for blockchain.CheckProofOfWorkHash(&header, header.Bits, cfg.Params) != nil {
		header.Nonce++
	}
	if err := blockchain.ValidateEquihashSolution(&header, cfg.Params); err == nil {
		t.Fatal("test header unexpectedly has a valid Equihash solution")
	}

	hash := header.BlockHash()
	chain := []*BlockNode{{Header: &header, Hash: &hash}}
	invalid, err := w.ValidateHeaderChainDifficulties(context.Background(), chain, 0)
	if err != nil {
		t.Fatalf("pre-agenda header rejected: %v", err)
	}
	if len(invalid) != 0 {
		t.Fatalf("pre-agenda header reported invalid: %v", invalid[0].Hash)
	}
}
