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
	"sync/atomic"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/txscript/v4"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/wallet/txrules"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

func TestForegroundPaymentSchedulesConfirmationWithoutResubmitting(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "accepted", true: "response timeout"}[fail], func(t *testing.T) {
			w, _, template := feeTestWallet(t)
			w.SetRelayFee(txrules.DefaultRelayFeePerKb)
			ctx := context.Background()
			address, err := w.NewExternalAddress(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			parent := feeTopUpFunding(t, w, address, 100, 151)
			ticket := template.Copy()
			ticket.TxIn[0].PreviousOutPoint.Hash = parent.TxHash()
			sigErrors, err := w.SignTransaction(ctx, ticket, txscript.SigHashAll, nil, nil, nil)
			if err != nil || len(sigErrors) != 0 {
				t.Fatalf("sign ticket: %v %v", err, sigErrors)
			}
			if err = w.AddTransaction(ctx, ticket, nil); err != nil {
				t.Fatal(err)
			}
			funding := feeTopUpFunding(t, w, address, 1e8, 152)
			op := wire.OutPoint{Hash: funding.TxHash(), Tree: wire.TxTreeRegular}
			w.LockOutpoint(&op.Hash, op.Index)
			feeTx := wire.NewMsgTx()
			feeTx.AddTxIn(wire.NewTxIn(&op, funding.TxOut[0].Value, nil))
			c := testFeeClient(w)
			defer c.Close()
			pub, key := vspTestKeys()
			c.pub = pub
			var payments, statuses int
			failPayment := fail
			providerStatus := "none"
			c.Transport = vspTestTransport(func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				var response any
				switch req.URL.Path {
				case "/api/v3/feeaddress":
					response = struct {
						FeeAddress string `json:"feeaddress"`
						FeeAmount  int64  `json:"feeamount"`
						Request    []byte `json:"request"`
					}{address.String(), 30000, body}
				case "/api/v3/payfee":
					payments++
					if failPayment {
						return nil, context.DeadlineExceeded
					}
					providerStatus = "received"
					response = struct {
						Request []byte `json:"request"`
					}{body}
				case "/api/v3/ticketstatus":
					statuses++
					response = ticketStatus{FeeTxStatus: providerStatus, Request: body}
				default:
					t.Errorf("unexpected request %s", req.URL.Path)
				}
				data, _ := json.Marshal(response)
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Vsp-Server-Signature": []string{base64.StdEncoding.EncodeToString(ed25519.Sign(key, data))}}, Body: io.NopCloser(bytes.NewReader(data))}, nil
			})
			hash := ticket.TxHash()
			fp := c.feePaymentWithTx(&hash, Policy{MaxFee: 1e8}, false, feeTx)
			if fp == nil {
				t.Fatal("missing payment worker")
			}
			fp.timerMu.Lock()
			fp.timer.Reset(time.Hour)
			fp.timerMu.Unlock()
			err = c.ProcessWithPolicy(ctx, &hash, feeTx, Policy{MaxFee: 1e8})
			if fail {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("missing timeout: %v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			fp.timerMu.Lock()
			phase, next := fp.phase, fp.nextRetry
			fp.timerMu.Unlock()
			want := "confirm payment"
			if fail {
				want = "reconcile payment"
			}
			if phase != want || next.IsZero() {
				t.Fatalf("scheduled %q, want %q", phase, want)
			}
			fp.schedule("", nil)
			if payments != 1 || statuses != 0 {
				t.Fatalf("foreground requests: payments=%d statuses=%d", payments, statuses)
			}
			info, err := w.VSPTicketInfo(ctx, &hash)
			if err != nil || info.FeeTxStatus != uint32(udb.VSPFeeProcessPaid) || info.FeeHash != feeTx.TxHash() {
				t.Fatalf("created fee was not durably retained: %+v %v", info, err)
			}
			// Recreate the client using only its saved fee record. PAID records
			// include locally-created fees whose submission was never accepted.
			c.Close()
			restarted := testFeeClient(w)
			defer restarted.Close()
			restarted.pub, restarted.Transport = c.pub, c.Transport
			job := restarted.feePayment(&hash, Policy{MaxFee: 1e8}, true)
			if job == nil {
				t.Fatal("saved payment could not restart")
			}
			job.timerMu.Lock()
			phase = job.phase
			job.timerMu.Unlock()
			if phase != "confirm payment" {
				t.Fatalf("saved fee restarts with %q instead of checking the provider", phase)
			}
			job.schedule("", nil)
			job.testTask("confirm payment", job.confirmPayment)()
			if payments != 1 || statuses != 1 {
				t.Fatalf("confirmation resubmitted fee: payments=%d statuses=%d", payments, statuses)
			}
			if fail {
				job.timerMu.Lock()
				phase = job.phase
				job.timerMu.Unlock()
				if phase != "reconcile payment" {
					t.Fatalf("provider missing fee scheduled %q", phase)
				}
				job.schedule("", nil)
				failPayment = false
				job.testTask("reconcile payment", job.reconcilePayment)()
				if payments != 2 || statuses != 2 {
					t.Fatalf("saved fee did not recover: payments=%d statuses=%d", payments, statuses)
				}
				if job.feeHash != info.FeeHash {
					t.Fatal("recovery replaced the saved fee transaction")
				}
			}
		})
	}
}

type scheduledOperationContext struct {
	context.Context
	armed   atomic.Bool
	waiting chan struct{}
	once    sync.Once
}

func (c *scheduledOperationContext) Done() <-chan struct{} {
	if c.armed.Load() {
		c.once.Do(func() { close(c.waiting) })
	}
	return c.Context.Done()
}

func TestReplacedTimerCannotRunAfterWaitingForAnOperation(t *testing.T) {
	w, _, ticket := feeTestWallet(t)
	c := testFeeClient(w)
	defer c.Close()
	ctx := &scheduledOperationContext{Context: context.Background(), waiting: make(chan struct{})}
	fp := &feePayment{client: c, ctx: ctx, ticketHash: ticket.TxHash()}
	finish, err := fp.beginOperation(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var release sync.Once
	defer release.Do(finish)
	var obsolete, replacements atomic.Int32
	oldMethod := func() error { obsolete.Add(1); return errors.New("obsolete failure") }
	fp.schedule("obsolete", oldMethod)
	fp.timerMu.Lock()
	fp.timer.Reset(time.Hour)
	fp.timerMu.Unlock()
	oldTask := fp.testTask("obsolete", oldMethod)
	oldDone := make(chan struct{})
	ctx.armed.Store(true)
	go func() { defer close(oldDone); oldTask() }()
	select {
	case <-ctx.waiting:
	case <-time.After(time.Second):
		t.Fatal("old task did not reach the operation lease")
	}
	newMethod := func() error { replacements.Add(1); return nil }
	fp.schedule("replacement", newMethod)
	fp.timerMu.Lock()
	fp.timer.Reset(time.Hour)
	fp.timerMu.Unlock()
	newTask := fp.testTask("replacement", newMethod)
	newDone := make(chan struct{})
	go func() { defer close(newDone); newTask() }()
	release.Do(finish)
	for _, done := range []chan struct{}{oldDone, newDone} {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("scheduled task did not finish")
		}
	}
	if obsolete.Load() != 0 || replacements.Load() != 1 {
		t.Fatalf("callback executions: obsolete=%d replacement=%d", obsolete.Load(), replacements.Load())
	}
	fp.mu.Lock()
	lastError := fp.err
	fp.mu.Unlock()
	if lastError != nil {
		t.Fatalf("obsolete callback overwrote replacement: %v", lastError)
	}
	fp.schedule("", nil)
}

func (fp *feePayment) testTask(name string, method func() error) func() {
	fp.timerMu.Lock()
	generation := fp.timerGeneration
	fp.timerMu.Unlock()
	return fp.task(name, method, generation)
}

func TestFeeJobIsScheduledBeforePublication(t *testing.T) {
	var gate *gatedFeeDB
	driver := "vsp-test-scheduled-constructor-" + t.TempDir()
	if err := walletdb.RegisterDriver(walletdb.Driver{DbType: driver, Create: func(args ...interface{}) (walletdb.DB, error) {
		db, err := walletdb.Create("bdb", args...)
		if err != nil {
			return nil, err
		}
		gate = &gatedFeeDB{DB: db}
		return gate, nil
	}}); err != nil {
		t.Fatal(err)
	}
	w, _, ticket := feeTestWalletDriver(t, driver)
	c := testFeeClient(w)
	defer c.Close()
	hash := ticket.TxHash()
	var unscheduled atomic.Int32
	gate.mu.Lock()
	gate.beforeRead = func() {
		c.mu.Lock()
		visible := c.jobs[hash]
		c.mu.Unlock()
		if visible != nil {
			visible.timerMu.Lock()
			if visible.phase == "" {
				unscheduled.Add(1)
			}
			visible.timerMu.Unlock()
		}
	}
	gate.mu.Unlock()
	done := make(chan *feePayment, 1)
	go func() { done <- c.feePayment(&hash, Policy{}, false) }()
	select {
	case fp := <-done:
		if fp == nil {
			t.Fatal("payment worker was not created")
		}
		if unscheduled.Load() != 0 {
			t.Fatal("published job was visible before its initial schedule; another request can be overwritten")
		}
		fp.timerMu.Lock()
		phase := fp.phase
		fp.timerMu.Unlock()
		if phase != "reconcile payment" {
			t.Fatalf("initial phase = %q", phase)
		}
	case <-time.After(time.Second):
		t.Fatal("constructor held the client lock across wallet DB reads")
	}
}
