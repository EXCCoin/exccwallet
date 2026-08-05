// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package rpcserver

import (
	"context"
	"errors"
	"net"
	"runtime"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccwallet/v2/internal/loader"
	"github.com/EXCCoin/exccwallet/v2/internal/netparams"
	pb "github.com/EXCCoin/exccwallet/v2/rpc/walletrpc"
	"github.com/EXCCoin/exccwallet/v2/wallet"
)

func testConcurrentServiceStart(t *testing.T, start func(int), matches func(int) bool) {
	t.Helper()
	type result struct {
		index    int
		panicked bool
	}
	gate := make(chan struct{})
	results := make(chan result, 2)
	for i := 0; i < 2; i++ {
		go func(i int) {
			r := result{index: i}
			defer func() {
				r.panicked = recover() != nil
				results <- r
			}()
			<-gate
			start(i)
		}(i)
	}
	close(gate)

	var winner int
	successes := 0
	panics := 0
	for i := 0; i < 2; i++ {
		r := <-results
		if r.panicked {
			panics++
		} else {
			successes++
			winner = r.index
		}
	}
	if successes != 1 || panics != 1 {
		t.Fatalf("service starts: successes=%d panics=%d, want 1 each",
			successes, panics)
	}
	if !matches(winner) {
		t.Fatalf("service dependencies do not match successful start %d", winner)
	}
}

func TestStartServicesSynchronizeDependencies(t *testing.T) {
	walletService = walletServer{}
	loaderService = loaderServer{}
	accountMixerService = accountMixerServer{}
	ticketBuyerV2Service = ticketbuyerV2Server{}
	agendaService = agendaServer{}
	votingService = votingServer{}
	networkService = networkServer{}

	w := new(wallet.Wallet)
	dial := dialFunc(func(context.Context, string, string) (net.Conn, error) {
		return nil, nil
	})
	type walletDependencies struct {
		wallet *wallet.Wallet
		dial   dialFunc
	}
	dependencies := make(chan walletDependencies, 1)
	go func() {
		for !walletService.checkReady() {
			runtime.Gosched()
		}
		dependencies <- walletDependencies{
			wallet: walletService.wallet,
			dial:   walletService.dialCSPPServer,
		}
	}()
	StartWalletService(nil, w, dial)
	got := <-dependencies
	if got.wallet != w || got.dial == nil {
		t.Fatal("wallet service became ready before dependencies were published")
	}

	walletService = walletServer{}
	wallets := [2]*wallet.Wallet{new(wallet.Wallet), new(wallet.Wallet)}
	dials := [2]dialFunc{
		func(context.Context, string, string) (net.Conn, error) { return nil, nil },
		func(context.Context, string, string) (net.Conn, error) { return nil, nil },
	}
	t.Run("wallet", func(t *testing.T) {
		testConcurrentServiceStart(t, func(i int) {
			StartWalletService(nil, wallets[i], dials[i])
		}, func(i int) bool {
			return walletService.wallet == wallets[i] &&
				walletService.dialCSPPServer != nil
		})
	})

	loaders := [2]*loader.Loader{new(loader.Loader), new(loader.Loader)}
	activeNets := [2]*netparams.Params{new(netparams.Params), new(netparams.Params)}
	dialErrs := [2]error{errors.New("dial 0"), errors.New("dial 1")}
	loaderDials := [2]dialFunc{
		func(context.Context, string, string) (net.Conn, error) {
			return nil, dialErrs[0]
		},
		func(context.Context, string, string) (net.Conn, error) {
			return nil, dialErrs[1]
		},
	}
	lookupIPs := [2]net.IP{net.IPv4(192, 0, 2, 1), net.IPv4(192, 0, 2, 2)}
	lookups := [2]func(string) ([]net.IP, error){
		func(string) ([]net.IP, error) { return []net.IP{lookupIPs[0]}, nil },
		func(string) ([]net.IP, error) { return []net.IP{lookupIPs[1]}, nil },
	}
	t.Run("loader", func(t *testing.T) {
		testConcurrentServiceStart(t, func(i int) {
			StartWalletLoaderService(nil, loaders[i], activeNets[i], loaderDials[i], lookups[i])
		}, func(i int) bool {
			_, dialErr := loaderService.dial(context.Background(), "tcp", "unused")
			ips, lookupErr := loaderService.lookup("unused")
			return loaderService.loader == loaders[i] &&
				loaderService.activeNet == activeNets[i] &&
				errors.Is(dialErr, dialErrs[i]) && lookupErr == nil &&
				len(ips) == 1 && ips[0].Equal(lookupIPs[i])
		})
	})

	t.Run("account mixer", func(t *testing.T) {
		testConcurrentServiceStart(t, func(i int) {
			StartAccountMixerService(nil, loaders[i])
		}, func(i int) bool {
			return accountMixerService.loader == loaders[i]
		})
	})

	t.Run("ticket buyer", func(t *testing.T) {
		testConcurrentServiceStart(t, func(i int) {
			StartTicketBuyerV2Service(nil, loaders[i])
		}, func(i int) bool {
			return ticketBuyerV2Service.loader == loaders[i]
		})
	})

	chainParams := [2]*chaincfg.Params{new(chaincfg.Params), new(chaincfg.Params)}
	t.Run("agenda", func(t *testing.T) {
		testConcurrentServiceStart(t, func(i int) {
			StartAgendaService(nil, chainParams[i])
		}, func(i int) bool {
			return agendaService.activeNet == chainParams[i]
		})
	})

	t.Run("voting", func(t *testing.T) {
		testConcurrentServiceStart(t, func(i int) {
			StartVotingService(nil, wallets[i])
		}, func(i int) bool {
			return votingService.wallet == wallets[i]
		})
	})

	t.Run("network", func(t *testing.T) {
		testConcurrentServiceStart(t, func(i int) {
			StartNetworkService(nil, wallets[i])
		}, func(i int) bool {
			return networkService.wallet == wallets[i]
		})
	})
}

func TestPurchaseTicketsClearsPassphraseOnValidationError(t *testing.T) {
	req := &pb.PurchaseTicketsRequest{
		Passphrase: []byte("private"),
		SpendLimit: -1,
	}
	_, err := new(walletServer).PurchaseTickets(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, b := range req.Passphrase {
		if b != 0 {
			t.Fatal("passphrase was not cleared")
		}
	}
}
