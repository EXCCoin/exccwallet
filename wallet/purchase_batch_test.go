package wallet

import (
	"context"
	stderrors "errors"
	"fmt"
	"testing"

	"github.com/EXCCoin/exccd/blockchain/stake/v4"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/wallet/txrules"
)

func TestVSPPurchaseBatchWithFewLargeInputs(t *testing.T) {
	for _, inputs := range []int{9, 1} {
		t.Run(fmt.Sprintf("%d inputs", inputs), func(t *testing.T) {
			cfg := basicWalletConfig
			w, teardown := testWallet(t, &cfg)
			defer teardown()
			ctx := context.Background()
			if err := w.Unlock(ctx, []byte("private"), nil); err != nil {
				t.Fatal(err)
			}
			address, err := w.NewExternalAddress(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			version, script := address.PaymentScript()
			price, err := w.NextStakeDifficulty(ctx)
			if err != nil {
				t.Fatal(err)
			}
			const requested = 20
			amount := int64(price)*25 + 1e8
			funding := wire.NewMsgTx()
			funding.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{99}}, amount*int64(inputs), nil))
			for range inputs {
				funding.AddTxOut(&wire.TxOut{Value: amount, Version: version, PkScript: script})
			}
			if err := w.AddTransaction(ctx, funding, nil); err != nil {
				t.Fatal(err)
			}
			n := &purchaseResultNetwork{}
			w.SetNetworkBackend(n)
			registered := 0
			request := &PurchaseTicketsRequest{Count: requested, SourceAccount: 0, MinConf: 0,
				VSPFeeProcess: func(context.Context) (float64, error) { return 1, nil },
				VSPFeePaymentProcess: func(_ context.Context, _ *chainhash.Hash, fee *wire.MsgTx) error {
					registered++
					fee.AddTxOut(&wire.TxOut{Value: 1, Version: version, PkScript: script})
					return nil
				},
			}
			result, err := w.PurchaseTickets(ctx, n, request)
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || len(result.TicketHashes) != requested || len(result.Results) != requested || registered != requested {
				t.Fatalf("requested %d with %d large inputs, got %d published, %d results and %d registrations (mutated count %d)",
					requested, inputs, len(result.TicketHashes), len(result.Results), registered, request.Count)
			}
		})
	}
}

type batchPurchaseNetwork struct {
	purchaseResultNetwork
	beforePublish func(*wire.MsgTx) error
}

func (n *batchPurchaseNetwork) PublishTransactions(ctx context.Context, txs ...*wire.MsgTx) error {
	for _, tx := range txs {
		if err := n.beforePublish(tx); err != nil {
			return err
		}
	}
	return n.purchaseResultNetwork.PublishTransactions(ctx, txs...)
}

func TestVSPPurchaseSplitReservations(t *testing.T) {
	for _, test := range []struct {
		name              string
		want              int
		shortFunds        bool
		noFunds           bool
		failSplit         int
		failTicket        int
		cancelSplit       bool
		cancelAfterTicket bool
		failFee           bool
		zeroFee           bool
	}{
		{name: "funded", want: 3},
		{name: "insufficient for requested count", shortFunds: true, want: 2},
		{name: "insufficient for one ticket", noFunds: true},
		{name: "first split publication fails", failSplit: 1},
		{name: "second split publication fails", failSplit: 2},
		{name: "first ticket publication fails", failTicket: 1},
		{name: "partial ticket publication", failTicket: 2, want: 1},
		{name: "split publication cancelled", cancelSplit: true},
		{name: "cancelled after one ticket", cancelAfterTicket: true, want: 1},
		{name: "fee callback failure", failFee: true, want: 3},
		{name: "zero fee retains previous funding path", zeroFee: true, want: 3},
	} {
		t.Run(test.name, func(t *testing.T) {
			cfg := basicWalletConfig
			w, teardown := testWallet(t, &cfg)
			defer teardown()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := w.Unlock(ctx, []byte("private"), nil); err != nil {
				t.Fatal(err)
			}
			address, err := w.NewExternalAddress(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			version, script := address.PaymentScript()
			price, err := w.NextStakeDifficulty(ctx)
			if err != nil {
				t.Fatal(err)
			}
			amount := int64(price)*5 + 1e8
			if test.shortFunds {
				amount = int64(price)*2 + int64(price)/2
			} else if test.noFunds {
				amount = int64(price) / 2
			}
			inputs := 1
			percentage := float64(1)
			if test.zeroFee {
				inputs, percentage = 10, 0
				w.SetRelayFee(0)
			}
			funding := wire.NewMsgTx()
			funding.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{100}}, amount*int64(inputs), nil))
			for range inputs {
				funding.AddTxOut(&wire.TxOut{Value: amount, Version: version, PkScript: script})
			}
			if err := w.AddTransaction(ctx, funding, nil); err != nil {
				t.Fatal(err)
			}
			failure := stderrors.New("test publication or fee failure")
			n := &batchPurchaseNetwork{purchaseResultNetwork: purchaseResultNetwork{
				failAt: test.failTicket, failure: failure,
			}}
			w.SetNetworkBackend(n)
			var split *wire.MsgTx
			var splitCalls, registrations, failedFees int
			request := &PurchaseTicketsRequest{Count: 3, SourceAccount: 0, MinConf: 0}
			n.beforePublish = func(tx *wire.MsgTx) error {
				if stake.IsSStx(tx) {
					if test.cancelAfterTicket && len(n.published) == 1 {
						cancel()
						return ctx.Err()
					}
					return nil
				}
				split, splitCalls = tx, splitCalls+1
				for _, output := range tx.TxOut {
					if txrules.IsDustOutput(output, w.RelayFee()) {
						t.Fatal("split contains a dust output")
					}
				}
				if !test.zeroFee {
					hash := tx.TxHash()
					for i := 0; i < request.Count; i++ {
						if !w.LockedOutpoint(&hash, uint32(i*2+1)) {
							t.Fatalf("fee output %d was selectable when split was published", i)
						}
					}
				}
				if test.cancelSplit {
					cancel()
					return ctx.Err()
				}
				if splitCalls == test.failSplit {
					return failure
				}
				return nil
			}
			request.VSPFeeProcess = func(context.Context) (float64, error) { return percentage, nil }
			feeInputs := make(map[wire.OutPoint]bool)
			request.VSPFeePaymentProcess = func(ctx context.Context, _ *chainhash.Hash, fee *wire.MsgTx) error {
				registrations++
				if len(fee.TxIn) != 1 {
					t.Fatalf("fee has %d inputs", len(fee.TxIn))
				}
				in := fee.TxIn[0]
				prev := in.PreviousOutPoint
				if feeInputs[prev] || !w.LockedOutpoint(&prev.Hash, prev.Index) {
					t.Fatal("fee input reused or not reserved")
				}
				feeInputs[prev] = true
				if !test.zeroFee && (prev.Hash != split.TxHash() || prev.Index%2 != 1 ||
					in.ValueIn != split.TxOut[prev.Index].Value) {
					t.Fatal("fee input does not match its funded split output")
				}
				if err := ctx.Err(); err != nil {
					failedFees++
					return err
				}
				if test.failFee && registrations == 1 {
					failedFees++
					return failure
				}
				fee.AddTxOut(&wire.TxOut{Value: 1, Version: version, PkScript: script})
				return nil
			}
			result, err := w.PurchaseTickets(ctx, n, request)
			wantError := test.noFunds || test.failSplit > 0 || test.failTicket > 0 ||
				test.cancelSplit || test.cancelAfterTicket || test.failFee
			if (err != nil) != wantError {
				t.Fatalf("error %v, want error %v", err, wantError)
			}
			published := 0
			if result != nil {
				published = len(result.TicketHashes)
			}
			if published != test.want || registrations != test.want || len(n.published) != test.want {
				t.Fatalf("published %d, registrations %d, network published %d; want %d", published, registrations, len(n.published), test.want)
			}
			if split != nil && !test.zeroFee {
				hash := split.TxHash()
				for i := 0; i < request.Count; i++ {
					wantLocked := i < test.want && !(test.failFee && i == 0) && !test.cancelAfterTicket
					if w.LockedOutpoint(&hash, uint32(i*2+1)) != wantLocked {
						t.Fatalf("incorrect retained reservation for fee %d", i)
					}
				}
			}
			w.lockedOutpointMu.Lock()
			locks := len(w.lockedOutpoints)
			w.lockedOutpointMu.Unlock()
			if locks != registrations-failedFees {
				t.Fatalf("retained %d reservations, expected %d unfinished successful callbacks", locks, registrations-failedFees)
			}
		})
	}
}
