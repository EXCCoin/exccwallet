package deployments

import (
	"context"
	"testing"

	"github.com/EXCCoin/exccd/chaincfg/v3"
)

func TestDCP0010InactiveOnSimNet(t *testing.T) {
	active, err := DCP0010Active(context.Background(), 0,
		chaincfg.SimNetParams(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if active {
		t.Fatal("DCP0010 unexpectedly active on EXCC simnet")
	}
}
