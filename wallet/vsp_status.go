package wallet

import (
	"context"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

// VSPFeeRecords reads registrations and local transaction presence together.
// Presence is checked only for incomplete registrations.
func (w *Wallet) VSPFeeRecords(ctx context.Context) (map[chainhash.Hash]*udb.VSPTicket, map[chainhash.Hash]bool, error) {
	var records map[chainhash.Hash]*udb.VSPTicket
	present := make(map[chainhash.Hash]bool)
	err := walletdb.View(ctx, w.db, func(tx walletdb.ReadTx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		var err error
		records, err = udb.VSPTickets(tx)
		if err != nil {
			return err
		}
		for hash, record := range records {
			if err := ctx.Err(); err != nil {
				return err
			}
			if record.FeeTxStatus == uint32(udb.VSPFeeProcessConfirmed) {
				continue
			}
			_, err := w.txStore.Tx(tx.ReadBucket(wtxmgrNamespaceKey), &hash)
			if err != nil && !errors.Is(err, errors.NotExist) {
				return err
			}
			present[hash] = err == nil
		}
		return nil
	})
	return records, present, err
}
