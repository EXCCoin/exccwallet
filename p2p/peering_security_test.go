package p2p

import (
	"context"
	"net"
	"strings"
	"testing"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/lru"
)

func TestReceivedHeadersRejectsInvalidResponses(t *testing.T) {
	locator := &chainhash.Hash{1}
	tests := []struct {
		name       string
		initHeight int32
		requested  bool
		locators   []*chainhash.Hash
		header     *wire.BlockHeader
		reason     string
	}{
		{"drip feed", 100, true, nil, &wire.BlockHeader{Height: 1}, "too few headers"},
		{"unrequested", 0, false, nil, &wire.BlockHeader{Height: 1}, "unrequested headers"},
		{"wrong locator", 1, true, []*chainhash.Hash{locator}, &wire.BlockHeader{Height: 1}, "block locators"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			local, remote := net.Pipe()
			defer remote.Close()

			rp := &RemotePeer{
				initHeight:          test.initHeight,
				raddr:               local.RemoteAddr(),
				c:                   local,
				requestedHeadersLoc: test.locators,
				knownHeaders:        lru.NewCache(invLRUSize),
				errc:                make(chan struct{}),
			}
			if test.requested {
				rp.requestedHeaders = make(chan *wire.MsgHeaders, 1)
			}
			rp.receivedHeaders(context.Background(), &wire.MsgHeaders{
				Headers: []*wire.BlockHeader{test.header},
			})

			if err := rp.Err(); err == nil || !strings.Contains(err.Error(), test.reason) {
				t.Fatalf("unexpected disconnect reason: %v", err)
			}
		})
	}
}
