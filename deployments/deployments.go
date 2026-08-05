// Copyright (c) 2018 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package deployments

import (
	"context"

	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccd/wire"
)

// HardcodedDeployment specifies hardcoded block heights that a deployment
// activates at.  If the value is negative, the deployment is either inactive or
// can't be determined due to the uniqueness properties of the network.
//
// Since these are hardcoded deployments, and cannot support every possible
// network, conditional logic should only be applied when a deployment is
// active, not when it is inactive.
type HardcodedDeployment struct {
	MainNetActivationHeight  int32
	TestNet2ActivationHeight int32
	TestNet3ActivationHeight int32
	SimNetActivationHeight   int32
}

// DCP0001 specifies hard forking changes to the stake difficulty algorithm as
// defined by https://github.com/decred/dcps/blob/master/dcp-0001/dcp-0001.mediawiki.
var DCP0001 = HardcodedDeployment{
	// EXCC launched with DCP0001 as base consensus.
	MainNetActivationHeight:  0,
	TestNet2ActivationHeight: 46128,
	TestNet3ActivationHeight: 0,
	SimNetActivationHeight:   0,
}

// DCP0002 specifies the activation of the OP_SHA256 hard fork as defined by
// https://github.com/decred/dcps/blob/master/dcp-0002/dcp-0002.mediawiki.
var DCP0002 = HardcodedDeployment{
	MainNetActivationHeight:  189568,
	TestNet2ActivationHeight: 151968,
	TestNet3ActivationHeight: 0,
	SimNetActivationHeight:   0,
}

// DCP0003 specifies the activation of a CSV soft fork as defined by
// https://github.com/decred/dcps/blob/master/dcp-0003/dcp-0003.mediawiki.
var DCP0003 = HardcodedDeployment{
	MainNetActivationHeight:  189568,
	TestNet2ActivationHeight: 151968,
	TestNet3ActivationHeight: 0,
	SimNetActivationHeight:   0,
}

// Active returns whether the hardcoded deployment is active at height on the
// network specified by params.  Active always returns false for unrecognized
// networks.
func (d *HardcodedDeployment) Active(height int32, net wire.CurrencyNet) bool {
	var activationHeight int32 = -1
	switch net {
	case wire.MainNet:
		activationHeight = d.MainNetActivationHeight
	case 0x48e7a065: // testnet2
		activationHeight = d.TestNet2ActivationHeight
	case wire.TestNet3:
		activationHeight = d.TestNet3ActivationHeight
	case wire.SimNet:
		activationHeight = d.SimNetActivationHeight
	}
	return activationHeight >= 0 && height >= activationHeight
}

// DCP0010Active returns whether EXCC uses the DCP0010 subsidy split.  exccd
// never activates this inherited Decred agenda on any EXCC network, so the
// answer is backend-independent and must remain the same for RPC and SPV.
func DCP0010Active(context.Context, int32, *chaincfg.Params, interface{}) (bool, error) {
	return false, nil
}
