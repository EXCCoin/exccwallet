package wallet

import (
	"bytes"
	"context"

	"github.com/EXCCoin/exccd/blockchain/stake/v4"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/internal/vspidentity"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

// RecordVSPFee atomically records an unpublished fee transaction and its ticket
// association. The boolean reports whether storage committed, including when a
// subsequent network filter update fails.
func (w *Wallet) RecordVSPFee(ctx context.Context, ticketHash *chainhash.Hash,
	tx *wire.MsgTx, host string, pubkey []byte) (bool, error) {

	if tx == nil || stake.DetermineTxType(tx, true, false) != stake.TxTypeRegular {
		return false, errors.E(errors.Invalid, "VSP fee must be a regular transaction")
	}
	feeHash := tx.TxHash()
	recorded, err := w.addTransaction(ctx, tx, nil, func(dbtx walletdb.ReadWriteTx) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		saved, err := udb.GetVSPTicket(dbtx, *ticketHash)
		if err != nil && !errors.Is(err, errors.NotExist) {
			return err
		}
		if saved != nil && saved.FeeHash != (chainhash.Hash{}) &&
			(saved.FeeHash != feeHash || !sameVSPProvider(saved.Host, host, saved.PubKey, pubkey)) {
			return errors.E(errors.Invalid, "ticket already has another saved VSP payment")
		}
		if err := w.txStore.SetPublished(dbtx, &feeHash, false); err != nil {
			return err
		}
		return w.setVSPTicket(dbtx, ticketHash, &udb.VSPTicket{
			FeeHash:     feeHash,
			FeeTxStatus: uint32(udb.VSPFeeProcessPaid),
			Host:        host,
			PubKey:      pubkey,
		})
	})
	if !recorded && err == nil {
		err = errors.E(errors.Invalid, "VSP fee transaction was not recorded")
	}
	return recorded, err
}

// ResetVSPTicketFee clears a payment explicitly rejected by its provider.
// Ordinary processing errors retain the saved payment and provider identity.
func (w *Wallet) ResetVSPTicketFee(ctx context.Context, ticketHash, feeHash *chainhash.Hash,
	host string, pubkey []byte) error {

	err := walletdb.Update(ctx, w.db, func(dbtx walletdb.ReadWriteTx) error {
		saved, err := udb.GetVSPTicket(dbtx, *ticketHash)
		if err != nil {
			return err
		}
		if saved == nil || saved.FeeHash != *feeHash ||
			!sameVSPProvider(saved.Host, host, saved.PubKey, pubkey) ||
			saved.FeeTxStatus == uint32(udb.VSPFeeProcessConfirmed) {
			return errors.E(errors.Invalid, "saved VSP payment changed or is already confirmed")
		}
		ns := dbtx.ReadWriteBucket(wtxmgrNamespaceKey)
		details, err := w.txStore.TxDetails(ns, feeHash)
		if err != nil {
			return err
		}
		if details == nil || details.Block.Height != -1 {
			return errors.E(errors.Invalid, "VSP fee is missing or already mined")
		}
		if err := w.txStore.RemoveUnconfirmed(ns, &details.MsgTx, feeHash); err != nil {
			return err
		}
		return w.setVSPTicket(dbtx, ticketHash, &udb.VSPTicket{
			FeeTxStatus: uint32(udb.VSPFeeProcessErrored),
			Host:        host,
			PubKey:      pubkey,
		})
	})
	if err == nil {
		w.NtfnServer.notifyRemovedTransaction(*feeHash)
	}
	return err
}

func sameVSPProvider(firstHost, secondHost string, firstKey, secondKey []byte) bool {
	if !bytes.Equal(firstKey, secondKey) {
		return false
	}
	first, err := vspidentity.CanonicalHost(firstHost)
	if err != nil {
		return false
	}
	second, err := vspidentity.CanonicalHost(secondHost)
	return err == nil && first == second
}
