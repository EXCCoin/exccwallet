package udb

import (
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
)

// VSPTickets reads fee records and their provider identities in one database view.
func VSPTickets(dbtx walletdb.ReadTx) (map[chainhash.Hash]*VSPTicket, error) {
	result := make(map[chainhash.Hash]*VSPTicket)
	bucket := dbtx.ReadBucket(vspBucketKey)
	if bucket == nil {
		return result, nil
	}
	err := bucket.ForEach(func(k, v []byte) error {
		if len(k) != chainhash.HashSize || len(v) != chainhash.HashSize+8 {
			return errors.E(errors.Encoding, "invalid VSP ticket record")
		}
		var hash chainhash.Hash
		copy(hash[:], k)
		record, err := GetVSPTicket(dbtx, hash)
		if err != nil {
			return err
		}
		if record.FeeTxStatus > uint32(VSPFeeProcessConfirmed) {
			return errors.E(errors.Encoding, "invalid VSP fee status")
		}
		result[hash] = record
		return nil
	})
	return result, err
}
