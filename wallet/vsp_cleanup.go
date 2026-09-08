package wallet

import (
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

// SetVSPTicketPruner connects wallet pruning to the loader's VSP clients.
// The loader must set this before making the wallet available to callers.
func (w *Wallet) SetVSPTicketPruner(prune func(chainhash.Hash)) {
	w.vspTicketPruner = prune
}

func (w *Wallet) pruneVSPTickets(dbtx walletdb.ReadWriteTx, hashes []*chainhash.Hash) error {
	for _, hash := range hashes {
		if err := udb.DeleteVSPTicket(dbtx, *hash); err != nil {
			return err
		}
		if w.vspTicketPruner != nil {
			w.vspTicketPruner(*hash)
		}
	}
	return nil
}
