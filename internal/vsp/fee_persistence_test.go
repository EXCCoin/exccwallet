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
	"sync/atomic"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/txscript/v4"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/wire"
	walletErrors "github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	"github.com/EXCCoin/exccwallet/v2/wallet/txrules"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

type atomicFeeDB struct {
	walletdb.DB
	path         string
	beforeCommit func(walletdb.ReadWriteTx) error
}

type atomicFeeTx struct {
	walletdb.ReadWriteTx
	db *atomicFeeDB
}

func (db *atomicFeeDB) BeginReadWriteTx() (walletdb.ReadWriteTx, error) {
	tx, err := db.DB.BeginReadWriteTx()
	if err != nil {
		return nil, err
	}
	return &atomicFeeTx{ReadWriteTx: tx, db: db}, nil
}

func (tx *atomicFeeTx) Commit() error {
	if tx.db.beforeCommit != nil {
		if err := tx.db.beforeCommit(tx.ReadWriteTx); err != nil {
			_ = tx.Rollback()
			return err
		}
	}
	return tx.ReadWriteTx.Commit()
}

type feeWatchFailureNetwork struct {
	wallet.NetworkBackend
	stored *atomic.Bool
}

func (n *feeWatchFailureNetwork) LoadTxFilter(context.Context, bool, []stdaddr.Address, []wire.OutPoint) error {
	if n.stored.Load() {
		return errFeeSave
	}
	return nil
}

func TestProcessFeePersistenceSurvivesFailureAndReopen(t *testing.T) {
	for _, failure := range []string{"commit rollback", "watch after commit"} {
		t.Run(failure, func(t *testing.T) {
			var gate *atomicFeeDB
			driver := "vsp-test-atomic-fee-" + t.TempDir()
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
			w.SetRelayFee(txrules.DefaultRelayFeePerKb)
			ctx := context.Background()
			address, err := w.NewExternalAddress(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			parent := feeTopUpFunding(t, w, address, 100, 130)
			ticket := template.Copy()
			ticket.TxIn[0].PreviousOutPoint.Hash = parent.TxHash()
			sigErrors, err := w.SignTransaction(ctx, ticket, txscript.SigHashAll, nil, nil, nil)
			if err != nil || len(sigErrors) != 0 {
				t.Fatalf("sign ticket: %v %v", err, sigErrors)
			}
			if err := w.AddTransaction(ctx, ticket, nil); err != nil {
				t.Fatal(err)
			}
			funding := feeTopUpFunding(t, w, address, 1e8, 131)
			op := wire.OutPoint{Hash: funding.TxHash(), Tree: wire.TxTreeRegular}
			w.LockOutpoint(&op.Hash, op.Index)
			provided := wire.NewMsgTx()
			provided.AddTxIn(wire.NewTxIn(&op, funding.TxOut[0].Value, nil))
			c := testFeeClient(w)
			defer c.Close()
			pub, key := vspTestKeys()
			c.pub = pub
			c.Transport = vspTestTransport(func(req *http.Request) (*http.Response, error) {
				body, _ := io.ReadAll(req.Body)
				var response []byte
				switch req.URL.Path {
				case "/api/v3/feeaddress":
					response, _ = json.Marshal(struct {
						FeeAddress string `json:"feeaddress"`
						FeeAmount  int64  `json:"feeamount"`
						Request    []byte `json:"request"`
					}{address.String(), 30000, body})
				case "/api/v3/payfee":
					response, _ = json.Marshal(struct {
						Request []byte `json:"request"`
					}{body})
				default:
					t.Errorf("unexpected request %s", req.URL.Path)
				}
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Vsp-Server-Signature": []string{base64.StdEncoding.EncodeToString(ed25519.Sign(key, response))}}, Body: io.NopCloser(bytes.NewReader(response))}, nil
			})
			hash := ticket.TxHash()
			fp := c.feePaymentWithTx(&hash, Policy{MaxFee: 1e8}, false, provided)
			if fp == nil {
				t.Fatal("missing worker")
			}
			fp.schedule("", nil)
			var attempted chainhash.Hash
			var stored atomic.Bool
			gate.beforeCommit = func(dbtx walletdb.ReadWriteTx) error {
				info, err := udb.GetVSPTicket(dbtx, hash)
				if err != nil || info == nil || info.FeeTxStatus != uint32(udb.VSPFeeProcessPaid) {
					return nil
				}
				attempted = info.FeeHash
				if failure == "commit rollback" {
					gate.beforeCommit = nil
					return errFeeSave
				}
				stored.Store(true)
				return nil
			}
			if failure == "watch after commit" {
				w.SetNetworkBackend(&feeWatchFailureNetwork{stored: &stored})
			}
			notifications := w.NtfnServer.TransactionNotifications()
			defer notifications.Done()
			events := make(chan *wallet.TransactionNotifications, 4)
			go func() {
				for event := range notifications.C {
					events <- event
				}
			}()

			err = c.ProcessWithPolicy(ctx, &hash, provided, Policy{MaxFee: 1e8})
			if !errors.Is(err, errFeeSave) {
				t.Fatalf("want injected failure, got %v", err)
			}
			info, err := w.VSPTicketInfo(ctx, &hash)
			if err != nil {
				t.Fatal(err)
			}
			txs, _, err := w.GetTransactionsByHashes(ctx, []*chainhash.Hash{&attempted})
			if err != nil && !(failure == "commit rollback" && errors.Is(err, walletErrors.NotExist)) {
				t.Fatal(err)
			}
			if failure == "commit rollback" {
				if info.FeeHash != (chainhash.Hash{}) || len(txs) != 0 || len(provided.TxOut) != 0 {
					t.Fatal("failed atomic commit left a transaction or fee association")
				}
				if !w.LockedOutpoint(&op.Hash, op.Index) {
					t.Fatal("rollback released the caller's reserved input")
				}
			} else {
				if len(txs) != 1 || info.FeeHash != attempted || fp.feeHash != attempted || provided.TxHash() != attempted {
					t.Fatal("post-commit failure lost the stored payment or association")
				}
			}
			select {
			case event := <-events:
				if failure == "commit rollback" {
					t.Fatal("rolled-back fee emitted a transaction notification")
				}
				if len(event.UnminedTransactions) != 1 {
					t.Fatal("committed fee notification did not contain one transaction")
				}
			case <-time.After(100 * time.Millisecond):
				if failure == "watch after commit" {
					t.Fatal("committed fee did not notify its subscriber")
				}
			}

			gate.beforeCommit = nil
			w.SetNetworkBackend(nil)
			if err := c.ProcessWithPolicy(ctx, &hash, provided, Policy{MaxFee: 1e8}); err != nil {
				t.Fatal(err)
			}
			select {
			case <-events:
				if failure != "commit rollback" {
					t.Fatal("retry notified the same committed fee twice")
				}
			case <-time.After(100 * time.Millisecond):
				if failure == "commit rollback" {
					t.Fatal("successful retry did not notify its committed fee")
				}
			}

			feeHash := provided.TxHash()
			if failure == "watch after commit" && feeHash != attempted {
				t.Fatal("retry replaced the committed fee")
			}
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
			info, err = reopened.VSPTicketInfo(ctx, &hash)
			if err != nil {
				t.Fatal(err)
			}
			txs, _, err = reopened.GetTransactionsByHashes(ctx, []*chainhash.Hash{&feeHash})
			if err != nil || len(txs) != 1 || len(txs[0].TxIn[0].SignatureScript) == 0 || info.FeeHash != feeHash || info.Host != c.url || !bytes.Equal(info.PubKey, c.pub) {
				t.Fatalf("reopened wallet lost signed payment identity: %+v %v", info, err)
			}
		})
	}
}
