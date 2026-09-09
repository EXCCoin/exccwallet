package vsp

import (
	"context"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
)

func TestClientRemovalCancelsOnlyItsJobs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	c := &Client{client: &client{}, ctx: ctx, cancel: cancel, jobs: make(map[chainhash.Hash]*feePayment)}
	addJob := func(hash chainhash.Hash) *feePayment {
		jobCtx, jobCancel := context.WithCancel(ctx)
		fp := &feePayment{client: c, ticketHash: hash, ctx: jobCtx, cancel: jobCancel}
		fp.timer = time.AfterFunc(time.Hour, func() { t.Error("cancelled job ran") })
		c.jobs[hash] = fp
		return fp
	}
	aHash, bHash := chainhash.Hash{1}, chainhash.Hash{2}
	a, b := addJob(aHash), addJob(bHash)
	c.RemoveTicket(aHash, "pruned")
	if a.ctx.Err() == nil || a.timer != nil || b.ctx.Err() != nil || len(c.jobs) != 1 {
		t.Fatal("removal did not cancel only its ticket")
	}
	called := false
	a.schedule("unexpected retry", func() error { called = true; return nil })
	a.testTask("unexpected running task", func() error { called = true; return nil })()
	if a.timer != nil || called {
		t.Fatal("pruned ticket resumed work")
	}
	c.Close()
	c.Close()
	if b.ctx.Err() == nil || b.timer != nil || !c.closed || len(c.jobs) != 0 {
		t.Fatal("close did not stop all wallet jobs")
	}
	if fp := c.feePayment(&aHash, Policy{}, false); fp != nil {
		t.Fatal("closed client recreated a job")
	}
}
