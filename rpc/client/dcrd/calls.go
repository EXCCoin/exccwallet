// Copyright (c) 2019 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

// TODO: consistent error wrapping

package dcrd

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"strings"

	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/dcrutil/v4"
	"github.com/EXCCoin/exccd/gcs/v3"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/wire"
	"github.com/jrick/bitset"
	"github.com/jrick/wsrpc/v2"
	"golang.org/x/sync/errgroup"
)

// Caller provides a client interface to perform JSON-RPC remote procedure calls.
type Caller interface {
	// Call performs the remote procedure call defined by method and
	// waits for a response or a broken client connection.
	// Args provides positional parameters for the call.
	// Res must be a pointer to a struct, slice, or map type to unmarshal
	// a result (if any), or nil if no result is needed.
	Call(ctx context.Context, method string, res interface{}, args ...interface{}) error
}

// RPC provides methods for calling dcrd JSON-RPCs without exposing the details
// of JSON encoding.
type RPC struct {
	Caller
}

// New creates a new RPC client instance from a caller.
func New(caller Caller) *RPC {
	return &RPC{caller}
}

func hashSliceToStrings(hashes []*chainhash.Hash) []string {
	s := make([]string, len(hashes))
	for i, h := range hashes {
		s[i] = h.String()
	}
	return s
}

func addrSliceToStrings(addrs []stdaddr.Address) []string {
	s := make([]string, len(addrs))
	for i, a := range addrs {
		s[i] = a.String()
	}
	return s
}

// exists serves as a common entry point for all exists* RPCs which take a
// single JSON parameter (usually an array) and return a hex-encoded bitset.
func exists(ctx context.Context, r *RPC, method string, res *bitset.Bytes, param json.RawMessage) error {
	var bitsetHex string
	err := r.Call(ctx, method, &bitsetHex, param)
	if err != nil {
		return errors.E(errors.Op(method), err)
	}
	decoded, err := hex.DecodeString(bitsetHex)
	if err != nil {
		return errors.E(errors.Op(method), errors.Encoding, err)
	}
	*res = decoded
	return nil
}

// ExistsLiveTicket returns whether a ticket identified by its hash is currently
// live and not immature.
func (r *RPC) ExistsLiveTicket(ctx context.Context, ticket *chainhash.Hash) (bool, error) {
	const op errors.Op = "exccd.ExistsLiveTicket"
	var exists bool
	err := r.Call(ctx, "existsliveticket", &exists, ticket.String())
	if err != nil {
		return false, errors.E(op, err)
	}
	return exists, err
}

// ExistsLiveExpiredTickets returns bitsets identifying whether each ticket
// is currently live or expired.
func (r *RPC) ExistsLiveExpiredTickets(ctx context.Context, tickets []*chainhash.Hash) (live, expired bitset.Bytes, err error) {
	const op errors.Op = "exccd.ExistsLiveExpiredTickets"
	// Reuse the single json.RawMessage for both calls
	ticketArray, _ := json.Marshal(hashSliceToStrings(tickets))
	errs := make(chan error, 2)
	go func() { errs <- exists(ctx, r, "existslivetickets", &live, ticketArray) }()
	go func() { errs <- exists(ctx, r, "existsexpiredtickets", &expired, ticketArray) }()
	for i := 0; i < cap(errs); i++ {
		if e := <-errs; err == nil && e != nil {
			// Must only return after all exists calls are
			// known to be completed to avoid a data race on the
			// return values.  Set the final error to return, and
			// only return after all errors have been read.
			err = errors.E(op, e)
		}
	}
	return
}

// ExistsExpiredMissedTickets returns bitsets identifying whether each ticket
// is currently expired or missed.
func (r *RPC) ExistsExpiredMissedTickets(ctx context.Context, tickets []*chainhash.Hash) (expired, missed bitset.Bytes, err error) {
	const op errors.Op = "exccd.ExistsExpiredMissedTickets"
	ticketArray, _ := json.Marshal(hashSliceToStrings(tickets))
	errs := make(chan error, 2)
	go func() { errs <- exists(ctx, r, "existsexpiredtickets", &expired, ticketArray) }()
	go func() { errs <- exists(ctx, r, "existsmissedtickets", &missed, ticketArray) }()
	for i := 0; i < cap(errs); i++ {
		if e := <-errs; err == nil && e != nil {
			err = errors.E(op, e)
		}
	}
	return
}

// UsedAddresses returns a bitset identifying whether each address has been
// publically used on the blockchain.  This feature requires the optional dcrd
// existsaddress index to be enabled.
func (r *RPC) UsedAddresses(ctx context.Context, addrs []stdaddr.Address) (bitset.Bytes, error) {
	const op errors.Op = "exccd.UsedAddresses"
	addrArray, _ := json.Marshal(addrSliceToStrings(addrs))
	var bits bitset.Bytes
	err := exists(ctx, r, "existsaddresses", &bits, addrArray)
	if err != nil {
		return nil, errors.E(op, err)
	}
	return bits, nil
}

// ExistsLiveTickets returns a bitset identifying whether each ticket is
// currently live.
func (r *RPC) ExistsLiveTickets(ctx context.Context, tickets []*chainhash.Hash) (bitset.Bytes, error) {
	const op errors.Op = "exccd.ExistsLiveTickets"
	ticketArray, _ := json.Marshal(hashSliceToStrings(tickets))
	var bits bitset.Bytes
	err := exists(ctx, r, "existslivetickets", &bits, ticketArray)
	if err != nil {
		return nil, errors.E(op, err)
	}
	return bits, nil
}

// MempoolCount returns the count of a particular kind of transaction in mempool.
// Kind may be one of:
//   "all"
//   "regular"
//   "tickets"
//   "votes"
//   "revocations"
func (r *RPC) MempoolCount(ctx context.Context, kind string) (int, error) {
	const op errors.Op = "exccd.MempoolCount"
	// This is rather inefficient, as only the count is needed, not all
	// matching hashes.
	var hashStrings []string
	err := r.Call(ctx, "getrawmempool", &hashStrings, false, kind)
	if err != nil {
		return 0, errors.E(op, err)
	}
	return len(hashStrings), nil
}

// getRawTransaction retrieve a transaction by hash"
func (r *RPC) getRawTransaction(ctx context.Context, hash string) (*wire.MsgTx, error) {
	tx := new(wire.MsgTx)
	err := r.Call(ctx, "getrawtransaction", unhex(tx), hash)
	return tx, err
}

// GetMempoolTSpends retrieves all mempool tspends.
func (r *RPC) GetMempoolTSpends(ctx context.Context) ([]*wire.MsgTx, error) {
	const op errors.Op = "exccd.GetMempoolTSpends"
	var hashStrings []string
	err := r.Call(ctx, "getrawmempool", &hashStrings, false, "tspend")
	if err != nil {
		return nil, errors.E(op, err)
	}

	txs := make([]*wire.MsgTx, 0, len(hashStrings))
	for _, h := range hashStrings {
		tx, err := r.getRawTransaction(ctx, h)
		if err != nil {
			return nil, errors.E(op, err)
		}
		txs = append(txs, tx)
	}
	return txs, nil
}

// PublishTransaction submits the transaction to dcrd mempool for acceptance.
// If accepted, the transaction is published to other peers.
// The transaction may not be an orphan.
func (r *RPC) PublishTransaction(ctx context.Context, tx *wire.MsgTx) error {
	const op errors.Op = "exccd.PublishTransaction"
	return r.publishTransaction(ctx, op, tx)
}

func (r *RPC) publishTransaction(ctx context.Context, op errors.Op, tx *wire.MsgTx) error {
	var b strings.Builder
	b.Grow(tx.SerializeSize() * 2)
	err := tx.Serialize(hex.NewEncoder(&b))
	if err != nil {
		return errors.E(op, errors.Encoding, err)
	}
	err = r.Call(ctx, "sendrawtransaction", nil, b.String())
	if err != nil {
		// Duplicate txs are not considered an error
		var e *wsrpc.Error
		if errors.As(err, &e) && e.Code == codeDuplicateTx {
			return nil
		}
		return errors.E(op, err)
	}
	return nil
}

// PublishTransactions submits each transaction to dcrd mempool for acceptance.
// If accepted, the transaction is published to other peers.
// Transactions are sent in order and later transactions may spend outputs of
// previous transactions.
// No transaction may be an orphan.
func (r *RPC) PublishTransactions(ctx context.Context, txs ...*wire.MsgTx) error {
	const op errors.Op = "exccd.PublishTransactions"

	// sendrawtransaction does not allow orphans, so we can not concurrently
	// send transactions.  All transaction sends are attempted, and the
	// first non-nil error is returned.
	var firstErr error
	for _, tx := range txs {
		err := r.publishTransaction(ctx, op, tx)
		if err != nil && firstErr == nil {
			firstErr = err
		}
	}
	if firstErr != nil {
		return errors.E(op, firstErr)
	}
	return nil
}

// Blocks returns the blocks for each block hash.
func (r *RPC) Blocks(ctx context.Context, blockHashes []*chainhash.Hash) ([]*wire.MsgBlock, error) {
	const op errors.Op = "exccd.Blocks"

	blocks := make([]*wire.MsgBlock, len(blockHashes))
	var g errgroup.Group
	for i := range blockHashes {
		i := i
		g.Go(func() error {
			blocks[i] = new(wire.MsgBlock)
			return r.Call(ctx, "getblock", unhex(blocks[i]), blockHashes[i].String(), false)
		})
	}
	err := g.Wait()
	if err != nil {
		return nil, errors.E(op, err)
	}
	return blocks, nil
}

// CFilterV2 returns the version 2 committed filter and the data required for
// verifying the inclusion proof of the cfilter for a block.
func (r *RPC) CFilterV2(ctx context.Context, blockHash *chainhash.Hash) (*gcs.FilterV2, uint32, []chainhash.Hash, error) {
	const opf = "exccd.CFilterV2(%v)"

	var res cfilterV2Reply
	err := r.Call(ctx, "getcfilterv2", &res, blockHash.String())
	if err != nil {
		op := errors.Opf(opf, blockHash)
		return nil, 0, nil, errors.E(op, err)
	}

	return res.Filter.Filter, res.ProofIndex, res.proofHashes(), nil
}

// filterProof is an alias to the same anonymous struct as wallet package's
// FilterProof struct.
type filterProof = struct {
	Filter     *gcs.FilterV2
	ProofIndex uint32
	Proof      []chainhash.Hash
}

// CFiltersV2 returns the version 2 committed filters for blocks.
// If this method errors, a partial result of filter proofs may be returned,
// with nil filters if the query errored.
func (r *RPC) CFiltersV2(ctx context.Context, blockHashes []*chainhash.Hash) ([]filterProof, error) {
	const opf = "exccd.CFiltersV2(%v)"

	filters := make([]filterProof, len(blockHashes))
	var g errgroup.Group
	for i := range blockHashes {
		i := i
		g.Go(func() error {
			var res cfilterV2Reply
			err := r.Call(ctx, "getcfilterv2", &res, blockHashes[i].String())
			if err != nil {
				op := errors.Opf(opf, blockHashes[i])
				err = errors.E(op, err)
				return err
			}
			filters[i] = filterProof{
				Filter:     res.Filter.Filter,
				ProofIndex: res.ProofIndex,
				Proof:      res.proofHashes(),
			}
			return err
		})
	}
	err := g.Wait()
	return filters, err
}

// Headers returns the block headers starting at the fork point between the
// client and the dcrd server identified by the client's block locators.
func (r *RPC) Headers(ctx context.Context, blockLocators []*chainhash.Hash, hashStop *chainhash.Hash) ([]*wire.BlockHeader, error) {
	const op errors.Op = "exccd.Headers"

	res := &struct {
		Headers *headers `json:"headers"`
	}{
		Headers: new(headers),
	}
	err := r.Call(ctx, "getheaders", res, &hashes{blockLocators}, hashStop.String())
	if err != nil {
		return nil, errors.E(op, err)
	}
	return res.Headers.Headers, nil
}

// LoadTxFilter loads or reloads the precise server-side transaction filter used
// for relevant transaction notifications and rescans.
// Addresses and outpoints are added to an existing filter if reload is false.
func (r *RPC) LoadTxFilter(ctx context.Context, reload bool, addrs []stdaddr.Address, outpoints []wire.OutPoint) error {
	const op errors.Op = "exccd.LoadTxFilter"

	type outpoint struct {
		Hash  string `json:"hash"`
		Index uint32 `json:"index"`
		Tree  int8   `json:"tree"`
	}
	outpointArray := make([]*outpoint, len(outpoints))
	for i, o := range outpoints {
		outpointArray[i] = &outpoint{
			Hash:  o.Hash.String(),
			Index: o.Index,
			Tree:  o.Tree,
		}
	}

	err := r.Call(ctx, "loadtxfilter", nil, reload, addrSliceToStrings(addrs), outpointArray)
	if err != nil {
		return errors.E(op, err)
	}
	return nil
}

// Rescan rescans the specified blocks in order, using the loaded transaction
// filter to determine which transactions are possibly relevant to the client.
// The save function is called for the discovered transactions from each block.
func (r *RPC) Rescan(ctx context.Context, blocks []chainhash.Hash, save func(block *chainhash.Hash, txs []*wire.MsgTx) error) error {
	const op errors.Op = "exccd.Rescan"

	var res struct {
		DiscoveredData []struct {
			Hash         string   `json:"hash"`
			Transactions []string `json:"transactions"`
		} `json:"discovereddata"`
	}
	err := r.Call(ctx, "rescan", &res, &hashesContiguous{blocks})
	if err != nil {
		return errors.E(op, err)
	}
	for _, d := range res.DiscoveredData {
		blockHash, err := chainhash.NewHashFromStr(d.Hash)
		if err != nil {
			return errors.E(op, errors.Encoding, err)
		}
		txs := make([]*wire.MsgTx, 0, len(d.Transactions))
		for _, txHex := range d.Transactions {
			tx := new(wire.MsgTx)
			err := tx.Deserialize(hex.NewDecoder(strings.NewReader(txHex)))
			if err != nil {
				return errors.E(op, errors.Encoding, err)
			}
			txs = append(txs, tx)
		}
		err = save(blockHash, txs)
		if err != nil {
			return err
		}
	}
	return nil
}

// StakeDifficulty returns the stake difficulty (AKA ticket price) of the next
// block.
func (r *RPC) StakeDifficulty(ctx context.Context) (dcrutil.Amount, error) {
	const op errors.Op = "exccd.StakeDifficulty"

	var res struct {
		Sdiff float64 `json:"nextstakedifficulty"`
	}
	err := r.Call(ctx, "getstakedifficulty", &res)
	if err != nil {
		return 0, errors.E(op, err)
	}
	sdiff, err := dcrutil.NewAmount(res.Sdiff)
	if err != nil {
		return 0, errors.E(op, err)
	}
	return sdiff, nil
}
