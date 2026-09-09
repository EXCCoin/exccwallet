package vsp

import (
	"bytes"
	"context"
	cryptorand "crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/EXCCoin/exccd/blockchain/stake/v4"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccd/dcrutil/v4"
	"github.com/EXCCoin/exccd/txscript/v4"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/txscript/v4/stdscript"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/internal/uniformprng"
	"github.com/EXCCoin/exccwallet/v2/rpc/client/dcrd"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	"github.com/EXCCoin/exccwallet/v2/wallet/txrules"
	"github.com/EXCCoin/exccwallet/v2/wallet/txsizes"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
)

var prng lockedRand

type lockedRand struct {
	mu   sync.Mutex
	rand *uniformprng.Source
}

func (r *lockedRand) int63n(n int64) int64 {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rand.Int63n(n)
}

// duration returns a random time.Duration in [0,d) with uniform distribution.
func (r *lockedRand) duration(d time.Duration) time.Duration {
	return time.Duration(r.int63n(int64(d)))
}

func (r *lockedRand) coinflip() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rand.Uint32n(2) == 0
}

func init() {
	source, err := uniformprng.RandSource(cryptorand.Reader)
	if err != nil {
		panic(err)
	}
	prng = lockedRand{
		rand: source,
	}
}

var (
	errStopped    = errors.New("fee processing stopped")
	errNotSolo    = errors.New("not a solo ticket")
	errMissingFee = errors.New("saved fee transaction is missing; replacement payment requires review")
)

// A random amount of delay (between zero and these jitter constants) is added
// before performing some background action with the VSP.  The delay is reduced
// when a ticket is currently live, as it may be called to vote any time.
const (
	immatureJitter = time.Hour
	liveJitter     = 5 * time.Minute
	unminedJitter  = 2 * time.Minute
)

type feePayment struct {
	client *Client
	ctx    context.Context
	cancel context.CancelFunc

	// Set at feepayment creation and never changes
	ticketHash     chainhash.Hash
	ticketTxExpiry uint32
	commitmentAddr stdaddr.StakeAddress
	votingAddr     stdaddr.StakeAddress
	policy         Policy

	// Requires locking for all access outside of Client.feePayment
	mu            sync.Mutex
	operation     chan struct{}
	votingKey     string
	ticketLive    int32
	ticketExpires int32
	fee           dcrutil.Amount
	feeAddr       stdaddr.Address
	feeHash       chainhash.Hash
	feeTx         *wire.MsgTx
	state         state
	err           error
	blocked       bool

	timerMu         sync.Mutex
	timer           *time.Timer
	timerGeneration uint64
	phase           string
	nextRetry       time.Time
}

type state uint32

const (
	_ state = iota
	unprocessed
	feePublished
	_ // ...
	ticketSpent
)

func parseTicket(ticket *wire.MsgTx, params *chaincfg.Params) (
	votingAddr, commitmentAddr stdaddr.StakeAddress, err error) {
	fail := func(err error) (_, _ stdaddr.StakeAddress, _ error) {
		return nil, nil, err
	}
	if !stake.IsSStx(ticket) {
		return fail(fmt.Errorf("%v is not a ticket", ticket))
	}
	_, addrs := stdscript.ExtractAddrs(ticket.TxOut[0].Version, ticket.TxOut[0].PkScript, params)
	if len(addrs) != 1 {
		return fail(fmt.Errorf("cannot parse voting addr"))
	}
	switch addr := addrs[0].(type) {
	case stdaddr.StakeAddress:
		votingAddr = addr
	default:
		return fail(fmt.Errorf("address cannot be used for voting rights: %v", err))
	}
	commitmentAddr, err = stake.AddrFromSStxPkScrCommitment(ticket.TxOut[1].PkScript, params)
	if err != nil {
		return fail(fmt.Errorf("cannot parse commitment address: %w", err))
	}
	return
}

// calcHeights checks if the ticket has been mined, and if so, sets the live
// height and expiry height fields.  The caller must hold fp.mu.
func (fp *feePayment) calcHeights() {
	_, minedHeight, err := fp.client.Wallet.TxBlock(fp.ctx, &fp.ticketHash)
	if err != nil {
		// The ticket was already fetched from the wallet before reaching here.
		log.Errorf("failed to query block which mines ticket: %v", err)
		return
	}
	if minedHeight < 2 {
		return
	}

	params := fp.client.Wallet.ChainParams()
	// Tickets become live one block after the parameter-indicated height.
	fp.ticketLive = minedHeight + int32(params.TicketMaturity) + 1
	fp.ticketExpires = fp.ticketLive + int32(params.TicketExpiry)
}

// expiryHeight returns zero until the ticket is mined.  The caller must hold
// fp.mu.
func (fp *feePayment) expiryHeight() int32 {
	if fp.ticketExpires == 0 {
		fp.calcHeights()
	}
	return fp.ticketExpires
}

// liveHeight returns zero until the ticket is mined.  The caller must hold
// fp.mu.
func (fp *feePayment) liveHeight() int32 {
	if fp.ticketLive == 0 {
		fp.calcHeights()
	}
	return fp.ticketLive
}

func (fp *feePayment) ticketSpent() bool {
	ctx := fp.ctx
	ticketOut := wire.OutPoint{Hash: fp.ticketHash, Index: 0, Tree: 1}
	_, _, err := fp.client.Wallet.Spender(ctx, &ticketOut)
	return err == nil
}

func (fp *feePayment) ticketExpired() bool {
	ctx := fp.ctx
	w := fp.client.Wallet
	_, tipHeight := w.MainChainTip(ctx)

	fp.mu.Lock()
	expires := fp.expiryHeight()
	fp.mu.Unlock()

	if expires > 0 {
		return tipHeight >= expires
	}
	return fp.ticketTxExpiry != wire.NoExpiryValue && uint32(tipHeight) >= fp.ticketTxExpiry
}

func (fp *feePayment) removedExpiredOrSpent() bool {
	var reason string
	switch {
	case fp.ticketExpired():
		reason = "expired"
	case fp.ticketSpent():
		reason = "spent"
	}
	if reason != "" {
		fp.remove(reason)
		// nothing scheduled
		return true
	}
	return false
}

func (fp *feePayment) remove(reason string) {
	fp.stop()
	log.Infof("ticket %v is %s; removing from VSP client", &fp.ticketHash, reason)
	fp.client.mu.Lock()
	if fp.client.jobs[fp.ticketHash] == fp {
		delete(fp.client.jobs, fp.ticketHash)
	}
	fp.client.mu.Unlock()
}

// feePayment returns an existing managed fee payment, or creates and begins
// processing a fee payment for a ticket.
func (c *Client) feePayment(ticketHash *chainhash.Hash, policy Policy, paidConfirmed bool) *feePayment {
	return c.feePaymentWithTx(ticketHash, policy, paidConfirmed, nil)
}

func (c *Client) feePaymentWithTx(ticketHash *chainhash.Hash, policy Policy, paidConfirmed bool,
	feeTx *wire.MsgTx) (fp *feePayment) {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return nil
	}
	fp = c.jobs[*ticketHash]
	generation := c.jobGeneration[*ticketHash]
	c.mu.Unlock()
	if fp != nil {
		return fp
	}

	defer func() {
		if fp == nil {
			return
		}
		finish, err := fp.beginOperation(fp.ctx)
		if err != nil {
			fp.stop()
			fp = nil
			return
		}
		defer finish()
		if fp.blocked || paidConfirmed {
			fp.schedule("confirm payment", fp.confirmPayment)
		} else {
			fp.schedule("reconcile payment", fp.reconcilePayment)
		}
		c.mu.Lock()
		if c.closed || fp.ctx.Err() != nil || c.jobGeneration[*ticketHash] != generation {
			c.mu.Unlock()
			fp.stop()
			fp = nil
			return
		}
		fp2 := c.jobs[*ticketHash]
		if fp2 != nil {
			fp.stop()
			fp = fp2
		} else {
			c.jobs[*ticketHash] = fp
		}
		c.mu.Unlock()
	}()

	parent := c.ctx
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	defer func() {
		if fp == nil {
			cancel()
		}
	}()
	w := c.Wallet
	params := w.ChainParams()

	fp = &feePayment{
		client:     c,
		ctx:        ctx,
		cancel:     cancel,
		ticketHash: *ticketHash,
		feeTx:      feeTx,
		policy:     policy,
	}

	// No VSP interaction is required for spent tickets.
	if fp.ticketSpent() {
		fp.state = ticketSpent
		return fp
	}

	ticket, err := c.tx(ctx, ticketHash)
	if err != nil {
		log.Warnf("no ticket found for %v", ticketHash)
		return nil
	}
	fp.ticketTxExpiry = ticket.Expiry
	if fp.ticketExpired() {
		return nil
	}

	fp.votingAddr, fp.commitmentAddr, err = parseTicket(ticket, params)
	if err != nil {
		log.Errorf("%v is not a ticket: %v", ticketHash, err)
		return nil
	}
	// Try to access the voting key.
	fp.votingKey, err = w.DumpWIFPrivateKey(ctx, fp.votingAddr)
	if err != nil {
		log.Errorf("no voting key for ticket %v: %v", ticketHash, err)
		return nil
	}
	feeHash, err := w.VSPFeeHashForTicket(ctx, ticketHash)
	if err != nil {
		if errors.Is(err, errors.NotExist) {
			return fp
		}
		log.Errorf("cannot read saved VSP payment for %v: %v", ticketHash, err)
		return nil
	}

	fee, err := c.tx(ctx, &feeHash)
	fp.feeHash = feeHash
	if err != nil {
		// Keep checking the saved payment without creating a replacement.
		fp.err = fmt.Errorf("fee transaction not found in wallet: %w", err)
		fp.blocked = true
		return fp
	}

	fp.feeTx = fee
	fp.feeHash = feeHash

	// If database has been updated to paid or confirmed status, we can forgo
	// this step.
	if !paidConfirmed {
		fp.state = unprocessed // XXX fee created, but perhaps not submitted with vsp.
		fp.fee = -1            // XXX fee amount (not needed anymore?)
	}
	return fp
}

func (c *Client) tx(ctx context.Context, hash *chainhash.Hash) (*wire.MsgTx, error) {
	txs, _, err := c.Wallet.GetTransactionsByHashes(ctx, []*chainhash.Hash{hash})
	if err != nil {
		return nil, err
	}
	return txs[0], nil
}

// Schedule a method to be executed.
// Any currently-scheduled method is replaced.
func (fp *feePayment) schedule(name string, method func() error) {
	var delay time.Duration
	if method != nil {
		if fp.ctx.Err() != nil {
			return
		}
		delay = fp.next()
	}

	fp.timerMu.Lock()
	defer fp.timerMu.Unlock()
	fp.timerGeneration++
	if fp.timer != nil {
		fp.timer.Stop()
		fp.timer = nil
	}
	if method != nil {
		if fp.ctx.Err() != nil {
			return
		}
		fp.phase = name
		fp.nextRetry = time.Now().Add(delay)
		log.Debugf("scheduling %q for ticket %s in %v", name, &fp.ticketHash, delay)
		fp.timer = time.AfterFunc(delay, fp.task(name, method, fp.timerGeneration))
	} else {
		fp.phase = ""
		fp.nextRetry = time.Time{}
	}
}

func (fp *feePayment) next() time.Duration {
	w := fp.client.Wallet
	params := w.ChainParams()
	_, tipHeight := w.MainChainTip(fp.ctx)

	fp.mu.Lock()
	ticketLive := fp.liveHeight()
	ticketExpires := fp.expiryHeight()
	fp.mu.Unlock()

	return prng.duration(nextJitter(tipHeight, ticketLive, ticketExpires,
		params.TargetTimePerBlock))
}

func nextJitter(tipHeight, ticketLive, ticketExpires int32,
	targetTimePerBlock time.Duration) time.Duration {

	var jitter time.Duration
	switch {
	case tipHeight < ticketLive: // immature, mined ticket
		blocksUntilLive := ticketLive - tipHeight
		jitter = targetTimePerBlock * time.Duration(blocksUntilLive)
		if jitter > immatureJitter {
			jitter = immatureJitter
		}
	case tipHeight < ticketExpires: // live ticket
		jitter = liveJitter
	default: // unmined ticket
		jitter = unminedJitter
	}
	return jitter
}

// beginOperation serializes foreground requests and background tasks for a ticket.
// No other payment or client lock may be held while waiting.
func (fp *feePayment) beginOperation(ctx context.Context) (func(), error) {
	fp.mu.Lock()
	if fp.operation == nil {
		fp.operation = make(chan struct{}, 1)
	}
	operation := fp.operation
	fp.mu.Unlock()
	select {
	case operation <- struct{}{}:
		if err := ctx.Err(); err != nil {
			<-operation
			return nil, err
		}
		if err := fp.ctx.Err(); err != nil {
			<-operation
			return nil, err
		}
		return func() { <-operation }, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-fp.ctx.Done():
		return nil, fp.ctx.Err()
	}
}

// task returns a function running a feePayment method.
// If the method errors, the error is logged, and the payment is put
// in an errored state and may require manual processing.
func (fp *feePayment) task(name string, method func() error, generation uint64) func() {
	return func() {
		finish, err := fp.beginOperation(fp.ctx)
		if err != nil {
			return
		}
		defer finish()
		fp.timerMu.Lock()
		if generation != fp.timerGeneration {
			fp.timerMu.Unlock()
			return
		}
		fp.nextRetry = time.Time{}
		fp.timerMu.Unlock()
		fp.mu.Lock()
		fp.err = nil
		fp.mu.Unlock()
		err = method()
		fp.mu.Lock()
		fp.err = err
		fp.mu.Unlock()
		if err != nil {
			log.Errorf("ticket %v: %v: %v", &fp.ticketHash, name, err)
		}
	}
}

func (fp *feePayment) stop() {
	if fp.cancel != nil {
		fp.cancel()
	}
	fp.schedule("", nil)
}

func (fp *feePayment) receiveFeeAddress() error {
	ctx := fp.ctx
	w := fp.client.Wallet
	params := w.ChainParams()

	// stop processing if ticket is expired or spent
	if fp.removedExpiredOrSpent() {
		// nothing scheduled
		return errStopped
	}

	// Fetch ticket and its parent transaction (typically, a split
	// transaction).
	ticket, err := fp.client.tx(ctx, &fp.ticketHash)
	if err != nil {
		return fmt.Errorf("failed to retrieve ticket: %w", err)
	}
	parentHash := &ticket.TxIn[0].PreviousOutPoint.Hash
	parent, err := fp.client.tx(ctx, parentHash)
	if err != nil {
		return fmt.Errorf("failed to retrieve parent %v of ticket: %w",
			parentHash, err)
	}

	var response struct {
		Timestamp  int64  `json:"timestamp"`
		FeeAddress string `json:"feeaddress"`
		FeeAmount  int64  `json:"feeamount"`
		Request    []byte `json:"request"`
	}
	requestBody, err := json.Marshal(&struct {
		Timestamp  int64          `json:"timestamp"`
		TicketHash string         `json:"tickethash"`
		TicketHex  json.Marshaler `json:"tickethex"`
		ParentHex  json.Marshaler `json:"parenthex"`
	}{
		Timestamp:  time.Now().Unix(),
		TicketHash: fp.ticketHash.String(),
		TicketHex:  txMarshaler(ticket),
		ParentHex:  txMarshaler(parent),
	})
	if err != nil {
		return err
	}
	err = fp.client.post(ctx, "/api/v3/feeaddress", fp.commitmentAddr, &response,
		json.RawMessage(requestBody))
	if err != nil {
		return err
	}

	// verify initial request matches server
	if !bytes.Equal(requestBody, response.Request) {
		return fmt.Errorf("server response contains differing request")
	}

	feeAmount := dcrutil.Amount(response.FeeAmount)
	feeAddr, err := stdaddr.DecodeAddress(response.FeeAddress, params)
	if err != nil {
		return fmt.Errorf("server fee address invalid: %w", err)
	}

	log.Infof("VSP requires fee %v", feeAmount)
	if feeAmount > fp.policy.MaxFee {
		return fmt.Errorf("server fee amount too high: %v > %v",
			feeAmount, fp.policy.MaxFee)
	}

	// XXX validate server timestamp?

	fp.mu.Lock()
	fp.fee = feeAmount
	fp.feeAddr = feeAddr
	fp.mu.Unlock()

	return nil
}

// makeFeeTx adds outputs to tx to pay a VSP fee, adding inputs when the supplied
// value does not cover the quoted fee and transaction fee.
//
// If tx is nil, fp.feeTx may be assigned or modified, but the pointer will not
// be dereferenced.
func (fp *feePayment) makeFeeTx(tx *wire.MsgTx) error {
	ctx := fp.ctx
	w := fp.client.Wallet
	if err := ctx.Err(); err != nil {
		return err
	}
	if fp.removedExpiredOrSpent() {
		return errStopped
	}

	fp.mu.Lock()
	if fp.blocked {
		fp.mu.Unlock()
		return errMissingFee
	}
	fee := fp.fee
	fpFeeTx := fp.feeTx
	feeAddr := fp.feeAddr
	fp.mu.Unlock()

	// The rest of this function will operate on the tx pointer, with fp.feeTx
	// assigned to the result on success.
	// Update tx to use the partially created fpFeeTx if any has been started.
	// The transaction pointed to by the caller will be dereferenced and modified
	// when non-nil.
	if fpFeeTx != nil {
		if tx != nil {
			*tx = *fpFeeTx
		} else {
			tx = fpFeeTx
		}
	}
	// Fee transaction with outputs is already finished.
	if fpFeeTx != nil && len(fpFeeTx.TxOut) != 0 {
		feeHash := fpFeeTx.TxHash()
		return w.UpdateVspTicketFeeToPaid(ctx, &fp.ticketHash, &feeHash, fp.client.url, fp.client.pub)
	}
	// When both transactions are nil, create a new empty transaction.
	if tx == nil {
		tx = wire.NewMsgTx()
	}
	// Keep the caller's reserved inputs intact if funding or signing fails.
	// Added inputs belong to this attempt until the fee has been recorded.
	providedTx := tx
	tx = tx.Copy()
	minconf := int32(1)
	if len(tx.TxIn) != 0 {
		// Purchase fees may need the unconfirmed change from their split.
		minconf = 0
	}
	var reserved []wallet.Input
	defer func() {
		for _, in := range reserved {
			w.UnlockOutpoint(&in.OutPoint.Hash, in.OutPoint.Index)
		}
	}()
	for _, in := range tx.TxIn {
		op := in.PreviousOutPoint
		if w.TryLockOutpoint(&op.Hash, op.Index) {
			reserved = append(reserved, wallet.Input{OutPoint: op})
		}
	}

	// XXX fp.fee == -1?
	if fee == 0 {
		err := fp.receiveFeeAddress()
		if err != nil {
			return err
		}
		fp.mu.Lock()
		fee = fp.fee
		feeAddr = fp.feeAddr
		fp.mu.Unlock()
	}

	var input int64
	for _, in := range tx.TxIn {
		input += in.ValueIn
	}

	vers, feeScript := feeAddr.PaymentScript()

	addr, err := w.NewChangeAddress(ctx, fp.policy.ChangeAcct)
	if err != nil {
		log.Warnf("failed to get new change address: %v", err)
		return err
	}
	var changeOut *wire.TxOut
	switch addr := addr.(type) {
	case wallet.Address:
		vers, script := addr.PaymentScript()
		changeOut = &wire.TxOut{PkScript: script, Version: vers}
	default:
		return fmt.Errorf("failed to convert '%T' to wallet.Address", addr)
	}

	tx.TxOut = append(tx.TxOut[:0], &wire.TxOut{
		Value:    int64(fee),
		Version:  vers,
		PkScript: feeScript,
	})
	feeRate := w.RelayFee()
	var change int64
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		scriptSizes := make([]int, len(tx.TxIn))
		for i := range scriptSizes {
			scriptSizes[i] = txsizes.RedeemP2PKHSigScriptSize
		}
		est := txsizes.EstimateSerializeSize(scriptSizes, tx.TxOut, txsizes.P2PKHPkScriptSize)
		maxSize := w.ChainParams().MaxTxSize
		if w.ChainParams().Net == wire.MainNet {
			maxSize = min(maxSize, 100000)
		}
		if est > maxSize {
			return fmt.Errorf("VSP fee transaction exceeds maximum relay size")
		}
		change = input - int64(fee) - int64(txrules.FeeForSerializeSize(feeRate, est))
		if change >= 0 && len(tx.TxIn) != 0 {
			break
		}
		// The provider quotes after the ticket is published. Fund a changed
		// quote and the relay cost of any additional inputs from this account.
		needed := dcrutil.Amount(max(-change, 1))
		inputs, err := w.ReserveOutputsForAmount(ctx, fp.policy.FeeAcct, needed, minconf)
		if err != nil {
			return fmt.Errorf("unable to reserve enough output value to "+
				"pay VSP fee for ticket %v: %w", fp.ticketHash, err)
		}
		reserved = append(reserved, inputs...)
		if len(inputs) == 0 {
			return fmt.Errorf("no spendable inputs for VSP fee")
		}
		for _, in := range inputs {
			tx.AddTxIn(wire.NewTxIn(&in.OutPoint, in.PrevOut.Value, nil))
			input += in.PrevOut.Value
		}
	}
	if !txrules.IsDustAmount(dcrutil.Amount(change), txsizes.P2PKHPkScriptSize, feeRate) {
		changeOut.Value = change
		tx.TxOut = append(tx.TxOut, changeOut)
		// randomize position
		if prng.coinflip() {
			tx.TxOut[0], tx.TxOut[1] = tx.TxOut[1], tx.TxOut[0]
		}
	}

	feeHash := tx.TxHash()

	// sign
	sigErrs, err := w.SignTransaction(ctx, tx, txscript.SigHashAll, nil, nil, nil)
	if err != nil || len(sigErrs) > 0 {
		log.Errorf("failed to sign transaction: %v", err)
		sigErrStr := ""
		for _, sigErr := range sigErrs {
			log.Errorf("\t%v", sigErr)
			sigErrStr = fmt.Sprintf("\t%v", sigErr) + " "
		}
		if err != nil {
			return err
		}
		return errors.New(sigErrStr)
	}

	recorded, err := w.RecordVSPFee(ctx, &fp.ticketHash, tx, fp.client.url, fp.client.pub)
	if !recorded {
		return err
	}
	// A filter update can fail after storage commits. Keep this exact payment
	// in memory as well as the database, so retry cannot replace it.
	fp.mu.Lock()
	*providedTx = *tx
	fp.feeTx = providedTx
	fp.feeHash = feeHash
	fp.mu.Unlock()

	return err
}

type ticketStatus struct {
	Timestamp       int64             `json:"timestamp"`
	TicketConfirmed bool              `json:"ticketconfirmed"`
	FeeTxStatus     string            `json:"feetxstatus"`
	FeeTxHash       string            `json:"feetxhash"`
	VoteChoices     map[string]string `json:"votechoices"`
	TSpendPolicy    map[string]string `json:"tspendpolicy"`
	TreasuryPolicy  map[string]string `json:"treasurypolicy"`
	Request         []byte            `json:"request"`
}

func (c *Client) status(ctx context.Context, ticketHash *chainhash.Hash) (*ticketStatus, error) {
	w := c.Wallet
	params := w.ChainParams()

	ticketTx, err := c.tx(ctx, ticketHash)
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve ticket %v: %w", ticketHash, err)
	}
	if len(ticketTx.TxOut) != 3 {
		return nil, fmt.Errorf("ticket %v has multiple commitments: %w", ticketHash, errNotSolo)
	}

	if !stake.IsSStx(ticketTx) {
		return nil, fmt.Errorf("%v is not a ticket", ticketHash)
	}
	commitmentAddr, err := stake.AddrFromSStxPkScrCommitment(ticketTx.TxOut[1].PkScript, params)
	if err != nil {
		return nil, fmt.Errorf("failed to extract commitment address from %v: %w",
			ticketHash, err)
	}

	var resp ticketStatus
	requestBody, err := json.Marshal(&struct {
		TicketHash string `json:"tickethash"`
	}{
		TicketHash: ticketHash.String(),
	})
	if err != nil {
		return nil, err
	}
	err = c.post(ctx, "/api/v3/ticketstatus", commitmentAddr, &resp,
		json.RawMessage(requestBody))
	if err != nil {
		return nil, err
	}

	// verify initial request matches server
	if !bytes.Equal(requestBody, resp.Request) {
		return nil, fmt.Errorf("server response contains differing request")
	}

	// XXX validate server timestamp?

	return &resp, nil
}

func (c *Client) setVoteChoices(ctx context.Context, ticketHash *chainhash.Hash,
	choices []wallet.AgendaChoice, tspendPolicy map[string]string, treasuryPolicy map[string]string) error {
	w := c.Wallet
	params := w.ChainParams()

	ticketTx, err := c.tx(ctx, ticketHash)
	if err != nil {
		return fmt.Errorf("failed to retrieve ticket %v: %w", ticketHash, err)
	}

	if !stake.IsSStx(ticketTx) {
		return fmt.Errorf("%v is not a ticket", ticketHash)
	}
	if len(ticketTx.TxOut) != 3 {
		return fmt.Errorf("ticket %v has multiple commitments: %w", ticketHash, errNotSolo)
	}

	commitmentAddr, err := stake.AddrFromSStxPkScrCommitment(ticketTx.TxOut[1].PkScript, params)
	if err != nil {
		return fmt.Errorf("failed to extract commitment address from %v: %w",
			ticketHash, err)
	}

	agendaChoices := make(map[string]string, len(choices))

	// Prepare agenda choice
	for _, c := range choices {
		agendaChoices[c.AgendaID] = c.ChoiceID
	}

	var resp ticketStatus
	requestBody, err := json.Marshal(&struct {
		Timestamp      int64             `json:"timestamp"`
		TicketHash     string            `json:"tickethash"`
		VoteChoices    map[string]string `json:"votechoices"`
		TSpendPolicy   map[string]string `json:"tspendpolicy"`
		TreasuryPolicy map[string]string `json:"treasurypolicy"`
	}{
		Timestamp:      time.Now().Unix(),
		TicketHash:     ticketHash.String(),
		VoteChoices:    agendaChoices,
		TSpendPolicy:   tspendPolicy,
		TreasuryPolicy: treasuryPolicy,
	})
	if err != nil {
		return err
	}

	err = c.post(ctx, "/api/v3/setvotechoices", commitmentAddr, &resp,
		json.RawMessage(requestBody))
	if err != nil {
		return err
	}

	// verify initial request matches server
	if !bytes.Equal(requestBody, resp.Request) {
		return fmt.Errorf("server response contains differing request")
	}

	// XXX validate server timestamp?

	return nil
}

func (fp *feePayment) reconcilePayment() error {
	ctx := fp.ctx
	w := fp.client.Wallet
	if err := ctx.Err(); err != nil {
		return err
	}

	// stop processing if ticket is expired or spent
	// XXX if ticket is no longer saved by wallet (because the tx expired,
	// or was double spent, etc) remove the fee payment.
	if fp.removedExpiredOrSpent() {
		// nothing scheduled
		return errStopped
	}

	// A fee amount and address must have been created by this point.
	// Ensure that the fee transaction can be created, otherwise reschedule
	// this method until it is.  There is no need to check the wallet for a
	// fee transaction matching a known hash; this is performed when
	// creating the feePayment.
	fp.mu.Lock()
	feeTx := fp.feeTx
	fp.mu.Unlock()
	if feeTx == nil || len(feeTx.TxOut) == 0 {
		err := fp.makeFeeTx(nil)
		if err != nil {
			var apiErr *BadRequestError
			if errors.As(err, &apiErr) && apiErr.Code == codeTicketCannotVote {
				fp.remove("ticket cannot vote")
				// Attempt to Revoke Tickets, we're not returning any errors here
				// and just logging.
				n, err := w.NetworkBackend()
				if err != nil {
					log.Errorf("unable to get network backend for revoking tickets %v", err)
				} else {
					if rpc, ok := n.(*dcrd.RPC); ok {
						err := w.RevokeTickets(ctx, rpc)
						if err != nil {
							log.Errorf("cannot revoke vsp tickets %v", err)
						}
					}
				}
			} else if !errors.Is(err, errMissingFee) && !errors.Is(err, errStopped) {
				fp.schedule("reconcile payment", fp.reconcilePayment)
			}
			return err
		}
	}

	// A fee address has been obtained, and the fee transaction has been
	// created, but it is unknown if the VSP has received the fee and will
	// vote using the ticket.
	//
	// If the fee is mined, then check the status of the ticket and payment
	// with the VSP, to ensure that it has marked the fee payment as paid.
	//
	// If the fee is not mined, an API call with the VSP is used so it may
	// receive and publish the transaction.  A follow up on the ticket
	// status is scheduled for some time in the future.

	err := fp.submitPayment()
	fp.mu.Lock()
	feeHash := fp.feeHash
	fp.mu.Unlock()
	var apiErr *BadRequestError
	if errors.As(err, &apiErr) {
		switch apiErr.Code {
		case codeFeeAlreadyReceived:
			// The provider may still be waiting to broadcast this payment.
			// Only a matching broadcast status makes its change spendable.
			err = nil
		}
	}
	if err != nil {
		// A provider error does not prove that an earlier submission was
		// never broadcast. Keep the exact payment and its spent inputs.
		fp.schedule("reconcile payment", fp.reconcilePayment)
		return err
	}

	err = w.UpdateVspTicketFeeToPaid(ctx, &fp.ticketHash, &feeHash, fp.client.url, fp.client.pub)
	if err != nil {
		fp.schedule("confirm payment", fp.confirmPayment)
		return err
	}

	// confirmPayment will remove the fee payment processing when the fee
	// has reached sufficient confirmations, and reschedule itself if the
	// fee is not confirmed yet.  If the fee tx is ever removed from the
	// wallet, this will schedule another reconcile.
	return fp.confirmPayment()

	/*
		// XXX? for each input, c.Wallet.UnlockOutpoint(&outpoint.Hash, outpoint.Index)
		// xxx, or let the published tx replace the unpublished one, and unlock
		// outpoints as it is processed.

	*/
}

func (fp *feePayment) submitPayment() (err error) {
	ctx := fp.ctx
	w := fp.client.Wallet

	// stop processing if ticket is expired or spent
	if fp.removedExpiredOrSpent() {
		// nothing scheduled
		return errStopped
	}

	// submitting a payment requires the fee tx to already be created.
	fp.mu.Lock()
	feeTx := fp.feeTx
	votingKey := fp.votingKey
	fp.mu.Unlock()
	if feeTx == nil {
		feeTx = new(wire.MsgTx)
	}
	if len(feeTx.TxOut) == 0 {
		err := fp.makeFeeTx(feeTx)
		if err != nil {
			return err
		}
	}
	if votingKey == "" {
		votingKey, err = w.DumpWIFPrivateKey(ctx, fp.votingAddr)
		if err != nil {
			return err
		}
		fp.mu.Lock()
		fp.votingKey = votingKey
		fp.mu.Unlock()
	}

	// Retrieve voting preferences
	voteChoices := make(map[string]string)
	agendaChoices, _, err := w.AgendaChoices(ctx, &fp.ticketHash)
	if err != nil {
		return err
	}
	for _, agendaChoice := range agendaChoices {
		voteChoices[agendaChoice.AgendaID] = agendaChoice.ChoiceID
	}

	var payfeeResponse struct {
		Timestamp int64  `json:"timestamp"`
		Request   []byte `json:"request"`
	}
	requestBody, err := json.Marshal(&struct {
		Timestamp      int64             `json:"timestamp"`
		TicketHash     string            `json:"tickethash"`
		FeeTx          json.Marshaler    `json:"feetx"`
		VotingKey      string            `json:"votingkey"`
		VoteChoices    map[string]string `json:"votechoices"`
		TSpendPolicy   map[string]string `json:"tspendpolicy"`
		TreasuryPolicy map[string]string `json:"treasurypolicy"`
	}{
		Timestamp:      time.Now().Unix(),
		TicketHash:     fp.ticketHash.String(),
		FeeTx:          txMarshaler(feeTx),
		VotingKey:      votingKey,
		VoteChoices:    voteChoices,
		TSpendPolicy:   w.TSpendPolicyForTicket(&fp.ticketHash),
		TreasuryPolicy: w.TreasuryKeyPolicyForTicket(&fp.ticketHash),
	})
	if err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		err = fp.client.post(ctx, "/api/v3/payfee", fp.commitmentAddr,
			&payfeeResponse, json.RawMessage(requestBody))
		if err == nil {
			break
		}
		var apiErr *BadRequestError
		if attempt != 0 || !errors.As(err, &apiErr) || apiErr.Code != codeFeeExpired {
			return fmt.Errorf("payfee: %w", err)
		}
		if refreshErr := fp.refreshExpiredFeeQuote(feeTx); refreshErr != nil {
			if errors.As(refreshErr, &apiErr) && apiErr.Code == codeFeeAlreadyReceived {
				return fmt.Errorf("payfee: %w", refreshErr)
			}
			return fmt.Errorf("payfee: %w; could not refresh expired quote: %w", err, refreshErr)
		}
	}

	// Check for matching original request.
	// This is signed by the VSP, and the signature
	// has already been checked above.
	if !bytes.Equal(requestBody, payfeeResponse.Request) {
		return fmt.Errorf("server response contains differing request")
	}
	// TODO - validate server timestamp?

	log.Infof("successfully processed %v", fp.ticketHash)
	return nil
}

func (fp *feePayment) refreshExpiredFeeQuote(feeTx *wire.MsgTx) error {
	feeHash := feeTx.TxHash()
	w := fp.client.Wallet
	info, err := w.VSPTicketInfo(fp.ctx, &fp.ticketHash)
	if err != nil {
		return err
	}
	if info.FeeHash != feeHash || info.FeeTxStatus == uint32(udb.VSPFeeProcessConfirmed) {
		return fmt.Errorf("saved VSP payment changed or is already confirmed; payment was retained")
	}
	_, height, err := w.TxBlock(fp.ctx, &feeHash)
	if err != nil {
		return err
	}
	if height != -1 {
		return fmt.Errorf("saved VSP fee is already mined; payment was retained")
	}
	if err := fp.receiveFeeAddress(); err != nil {
		return err
	}
	fp.mu.Lock()
	fee, address := fp.fee, fp.feeAddr
	fp.mu.Unlock()
	if fee <= 0 {
		return fmt.Errorf("server fee amount is invalid; payment was retained")
	}
	version, script := address.PaymentScript()
	for _, output := range feeTx.TxOut {
		if output.Version == version && bytes.Equal(output.PkScript, script) {
			if output.Value >= int64(fee) {
				return nil
			}
			break
		}
	}
	return fmt.Errorf("updated VSP quote is not covered by the saved payment; payment was retained for review")
}

func (fp *feePayment) confirmPayment() (err error) {
	ctx := fp.ctx
	if err := ctx.Err(); err != nil {
		return err
	}

	// stop processing if ticket is expired or spent
	if fp.removedExpiredOrSpent() {
		// nothing scheduled
		return errStopped
	}

	status, err := fp.client.status(ctx, &fp.ticketHash)
	if err != nil {
		log.Warnf("Rescheduling status check for %v: %v", &fp.ticketHash, err)
		fp.schedule("confirm payment", fp.confirmPayment)
		return err
	}

	switch status.FeeTxStatus {
	case "received":
		// VSP has received the fee tx but has not yet broadcast it.
		// VSP will only broadcast the tx when ticket has 6+ confirmations.
		fp.schedule("confirm payment", fp.confirmPayment)
		return nil
	case "broadcast":
		// Retain confirmation polling even if validation or storage fails.
		fp.schedule("confirm payment", fp.confirmPayment)
		fp.mu.Lock()
		feeHash, feeTx, blocked := fp.feeHash, fp.feeTx, fp.blocked
		fp.mu.Unlock()
		if blocked || feeTx == nil {
			return errMissingFee
		}
		providerHash, err := chainhash.NewHashFromStr(status.FeeTxHash)
		if err != nil || status.FeeTxHash == "" || feeHash == (chainhash.Hash{}) ||
			*providerHash != feeHash || feeTx.TxHash() != feeHash {
			return fmt.Errorf("VSP broadcast status does not match the saved fee transaction")
		}
		if err := fp.client.Wallet.SetPublished(ctx, &feeHash, true); err != nil {
			return err
		}
		log.Infof("VSP has successfully sent the fee tx for %v", &fp.ticketHash)
		return nil
	case "confirmed":
		return fp.recordConfirmedFee()
	case "none", "error":
		fp.mu.Lock()
		blocked := fp.blocked
		fp.mu.Unlock()
		if blocked {
			fp.schedule("confirm payment", fp.confirmPayment)
			return errMissingFee
		}
		log.Warnf("VSP reports fee status %q for %v -- reconciling payment",
			status.FeeTxStatus, &fp.ticketHash)
		fp.schedule("reconcile payment", fp.reconcilePayment)
		return nil
	default:
		// XXX put in unknown state
		log.Warnf("VSP responded with %v for %v", status.FeeTxStatus,
			&fp.ticketHash)
		fp.schedule("confirm payment", fp.confirmPayment)
		return fmt.Errorf("VSP returned unknown fee status %q", status.FeeTxStatus)
	}

}

func (fp *feePayment) recordConfirmedFee() error {
	fp.mu.Lock()
	feeHash := fp.feeHash
	fp.mu.Unlock()
	err := fp.client.Wallet.UpdateVspTicketFeeToConfirmed(fp.ctx, &fp.ticketHash,
		&feeHash, fp.client.url, fp.client.pub)
	if err != nil {
		fp.schedule("confirm payment", fp.confirmPayment)
		return err
	}
	fp.remove("confirmed by VSP")
	return nil
}

type marshaler struct {
	marshaled []byte
	err       error
}

func (m *marshaler) MarshalJSON() ([]byte, error) {
	return m.marshaled, m.err
}

func txMarshaler(tx *wire.MsgTx) json.Marshaler {
	var buf bytes.Buffer
	buf.Grow(2 + tx.SerializeSize()*2)
	buf.WriteByte('"')
	err := tx.Serialize(hex.NewEncoder(&buf))
	buf.WriteByte('"')
	return &marshaler{buf.Bytes(), err}
}
