// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package vsp

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/errors"
)

func TestBindPolicyIsRequestLocal(t *testing.T) {
	want := []Policy{
		{MaxFee: 5, FeeAcct: 7, ChangeAcct: 9},
		{MaxFee: 6, FeeAcct: 8, ChangeAcct: 10},
	}
	var got []Policy
	process := func(_ context.Context, _ *chainhash.Hash, _ *wire.MsgTx,
		policy Policy) error {

		got = append(got, policy)
		return nil
	}

	for _, policy := range want {
		if err := BindPolicy(process, policy)(context.Background(), nil, nil); err != nil {
			t.Fatal(err)
		}
	}
	if len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("wrong policy: got %+v, want %+v", got, want)
	}
}

func TestSkipManagedTicket(t *testing.T) {
	unexpectedErr := errors.E(errors.IO)
	tests := []struct {
		name      string
		confirmed bool
		err       error
		localOnly bool
		wantSkip  bool
		wantErr   error
	}{
		{name: "seed restored", err: errors.E(errors.NotExist)},
		{name: "automatic resume", err: errors.E(errors.NotExist), localOnly: true, wantSkip: true},
		{name: "unexpected error", err: unexpectedErr, wantErr: unexpectedErr},
		{name: "confirmed", confirmed: true, wantSkip: true},
		{name: "unconfirmed"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			gotSkip, gotErr := skipManagedTicket(test.confirmed, test.err, test.localOnly)
			if gotSkip != test.wantSkip {
				t.Fatalf("skip: got %v, want %v", gotSkip, test.wantSkip)
			}
			if gotErr != test.wantErr {
				t.Fatalf("error: got %v, want %v", gotErr, test.wantErr)
			}
		})
	}
}

func TestIsUnknownTicketError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "unknown ticket", err: &BadRequestError{Code: codeUnknownTicket}, want: true},
		{name: "wrapped unknown ticket", err: errors.E(&BadRequestError{Code: codeUnknownTicket}), want: true},
		{name: "other API error", err: &BadRequestError{Code: codeBadSignature}},
		{name: "deadline", err: context.DeadlineExceeded},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := isUnknownTicketError(test.err); got != test.want {
				t.Fatalf("is unknown ticket = %v, want %v", got, test.want)
			}
		})
	}
}

func TestNextJitter(t *testing.T) {
	tests := []struct {
		name                              string
		tip, live, expires                int32
		targetTimePerBlock, wantMaxJitter time.Duration
	}{
		{"unmined", 100, 0, 0, 5 * time.Minute, unminedJitter},
		{"immature", 100, 102, 40962, 5 * time.Minute, 10 * time.Minute},
		{"immature capped", 100, 200, 40962, 5 * time.Minute, immatureJitter},
		{"live", 102, 102, 40962, 5 * time.Minute, liveJitter},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := nextJitter(test.tip, test.live, test.expires,
				test.targetTimePerBlock)
			if got != test.wantMaxJitter {
				t.Fatalf("got %v, want %v", got, test.wantMaxJitter)
			}
		})
	}
}

func TestTrackedTicketsConcurrentJobs(t *testing.T) {
	c := &Client{jobs: make(map[chainhash.Hash]*feePayment)}
	const iterations = 1000

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			hash := chainhash.Hash{byte(i)}
			c.mu.Lock()
			c.jobs[hash] = &feePayment{ticketHash: hash}
			if i > 1 {
				delete(c.jobs, chainhash.Hash{byte(i - 2)})
			}
			c.mu.Unlock()
		}
	}()
	go func() {
		defer wg.Done()
		for i := 0; i < iterations; i++ {
			_ = c.TrackedTickets()
		}
	}()
	wg.Wait()
}
