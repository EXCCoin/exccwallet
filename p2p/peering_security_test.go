package p2p

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/lru"
	"github.com/decred/go-socks/socks"
)

type addrConn struct {
	local, remote net.Addr
}

func (c *addrConn) Read([]byte) (int, error)         { return 0, io.EOF }
func (c *addrConn) Write(p []byte) (int, error)      { return len(p), nil }
func (c *addrConn) Close() error                     { return nil }
func (c *addrConn) LocalAddr() net.Addr              { return c.local }
func (c *addrConn) RemoteAddr() net.Addr             { return c.remote }
func (c *addrConn) SetDeadline(time.Time) error      { return nil }
func (c *addrConn) SetReadDeadline(time.Time) error  { return nil }
func (c *addrConn) SetWriteDeadline(time.Time) error { return nil }

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

func TestNewNetAddressPrivacy(t *testing.T) {
	tests := []struct {
		name     string
		addr     net.Addr
		wantIP   string
		wantPort uint16
	}{
		{"public", &net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 9108}, "8.8.8.8", 9108},
		{"proxied public", &socks.ProxiedAddr{Net: "tcp", Host: "1.1.1.1", Port: 19108}, "1.1.1.1", 19108},
		{"loopback", &net.TCPAddr{IP: net.ParseIP("127.0.0.1"), Port: 9108}, "", 0},
		{"private", &net.TCPAddr{IP: net.ParseIP("10.0.0.2"), Port: 9108}, "", 0},
		{"shared", &net.TCPAddr{IP: net.ParseIP("100.64.0.1"), Port: 9108}, "", 0},
		{"unspecified", &net.TCPAddr{IP: net.IPv4zero, Port: 9108}, "", 0},
		{"proxied hostname", &socks.ProxiedAddr{Net: "tcp", Host: "peer.invalid", Port: 9108}, "", 0},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			na, err := newNetAddress(test.addr, wire.SFNodeNetwork)
			if err != nil {
				t.Fatal(err)
			}
			gotIP := ""
			if len(na.IP) != 0 {
				gotIP = na.IP.String()
			}
			if gotIP != test.wantIP || na.Port != test.wantPort {
				t.Fatalf("got %q:%d, want %q:%d", gotIP, na.Port,
					test.wantIP, test.wantPort)
			}
		})
	}
}

func TestVersionMessageDoesNotAdvertiseLocalAddress(t *testing.T) {
	lp := new(LocalPeer)
	c := &addrConn{
		local:  &net.TCPAddr{IP: net.ParseIP("10.0.0.2"), Port: 49152},
		remote: &net.TCPAddr{IP: net.ParseIP("8.8.8.8"), Port: 9108},
	}
	msg, err := lp.newMsgVersion(Pver, c)
	if err != nil {
		t.Fatal(err)
	}
	if len(msg.AddrMe.IP) != 0 || msg.AddrMe.Port != 0 {
		t.Fatalf("local address disclosed as %v:%d", msg.AddrMe.IP, msg.AddrMe.Port)
	}
	if !msg.AddrYou.IP.Equal(net.ParseIP("8.8.8.8")) || msg.AddrYou.Port != 9108 {
		t.Fatalf("wrong remote address %v:%d", msg.AddrYou.IP, msg.AddrYou.Port)
	}
}

func TestLocalPeerDialFunc(t *testing.T) {
	lp := NewLocalPeer(nil, nil, nil)
	if lp.dial == nil {
		t.Fatal("default dial function is nil")
	}

	wantErr := errors.New("dial called")
	lp.SetDialFunc(func(context.Context, string, string) (net.Conn, error) {
		return nil, wantErr
	})
	_, err := lp.dial(context.Background(), "tcp", "example.invalid:9108")
	if !errors.Is(err, wantErr) {
		t.Fatalf("custom dial function was not used: %v", err)
	}
}
