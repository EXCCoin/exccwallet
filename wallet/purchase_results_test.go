package wallet

import (
	"context"
	stderrors "errors"
	"testing"

	"github.com/EXCCoin/exccd/blockchain/stake/v4"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/wire"
)

type purchaseResultNetwork struct {
	mockNetwork
	failAt    int
	published []chainhash.Hash
	failure   error
}

func (n *purchaseResultNetwork) PublishTransactions(_ context.Context, txs ...*wire.MsgTx) error {
	for _, tx := range txs {
		if !stake.IsSStx(tx) {
			continue
		}
		if len(n.published)+1 == n.failAt {
			return n.failure
		}
		n.published = append(n.published, tx.TxHash())
	}
	return nil
}

func TestPurchaseTicketsPreservesPartialPublicationAndFeeFailures(t *testing.T) {
	for _, failAt := range []int{0, 2} {
		t.Run(map[int]string{0: "fee failure", 2: "publication failure"}[failAt], func(t *testing.T) {
			cfg := basicWalletConfig
			w, teardown := testWallet(t, &cfg)
			defer teardown()
			ctx := context.Background()
			if err := w.Unlock(ctx, []byte("private"), nil); err != nil {
				t.Fatal(err)
			}
			address, err := w.NewExternalAddress(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			version, script := address.PaymentScript()
			funding := wire.NewMsgTx()
			funding.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{99}}, 1000e8, nil))
			for range 10 {
				funding.AddTxOut(&wire.TxOut{Value: 100e8, Version: version, PkScript: script})
			}
			if err := w.AddTransaction(ctx, funding, nil); err != nil {
				t.Fatal(err)
			}
			publicationErr, feeErr := stderrors.New("test publication failure"), stderrors.New("test fee failure")
			n := &purchaseResultNetwork{failAt: failAt, failure: publicationErr}
			w.SetNetworkBackend(n)
			var registered []chainhash.Hash
			request := &PurchaseTicketsRequest{Count: 3, SourceAccount: 0, MinConf: 0,
				VSPFeeProcess: func(context.Context) (float64, error) { return 1, nil },
				VSPFeePaymentProcess: func(_ context.Context, hash *chainhash.Hash, fee *wire.MsgTx) error {
					registered = append(registered, *hash)
					if len(registered) == 1 {
						return feeErr
					}
					fee.AddTxOut(&wire.TxOut{Value: 1, Version: version, PkScript: script})
					return nil
				},
			}
			result, err := w.PurchaseTickets(ctx, n, request)
			if !stderrors.Is(err, feeErr) {
				t.Fatalf("fee failure was lost: result=%+v err=%v", result, err)
			}
			wantPublished, wantResults := 3, 3
			if failAt != 0 {
				wantPublished, wantResults = 1, 2
				if !stderrors.Is(err, publicationErr) {
					t.Fatalf("publication failure lost: %v", err)
				}
			}
			if result == nil || len(result.TicketHashes) != wantPublished || len(result.Tickets) != wantPublished || len(result.Results) != wantResults {
				t.Fatalf("wrong partial results: %+v", result)
			}
			if len(registered) != wantPublished {
				t.Fatalf("registered %d, published %d", len(registered), wantPublished)
			}
			for i, hash := range result.TicketHashes {
				if *hash != n.published[i] || !result.Results[i].Published || !result.Results[i].VSPAttempted {
					t.Fatalf("unpublished ticket counted: %+v", result.Results[i])
				}
			}
			if !stderrors.Is(result.Results[0].VSPError, feeErr) {
				t.Fatal("per-ticket fee error missing")
			}
			if failAt != 0 {
				failed := result.Results[1]
				if failed.Published || failed.VSPAttempted || !stderrors.Is(failed.PublicationError, publicationErr) {
					t.Fatalf("failed publication result: %+v", failed)
				}
			} else if result.Results[2].VSPError != nil {
				t.Fatal("later successful registration reported failed")
			}
		})
	}
}
