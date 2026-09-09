package vsp

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
)

func TestVSPBroadcastFailureRetainsPaymentUntilProviderConfirms(t *testing.T) {
	c, fp, _ := publicationTestJob(t, false)
	w := c.Wallet
	ctx := context.Background()
	feeHash := fp.feeHash
	feeBytes, err := fp.feeTx.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	input := fp.feeTx.TxIn[0].PreviousOutPoint
	paymentErrorCode, providerStatus := codeCannotBroadcastFee, "error"
	var requests []string
	c.Transport = vspTestTransport(func(req *http.Request) (*http.Response, error) {
		requests = append(requests, req.URL.Path)
		if req.URL.Path == "/api/v3/payfee" {
			body, err := io.ReadAll(req.Body)
			if err != nil {
				return nil, err
			}
			var payment struct {
				FeeTx string `json:"feetx"`
			}
			if err := json.Unmarshal(body, &payment); err != nil {
				t.Fatal(err)
			}
			if payment.FeeTx != hex.EncodeToString(feeBytes) {
				t.Fatal("retry submitted a different signed fee transaction")
			}
			req.Body = io.NopCloser(bytes.NewReader(body))
		}
		return publicationTestTransport(t, fp, providerStatus, paymentErrorCode)(req)
	})
	assertStored := func(status udb.FeeStatus) {
		t.Helper()
		info, err := w.VSPTicketInfo(ctx, &fp.ticketHash)
		if err != nil || info.FeeHash != feeHash || info.FeeTxStatus != uint32(status) ||
			info.Host != c.url || !bytes.Equal(info.PubKey, c.pub) || fp.feeHash != feeHash || fp.feeTx == nil {
			t.Fatalf("fee transaction or durable association changed: %+v %v", info, err)
		}
		txs, _, err := w.GetTransactionsByHashes(ctx, []*chainhash.Hash{&feeHash})
		if err != nil || len(txs) != 1 {
			t.Fatalf("saved fee transaction was removed: %v", err)
		}
		stored, err := txs[0].Bytes()
		if err != nil || !bytes.Equal(stored, feeBytes) {
			t.Fatal("saved signed transaction changed")
		}
		spender, _, err := w.Spender(ctx, &input)
		if err != nil || spender.TxHash() != feeHash {
			t.Fatalf("fee input was released: %v", err)
		}
		outputs, err := w.UnspentOutputs(ctx, wallet.OutputSelectionPolicy{Account: 0, RequiredConfirmations: 0})
		if err != nil {
			t.Fatal(err)
		}
		for _, output := range outputs {
			if output.OutPoint == input {
				t.Fatal("reserved fee input became spendable")
			}
		}
	}

	// A node RPC failure may be returned after the node accepted the fee.
	// Retrying must keep both its exact signed transaction and spent inputs.
	fp.testTask("reconcile payment", fp.reconcilePayment)()
	var apiErr *BadRequestError
	if !errors.As(fp.err, &apiErr) || apiErr.Code != codeCannotBroadcastFee {
		t.Fatalf("broadcast failure was not retained: %v", fp.err)
	}
	fp.timerMu.Lock()
	requeued := fp.timer != nil && !fp.nextRetry.IsZero() && fp.phase == "reconcile payment"
	fp.timerMu.Unlock()
	fp.schedule("", nil)
	if !requeued {
		t.Fatal("broadcast failure did not schedule reconciliation")
	}
	assertStored(udb.VSPFeeProcessPaid)
	assertFeePublication(t, fp, false)

	paymentErrorCode, providerStatus = codeFeeAlreadyReceived, "received"
	fp.testTask("reconcile payment", fp.reconcilePayment)()
	fp.schedule("", nil)
	if fp.err != nil {
		t.Fatalf("already-received acknowledgement did not recover: %v", fp.err)
	}
	assertStored(udb.VSPFeeProcessPaid)
	assertFeePublication(t, fp, false)

	providerStatus = "confirmed"
	fp.testTask("confirm payment", fp.confirmPayment)()
	if fp.err != nil {
		t.Fatalf("confirmation failed: %v", fp.err)
	}
	assertStored(udb.VSPFeeProcessConfirmed)
	if _, tracked := c.jobs[fp.ticketHash]; tracked {
		t.Fatal("confirmed fee remained tracked")
	}
	wantRequests := "/api/v3/payfee,/api/v3/payfee,/api/v3/ticketstatus,/api/v3/ticketstatus"
	if strings.Join(requests, ",") != wantRequests {
		t.Fatalf("unexpected requests or replacement fee: %v", requests)
	}
}

func TestVSPAmbiguousBroadcastThenRejectionRetainsPaymentAfterReopen(t *testing.T) {
	for _, rejection := range []struct {
		name string
		code int
	}{{"expired", codeFeeExpired}, {"invalid", codeInvalidFeeTx}} {
		t.Run(rejection.name, func(t *testing.T) {
			c, fp, gate := publicationTestJob(t, true)
			ctx := context.Background()
			feeHash, ticketHash := fp.feeHash, fp.ticketHash
			input := fp.feeTx.TxIn[0].PreviousOutPoint
			feeBytes, err := fp.feeTx.Bytes()
			if err != nil {
				t.Fatal(err)
			}
			c.Transport = publicationTestTransport(t, fp, "error", codeCannotBroadcastFee)
			fp.testTask("reconcile payment", fp.reconcilePayment)()
			fp.schedule("", nil)
			var apiErr *BadRequestError
			if !errors.As(fp.err, &apiErr) || apiErr.Code != codeCannotBroadcastFee {
				t.Fatalf("broadcast failure was not retained: %v", fp.err)
			}
			c.Close()
			if err := gate.Close(); err != nil {
				t.Fatal(err)
			}
			db, err := wallet.OpenDB("bdb", gate.path)
			if err != nil {
				t.Fatal(err)
			}
			defer db.Close()
			w, err := wallet.Open(ctx, &wallet.Config{DB: db,
				PubPassphrase: []byte(wallet.InsecurePubPassphrase), Params: c.Wallet.ChainParams(), GapLimit: 20})
			if err != nil {
				t.Fatal(err)
			}
			if err := w.Unlock(ctx, []byte("test-wallet-password"), nil); err != nil {
				t.Fatal(err)
			}
			restarted := testFeeClient(w)
			defer restarted.Close()
			restarted.pub = c.pub
			job := restarted.feePayment(&ticketHash, Policy{MaxFee: 1e8}, true)
			if job == nil {
				t.Fatal("saved payment did not reopen")
			}
			job.schedule("", nil)
			transport := publicationTestTransport(t, job, "error", rejection.code)
			restarted.Transport = vspTestTransport(func(req *http.Request) (*http.Response, error) {
				if req.URL.Path != "/api/v3/feeaddress" {
					return transport(req)
				}
				address, err := w.NewExternalAddress(ctx, 0)
				if err != nil {
					return nil, err
				}
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				data, err := json.Marshal(struct {
					FeeAddress string `json:"feeaddress"`
					FeeAmount  int64  `json:"feeamount"`
					Request    []byte `json:"request"`
				}{address.String(), 35000, body})
				if err != nil {
					return nil, err
				}
				_, key := vspTestKeys()
				response := httptest.NewRecorder()
				writeVSPTestResponse(response, key, http.StatusOK, data)
				return response.Result(), nil
			})
			job.testTask("reconcile payment", job.reconcilePayment)()
			job.schedule("", nil)
			if !errors.As(job.err, &apiErr) || apiErr.Code != rejection.code {
				t.Fatalf("expired quote response was not retained: %v", job.err)
			}
			info, err := w.VSPTicketInfo(ctx, &ticketHash)
			if err != nil || info.FeeHash != feeHash || job.feeHash != feeHash || job.feeTx == nil {
				t.Fatalf("expiration discarded a payment which may already be broadcast: %+v %v", info, err)
			}
			txs, _, err := w.GetTransactionsByHashes(ctx, []*chainhash.Hash{&feeHash})
			if err != nil || len(txs) != 1 {
				t.Fatalf("original fee was removed after reopening: %v", err)
			}
			stored, err := txs[0].Bytes()
			if err != nil || !bytes.Equal(stored, feeBytes) {
				t.Fatal("original signed transaction changed after reopening")
			}
			spender, _, err := w.Spender(ctx, &input)
			if err != nil || spender.TxHash() != feeHash {
				t.Fatalf("original fee input was released after reopening: %v", err)
			}
			assertFeePublication(t, job, false)

		})
	}
}
