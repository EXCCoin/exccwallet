package vsp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/wallet/txrules"
)

func TestReconcileRetriesTopUpAfterEarlierFeePublication(t *testing.T) {
	w, _, ticket := feeTestWallet(t)
	w.SetRelayFee(txrules.DefaultRelayFeePerKb)
	ctx := context.Background()
	address, err := w.NewExternalAddress(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	providerAddress, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(bytes.Repeat([]byte{2}, 20), w.ChainParams())
	if err != nil {
		t.Fatal(err)
	}
	version, script := address.PaymentScript()
	split := wire.NewMsgTx()
	split.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{90}}, 100060000, nil))
	for _, amount := range []int64{30000, 30000, 100000000} {
		split.AddTxOut(&wire.TxOut{Value: amount, Version: version, PkScript: script})
	}
	if err := w.AddTransaction(ctx, split, nil); err != nil {
		t.Fatal(err)
	}
	secondTicket := ticket.Copy()
	secondTicket.TxIn[0].PreviousOutPoint.Hash = chainhash.Hash{91}
	if err := w.AddTransaction(ctx, secondTicket, nil); err != nil {
		t.Fatal(err)
	}
	c := testFeeClient(w)
	defer c.Close()
	pub, key := vspTestKeys()
	c.pub = pub
	payments := 0
	c.Transport = vspTestTransport(func(req *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		var response interface{}
		switch req.URL.Path {
		case "/api/v3/payfee":
			payments++
			response = struct {
				Request []byte `json:"request"`
			}{body}
		case "/api/v3/ticketstatus":
			response = ticketStatus{FeeTxStatus: "received", Request: body}
		default:
			t.Errorf("unexpected provider request: %s", req.URL.Path)
		}
		data, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{
			"Vsp-Server-Signature": []string{base64.StdEncoding.EncodeToString(ed25519.Sign(key, data))},
		}, Body: io.NopCloser(bytes.NewReader(data))}, nil
	})
	fees := make([]*feePayment, 2)
	for i, ticket := range []*wire.MsgTx{ticket, secondTicket} {
		op := wire.OutPoint{Hash: split.TxHash(), Index: uint32(i), Tree: wire.TxTreeRegular}
		w.LockOutpoint(&op.Hash, op.Index)
		feeTx := wire.NewMsgTx()
		feeTx.AddTxIn(wire.NewTxIn(&op, split.TxOut[i].Value, nil))
		voting, commitment, err := parseTicket(ticket, w.ChainParams())
		if err != nil {
			t.Fatal(err)
		}
		jobCtx, cancel := context.WithCancel(ctx)
		fp := &feePayment{client: c, ctx: jobCtx, cancel: cancel, ticketHash: ticket.TxHash(),
			fee: 50000, feeAddr: providerAddress, feeTx: feeTx, votingAddr: voting, commitmentAddr: commitment}
		fees[i] = fp
		c.jobs[fp.ticketHash] = fp
	}
	first, waiting := fees[0], fees[1]
	if err := first.makeFeeTx(nil); err != nil {
		t.Fatal(err)
	}
	if len(first.feeTx.TxIn) != 2 {
		t.Fatal("first higher quote did not consume the split change")
	}
	// Execute the scheduled worker directly, without waiting for random jitter.
	waiting.testTask("reconcile payment", waiting.reconcilePayment)()
	if !errors.Is(waiting.err, errors.InsufficientBalance) {
		t.Fatalf("want unavailable unpublished change, got %v", waiting.err)
	}
	waiting.timerMu.Lock()
	requeued := waiting.timer != nil && !waiting.nextRetry.IsZero() && waiting.phase == "reconcile payment"
	waiting.timerMu.Unlock()
	if !requeued {
		t.Fatal("temporary top-up failure left a tracked job with no retry")
	}
	waiting.schedule("", nil)
	if payments != 0 || len(waiting.feeTx.TxOut) != 0 {
		t.Fatal("waiting fee spent or submitted unpublished change")
	}
	// Model the wallet learning that the earlier VSP fee was published.
	if err := w.SetPublished(ctx, &first.feeHash, true); err != nil {
		t.Fatal(err)
	}
	waiting.testTask("reconcile payment", waiting.reconcilePayment)()
	if waiting.err != nil || payments != 1 {
		t.Fatalf("requeued fee did not recover after publication: %v, payments=%d", waiting.err, payments)
	}
	if len(waiting.feeTx.TxIn) != 2 {
		t.Fatal("retry did not add a funding input")
	}
	foundChange := false
	for _, input := range waiting.feeTx.TxIn {
		foundChange = foundChange || input.PreviousOutPoint.Hash == first.feeHash
	}
	if !foundChange {
		t.Fatal("retry did not use the newly published fee change")
	}
}

func TestReconcileDoesNotRequeueCancellationOrManualRepair(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		t.Run(map[bool]string{false: "manual repair", true: "cancelled"}[cancelled], func(t *testing.T) {
			w, _, ticket := feeTestWallet(t)
			c := testFeeClient(w)
			defer c.Close()
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			fp := &feePayment{client: c, ctx: ctx, cancel: cancel, ticketHash: ticket.TxHash(), blocked: !cancelled}
			if cancelled {
				cancel()
			}
			if err := fp.reconcilePayment(); err == nil {
				t.Fatal("expected stopped or review-required reconciliation")
			}
			if fp.timer != nil || !fp.nextRetry.IsZero() {
				t.Fatal("stopped or review-required job was requeued")
			}
		})
	}
}
