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
	"strings"
	"testing"

	"github.com/EXCCoin/exccd/txscript/v4"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

func publicationTestJob(t *testing.T, gated bool) (*Client, *feePayment, *atomicFeeDB) {
	t.Helper()
	var gate *atomicFeeDB
	driver := "bdb"
	if gated {
		driver = "vsp-publication-retry-" + t.TempDir()
		if err := walletdb.RegisterDriver(walletdb.Driver{DbType: driver, Create: func(args ...interface{}) (walletdb.DB, error) {
			db, err := walletdb.Create("bdb", args...)
			if err != nil {
				return nil, err
			}
			gate = &atomicFeeDB{DB: db, path: args[0].(string)}
			return gate, nil
		}}); err != nil {
			t.Fatal(err)
		}
	}
	w, _, template := feeTestWalletDriver(t, driver)
	address, err := w.NewExternalAddress(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	parent := feeTopUpFunding(t, w, address, 100, 210)
	ticket := template.Copy()
	ticket.TxIn[0].PreviousOutPoint.Hash = parent.TxHash()
	sigErrors, err := w.SignTransaction(context.Background(), ticket, txscript.SigHashAll, nil, nil, nil)
	if err != nil || len(sigErrors) != 0 {
		t.Fatalf("sign ticket: %v %v", err, sigErrors)
	}
	if err := w.AddTransaction(context.Background(), ticket, nil); err != nil {
		t.Fatal(err)
	}
	c := testFeeClient(w)
	t.Cleanup(c.Close)
	pub, _ := vspTestKeys()
	c.pub = pub
	fee := signedRecoveryFee(t, w, 211)
	hash := ticket.TxHash()
	if recorded, err := w.RecordVSPFee(context.Background(), &hash, fee, c.url, c.pub); err != nil || !recorded {
		t.Fatalf("record unpublished fee: %v", err)
	}
	voting, commitment, err := parseTicket(ticket, w.ChainParams())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	fp := &feePayment{client: c, ctx: ctx, cancel: cancel, ticketHash: hash,
		policy: Policy{MaxFee: 1e8}, feeTx: fee, feeHash: fee.TxHash(), votingAddr: voting, commitmentAddr: commitment}
	c.jobs[hash] = fp
	return c, fp, gate
}

func publicationTestTransport(t *testing.T, fp *feePayment, status string, paymentErrorCode int, providerFeeHash ...string) vspTestTransport {
	t.Helper()
	_, key := vspTestKeys()
	return func(req *http.Request) (*http.Response, error) {
		requestBody, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		code := http.StatusOK
		var response any
		switch req.URL.Path {
		case "/api/v3/payfee":
			if paymentErrorCode != 0 {
				code, response = http.StatusBadRequest, BadRequestError{Code: paymentErrorCode, Message: "provider payment error"}
			} else {
				response = struct {
					Request []byte `json:"request"`
				}{requestBody}
			}
		case "/api/v3/ticketstatus":
			hash := fp.feeHash.String()
			if len(providerFeeHash) != 0 {
				hash = providerFeeHash[0]
			}
			response = ticketStatus{FeeTxStatus: status, FeeTxHash: hash, Request: requestBody}
		default:
			t.Errorf("unexpected request %s", req.URL.Path)
		}
		body, err := json.Marshal(response)
		if err != nil {
			return nil, err
		}
		return &http.Response{StatusCode: code, Header: http.Header{
			"Vsp-Server-Signature": []string{base64.StdEncoding.EncodeToString(ed25519.Sign(key, body))},
		}, Body: io.NopCloser(bytes.NewReader(body))}, nil
	}
}

func assertFeePublication(t *testing.T, fp *feePayment, want bool) {
	t.Helper()
	w := fp.client.Wallet
	transactions, err := w.UnminedTransactions(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var published bool
	for _, tx := range transactions {
		published = published || tx.TxHash() == fp.feeHash
	}
	if published != want {
		t.Errorf("fee publication eligibility = %t, want %t", published, want)
	}
	outputs, err := w.UnspentOutputs(context.Background(), wallet.OutputSelectionPolicy{Account: 0, RequiredConfirmations: 0})
	if err != nil {
		t.Fatal(err)
	}
	var spendable bool
	for _, output := range outputs {
		spendable = spendable || output.OutPoint.Hash == fp.feeHash
	}
	if spendable != want {
		t.Errorf("fee change spendability = %t, want %t", spendable, want)
	}
}

func TestVSPAlreadyReceivedDoesNotPublishFeeChange(t *testing.T) {
	c, fp, _ := publicationTestJob(t, false)
	c.Transport = publicationTestTransport(t, fp, "received", codeFeeAlreadyReceived)
	assertFeePublication(t, fp, false)
	fp.testTask("reconcile payment", fp.reconcilePayment)()
	fp.schedule("", nil)
	if fp.err != nil {
		t.Fatalf("reconcile accepted fee: %v", fp.err)
	}
	assertFeePublication(t, fp, false)
}

func TestVSPSignedBroadcastMakesSavedFeeChangeSpendable(t *testing.T) {
	c, fp, _ := publicationTestJob(t, false)
	c.Transport = publicationTestTransport(t, fp, "broadcast", 0)
	assertFeePublication(t, fp, false)
	fp.testTask("confirm payment", fp.confirmPayment)()
	fp.schedule("", nil)
	if fp.err != nil {
		t.Fatalf("confirm broadcast fee: %v", fp.err)
	}
	assertFeePublication(t, fp, true)
}

func TestVSPBroadcastRequiresMatchingSavedFee(t *testing.T) {
	for _, invalid := range []string{"empty", "different", "blocked"} {
		t.Run(invalid, func(t *testing.T) {
			c, fp, _ := publicationTestJob(t, false)
			providerHash := fp.feeHash.String()
			switch invalid {
			case "empty":
				providerHash = ""
			case "different":
				providerHash = strings.Repeat("1", 64)
			case "blocked":
				fp.blocked = true
			}
			c.Transport = publicationTestTransport(t, fp, "broadcast", 0, providerHash)
			fp.testTask("confirm payment", fp.confirmPayment)()
			fp.timerMu.Lock()
			pending := fp.timer != nil && !fp.nextRetry.IsZero() && fp.phase == "confirm payment"
			fp.timerMu.Unlock()
			fp.schedule("", nil)
			if fp.err == nil {
				t.Error("unsafe broadcast claim was accepted")
			}
			if !pending {
				t.Error("unsafe broadcast claim stopped confirmation polling")
			}
			assertFeePublication(t, fp, false)
		})
	}
}

func TestVSPDatabaseFailureRetainsBackgroundRetry(t *testing.T) {
	for _, stage := range []string{"accepted payment", "already received payment", "broadcast confirmation"} {
		t.Run(stage, func(t *testing.T) {
			c, fp, gate := publicationTestJob(t, true)
			broadcast := stage == "broadcast confirmation"
			status := "received"
			if broadcast {
				status = "broadcast"
			}
			paymentCode := 0
			switch stage {
			case "already received payment":
				paymentCode = codeFeeAlreadyReceived
			}
			c.Transport = publicationTestTransport(t, fp, status, paymentCode)
			commitErr := errors.New("injected VSP database commit failure")
			failed := false
			gate.beforeCommit = func(walletdb.ReadWriteTx) error {
				failed = true
				gate.beforeCommit = nil
				return commitErr
			}
			name, method := "reconcile payment", fp.reconcilePayment
			if broadcast {
				name, method = "confirm payment", fp.confirmPayment
			}
			fp.testTask(name, method)()
			if !failed || !errors.Is(fp.err, commitErr) {
				t.Errorf("database failure was not returned: attempted=%t err=%v", failed, fp.err)
			}
			fp.timerMu.Lock()
			pending := fp.timer != nil && !fp.nextRetry.IsZero()
			phase := fp.phase
			fp.timerMu.Unlock()
			if !pending {
				t.Errorf("database failure stranded tracked job: phase=%q", phase)
			}
			fp.schedule("", nil)
			if got := c.jobs[fp.ticketHash]; got != fp {
				t.Error("database failure removed the tracked job")
			}
			saved, err := c.Wallet.VSPTicketInfo(context.Background(), &fp.ticketHash)
			if err != nil || saved.FeeHash != fp.feeHash || fp.feeTx.TxHash() != fp.feeHash {
				t.Fatalf("failed database transition changed the saved payment: %v", err)
			}
			assertFeePublication(t, fp, false)
		})
	}
}
