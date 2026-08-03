// Copyright (c) 2026 The ExchangeCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package deployments

import (
	"context"
	"testing"

	"github.com/EXCCoin/exccd/chaincfg/v3"
)

func TestDCP0010InactiveWithoutRPC(t *testing.T) {
	t.Parallel()

	for _, params := range []*chaincfg.Params{
		chaincfg.MainNetParams(),
		chaincfg.TestNet3Params(),
		chaincfg.SimNetParams(),
		chaincfg.RegNetParams(),
	} {
		active, err := DCP0010Active(context.Background(), 1<<30, params, nil)
		if err != nil {
			t.Fatalf("%s: unexpected error: %v", params.Name, err)
		}
		if active {
			t.Fatalf("%s: DCP0010 unexpectedly active", params.Name)
		}
	}
}
