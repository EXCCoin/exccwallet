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
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/txscript/v4"
	"github.com/EXCCoin/exccd/wire"
	walletErrors "github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

func signedRecoveryFee(t *testing.T, w *wallet.Wallet, id byte) *wire.MsgTx {
	t.Helper()
	ctx := context.Background()
	address, err := w.NewExternalAddress(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	funding := feeTopUpFunding(t, w, address, 100000, id)
	op := wire.OutPoint{Hash: funding.TxHash(), Tree: wire.TxTreeRegular}
	fee := wire.NewMsgTx()
	fee.AddTxIn(wire.NewTxIn(&op, 100000, nil))
	version, script := address.PaymentScript()
	fee.AddTxOut(&wire.TxOut{Value: 90000, Version: version, PkScript: script})
	sigErrors, err := w.SignTransaction(ctx, fee, txscript.SigHashAll, nil, nil, nil)
	if err != nil || len(sigErrors) != 0 {
		t.Fatalf("sign fee: %v %v", err, sigErrors)
	}
	return fee
}

func TestErroredPaymentRetainsProvider(t *testing.T) {
	for _, different := range []string{"host", "key", "port", "path"} {
		t.Run(different, func(t *testing.T) {
			w, _, ticket := feeTestWallet(t)
			ctx := context.Background()
			hash := ticket.TxHash()
			original := testFeeClient(w)
			defer original.Close()
			fee := signedRecoveryFee(t, w, 150)
			if committed, err := w.RecordVSPFee(ctx, &hash, fee, original.url, original.pub); err != nil || !committed {
				t.Fatalf("record original: %v", err)
			}
			other := testFeeClient(w)
			defer other.Close()
			switch different {
			case "host":
				other.url = "https://other-vsp.example"
			case "key":
				other.pub = bytes.Repeat([]byte{2}, 32)
			case "port":
				other.url = "https://vsp.example:444"
			case "path":
				other.url = "https://vsp.example/other"
			}
			if err := w.UpdateVspTicketFeeToErrored(ctx, &hash, other.url, other.pub); err != nil {
				t.Fatal(err)
			}
			var requests atomic.Int32
			other.Transport = vspTestTransport(func(*http.Request) (*http.Response, error) {
				requests.Add(1)
				return nil, errors.New("unexpected provider request")
			})
			if err := other.ProcessWithPolicy(ctx, &hash, wire.NewMsgTx(), Policy{MaxFee: 1e8}); err == nil {
				t.Fatal("another provider accepted the existing payment")
			}
			info, err := w.VSPTicketInfo(ctx, &hash)
			if err != nil || info.FeeHash != fee.TxHash() || info.Host != original.url || !bytes.Equal(info.PubKey, original.pub) || info.FeeTxStatus != uint32(udb.VSPFeeProcessErrored) {
				t.Fatalf("original payment identity changed: %+v %v", info, err)
			}
			if requests.Load() != 0 || len(other.jobs) != 0 {
				t.Fatal("mismatched provider started processing the existing payment")
			}
		})
	}
}

func TestSavedVSPFeeAcceptsCanonicalProviderAlias(t *testing.T) {
	w, _, ticket := feeTestWallet(t)
	ctx := context.Background()
	hash := ticket.TxHash()
	fee := signedRecoveryFee(t, w, 156)
	feeHash := fee.TxHash()
	pub := bytes.Repeat([]byte{1}, 32)
	const alias, canonical = "https://VSP.EXAMPLE:443/", "https://vsp.example"
	if committed, err := w.RecordVSPFee(ctx, &hash, fee, alias, pub); err != nil || !committed {
		t.Fatal(err)
	}
	// Model an existing database which saved the originally entered URL.
	if err := w.UpdateVspTicketFeeToPaid(ctx, &hash, &feeHash, alias, pub); err != nil {
		t.Fatal(err)
	}
	if committed, err := w.RecordVSPFee(ctx, &hash, fee, canonical, pub); err != nil || !committed {
		t.Fatalf("canonical alias conflicted with the same payment: %v", err)
	}
	if err := w.UpdateVspTicketFeeToPaid(ctx, &hash, &feeHash, alias, pub); err != nil {
		t.Fatal(err)
	}
	if err := w.ResetVSPTicketFee(ctx, &hash, &feeHash, canonical, pub); err != nil {
		t.Fatalf("canonical alias could not reset the same payment: %v", err)
	}
}

func TestRecordVSPFeeRejectsConflictingAssociation(t *testing.T) {
	w, _, ticket := feeTestWallet(t)
	ctx := context.Background()
	hash := ticket.TxHash()
	fees := []*wire.MsgTx{signedRecoveryFee(t, w, 151), signedRecoveryFee(t, w, 152)}
	hosts := []string{"https://first-vsp.example", "https://second-vsp.example"}
	pub := bytes.Repeat([]byte{1}, 32)
	type result struct {
		index     int
		committed bool
		err       error
	}
	start := make(chan struct{})
	results := make(chan result, 2)
	for i := range fees {
		go func() {
			<-start
			committed, err := w.RecordVSPFee(ctx, &hash, fees[i], hosts[i], pub)
			results <- result{i, committed, err}
		}()
	}
	close(start)
	winner, loser := <-results, <-results
	if !winner.committed {
		winner, loser = loser, winner
	}
	if !winner.committed || winner.err != nil || loser.committed || loser.err == nil {
		t.Fatalf("conflicting writes: winner=%+v loser=%+v", winner, loser)
	}
	info, err := w.VSPTicketInfo(ctx, &hash)
	if err != nil || info.FeeHash != fees[winner.index].TxHash() || info.Host != hosts[winner.index] {
		t.Fatalf("winning association was replaced: %+v %v", info, err)
	}
	losingHash := fees[loser.index].TxHash()
	txs, _, err := w.GetTransactionsByHashes(ctx, []*chainhash.Hash{&losingHash})
	if !errors.Is(err, walletErrors.NotExist) || len(txs) != 0 {
		t.Fatalf("conflicting fee was recorded: %v", err)
	}
	if committed, err := w.RecordVSPFee(ctx, &hash, fees[winner.index], hosts[winner.index], pub); err != nil || !committed {
		t.Fatalf("same payment could not be recorded idempotently: %v", err)
	}
}

type feeLookupFailureDB struct {
	walletdb.DB
	fail   atomic.Bool
	failed atomic.Int32
}

func (db *feeLookupFailureDB) BeginReadTx() (walletdb.ReadTx, error) {
	if db.fail.Load() {
		// Target the saved-payment lookup while allowing earlier ticket/key
		// reads to succeed, without coupling the fault to their call count.
		var pcs [16]uintptr
		frames := runtime.CallersFrames(pcs[:runtime.Callers(2, pcs[:])])
		for {
			frame, more := frames.Next()
			if strings.HasSuffix(frame.Function, ".VSPFeeHashForTicket") {
				db.failed.Add(1)
				return nil, errFeeSave
			}
			if !more {
				break
			}
		}
	}
	return db.DB.BeginReadTx()
}

func TestFeeConstructorDoesNotReplacePaymentAfterLookupFailure(t *testing.T) {
	var gate *feeLookupFailureDB
	driver := "vsp-test-fee-lookup-" + t.TempDir()
	if err := walletdb.RegisterDriver(walletdb.Driver{DbType: driver, Create: func(args ...interface{}) (walletdb.DB, error) {
		db, err := walletdb.Create("bdb", args...)
		if err != nil {
			return nil, err
		}
		gate = &feeLookupFailureDB{DB: db}
		return gate, nil
	}}); err != nil {
		t.Fatal(err)
	}
	w, _, ticket := feeTestWalletDriver(t, driver)
	ctx := context.Background()
	hash := ticket.TxHash()
	c := testFeeClient(w)
	defer c.Close()
	fee := signedRecoveryFee(t, w, 153)
	if committed, err := w.RecordVSPFee(ctx, &hash, fee, c.url, c.pub); err != nil || !committed {
		t.Fatal(err)
	}
	if err := w.UpdateVspTicketFeeToErrored(ctx, &hash, c.url, c.pub); err != nil {
		t.Fatal(err)
	}
	gate.fail.Store(true)
	if fp := c.feePaymentWithTx(&hash, Policy{}, false, wire.NewMsgTx()); fp != nil {
		t.Fatal("failed payment lookup was treated as permission to create a fee")
	}
	if gate.failed.Load() != 1 || len(c.jobs) != 0 {
		t.Fatal("saved-payment failure was not exercised or left an active job")
	}
	gate.fail.Store(false)
	fp := c.feePaymentWithTx(&hash, Policy{}, false, wire.NewMsgTx())
	if fp == nil {
		t.Fatal("payment was not recoverable after storage recovered")
	}
	fp.schedule("", nil)
	if fp.feeTx == nil || fp.feeHash != fee.TxHash() || fp.feeTx.TxHash() != fee.TxHash() {
		t.Fatal("recovery did not reuse the saved signed payment")
	}
}

func TestExpiredFeeRecoveryIsDurableAndDoesNotReplaceAcceptedPayment(t *testing.T) {
	for _, scenario := range []string{"expired", "reset rollback", "confirmed payment", "already received"} {
		t.Run(scenario, func(t *testing.T) {
			var gate *atomicFeeDB
			driver := "vsp-test-expired-fee-" + t.TempDir()
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
			w, db, template := feeTestWalletDriver(t, driver)
			ctx := context.Background()
			address, err := w.NewExternalAddress(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			parent := feeTopUpFunding(t, w, address, 100, 154)
			ticket := template.Copy()
			ticket.TxIn[0].PreviousOutPoint.Hash = parent.TxHash()
			sigErrors, err := w.SignTransaction(ctx, ticket, txscript.SigHashAll, nil, nil, nil)
			if err != nil || len(sigErrors) != 0 {
				t.Fatalf("sign ticket: %v %v", err, sigErrors)
			}
			if err := w.AddTransaction(ctx, ticket, nil); err != nil {
				t.Fatal(err)
			}
			hash := ticket.TxHash()
			c := testFeeClient(w)
			defer c.Close()
			pub, key := vspTestKeys()
			c.pub = pub
			fee := signedRecoveryFee(t, w, 155)
			feeHash := fee.TxHash()
			if committed, err := w.RecordVSPFee(ctx, &hash, fee, c.url, c.pub); err != nil || !committed {
				t.Fatal(err)
			}
			code := codeFeeExpired
			if scenario == "already received" {
				// EXCC vspd returns this before checking expiration for fees
				// already received, broadcast, or confirmed by the provider.
				code = codeFeeAlreadyReceived
			}
			transport := vspTestTransport(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					return nil, err
				}
				status := http.StatusBadRequest
				var response interface{} = &BadRequestError{Code: code, Message: "provider rejected fee"}
				if req.URL.Path == "/api/v3/feeaddress" {
					status = http.StatusOK
					response = struct {
						FeeAddress string `json:"feeaddress"`
						FeeAmount  int64  `json:"feeamount"`
						Request    []byte `json:"request"`
					}{address.String(), 35000, body}
				} else if req.URL.Path != "/api/v3/payfee" {
					t.Errorf("unexpected request %s", req.URL.Path)
				}
				data, err := json.Marshal(response)
				if err != nil {
					return nil, err
				}
				return &http.Response{StatusCode: status, Header: http.Header{
					"Vsp-Server-Signature": []string{base64.StdEncoding.EncodeToString(ed25519.Sign(key, data))},
				}, Body: io.NopCloser(bytes.NewReader(data))}, nil
			})
			c.Transport = transport
			fp := c.feePaymentWithTx(&hash, Policy{MaxFee: 1e8}, true, nil)
			if fp == nil {
				t.Fatal("saved fee was not loaded")
			}
			fp.schedule("", nil)
			fp.fee, fp.feeAddr = 30000, address
			if scenario == "confirmed payment" {
				if err := w.UpdateVspTicketFeeToConfirmed(ctx, &hash, &feeHash, c.url, c.pub); err != nil {
					t.Fatal(err)
				}
			}
			resetAttempted := false
			if scenario == "reset rollback" {
				gate.beforeCommit = func(dbtx walletdb.ReadWriteTx) error {
					info, err := udb.GetVSPTicket(dbtx, hash)
					if err == nil && info.FeeHash == (chainhash.Hash{}) {
						resetAttempted = true
						return errFeeSave
					}
					return nil
				}
			}
			err = fp.submitPayment()
			var apiErr *BadRequestError
			if !errors.As(err, &apiErr) || apiErr.Code != code {
				t.Fatalf("signed provider error was not preserved: %v", err)
			}
			fp.timerMu.Lock()
			scheduled := fp.timer != nil && fp.phase == "reconcile payment" && !fp.nextRetry.IsZero()
			fp.timerMu.Unlock()
			fp.schedule("", nil)
			cleared := scenario == "expired"
			if cleared {
				if fp.feeTx != nil || fp.feeHash != (chainhash.Hash{}) || fp.fee != 0 || fp.feeAddr != nil || !scheduled {
					t.Fatal("expired quote was not cleared and rescheduled")
				}
			} else if fp.feeTx == nil || fp.feeHash != feeHash || fp.fee != 30000 || fp.feeAddr != address || scheduled {
				t.Fatal("unreset payment or quote was replaced")
			}
			if scenario == "reset rollback" && (!resetAttempted || !errors.Is(err, errFeeSave)) {
				t.Fatal("reset commit failure was not exercised")
			}
			gate.beforeCommit = nil
			c.Close()
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopenedDB, err := wallet.OpenDB("bdb", gate.path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopenedDB.Close()
			reopened, err := wallet.Open(ctx, &wallet.Config{DB: reopenedDB, PubPassphrase: []byte(wallet.InsecurePubPassphrase), Params: w.ChainParams(), GapLimit: 20})
			if err != nil {
				t.Fatal(err)
			}
			info, err := reopened.VSPTicketInfo(ctx, &hash)
			if err != nil {
				t.Fatal(err)
			}
			txs, _, err := reopened.GetTransactionsByHashes(ctx, []*chainhash.Hash{&feeHash})
			if cleared {
				if info.FeeHash != (chainhash.Hash{}) || info.FeeTxStatus != uint32(udb.VSPFeeProcessErrored) || len(txs) != 0 || !errors.Is(err, walletErrors.NotExist) {
					t.Fatalf("expired fee reset was not durable: %+v %v", info, err)
				}
				if err := reopened.Unlock(ctx, []byte("test-wallet-password"), nil); err != nil {
					t.Fatal(err)
				}
				recovered := testFeeClient(reopened)
				defer recovered.Close()
				recovered.pub, recovered.Transport = pub, transport
				job := recovered.feePaymentWithTx(&hash, Policy{MaxFee: 1e8}, false, nil)
				if job == nil {
					t.Fatal("expired payment could not restart")
				}
				job.schedule("", nil)
				if job.blocked || job.feeTx != nil || job.feeHash != (chainhash.Hash{}) {
					t.Fatal("expired payment restarted with stale transaction")
				}
				if err := job.receiveFeeAddress(); err != nil || job.fee != 35000 {
					t.Fatalf("restart did not fetch a fresh quote: %v", err)
				}
			} else if err != nil || len(txs) != 1 || info.FeeHash != feeHash || len(txs[0].TxIn[0].SignatureScript) == 0 {
				t.Fatalf("unreset signed fee did not survive reopen: %+v %v", info, err)
			}
			if info.Host != c.url || !bytes.Equal(info.PubKey, c.pub) {
				t.Fatal("recovery changed the provider identity")
			}
		})
	}
}
