// Copyright (c) 2026 The ExchangeCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"context"
	"encoding/hex"
	"strings"
	"testing"

	blockchain "github.com/EXCCoin/exccd/blockchain/standalone/v2"
	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/errors"
)

func TestValidateHeaderChainDifficultiesRejectsInvalidEquihash(t *testing.T) {
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
	if !errors.Is(err, errors.Consensus) {
		t.Fatalf("error = %v, want consensus error", err)
	}
	if len(invalid) != 1 || *invalid[0].Hash != hash {
		t.Fatalf("invalid suffix = %v, want header %v", invalid, hash)
	}
}

func TestValidateHeaderChainDifficultiesRejectsEarlyMainNetSBits(t *testing.T) {
	t.Parallel()

	// Keep fast test PoW while exercising the mainnet deployment selector.
	params := chaincfg.SimNetParams()
	params.Net = wire.MainNet
	cfg := basicWalletConfig
	cfg.Params = params
	w, teardown := testWallet(t, &cfg)
	defer teardown()
	ctx := context.Background()
	if _, err := w.NextStakeDifficulty(ctx); err != nil {
		t.Fatalf("next stake difficulty at genesis: %v", err)
	}

	tg := maketg(t, params)
	block := tg.CreateBlockOne("bad-sbits", 0, func(b *wire.MsgBlock) {
		b.Header.Bits = params.GenesisBlock.Header.Bits
		b.Header.SBits = params.MinimumStakeDiff + 1
	})
	header := block.Header
	if err := blockchain.CheckProofOfWork(&header, header.Bits, params); err != nil {
		t.Fatalf("test header proof of work: %v", err)
	}

	hash := header.BlockHash()
	chain := []*BlockNode{{Header: &header, Hash: &hash}}
	invalid, err := w.ValidateHeaderChainDifficulties(ctx, chain, 0)
	if !errors.Is(err, errors.Consensus) ||
		!strings.Contains(err.Error(), "invalid PoS difficulty") {
		t.Fatalf("error = %v, want invalid PoS difficulty consensus error", err)
	}
	if len(invalid) != 1 || *invalid[0].Hash != hash {
		t.Fatalf("invalid suffix = %v, want header %v", invalid, hash)
	}
}

func TestHistoricalEquihashCompatibility(t *testing.T) {
	t.Parallel()

	const headerHex = "09000000aabad8ffdae7c37f33616e4dae133354718f73f7c55315fbc3f3d3a9533a0306fc3ea8340d7b6cb357585e83ef7367000eddc87aa6cb54fd470b04c083f37b2ac789abc97707099649fba6aa862c25d09c6109f60894b1282af8f4cdbb859a520100d76534d306810400000077c10000fa1b0c20c11afbfd00000000ec820f008c7900001f4a9364000000007f13d9a3ffb139cd00000000000000000000000000000000000000000000000009000000070165bccad80639c78af161f0e608738125c5f7b0295a53bf1d9436a68a1b5fa8997ece10c2c0f802a4a5a1450355cc6fe2195d3f279f4ae23e7dbd98616484db9accbd9e6ba75f7a8c6b7114fee3f6df69224a957252eb4c8dd5d7cf031169ddb96ed3"
	headerBytes, err := hex.DecodeString(headerHex)
	if err != nil {
		t.Fatal(err)
	}
	var header wire.BlockHeader
	if err := header.FromBytes(headerBytes); err != nil {
		t.Fatal(err)
	}
	const wantHash = "080dca3e1603a4ce3fdb6eb7e86fb460d95ec46df7132b80f8a5547ad877e732"
	if got := header.BlockHash().String(); got != wantHash {
		t.Fatalf("header hash = %s, want %s", got, wantHash)
	}
	params := chaincfg.MainNetParams()
	if err := blockchain.CheckProofOfWork(&header, header.Bits, params); err != nil {
		t.Fatalf("historical proof of work: %v", err)
	}

	header.EquihashSolution[0] ^= 1
	err = blockchain.ValidateEquihashSolution(&header, params)
	if !errors.Is(err, blockchain.ErrInvalidEquihashSolution) {
		t.Fatalf("mutated solution error = %v, want %v", err,
			blockchain.ErrInvalidEquihashSolution)
	}
}
