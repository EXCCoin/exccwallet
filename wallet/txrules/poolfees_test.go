package txrules_test

import (
	"testing"

	. "github.com/EXCCoin/exccwallet/v2/wallet/txrules"
	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccd/dcrutil/v4"
)

func TestStakePoolTicketFee(t *testing.T) {
	params := chaincfg.MainNetParams()
	tests := []struct {
		StakeDiff dcrutil.Amount
		Fee       dcrutil.Amount
		Height    int32
		PoolFee   float64
		Expected  dcrutil.Amount
	}{
		0: {10 * 1e8, 0.01 * 1e8, 25000, 1.00, 0.01796861 * 1e8},
		1: {20 * 1e8, 0.01 * 1e8, 25000, 1.00, 0.01973051 * 1e8},
		2: {5 * 1e8, 0.05 * 1e8, 50000, 2.59, 0.03809691 * 1e8},
		3: {15 * 1e8, 0.05 * 1e8, 50000, 2.59, 0.04696521 * 1e8},
	}
	for i, test := range tests {
		poolFeeAmt := StakePoolTicketFee(test.StakeDiff, test.Fee, test.Height,
			test.PoolFee, params, false)
		if poolFeeAmt != test.Expected {
			t.Errorf("Test %d: Got %v: Want %v", i, poolFeeAmt, test.Expected)
		}
	}
}
