// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package rpcserver

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccwallet/v2/internal/loader"
	pb "github.com/EXCCoin/exccwallet/v2/rpc/walletrpc"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func newTicketBuyerTestServer(t *testing.T) *ticketbuyerV2Server {
	t.Helper()
	l := loader.NewLoader(chaincfg.SimNetParams(), t.TempDir(), &loader.StakeOptions{}, 20, false, 1e4, 1, false, false, 0)
	_, err := l.CreateNewWallet(context.Background(), []byte(wallet.InsecurePubPassphrase), []byte("test-private"), bytes.Repeat([]byte{1}, 32))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, loaded := l.LoadedWallet(); loaded {
			if err := l.UnloadWallet(); err != nil {
				t.Error(err)
			}
		}
	})
	return &ticketbuyerV2Server{loader: l}
}

func TestStopTicketBuyerWaitsForTeardownAndRejectsOverlap(t *testing.T) {
	s := newTicketBuyerTestServer(t)
	run, err := s.beginTicketBuyer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.finishTicketBuyer(run)
	if _, err := s.beginTicketBuyer(context.Background()); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("overlapping start accepted: %v", err)
	}
	stopped := make(chan error, 1)
	go func() {
		_, err := s.StopTicketBuyer(context.Background(), &pb.StopTicketBuyerRequest{RunId: run.id})
		stopped <- err
	}()
	select {
	case <-run.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Stop did not cancel the buyer")
	}
	select {
	case err := <-stopped:
		t.Fatalf("Stop acknowledged before teardown: %v", err)
	default:
	}
	if _, err := s.beginTicketBuyer(context.Background()); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("start accepted while old buyer was stopping: %v", err)
	}
	s.finishTicketBuyer(run)
	select {
	case err := <-stopped:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Stop did not acknowledge completed teardown")
	}
	if _, err := s.StopTicketBuyer(context.Background(), &pb.StopTicketBuyerRequest{RunId: run.id}); err != nil {
		t.Fatalf("completed Stop is not idempotent: %v", err)
	}
	next, err := s.beginTicketBuyer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.finishTicketBuyer(next)
	if next.id <= run.id {
		t.Fatal("run IDs were reused")
	}
	if _, err := s.StopTicketBuyer(context.Background(), &pb.StopTicketBuyerRequest{RunId: run.id}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("stale Stop did not identify the changed run: %v", err)
	}
	if next.ctx.Err() != nil {
		t.Fatal("stale Stop canceled the new run")
	}
}

func TestStopTicketBuyerDeadlineDoesNotReleaseActiveRun(t *testing.T) {
	s := newTicketBuyerTestServer(t)
	run, err := s.beginTicketBuyer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.finishTicketBuyer(run)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if _, err := s.StopTicketBuyer(ctx, &pb.StopTicketBuyerRequest{RunId: run.id}); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("Stop deadline changed: %v", err)
	}
	if _, err := s.beginTicketBuyer(context.Background()); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("Stop deadline released a still-running buyer: %v", err)
	}
	for _, test := range []struct {
		id   uint64
		code codes.Code
	}{{0, codes.InvalidArgument}, {run.id + 1, codes.FailedPrecondition}} {
		if _, err := s.StopTicketBuyer(context.Background(), &pb.StopTicketBuyerRequest{RunId: test.id}); status.Code(err) != test.code {
			t.Fatalf("Stop(%d) code %v, want %v", test.id, status.Code(err), test.code)
		}
	}
	s.finishTicketBuyer(run)
	if _, err := s.StopTicketBuyer(context.Background(), &pb.StopTicketBuyerRequest{RunId: run.id + 1}); status.Code(err) != codes.NotFound {
		t.Fatalf("unknown run acknowledged: %v", err)
	}
}

type idleTicketBuyerStream struct {
	grpc.ServerStream
	started chan uint64
}

func (s *idleTicketBuyerStream) Context() context.Context { return context.Background() }

func (s *idleTicketBuyerStream) Send(response *pb.RunTicketBuyerResponse) error {
	s.started <- response.RunId
	return nil
}

func TestCloseWalletStopsIdleTicketBuyerAndAllowsReopen(t *testing.T) {
	s := newTicketBuyerTestServer(t)
	stream := &idleTicketBuyerStream{started: make(chan uint64, 1)}
	finished := make(chan error, 1)
	go func() {
		finished <- s.RunTicketBuyer(&pb.RunTicketBuyerRequest{}, stream)
	}()
	var previousID uint64
	select {
	case previousID = <-stream.started:
	case <-time.After(time.Second):
		t.Fatal("idle ticket buyer did not start")
	}
	oldWallet, _ := s.loader.LoadedWallet()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := s.unloadWallet(ctx, s.loader); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-finished:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("unexpected idle buyer result: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("idle buyer survived wallet close")
	}
	if _, loaded := s.loader.LoadedWallet(); loaded {
		t.Fatal("wallet remained loaded")
	}
	if _, err := s.beginTicketBuyer(context.Background()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("buyer started without a loaded wallet: %v", err)
	}
	reopened, err := s.loader.OpenExistingWallet(ctx, []byte(wallet.InsecurePubPassphrase))
	if err != nil {
		t.Fatal(err)
	}
	next, err := s.beginTicketBuyer(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer s.finishTicketBuyer(next)
	if next.wallet != reopened || next.wallet == oldWallet || next.id <= previousID {
		t.Fatal("restarted buyer did not use the newly loaded wallet and a fresh run ID")
	}
}

func TestCloseWalletWaitsForBuyerAndExcludesStarts(t *testing.T) {
	s := newTicketBuyerTestServer(t)
	run, err := s.beginTicketBuyer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.finishTicketBuyer(run)
	closed := make(chan error, 1)
	go func() { closed <- s.unloadWallet(context.Background(), s.loader) }()
	select {
	case <-run.ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("wallet close did not cancel the buyer")
	}
	if _, loaded := s.loader.LoadedWallet(); !loaded {
		t.Fatal("database closed before buyer teardown")
	}
	if _, err := s.beginTicketBuyer(context.Background()); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("start was accepted while the wallet was closing: %v", err)
	}
	if err := s.unloadWallet(context.Background(), s.loader); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("overlapping close was accepted: %v", err)
	}
	select {
	case err := <-closed:
		t.Fatalf("wallet close completed before buyer teardown: %v", err)
	default:
	}
	s.finishTicketBuyer(run)
	select {
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("wallet did not close after buyer teardown")
	}
}

func TestCloseWalletDeadlineKeepsDatabaseOpen(t *testing.T) {
	s := newTicketBuyerTestServer(t)
	run, err := s.beginTicketBuyer(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer s.finishTicketBuyer(run)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	if err := s.unloadWallet(ctx, s.loader); status.Code(err) != codes.DeadlineExceeded {
		t.Fatalf("close deadline changed: %v", err)
	}
	if _, loaded := s.loader.LoadedWallet(); !loaded {
		t.Fatal("database closed while buyer teardown was incomplete")
	}
	if _, err := s.beginTicketBuyer(context.Background()); status.Code(err) != codes.AlreadyExists {
		t.Fatalf("timed-out close released an active buyer: %v", err)
	}
	s.finishTicketBuyer(run)
	if err := s.unloadWallet(context.Background(), s.loader); err != nil {
		t.Fatalf("could not retry close after teardown: %v", err)
	}
}
