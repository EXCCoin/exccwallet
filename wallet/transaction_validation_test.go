package wallet

import (
	"context"
	"github.com/EXCCoin/exccd/blockchain/stake/v4"
	gcs2 "github.com/EXCCoin/exccd/gcs/v3"
	"github.com/EXCCoin/exccd/gcs/v3/blockcf2"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"strings"
	"testing"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/txscript/v4"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

func TestAddTransactionValidatesOwnedUnminedInputs(t *testing.T) {
	for _, name := range []string{"valid", "unset witness value", "incorrect witness value", "missing signature", "malformed signature", "wrong tree"} {
		t.Run(name, func(t *testing.T) {
			cfg := basicWalletConfig
			w, teardown := testWallet(t, &cfg)
			defer teardown()
			ctx := context.Background()
			if err := w.Unlock(ctx, []byte("private"), nil); err != nil {
				t.Fatal(err)
			}
			addr, err := w.NewExternalAddress(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			version, script := addr.PaymentScript()
			funding := wire.NewMsgTx()
			funding.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{77}}, 100000, nil))
			funding.AddTxOut(&wire.TxOut{Value: 100000, Version: version, PkScript: script})
			if err := w.AddTransaction(ctx, funding, nil); err != nil {
				t.Fatal(err)
			}
			out := wire.OutPoint{Hash: funding.TxHash(), Tree: wire.TxTreeRegular}
			spend := wire.NewMsgTx()
			spend.AddTxIn(wire.NewTxIn(&out, 100000, nil))
			spend.AddTxOut(&wire.TxOut{Value: 90000, Version: version, PkScript: script})
			if name == "wrong tree" {
				spend.TxIn[0].PreviousOutPoint.Tree = wire.TxTreeStake
			}
			sigErrors, err := w.SignTransaction(ctx, spend, txscript.SigHashAll, nil, nil, nil)
			if err != nil || len(sigErrors) != 0 {
				t.Fatalf("sign: %v, %v", err, sigErrors)
			}
			invalid := false
			switch name {
			case "unset witness value":
				spend.TxIn[0].ValueIn = wire.NullValueIn
			case "incorrect witness value":
				spend.TxIn[0].ValueIn = 1
			case "missing signature":
				spend.TxIn[0].SignatureScript = nil
				invalid = true
			case "malformed signature":
				spend.TxIn[0].SignatureScript = []byte{txscript.OP_PUSHDATA1}
				invalid = true
			case "wrong tree":
				invalid = true
			}
			err = w.AddTransaction(ctx, spend, nil)
			if invalid {
				if name == "wrong tree" && (err == nil || !strings.Contains(err.Error(), "wrong transaction tree")) {
					t.Fatalf("wrong-tree signature was rejected for the wrong reason: %v", err)
				}
				if !errors.Is(err, errors.ScriptFailure) {
					t.Fatalf("invalid spend error = %v", err)
				}
			} else if err != nil {
				t.Fatalf("valid spend rejected: %v", err)
			}
			hash := spend.TxHash()
			txs, missing, err := w.GetTransactionsByHashes(ctx, []*chainhash.Hash{&hash})
			if err != nil && !(invalid && errors.Is(err, errors.NotExist)) {
				t.Fatal(err)
			}
			if invalid {
				if len(txs) != 0 || len(missing) != 1 {
					t.Fatal("forged spend was recorded")
				}
				err = walletdb.View(ctx, w.db, func(dbtx walletdb.ReadTx) error {
					_, err := w.txStore.UnspentOutput(dbtx.ReadBucket(wtxmgrNamespaceKey), out, true)
					return err
				})
				if err != nil {
					t.Fatalf("forged spend made funding unavailable: %v", err)
				}
			} else if len(txs) != 1 || len(missing) != 0 {
				t.Fatal("valid spend was not recorded")
			}
		})
	}
}

func TestAutomaticRevocationsWaitForMining(t *testing.T) {
	ctx := context.Background()
	cfg := basicWalletConfig
	w, teardown := testWallet(t, &cfg)
	defer teardown()
	addr, err := w.NewExternalAddress(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	stakeAddr := addr.(stdaddr.StakeAddress)
	const amount = 100000
	ticket := wire.NewMsgTx()
	ticket.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{19}}, amount, nil))
	v, script := stakeAddr.VotingRightsScript()
	ticket.AddTxOut(&wire.TxOut{Value: amount, Version: v, PkScript: script})
	v, script = stakeAddr.RewardCommitmentScript(amount, 0, 0)
	ticket.AddTxOut(&wire.TxOut{Version: v, PkScript: script})
	v, script = stakeAddr.StakeChangeScript()
	ticket.AddTxOut(&wire.TxOut{Version: v, PkScript: script})
	if err := w.AddTransaction(ctx, ticket, nil); err != nil {
		t.Fatal(err)
	}
	ticketHash := ticket.TxHash()
	headerBytes, err := w.chainParams.GenesisBlock.Header.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	revocation, err := stake.CreateRevocationFromTicket(&ticketHash, stake.ConvertToMinimalOutputs(ticket), 0, stake.TxVersionAutoRevocations, w.chainParams, headerBytes, true)
	if err != nil {
		t.Fatal(err)
	}
	if !stake.IsSSRtx(revocation, true) || len(revocation.TxIn[0].SignatureScript) != 0 {
		t.Fatal("fixture is not an automatic revocation")
	}
	if err := w.AddTransaction(ctx, revocation, nil); err != nil {
		t.Fatalf("automatic revocation punished as forged signature: %v", err)
	}
	hash := revocation.TxHash()
	if _, _, err := w.GetTransactionsByHashes(ctx, []*chainhash.Hash{&hash}); !errors.Is(err, errors.NotExist) {
		t.Fatalf("unmined unsigned revocation recorded: %v", err)
	}
	// SimNet enables automatic revocations. A block accepted by the chain
	// syncer is authoritative; once mined the revocation must still be stored.
	filter, err := gcs2.FromBytesV2(blockcf2.B, blockcf2.M, nil)
	if err != nil {
		t.Fatal(err)
	}
	header := &wire.BlockHeader{PrevBlock: w.chainParams.GenesisHash, Height: 1, Bits: w.chainParams.PowLimitBits, SBits: w.chainParams.MinimumStakeDiff}
	err = walletdb.Update(ctx, w.db, func(tx walletdb.ReadWriteTx) error {
		return w.txStore.ExtendMainChain(tx.ReadWriteBucket(wtxmgrNamespaceKey), header, filter)
	})
	if err != nil {
		t.Fatal(err)
	}
	blockHash := header.BlockHash()
	if err := w.AddTransaction(ctx, revocation, &blockHash); err != nil {
		t.Fatalf("mined automatic revocation rejected: %v", err)
	}
	stored, _, err := w.GetTransactionsByHashes(ctx, []*chainhash.Hash{&hash})
	if err != nil || len(stored) != 1 {
		t.Fatalf("mined revocation was not recorded: %v", err)
	}
}
