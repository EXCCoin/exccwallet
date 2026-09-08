package wallet

import (
	"context"

	"github.com/EXCCoin/exccd/blockchain/stake/v4"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

// UnminedVSPTickets returns incomplete registrations that can be resumed using
// the same saved provider. Unassociated tickets are never included.
func (w *Wallet) UnminedVSPTickets(ctx context.Context, matchesProvider func(string, []byte) bool) ([]chainhash.Hash, error) {
	var unmined []chainhash.Hash
	err := w.GetTickets(ctx, func(tickets []*TicketSummary, _ *wire.BlockHeader) (bool, error) {
		for _, ticket := range tickets {
			if ticket.Status == TicketStatusUnmined {
				unmined = append(unmined, *ticket.Ticket.Hash)
			}
		}
		return false, nil
	}, NewBlockIdentifierFromHeight(-1), nil)
	if err != nil {
		return nil, err
	}
	var managed []chainhash.Hash
	for _, hash := range unmined {
		info, err := w.VSPTicketInfo(ctx, &hash)
		if errors.Is(err, errors.NotExist) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if !matchesProvider(info.Host, info.PubKey) {
			continue
		}
		switch udb.FeeStatus(info.FeeTxStatus) {
		case udb.VSPFeeProcessStarted, udb.VSPFeeProcessPaid, udb.VSPFeeProcessErrored:
			managed = append(managed, hash)
		}
	}
	return managed, nil
}

func (w *Wallet) setVSPTicket(dbtx walletdb.ReadWriteTx, hash *chainhash.Hash, record *udb.VSPTicket) error {
	details, err := w.txStore.TxDetails(dbtx.ReadBucket(wtxmgrNamespaceKey), hash)
	if err != nil {
		return err
	}
	if !stake.IsSStx(&details.MsgTx) {
		return errors.E(errors.Invalid, "VSP registration requires a ticket transaction")
	}
	return udb.SetVSPTicket(dbtx, hash, record)
}
