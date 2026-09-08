package vsp

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/blockchain/v4/chaingen"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccd/gcs/v3/blockcf2"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	_ "github.com/EXCCoin/exccwallet/v2/wallet/drivers/bdb"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

func feeTestWallet(t *testing.T) (*wallet.Wallet, wallet.DB, *wire.MsgTx) {
	return feeTestWalletDriver(t, "bdb")
}

func feeTestWalletDriver(t *testing.T, driver string) (*wallet.Wallet, wallet.DB, *wire.MsgTx) {
	t.Helper()
	ctx := context.Background()
	db, err := wallet.CreateDB(driver, filepath.Join(t.TempDir(), "wallet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	params := chaincfg.SimNetParams()
	pub, priv := []byte(wallet.InsecurePubPassphrase), []byte("test-wallet-password")
	err = wallet.Create(ctx, db, pub, priv, bytes.Repeat([]byte{1}, 32), params)
	if err != nil {
		t.Fatal(err)
	}
	w, err := wallet.Open(ctx, &wallet.Config{DB: db, PubPassphrase: pub, Params: params, GapLimit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Unlock(ctx, priv, nil); err != nil {
		t.Fatal(err)
	}
	address, err := w.NewExternalAddress(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	addr := address.(stdaddr.StakeAddress)
	ticket := wire.NewMsgTx()
	ticket.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{1}}, 100, nil))
	version, script := addr.VotingRightsScript()
	ticket.AddTxOut(&wire.TxOut{Value: 100, Version: version, PkScript: script})
	version, script = addr.RewardCommitmentScript(100, 0, 0)
	ticket.AddTxOut(&wire.TxOut{Version: version, PkScript: script})
	version, script = addr.StakeChangeScript()
	ticket.AddTxOut(&wire.TxOut{Version: version, PkScript: script})
	if err := w.AddTransaction(ctx, ticket, nil); err != nil {
		t.Fatal(err)
	}
	return w, db, ticket
}

type gatedFeeDB struct {
	walletdb.DB
	mu         sync.Mutex
	beforeRead func()
}

func (db *gatedFeeDB) BeginReadTx() (walletdb.ReadTx, error) {
	db.mu.Lock()
	before := db.beforeRead
	db.mu.Unlock()
	if before != nil {
		before()
	}
	return db.DB.BeginReadTx()
}

func TestPruningRejectsAnInFlightFeeConstructor(t *testing.T) {
	var gate *gatedFeeDB
	driver := "vsp-test-constructor-gate-" + t.TempDir()
	err := walletdb.RegisterDriver(walletdb.Driver{DbType: driver, Create: func(args ...interface{}) (walletdb.DB, error) {
		db, err := walletdb.Create("bdb", args...)
		if err != nil {
			return nil, err
		}
		gate = &gatedFeeDB{DB: db}
		return gate, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	w, _, ticket := feeTestWalletDriver(t, driver)
	c := testFeeClient(w)
	defer c.Close()
	hash := ticket.TxHash()
	entered, release := make(chan struct{}), make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	var once sync.Once
	gate.mu.Lock()
	gate.beforeRead = func() { once.Do(func() { close(entered); <-release }) }
	gate.mu.Unlock()
	done := make(chan *feePayment, 1)
	go func() { done <- c.feePayment(&hash, Policy{}, false) }()
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("fee constructor did not reach wallet lookup")
	}
	c.RemoveTicket(hash, "pruned")
	close(release)
	if fp := <-done; fp != nil {
		t.Fatal("constructor revived a pruned job")
	}
	if len(c.TrackedTickets()) != 0 {
		t.Fatal("pruned job remains tracked")
	}
	// The generation guard must not permanently forbid the same transaction
	// if a later rescan restores it.
	if fp := c.feePayment(&hash, Policy{}, false); fp == nil {
		t.Fatal("later explicit recovery was blocked")
	}
}

func testFeeClient(w *wallet.Wallet) *Client {
	return &Client{Wallet: w, client: newClient("https://vsp.example", bytes.Repeat([]byte{1}, 32), w), jobs: make(map[chainhash.Hash]*feePayment)}
}

func TestFeeErrorPreservesOriginalAfterSavingStatus(t *testing.T) {
	w, _, ticket := feeTestWallet(t)
	c := testFeeClient(w)
	defer c.Close()
	hash := ticket.TxHash()
	err := c.ProcessWithPolicy(context.Background(), &hash, wire.NewMsgTx(), Policy{})
	if err == nil || !strings.Contains(err.Error(), "failed to retrieve parent") {
		t.Fatalf("original fee failure was lost: %v", err)
	}
	info, err := w.VSPTicketInfo(context.Background(), &hash)
	if err != nil {
		t.Fatal(err)
	}
	if info.FeeTxStatus != uint32(udb.VSPFeeProcessErrored) {
		t.Fatalf("fee state = %d", info.FeeTxStatus)
	}
}

func TestFeeConfirmationPersistsBeforeRemovingJob(t *testing.T) {
	for _, failDB := range []bool{false, true} {
		t.Run(map[bool]string{false: "confirmed", true: "database failure"}[failDB], func(t *testing.T) {
			w, db, ticket := feeTestWallet(t)
			c := testFeeClient(w)
			defer c.Close()
			hash, fee := ticket.TxHash(), chainhash.Hash{2}
			ctx, cancel := context.WithCancel(context.Background())
			fp := &feePayment{client: c, ctx: ctx, cancel: cancel, ticketHash: hash, feeHash: fee}
			c.jobs[hash] = fp
			if err := w.UpdateVspTicketFeeToPaid(ctx, &hash, &fee, c.url, c.pub); err != nil {
				t.Fatal(err)
			}
			if failDB {
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			err := fp.recordConfirmedFee()
			c.mu.Lock()
			_, tracked := c.jobs[hash]
			c.mu.Unlock()
			if failDB {
				if err == nil || !tracked || fp.nextRetry.IsZero() {
					t.Fatalf("failed confirmation lost job/retry: err=%v tracked=%v retry=%v", err, tracked, fp.nextRetry)
				}
				return
			}
			if err != nil || tracked {
				t.Fatalf("confirmed job retained: err=%v tracked=%v", err, tracked)
			}
			confirmed, err := w.IsVSPTicketConfirmed(context.Background(), &hash)
			if err != nil || !confirmed {
				t.Fatalf("confirmation missing from database: %v, %v", confirmed, err)
			}
		})
	}
}

func TestMissingFeeTransactionCannotCreateReplacement(t *testing.T) {
	w, _, ticket := feeTestWallet(t)
	c := testFeeClient(w)
	defer c.Close()
	hash, fee := ticket.TxHash(), chainhash.Hash{2}
	if err := w.UpdateVspTicketFeeToPaid(context.Background(), &hash, &fee, c.url, c.pub); err != nil {
		t.Fatal(err)
	}
	fp := c.feePayment(&hash, Policy{}, true)
	if fp == nil {
		t.Fatal("missing fee should remain visible for status checks")
	}
	if err := fp.makeFeeTx(nil); err == nil || !strings.Contains(err.Error(), "replacement payment requires review") {
		t.Fatalf("missing fee allowed replacement: %v", err)
	}
	if fp.feeTx != nil || fp.feeHash != fee {
		t.Fatal("saved fee identity was replaced")
	}
	fp.mu.Lock()
	fp.err = nil // A successful status poll does not restore the missing transaction.
	fp.mu.Unlock()
	if jobs := c.TrackedTickets(); len(jobs) != 1 || jobs[0].LastError != errMissingFee.Error() {
		t.Fatalf("missing payment lost its review reason: %+v", jobs)
	}
}

func TestExpiredUnminedTicketCannotCreateFee(t *testing.T) {
	w, _, ticket := feeTestWallet(t)
	ctx := context.Background()
	ticket = ticket.Copy()
	ticket.TxIn[0].PreviousOutPoint.Hash = chainhash.Hash{2}
	ticket.Expiry = 1
	if err := w.AddTransaction(ctx, ticket, nil); err != nil {
		t.Fatal(err)
	}
	c := testFeeClient(w)
	defer c.Close()
	hash := ticket.TxHash()
	fp := c.feePayment(&hash, Policy{}, false)
	if fp == nil {
		t.Fatal("ticket was not tracked before expiry")
	}
	fp.schedule("", nil)
	g, err := chaingen.MakeGenerator(w.ChainParams(), nil)
	if err != nil {
		t.Fatal(err)
	}
	block := g.CreateBlockOne("block1", 0)
	filter, err := blockcf2.Regular(block, nil)
	if err != nil {
		t.Fatal(err)
	}
	blockHash := block.BlockHash()
	node := &wallet.BlockNode{Header: &block.Header, Hash: &blockHash, FilterV2: filter, RelevantTxs: []*wire.MsgTx{}}
	var forest wallet.SidechainForest
	forest.AddBlockNode(node)
	if _, err := w.ChainSwitch(ctx, &forest, []*wallet.BlockNode{node}); err != nil {
		t.Fatal(err)
	}
	if err := fp.makeFeeTx(nil); err != errStopped {
		t.Fatalf("expired ticket attempted fee creation: %v", err)
	}
	if fp.feeTx != nil || len(c.TrackedTickets()) != 0 {
		t.Fatal("expired ticket retained a fee transaction or job")
	}
}

func TestOldFeeCallbackCannotRemoveReplacementJob(t *testing.T) {
	w, _, ticket := feeTestWallet(t)
	c := testFeeClient(w)
	defer c.Close()
	hash := ticket.TxHash()
	old := c.feePayment(&hash, Policy{}, false)
	if old == nil {
		t.Fatal("old job was not created")
	}
	c.RemoveTicket(hash, "pruned")
	replacement := c.feePayment(&hash, Policy{}, false)
	if replacement == nil || replacement == old {
		t.Fatal("replacement job was not created")
	}
	old.remove("stale callback")
	c.mu.Lock()
	got := c.jobs[hash]
	c.mu.Unlock()
	if got != replacement || replacement.ctx.Err() != nil {
		t.Fatal("stale callback removed the replacement job")
	}
}

func TestManagedRecoveryRestoresUnminedJobsAndReportsEachOutcome(t *testing.T) {
	w, _, template := feeTestWallet(t)
	ctx := context.Background()
	pub, key := vspTestKeys()
	hashes := make([]chainhash.Hash, 3)
	server := httptest.NewTLSServer(http.HandlerFunc(func(out http.ResponseWriter, req *http.Request) {
		if req.URL.Path != "/api/v3/ticketstatus" {
			t.Errorf("unexpected payment request: %s", req.URL.Path)
			out.WriteHeader(500)
			return
		}
		body, err := io.ReadAll(req.Body)
		if err != nil {
			t.Error(err)
			return
		}
		var request struct {
			TicketHash string `json:"tickethash"`
		}
		if err := json.Unmarshal(body, &request); err != nil {
			t.Error(err)
			return
		}
		switch request.TicketHash {
		case hashes[1].String():
			data, _ := json.Marshal(&BadRequestError{Code: codeUnknownTicket, Message: "ticket is unknown"})
			writeVSPTestResponse(out, key, http.StatusBadRequest, data)
		case hashes[2].String():
			data, _ := json.Marshal(&BadRequestError{Code: codeBadSignature, Message: "request could not be verified"})
			writeVSPTestResponse(out, key, http.StatusBadRequest, data)
		default:
			info, err := w.VSPTicketInfo(ctx, &hashes[0])
			if err != nil {
				t.Error(err)
				return
			}
			data, _ := json.Marshal(ticketStatus{FeeTxStatus: "received", FeeTxHash: info.FeeHash.String(), Request: body})
			writeVSPTestResponse(out, key, http.StatusOK, data)
		}
	}))
	defer server.Close()
	c := testFeeClient(w)
	c.url, c.pub = server.URL, pub
	c.Transport = server.Client().Transport
	defer c.Close()
	addr, _, err := parseTicket(template, w.ChainParams())
	if err != nil {
		t.Fatal(err)
	}
	version, script := addr.PaymentScript()
	fee := wire.NewMsgTx()
	fee.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{9}}, 10, nil))
	fee.AddTxOut(&wire.TxOut{Value: 10, Version: version, PkScript: script})
	if err := w.AddTransaction(ctx, fee, nil); err != nil {
		t.Fatal(err)
	}
	feeHash := fee.TxHash()
	for i := range hashes {
		ticket := template.Copy()
		ticket.TxIn[0].PreviousOutPoint.Hash[0] = byte(20 + i)
		hashes[i] = ticket.TxHash()
		if err := w.AddTransaction(ctx, ticket, nil); err != nil {
			t.Fatal(err)
		}
		if err := w.UpdateVspTicketFeeToPaid(ctx, &hashes[i], &feeHash, c.url+"/", c.pub); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := range 2 {
		results, err := c.RecoverManagedTicketsWithResults(ctx, Policy{})
		if err == nil || len(results) != 3 {
			t.Fatalf("partial recovery lost outcomes: %v %v", results, err)
		}
		want := map[chainhash.Hash]ManagedTicketStatus{hashes[0]: ManagedTicketStarted, hashes[1]: ManagedTicketUnknownToVSP, hashes[2]: ManagedTicketFailed}
		if attempt != 0 {
			want[hashes[0]] = ManagedTicketAlreadyTracked
		}
		for _, result := range results {
			if result.Status != want[result.TicketHash] {
				t.Errorf("ticket %v outcome = %v, want %v", result.TicketHash, result.Status, want[result.TicketHash])
			}
			if (result.Err != nil) != (result.Status == ManagedTicketFailed) {
				t.Errorf("unexpected result error: %+v", result)
			}
		}
		if jobs := c.TrackedTickets(); len(jobs) != 1 || jobs[0].TicketHash != hashes[0] {
			t.Fatalf("unmined tracking was not restored exactly once: %+v", jobs)
		}
	}
}
