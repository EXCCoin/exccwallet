// Copyright (c) 2018 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"encoding/hex"
	"math"
	"testing"

	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccd/chaincfg/v3"
)

func TestCoinbaseMatured(t *testing.T) {
	t.Parallel()
	params := chaincfg.MainNetParams()
	maturity := int32(params.CoinbaseMaturity)
	tests := []struct {
		txHeight, tipHeight int32
		matured             bool
	}{
		{0, 0, false},
		{0, maturity - 1, false},
		{0, maturity, true},
		{0, maturity + 1, true},
		{1, 0, false},
		{maturity, 0, false},
		{0, -1, false},
		{-1, maturity, false},
		{-1, math.MaxInt32, false},
	}

	for i, test := range tests {
		result := coinbaseMatured(params, test.txHeight, test.tipHeight)
		if result != test.matured {
			t.Errorf("test %d: result (%v) != expected (%v)", i, result, test.matured)
		}
	}
}

func TestTicketMatured(t *testing.T) {
	t.Parallel()
	params := chaincfg.MainNetParams()
	maturity := int32(params.TicketMaturity)
	tests := []struct {
		txHeight, tipHeight int32
		matured             bool
	}{
		{0, 0, false},
		{0, maturity - 1, false},
		{0, maturity, false}, // dcrd off-by-one results in this being false
		{0, maturity + 1, true},
		{1, 0, false},
		{maturity, 0, false},
		{0, -1, false},
		{-1, maturity, false},
		{-1, math.MaxInt32, false},
	}

	for i, test := range tests {
		result := ticketMatured(params, test.txHeight, test.tipHeight)
		if result != test.matured {
			t.Errorf("test %d: result (%v) != expected (%v)", i, result, test.matured)
		}
	}
}

func TestTicketExpired(t *testing.T) {
	t.Parallel()
	params := chaincfg.MainNetParams()
	expiry := int32(params.TicketMaturity) + int32(params.TicketExpiry)
	tests := []struct {
		txHeight, tipHeight int32
		expired             bool
	}{
		{0, 0, false},
		{0, expiry - 1, false},
		{0, expiry, false}, // dcrd off-by-one results in this being false
		{0, expiry + 1, true},
		{1, 0, false},
		{expiry, 0, false},
		{0, -1, false},
		{-1, expiry, false},
		{-1, math.MaxInt32, false},
	}

	for i, test := range tests {
		result := ticketExpired(params, test.txHeight, test.tipHeight)
		if result != test.expired {
			t.Errorf("test %d: result (%v) != expected (%v)", i, result, test.expired)
		}
	}
}

// TestVotingXprivFromSeed tests that creating voting xprivs works properly.
func TestVotingXprivFromSeed(t *testing.T) {
	seed, err := hex.DecodeString("0000000000000000000000000000000000000" +
		"000000000000000000000000000")
	if err != nil {
		t.Fatalf("unable to create seed: %v", err)
	}
	wantXpriv := "xprv9xgs8f1C9ZQcxMbbAqQFLMZdVzY5SuSTibUeKYGB4AjSJ6ZrDn7mgHAnveMU8gsKmCCJzhbZsizw9unBYV9VTDzXvvVENQwDjN7V2JSYR5K"

	tests := []struct {
		name, want string
		seed       []byte
		wantErr    *errors.Error
	}{{
		name: "ok",
		seed: seed,
		want: wantXpriv,
	}, {
		name:    "bad seed",
		seed:    seed[:1],
		wantErr: &errors.Error{Kind: errors.Invalid},
	}}

	for _, test := range tests {
		got, err := votingXprivFromSeed(test.seed, chaincfg.MainNetParams())
		if test.wantErr != nil {
			if err == nil {
				t.Fatalf("wanted error %v but got none", test.wantErr)
			}
			kind := err.(*errors.Error).Kind
			if !test.wantErr.Is(kind) {
				t.Fatalf("wanted error %v but got %v", test.wantErr, kind)
			}
			continue
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got.String() != test.want {
			t.Fatalf("wanted xpriv %v but got %v", test.want, got)
		}
	}
}
