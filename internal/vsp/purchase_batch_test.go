package vsp

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"runtime"
	"strings"
	"testing"

	"github.com/EXCCoin/exccd/blockchain/stake/v4"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/dcrutil/v4"
	"github.com/EXCCoin/exccd/gcs/v3/blockcf2"
	"github.com/EXCCoin/exccd/txscript/v4"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	"github.com/EXCCoin/exccwallet/v2/wallet/txrules"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

type feeBatchNetwork struct {
	wallet.NetworkBackend
	split     *wire.MsgTx
	published int
}

func feeTopUpFunding(t *testing.T, w *wallet.Wallet, address stdaddr.Address, value int64, id byte) *wire.MsgTx {
	t.Helper()
	version, script := address.PaymentScript()
	tx := wire.NewMsgTx()
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{id}}, value, nil))
	tx.AddTxOut(&wire.TxOut{Value: value, Version: version, PkScript: script})
	if err := w.AddTransaction(context.Background(), tx, nil); err != nil {
		t.Fatal(err)
	}
	return tx
}

func recordingVSPFee() bool {
	var callers [20]uintptr
	frames := runtime.CallersFrames(callers[:runtime.Callers(2, callers[:])])
	for {
		frame, more := frames.Next()
		if strings.HasSuffix(frame.Function, ".(*Wallet).RecordVSPFee") {
			return true
		}
		if !more {
			return false
		}
	}
}

func TestFeeTopUpFailurePreservesOriginalInputsForRetry(t *testing.T) {
	for _, failure := range []string{"insufficient", "locked", "cancelled"} {
		t.Run(failure, func(t *testing.T) {
			w, ticket, gate := feeSaveTestWallet(t)
			w.SetRelayFee(txrules.DefaultRelayFeePerKb)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			address, err := w.NewExternalAddress(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			funding := feeTopUpFunding(t, w, address, 30000, 90)
			original := wire.OutPoint{Hash: funding.TxHash(), Tree: wire.TxTreeRegular}
			w.LockOutpoint(&original.Hash, original.Index)
			provided := wire.NewMsgTx()
			provided.AddTxIn(wire.NewTxIn(&original, funding.TxOut[0].Value, nil))
			before := provided.Copy()
			c := testFeeClient(w)
			defer c.Close()
			fp := &feePayment{client: c, ctx: ctx, ticketHash: ticket.TxHash(), fee: 50000, feeAddr: address, feeTx: provided}
			var extra *wire.MsgTx
			if failure != "insufficient" {
				extra = feeTopUpFunding(t, w, address, 1e8, 91)
			}
			switch failure {
			case "locked":
				w.Lock()
			case "cancelled":
				gate.writeError = context.Canceled
				gate.failWrite = func() bool {
					// Fee recording follows funding and signing. The recording
					// path already owns the outpoint lock, so do not reacquire it.
					if !recordingVSPFee() {
						return false
					}
					cancel()
					return true
				}
			}
			err = fp.makeFeeTx(provided)
			if err == nil {
				t.Fatal("expected funding failure")
			}
			if failure == "cancelled" && (!stderrors.Is(err, context.Canceled) || ctx.Err() == nil) {
				t.Fatalf("expected cancellation after top-up reservation, got %v", err)
			}
			if provided.TxHash() != before.TxHash() || len(provided.TxIn) != 1 || len(provided.TxOut) != 0 || len(provided.TxIn[0].SignatureScript) != 0 {
				t.Fatal("failed attempt changed the supplied fee transaction")
			}
			if !w.LockedOutpoint(&original.Hash, original.Index) {
				t.Fatal("caller reservation was released")
			}
			if extra != nil {
				hash := extra.TxHash()
				if w.LockedOutpoint(&hash, 0) {
					t.Fatal("failed attempt retained a top-up reservation")
				}
			}
			// The purchase caller releases its original reservation on failure.
			// A later job retry must not select that same input again as top-up.
			w.UnlockOutpoint(&original.Hash, original.Index)
			if extra == nil {
				extra = feeTopUpFunding(t, w, address, 1e8, 91)
			}
			fp.ctx = context.Background()
			if err := w.Unlock(fp.ctx, []byte("test-wallet-password"), nil); err != nil {
				t.Fatal(err)
			}
			checkedOriginalReservation := false
			gate.failWrite = func() bool {
				if recordingVSPFee() {
					return false
				}
				gate.failWrite = nil
				checkedOriginalReservation = true
				if !w.LockedOutpoint(&original.Hash, original.Index) {
					t.Fatal("retry did not reserve the original input before selecting top-up funds")
				}
				return false
			}
			if err := fp.makeFeeTx(nil); err != nil {
				t.Fatal(err)
			}
			if !checkedOriginalReservation {
				t.Fatal("retry did not reach the funding reservation check")
			}
			if len(fp.feeTx.TxIn) != 2 {
				t.Fatalf("want original plus top-up input, got %d", len(fp.feeTx.TxIn))
			}
			seen := make(map[wire.OutPoint]bool)
			for _, in := range fp.feeTx.TxIn {
				if seen[in.PreviousOutPoint] {
					t.Fatal("retry selected a duplicate input")
				}
				seen[in.PreviousOutPoint] = true
				if w.LockedOutpoint(&in.PreviousOutPoint.Hash, in.PreviousOutPoint.Index) {
					t.Fatal("recorded fee retained an input reservation")
				}
			}
			if !seen[original] || !seen[wire.OutPoint{Hash: extra.TxHash(), Tree: wire.TxTreeRegular}] {
				t.Fatal("retry spent unexpected inputs")
			}
			var paid, inputs, outputs int64
			for _, in := range fp.feeTx.TxIn {
				inputs += in.ValueIn
			}
			for _, out := range fp.feeTx.TxOut {
				outputs += out.Value
				if out.Value == int64(fp.fee) {
					paid += out.Value
				}
			}
			if paid != int64(fp.fee) || inputs-outputs < int64(txrules.FeeForSerializeSize(w.RelayFee(), fp.feeTx.SerializeSize())) {
				t.Fatal("top-up failed to pay the quote and relay fee")
			}
		})
	}
}

type feeSaveFailureDB struct {
	walletdb.DB
	failWrite  func() bool
	writeError error
}

var errFeeSave = stderrors.New("injected fee status save failure")

func (db *feeSaveFailureDB) BeginReadWriteTx() (walletdb.ReadWriteTx, error) {
	if db.failWrite != nil && db.failWrite() {
		db.failWrite = nil
		if db.writeError != nil {
			return nil, db.writeError
		}
		return nil, errFeeSave
	}
	return db.DB.BeginReadWriteTx()
}

func feeSaveTestWallet(t *testing.T) (*wallet.Wallet, *wire.MsgTx, *feeSaveFailureDB) {
	t.Helper()
	var gate *feeSaveFailureDB
	driver := "vsp-test-fee-save-" + t.TempDir()
	if err := walletdb.RegisterDriver(walletdb.Driver{DbType: driver, Create: func(args ...interface{}) (walletdb.DB, error) {
		db, err := walletdb.Create("bdb", args...)
		if err != nil {
			return nil, err
		}
		gate = &feeSaveFailureDB{DB: db}
		return gate, nil
	}}); err != nil {
		t.Fatal(err)
	}
	w, _, ticket := feeTestWalletDriver(t, driver)
	return w, ticket, gate
}

func (n *feeBatchNetwork) PublishTransactions(_ context.Context, txs ...*wire.MsgTx) error {
	for _, tx := range txs {
		if stake.IsSStx(tx) {
			n.published++
		} else {
			n.split = tx
		}
	}
	return nil
}

func (*feeBatchNetwork) LoadTxFilter(context.Context, bool, []stdaddr.Address, []wire.OutPoint) error {
	return nil
}

func TestPurchaseBatchSignsDistinctVSPFeesFromOneInput(t *testing.T) {
	for _, relayFee := range []dcrutil.Amount{txrules.DefaultRelayFeePerKb, 1e3, 0} {
		t.Run(fmt.Sprintf("relay=%d", relayFee), func(t *testing.T) {
			w, _, _ := feeTestWallet(t)
			w.SetRelayFee(relayFee)
			ctx := context.Background()
			// Connect synthetic headers past vote activation so the provider quote
			// includes a nonzero vote subsidy, unlike an empty genesis-only wallet.
			params := w.ChainParams()
			previous := params.GenesisHash
			var chain []*wallet.BlockNode
			var forest wallet.SidechainForest
			for height := uint32(1); height <= uint32(params.StakeValidationHeight); height++ {
				header := params.GenesisBlock.Header
				header.Height, header.PrevBlock = height, previous
				header.SBits, header.VoteBits = params.MinimumStakeDiff, 1
				hash := header.BlockHash()
				filter, err := blockcf2.Regular(&wire.MsgBlock{Header: header}, nil)
				if err != nil {
					t.Fatal(err)
				}
				node := wallet.NewBlockNode(&header, &hash, filter, []*wire.MsgTx{})
				forest.AddBlockNode(node)
				chain = append(chain, node)
				previous = hash
			}
			if _, err := w.ChainSwitch(ctx, &forest, chain); err != nil {
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
			funding := wire.NewMsgTx()
			amount := int64(price)*25 + 1e8
			funding.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{99}}, amount, nil))
			funding.AddTxOut(&wire.TxOut{Value: amount, Version: version, PkScript: script})
			if err := w.AddTransaction(ctx, funding, nil); err != nil {
				t.Fatal(err)
			}
			n := new(feeBatchNetwork)
			w.SetNetworkBackend(n)
			c := testFeeClient(w)
			defer c.Close()
			const count = 20
			spent := make(map[wire.OutPoint]bool)
			fees := make(map[chainhash.Hash]chainhash.Hash)
			result, err := w.PurchaseTickets(ctx, n, &wallet.PurchaseTicketsRequest{
				Count: count, SourceAccount: 0, MinConf: 0,
				VSPFeeProcess: func(context.Context) (float64, error) { return 1, nil },
				VSPFeePaymentProcess: func(ctx context.Context, hash *chainhash.Hash, feeTx *wire.MsgTx) error {
					ticket, err := c.tx(ctx, hash)
					if err != nil {
						return err
					}
					if len(feeTx.TxIn) != 1 || n.split == nil {
						t.Fatal("fee was not funded by one reserved split output")
					}
					outpoint := feeTx.TxIn[0].PreviousOutPoint
					if outpoint.Hash != n.split.TxHash() || outpoint.Tree != wire.TxTreeRegular || spent[outpoint] {
						t.Fatalf("fee input is not a distinct split outpoint: %v", outpoint)
					}
					if outpoint == ticket.TxIn[0].PreviousOutPoint {
						t.Fatal("ticket and fee spend the same output")
					}
					spent[outpoint] = true
					prevOut := n.split.TxOut[outpoint.Index]
					if feeTx.TxIn[0].ValueIn != prevOut.Value {
						t.Fatal("fee input value does not match its split output")
					}
					_, height := w.MainChainTip(ctx)
					// The provider quotes with a 1e4 ticket-fee estimate.
					fee := txrules.StakePoolTicketFee(price, dcrutil.Amount(1e4), height, 1, w.ChainParams(), false)
					if fee <= 0 {
						t.Fatal("fixture has no provider fee")
					}
					// Seed the provider's quote; all construction, signing and fee
					// persistence below use the production fee-payment implementation.
					fp := &feePayment{client: c, ctx: ctx, ticketHash: *hash, fee: fee, feeAddr: address}
					if err := fp.makeFeeTx(feeTx); err != nil {
						return err
					}
					const flags = txscript.ScriptDiscourageUpgradableNops | txscript.ScriptVerifyCleanStack |
						txscript.ScriptVerifyCheckLockTimeVerify | txscript.ScriptVerifyCheckSequenceVerify | txscript.ScriptVerifyTreasury
					vm, err := txscript.NewEngine(prevOut.PkScript, feeTx, 0, flags, prevOut.Version, nil)
					if err != nil {
						return err
					}
					if err := vm.Execute(); err != nil {
						return err
					}
					remaining := prevOut.Value
					paidProvider := false
					for _, out := range feeTx.TxOut {
						remaining -= out.Value
						paidProvider = paidProvider || out.Value == int64(fee) && bytes.Equal(out.PkScript, script)
					}
					if !paidProvider || remaining < int64(txrules.FeeForSerializeSize(w.RelayFee(), feeTx.SerializeSize())) {
						t.Fatalf("fee transaction omitted provider fee or relay allowance: %v", feeTx)
					}
					fees[*hash] = feeTx.TxHash()
					return nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			if result == nil || len(result.TicketHashes) != count || len(result.Results) != count || len(fees) != count || n.published != count {
				t.Fatalf("expected %d published tickets and signed fees: result=%+v fees=%d published=%d", count, result, len(fees), n.published)
			}
			if len(n.split.TxIn) != 1 || n.split.TxIn[0].PreviousOutPoint.Hash != funding.TxHash() {
				t.Fatal("batch did not spend the single large funding output")
			}
			for _, result := range result.Results {
				if !result.Published || !result.VSPAttempted || result.VSPError != nil {
					t.Fatalf("incomplete ticket result: %+v", result)
				}
				info, err := w.VSPTicketInfo(ctx, &result.TicketHash)
				if err != nil {
					t.Fatal(err)
				}
				if info.FeeTxStatus != uint32(udb.VSPFeeProcessPaid) || info.FeeHash != fees[result.TicketHash] ||
					info.Host != c.url || !bytes.Equal(info.PubKey, c.pub) {
					t.Fatalf("wrong durable fee registration: %+v", info)
				}
				storedFee, err := c.tx(ctx, &info.FeeHash)
				if err != nil {
					t.Fatal(err)
				}
				if len(storedFee.TxIn) != 1 || len(storedFee.TxIn[0].SignatureScript) == 0 || !spent[storedFee.TxIn[0].PreviousOutPoint] {
					t.Fatal("signed split-funded fee transaction was not persisted")
				}
			}
		})
	}
}
