package vsp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/txscript/v4"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/wallet/txrules"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
)

type gatedFeeAddress struct {
	stdaddr.Address
	entered chan struct{}
	release chan struct{}
}

func (a *gatedFeeAddress) PaymentScript() (uint16, []byte) {
	a.entered <- struct{}{}
	<-a.release
	return a.Address.PaymentScript()
}

func TestConcurrentFeeTasksRecordOnePayment(t *testing.T) {
	w, ticket, _ := feeSaveTestWallet(t)
	w.SetRelayFee(txrules.DefaultRelayFeePerKb)
	ctx := context.Background()
	address, err := w.NewExternalAddress(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	c := testFeeClient(w)
	defer c.Close()
	gate := &gatedFeeAddress{Address: address, entered: make(chan struct{}, 2), release: make(chan struct{})}
	var release sync.Once
	defer release.Do(func() { close(gate.release) })
	fp := &feePayment{client: c, ctx: ctx, ticketHash: ticket.TxHash(), fee: 30000, feeAddr: gate}
	var payments [2]*wire.MsgTx
	for i := range payments {
		funding := feeTopUpFunding(t, w, address, 1e8, byte(120+i))
		outpoint := wire.OutPoint{Hash: funding.TxHash(), Tree: wire.TxTreeRegular}
		w.LockOutpoint(&outpoint.Hash, outpoint.Index)
		payments[i] = wire.NewMsgTx()
		payments[i].AddTxIn(wire.NewTxIn(&outpoint, funding.TxOut[0].Value, nil))
	}
	results := make(chan error, 2)
	run := func(i int) {
		var result error
		fp.testTask("create fee", func() error {
			result = fp.makeFeeTx(payments[i])
			return result
		})()
		results <- result
	}
	go run(0)
	select {
	case <-gate.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("first payment did not start")
	}
	go run(1)
	// The first operation is held after reading feeTx and before constructing
	// outputs. A competing task must wait instead of creating another payment.
	select {
	case <-gate.entered:
	case <-time.After(100 * time.Millisecond):
	}
	release.Do(func() { close(gate.release) })
	for range payments {
		select {
		case err := <-results:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("payment task did not finish")
		}
	}
	if payments[0].TxHash() != payments[1].TxHash() {
		t.Fatalf("one ticket created two fee transactions: %v and %v", payments[0].TxHash(), payments[1].TxHash())
	}
	for _, payment := range payments {
		if len(payment.TxIn[0].SignatureScript) == 0 {
			t.Fatal("fee transaction was not signed")
		}
		hash := payment.TxHash()
		if _, err := c.tx(ctx, &hash); err != nil {
			t.Fatal(err)
		}
	}
	info, err := w.VSPTicketInfo(ctx, &fp.ticketHash)
	if err != nil || info.FeeHash != payments[0].TxHash() || fp.feeHash != info.FeeHash {
		t.Fatalf("fee association does not match the single payment: %+v %v", info, err)
	}
}

func TestProcessWaitingForFeeTaskCanBeCancelled(t *testing.T) {
	for _, closeClient := range []bool{false, true} {
		t.Run(map[bool]string{false: "caller cancellation", true: "client close"}[closeClient], func(t *testing.T) {
			w, _, ticket := feeTestWallet(t)
			c := testFeeClient(w)
			defer c.Close()
			hash := ticket.TxHash()
			seed := wire.NewMsgTx()
			seed.AddTxIn(wire.NewTxIn(&ticket.TxIn[0].PreviousOutPoint, 100, nil))
			fp := c.feePaymentWithTx(&hash, Policy{}, false, seed)
			if fp == nil || fp.feeTx != seed {
				t.Fatal("new fee worker did not receive the purchase inputs")
			}
			fp.schedule("", nil)
			finish, err := fp.beginOperation(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer finish()
			priorError := errors.New("active operation failure")
			fp.err = priorError
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			done := make(chan error, 1)
			go func() { done <- c.ProcessWithPolicy(ctx, &hash, wire.NewMsgTx(), Policy{}) }()
			// A processing request must wait for the existing operation, rather
			// than fetching a new quote or recording an error over its result.
			select {
			case err := <-done:
				t.Fatalf("processing bypassed the active fee operation: %v", err)
			case <-time.After(50 * time.Millisecond):
			}
			if closeClient {
				c.Close()
			} else {
				cancel()
			}
			select {
			case err := <-done:
				if !errors.Is(err, context.Canceled) {
					t.Fatalf("cancellation returned %v", err)
				}
			case <-time.After(time.Second):
				t.Fatal("cancellation waited for the fee operation")
			}
			if fp.err != priorError {
				t.Fatal("cancelled waiter changed the active operation diagnostic")
			}
		})
	}
}

type feeOperationContext struct {
	context.Context
	waiting chan struct{}
	once    sync.Once
}

func (ctx *feeOperationContext) Done() <-chan struct{} {
	ctx.once.Do(func() { close(ctx.waiting) })
	return ctx.Context.Done()
}

func TestProcessReadsPaymentStateAfterWaiting(t *testing.T) {
	for _, state := range []string{"paid", "confirmed", "confirmed and removed"} {
		t.Run(state, func(t *testing.T) {
			w, _, ticket := feeTestWallet(t)
			c := testFeeClient(w)
			defer c.Close()
			pub, key := vspTestKeys()
			c.pub = pub
			statusCalls := 0
			c.Transport = vspTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/api/v3/ticketstatus" {
					t.Errorf("request attempted a new payment after the prior operation finished: %s", req.URL.Path)
				}
				statusCalls++
				body, _ := io.ReadAll(req.Body)
				response, _ := json.Marshal(ticketStatus{FeeTxStatus: "received", Request: body})
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{
					"Vsp-Server-Signature": []string{base64.StdEncoding.EncodeToString(ed25519.Sign(key, response))},
				}, Body: io.NopCloser(bytes.NewReader(response))}, nil
			})
			hash := ticket.TxHash()
			fp := c.feePayment(&hash, Policy{}, false)
			if fp == nil {
				t.Fatal("payment worker was not created")
			}
			fp.schedule("", nil)
			finish, err := fp.beginOperation(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			var release sync.Once
			defer release.Do(finish)
			ctx := &feeOperationContext{Context: context.Background(), waiting: make(chan struct{})}
			done := make(chan error, 1)
			go func() { done <- c.ProcessWithPolicy(ctx, &hash, wire.NewMsgTx(), Policy{}) }()
			select {
			case <-ctx.waiting:
			case <-time.After(time.Second):
				t.Fatal("processing did not wait for the existing operation")
			}
			feeHash := chainhash.Hash{99}
			fp.mu.Lock()
			fp.feeHash = feeHash
			fp.mu.Unlock()
			if state == "paid" {
				err = w.UpdateVspTicketFeeToPaid(context.Background(), &hash, &feeHash, c.url, c.pub)
			} else if state == "confirmed" {
				err = w.UpdateVspTicketFeeToConfirmed(context.Background(), &hash, &feeHash, c.url, c.pub)
			} else {
				err = fp.recordConfirmedFee()
			}
			if err != nil {
				t.Fatal(err)
			}
			release.Do(finish)
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("processing did not finish after the preceding operation")
			}
			wantCalls := 0
			if state == "paid" {
				wantCalls = 1
			}
			if statusCalls != wantCalls {
				t.Fatalf("provider status calls = %d, want %d", statusCalls, wantCalls)
			}
		})
	}
}

func TestPaymentResponseMismatchDoesNotIncludeSecrets(t *testing.T) {
	w, _, ticket := feeTestWallet(t)
	c := testFeeClient(w)
	defer c.Close()
	pub, key := vspTestKeys()
	c.pub = pub
	const votingKey = "private-test-voting-key"
	const providerEcho = "private-test-provider-echo"
	c.Transport = vspTestTransport(func(req *http.Request) (*http.Response, error) {
		body, _ := io.ReadAll(req.Body)
		if req.URL.Path != "/api/v3/payfee" || !bytes.Contains(body, []byte(votingKey)) {
			t.Error("test did not exercise a payment containing a voting key")
		}
		response, _ := json.Marshal(struct {
			Request []byte `json:"request"`
		}{[]byte(providerEcho)})
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{
			"Vsp-Server-Signature": []string{base64.StdEncoding.EncodeToString(ed25519.Sign(key, response))},
		}, Body: io.NopCloser(bytes.NewReader(response))}, nil
	})
	voting, commitment, err := parseTicket(ticket, w.ChainParams())
	if err != nil {
		t.Fatal(err)
	}
	feeTx := wire.NewMsgTx()
	feeTx.AddTxOut(&wire.TxOut{Value: 1})
	fp := &feePayment{client: c, ctx: context.Background(), ticketHash: ticket.TxHash(),
		feeTx: feeTx, votingKey: votingKey, votingAddr: voting, commitmentAddr: commitment}
	if err := fp.submitPayment(); err == nil || err.Error() != "server response contains differing request" {
		t.Fatalf("payment mismatch did not return a constant error: %v", err)
	}
}

func TestFeeTaskClearsPreviousErrorDuringRetry(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	previous := errors.New("context deadline exceeded")
	fp := &feePayment{ctx: ctx, err: previous}
	fp.testTask("confirm payment", func() error {
		fp.mu.Lock()
		defer fp.mu.Unlock()
		if fp.err != nil {
			t.Error("active retry still exposes the previous failure")
		}
		return previous
	})()
	fp.mu.Lock()
	defer fp.mu.Unlock()
	if !errors.Is(fp.err, previous) {
		t.Fatal("retry did not expose its new failure")
	}
}

func TestProcessPublishesForegroundFailureAndClearsItDuringRetry(t *testing.T) {
	w, _, template := feeTestWallet(t)
	ctx := context.Background()
	address, err := w.NewExternalAddress(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := feeTopUpFunding(t, w, address, 100, 150)
	ticket := template.Copy()
	ticket.TxIn[0].PreviousOutPoint.Hash = parent.TxHash()
	sigErrors, err := w.SignTransaction(ctx, ticket, txscript.SigHashAll, nil, nil, nil)
	if err != nil || len(sigErrors) != 0 {
		t.Fatalf("sign ticket: %v %v", err, sigErrors)
	}
	if err := w.AddTransaction(ctx, ticket, nil); err != nil {
		t.Fatal(err)
	}
	c := testFeeClient(w)
	defer c.Close()
	hash := ticket.TxHash()
	fp := c.feePayment(&hash, Policy{}, false)
	if fp == nil {
		t.Fatal("missing payment worker")
	}
	fp.schedule("", nil)
	fp.mu.Lock()
	fp.err = errors.New("previous operation failed")
	fp.mu.Unlock()
	fp.timerMu.Lock()
	fp.phase = "reconcile payment"
	fp.nextRetry = time.Now().Add(time.Minute)
	fp.timerMu.Unlock()
	requests := 0
	c.Transport = vspTestTransport(func(req *http.Request) (*http.Response, error) {
		requests++
		if req.URL.Path != "/api/v3/feeaddress" {
			t.Errorf("unexpected request: %s", req.URL.Path)
		}
		tracked := c.TrackedTickets()
		if len(tracked) != 1 || tracked[0].LastError != "" {
			t.Error("foreground retry still exposes its previous failure")
		}
		return nil, context.DeadlineExceeded
	})
	err = c.ProcessWithPolicy(ctx, &hash, wire.NewMsgTx(), Policy{})
	if !errors.Is(err, context.DeadlineExceeded) || requests != 1 {
		t.Fatalf("foreground request did not reach injected timeout: %v (%d calls)", err, requests)
	}
	info, readErr := w.VSPTicketInfo(ctx, &hash)
	if readErr != nil || info.FeeTxStatus != uint32(udb.VSPFeeProcessErrored) {
		t.Fatalf("foreground failure was not persisted: %+v %v", info, readErr)
	}
	tracked := c.TrackedTickets()
	if len(tracked) != 1 || tracked[0].LastError != err.Error() || tracked[0].NextRetry.IsZero() {
		t.Fatalf("scheduled job lost its foreground failure diagnostic: %+v", tracked)
	}
	nextError := errors.New("background attempt failed")
	fp.testTask("reconcile payment", func() error {
		tracked := c.TrackedTickets()
		if len(tracked) != 1 || tracked[0].LastError != "" || !tracked[0].NextRetry.IsZero() {
			t.Error("background retry retained the completed foreground failure")
		}
		return nextError
	})()
	tracked = c.TrackedTickets()
	if len(tracked) != 1 || tracked[0].LastError != nextError.Error() {
		t.Fatal("completed foreground operation overwrote the background result")
	}
}
