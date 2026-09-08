package rpcserver

import (
	"context"

	"github.com/EXCCoin/exccwallet/v2/internal/loader"
	pb "github.com/EXCCoin/exccwallet/v2/rpc/walletrpc"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ticketBuyerRun struct {
	id     uint64
	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
	wallet *wallet.Wallet
}

func (t *ticketbuyerV2Server) beginTicketBuyer(ctx context.Context) (*ticketBuyerRun, error) {
	t.runMu.Lock()
	defer t.runMu.Unlock()
	if t.closing {
		return nil, status.Error(codes.FailedPrecondition, "wallet is closing")
	}
	if t.run != nil {
		return nil, status.Error(codes.AlreadyExists, "a ticket buyer is already running or stopping")
	}
	w, loaded := t.loader.LoadedWallet()
	if !loaded {
		return nil, status.Error(codes.FailedPrecondition, "wallet has not been loaded")
	}
	ctx, cancel := context.WithCancel(ctx)
	t.nextRunID++
	run := &ticketBuyerRun{id: t.nextRunID, ctx: ctx, cancel: cancel, done: make(chan struct{}), wallet: w}
	t.run = run
	return run, nil
}

// unloadWallet keeps the database open until the buyer and its workers have
// stopped, and excludes new runs until the loader has finished closing it.
func (t *ticketbuyerV2Server) unloadWallet(ctx context.Context, l *loader.Loader) error {
	t.readyMtx.RLock()
	matchingLoader := t.loader == l
	t.readyMtx.RUnlock()
	if !matchingLoader {
		return l.UnloadWallet()
	}
	t.runMu.Lock()
	if t.closing {
		t.runMu.Unlock()
		return status.Error(codes.FailedPrecondition, "wallet is already closing")
	}
	t.closing = true
	run := t.run
	if run != nil {
		run.cancel()
	}
	t.runMu.Unlock()
	defer func() {
		t.runMu.Lock()
		t.closing = false
		t.runMu.Unlock()
	}()
	if run != nil {
		select {
		case <-run.done:
		case <-ctx.Done():
			return translateError(ctx.Err())
		}
	}
	if err := ctx.Err(); err != nil {
		return translateError(err)
	}
	return l.UnloadWallet()
}

func (t *ticketbuyerV2Server) finishTicketBuyer(run *ticketBuyerRun) {
	run.cancel()
	t.runMu.Lock()
	defer t.runMu.Unlock()
	if t.run == run {
		t.run = nil
		close(run.done)
	}
}

func (t *ticketbuyerV2Server) StopTicketBuyer(ctx context.Context, req *pb.StopTicketBuyerRequest) (*pb.StopTicketBuyerResponse, error) {
	if req.RunId == 0 {
		return nil, status.Error(codes.InvalidArgument, "a ticket buyer run ID is required")
	}
	t.runMu.Lock()
	run := t.run
	if run == nil {
		known := req.RunId <= t.nextRunID
		t.runMu.Unlock()
		if !known {
			return nil, status.Error(codes.NotFound, "ticket buyer run not found")
		}
		return &pb.StopTicketBuyerResponse{}, nil
	}
	if run.id != req.RunId {
		t.runMu.Unlock()
		return nil, status.Error(codes.FailedPrecondition, "a different ticket buyer run is active")
	}
	run.cancel()
	t.runMu.Unlock()
	select {
	case <-run.done:
		return &pb.StopTicketBuyerResponse{}, nil
	case <-ctx.Done():
		return nil, translateError(ctx.Err())
	}
}
