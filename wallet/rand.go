// Copyright (c) 2019-2021 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"crypto/rand"
	"sync"

	"github.com/EXCCoin/exccwallet/v2/internal/uniformprng"
	"github.com/EXCCoin/exccwallet/v2/wallet/txauthor"
)

var prng *uniformprng.Source
var prngMu sync.Mutex

func init() {
	var err error
	prng, err = uniformprng.RandSource(rand.Reader)
	if err != nil {
		panic(err)
	}
}

func randInt63n(n int64) int64 {
	defer prngMu.Unlock()
	prngMu.Lock()
	return prng.Int63n(n)
}

func shuffle(n int, swap func(i, j int)) {
	if n < 0 {
		panic("shuffle: negative n")
	}
	if int64(n) >= 1<<32 {
		panic("shuffle: large n")
	}

	defer prngMu.Unlock()
	prngMu.Lock()

	// Fisher-Yates shuffle: https://en.wikipedia.org/wiki/Fisher%E2%80%93Yates_shuffle
	for i := uint32(0); i < uint32(n); i++ {
		j := prng.Uint32n(uint32(n)-i) + i
		swap(int(i), int(j))
	}
}

func shuffleUTXOs(u *txauthor.InputDetail) {
	shuffle(len(u.Inputs), func(i, j int) {
		u.Inputs[i], u.Inputs[j] = u.Inputs[j], u.Inputs[i]
		u.Scripts[i], u.Scripts[j] = u.Scripts[j], u.Scripts[i]
		u.RedeemScriptSizes[i], u.RedeemScriptSizes[j] = u.RedeemScriptSizes[j], u.RedeemScriptSizes[i]
	})
}
