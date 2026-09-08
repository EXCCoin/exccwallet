package vsp

import (
	"bytes"
	"context"
	stderrors "errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/dcrutil/v4"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"golang.org/x/sync/errgroup"
)

type DialFunc func(ctx context.Context, network, addr string) (net.Conn, error)

type Policy struct {
	MaxFee     dcrutil.Amount
	ChangeAcct uint32 // to derive fee addresses
	FeeAcct    uint32 // to pay fees from, if inputs are not provided to Process
}

// BindPolicy returns a fee payment callback using a request-specific policy.
func BindPolicy(process func(context.Context, *chainhash.Hash, *wire.MsgTx, Policy) error,
	policy Policy) func(context.Context, *chainhash.Hash, *wire.MsgTx) error {

	return func(ctx context.Context, hash *chainhash.Hash, tx *wire.MsgTx) error {
		return process(ctx, hash, tx, policy)
	}
}

type Client struct {
	Wallet *wallet.Wallet
	Policy Policy
	*client

	mu            sync.Mutex
	jobs          map[chainhash.Hash]*feePayment
	jobGeneration map[chainhash.Hash]uint64
	ctx           context.Context
	cancel        context.CancelFunc
	closed        bool
}

type Config struct {
	// URL specifies the base URL of the VSP
	URL string

	// PubKey specifies the VSP's base64 encoded public key
	PubKey string

	// Dialer specifies an optional dialer when connecting to the VSP.
	Dialer DialFunc

	// Wallet specifies a loaded wallet.
	Wallet *wallet.Wallet

	// Default policy for fee payments unless another is provided by the
	// caller.
	Policy Policy
}

func New(cfg Config) (*Client, error) {
	host, pubKey, err := ParseVSPIdentity(cfg.URL, cfg.PubKey)
	if err != nil {
		return nil, err
	}
	if cfg.Wallet == nil {
		return nil, fmt.Errorf("wallet option not set")
	}

	client := newClient(host, pubKey, cfg.Wallet)
	client.Transport = &http.Transport{
		DialContext: cfg.Dialer,
	}

	ctx, cancel := context.WithCancel(context.Background())
	v := &Client{
		Wallet:        cfg.Wallet,
		Policy:        cfg.Policy,
		client:        client,
		jobs:          make(map[chainhash.Hash]*feePayment),
		jobGeneration: make(map[chainhash.Hash]uint64),
		ctx:           ctx,
		cancel:        cancel,
	}
	return v, nil
}

func (c *Client) FeePercentage(ctx context.Context) (float64, error) {
	var resp struct {
		FeePercentage float64 `json:"feepercentage"`
	}
	err := c.get(ctx, "/api/v3/vspinfo", &resp)
	if err != nil {
		return -1, err
	}
	return resp.FeePercentage, nil
}

// ProcessUnprocessedTickets processes all tickets that don't currently have
// any association with a VSP.
func (c *Client) ProcessUnprocessedTickets(ctx context.Context, policy Policy) error {
	var wg sync.WaitGroup
	err := c.Wallet.ForUnspentUnexpiredTickets(ctx, func(hash *chainhash.Hash) error {
		// Skip tickets which have a fee tx already associated with
		// them; they are already processed by some vsp.
		_, err := c.Wallet.VSPFeeHashForTicket(ctx, hash)
		if err == nil {
			return nil
		}
		if !errors.Is(err, errors.NotExist) {
			return err
		}
		confirmed, err := c.Wallet.IsVSPTicketConfirmed(ctx, hash)
		if err != nil && !errors.Is(err, errors.NotExist) {
			return err
		}

		if confirmed {
			return nil
		}

		c.mu.Lock()
		fp := c.jobs[*hash]
		c.mu.Unlock()
		if fp != nil {
			// Already processing this ticket with the VSP.
			return nil
		}

		// Start processing in the background.
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := c.ProcessWithPolicy(ctx, hash, nil, policy)
			if err != nil {
				log.Error(err)
			}
		}()

		return nil
	})
	wg.Wait()
	return err
}

// ProcessTicket attempts to process a given ticket based on the hash provided.
func (c *Client) ProcessTicket(ctx context.Context, hash *chainhash.Hash) error {
	err := c.Process(ctx, hash, nil)
	if err != nil {
		return err
	}
	return nil
}

func isUnknownTicketError(err error) bool {
	var apiErr *BadRequestError
	return errors.As(err, &apiErr) && apiErr.Code == codeUnknownTicket
}

// ProcessManagedTickets resumes locally associated tickets at the VSP.
func (c *Client) ProcessManagedTickets(ctx context.Context, policy Policy) error {
	_, err := c.processManagedTickets(ctx, policy, true)
	return err
}

// RecoverManagedTickets discovers tickets previously registered with a VSP.
// This recovers tracking after seed restores and only considers unspent and
// unexpired tickets.
func (c *Client) RecoverManagedTickets(ctx context.Context, policy Policy) error {
	_, err := c.RecoverManagedTicketsWithResults(ctx, policy)
	return err
}

type ManagedTicketStatus string

const (
	ManagedTicketStarted        ManagedTicketStatus = "started"
	ManagedTicketAlreadyTracked ManagedTicketStatus = "already_tracked"
	ManagedTicketConfirmed      ManagedTicketStatus = "confirmed"
	ManagedTicketUnknownToVSP   ManagedTicketStatus = "unknown_to_vsp"
	ManagedTicketNotManaged     ManagedTicketStatus = "not_managed"
	ManagedTicketDifferentVSP   ManagedTicketStatus = "different_vsp"
	ManagedTicketFailed         ManagedTicketStatus = "failed"
)

type ManagedTicketResult struct {
	TicketHash chainhash.Hash
	Status     ManagedTicketStatus
	Err        error
}

// RecoverManagedTicketsWithResults preserves each result even when another
// ticket fails. An empty result list with an error means discovery failed.
func (c *Client) RecoverManagedTicketsWithResults(ctx context.Context, policy Policy) ([]ManagedTicketResult, error) {
	return c.processManagedTickets(ctx, policy, false)
}

func (c *Client) sameProvider(host string, pubkey []byte) bool {
	canonical, err := CanonicalVSPHost(host)
	return err == nil && canonical == c.client.url && bytes.Equal(pubkey, c.client.pub)
}

const maxConcurrentManagedTickets = 8

func processManagedTicketHashes(hashes []chainhash.Hash,
	process func(*chainhash.Hash) error) error {

	var group errgroup.Group
	group.SetLimit(maxConcurrentManagedTickets)
	for i := range hashes {
		hash := &hashes[i]
		group.Go(func() error { return process(hash) })
	}
	return group.Wait()
}

func (c *Client) processManagedTickets(ctx context.Context, policy Policy, localOnly bool) ([]ManagedTicketResult, error) {
	var hashes []chainhash.Hash
	err := c.Wallet.ForUnspentUnexpiredTickets(ctx, func(hash *chainhash.Hash) error {
		hashes = append(hashes, *hash)
		return nil
	})
	if err != nil {
		return nil, err
	}
	unmined, err := c.Wallet.UnminedVSPTickets(ctx, c.sameProvider)
	if err != nil {
		return nil, err
	}
	seen := make(map[chainhash.Hash]bool, len(hashes))
	for _, hash := range hashes {
		seen[hash] = true
	}
	for _, hash := range unmined {
		if !seen[hash] {
			hashes = append(hashes, hash)
			seen[hash] = true
		}
	}
	results := make([]ManagedTicketResult, len(hashes))
	indices := make(map[chainhash.Hash]int, len(hashes))
	for i, hash := range hashes {
		indices[hash] = i
	}
	err = processManagedTicketHashes(hashes, func(hash *chainhash.Hash) (err error) {
		result := &results[indices[*hash]]
		result.TicketHash = *hash
		result.Status = ManagedTicketFailed
		defer func() { result.Err = err }()
		info, err := c.Wallet.VSPTicketInfo(ctx, hash)
		if errors.Is(err, errors.NotExist) {
			if localOnly {
				result.Status = ManagedTicketNotManaged
				return nil
			}
		} else if err != nil {
			return err
		} else {
			if info.Host != "" && !c.sameProvider(info.Host, info.PubKey) {
				result.Status = ManagedTicketDifferentVSP
				return nil
			}
			if udb.FeeStatus(info.FeeTxStatus) == udb.VSPFeeProcessConfirmed {
				result.Status = ManagedTicketConfirmed
				return nil
			}
		}
		c.mu.Lock()
		_, ok := c.jobs[*hash]
		c.mu.Unlock()
		if ok {
			// Already processing this ticket with the VSP.
			result.Status = ManagedTicketAlreadyTracked
			return nil
		}

		// Make ticketstatus api call and only continue if ticket is
		// found managed by this vsp.  The rest is the same codepath as
		// for processing a new ticket.
		status, err := c.status(ctx, hash)
		if err != nil {
			if isUnknownTicketError(err) {
				result.Status = ManagedTicketUnknownToVSP
				return nil
			}
			return err
		}

		var fp *feePayment
		if status.FeeTxStatus == "confirmed" {
			feeHash, err := chainhash.NewHashFromStr(status.FeeTxHash)
			if err != nil {
				return err
			}
			err = c.Wallet.UpdateVspTicketFeeToConfirmed(ctx, hash, feeHash, c.client.url, c.client.pub)
			if err != nil {
				return err
			}
			result.Status = ManagedTicketConfirmed
			return nil
		} else if status.FeeTxHash != "" {
			feeHash, err := chainhash.NewHashFromStr(status.FeeTxHash)
			if err != nil {
				return err
			}
			err = c.Wallet.UpdateVspTicketFeeToPaid(ctx, hash, feeHash, c.client.url, c.client.pub)
			if err != nil {
				return err
			}
			fp = c.feePayment(hash, policy, true)
		} else {
			// Fee hasn't been paid at the provided VSP, so this should do that if needed.
			fp = c.feePayment(hash, policy, false)
		}
		if fp == nil {
			return fmt.Errorf("fee payment cannot be processed")
		}
		fp.mu.Lock()
		blocked, failure := fp.blocked, fp.err
		fp.mu.Unlock()
		if blocked {
			if failure != nil {
				return failure
			}
			return errMissingFee
		}
		result.Status = ManagedTicketStarted
		return nil
	})
	return results, err
}

// Process begins processing a VSP fee payment for a ticket.  If feeTx contains
// inputs, is used to pay the VSP fee.  Otherwise, new inputs are selected and
// locked to prevent double spending the fee.
//
// feeTx must not be nil, but may point to an empty transaction, and is modified
// with the inputs and the fee and change outputs before returning without an
// error.  The fee transaction is also recorded as unpublised in the wallet, and
// the fee hash is associated with the ticket.
func (c *Client) Process(ctx context.Context, ticketHash *chainhash.Hash, feeTx *wire.MsgTx) error {
	return c.ProcessWithPolicy(ctx, ticketHash, feeTx, c.Policy)
}

// ProcessWithPolicy is the same as Process but allows a fee payment policy to
// be specified, instead of using the client's default policy.
func (c *Client) ProcessWithPolicy(ctx context.Context, ticketHash *chainhash.Hash, feeTx *wire.MsgTx,
	policy Policy) error {

	vspTicket, err := c.Wallet.VSPTicketInfo(ctx, ticketHash)
	if err != nil && !errors.Is(err, errors.NotExist) {
		return err
	}
	feeStatus := udb.VSPFeeProcessStarted // Will be used if the ticket isn't registered to the vsp yet.
	if vspTicket != nil {
		feeStatus = udb.FeeStatus(vspTicket.FeeTxStatus)
	}

	switch feeStatus {
	case udb.VSPFeeProcessStarted, udb.VSPFeeProcessErrored:
		// If VSPTicket has been started or errored then attempt to create a new fee
		// transaction, submit it then confirm.
		fp := c.feePayment(ticketHash, policy, false)
		if fp == nil {
			err := c.Wallet.UpdateVspTicketFeeToErrored(ctx, ticketHash, c.client.url, c.client.pub)
			if err != nil {
				return err
			}
			return fmt.Errorf("fee payment cannot be processed")
		}
		fp.mu.Lock()
		if fp.feeTx == nil {
			fp.feeTx = feeTx
		}
		fp.mu.Unlock()
		err := fp.receiveFeeAddress()
		if err != nil {
			updateErr := c.Wallet.UpdateVspTicketFeeToErrored(ctx, ticketHash, c.client.url, c.client.pub)
			if updateErr != nil {
				return stderrors.Join(err, updateErr)
			}
			// XXX, retry? (old Process retried)
			// but this may not be necessary any longer as the parent of
			// the ticket is always relayed to the vsp as well.
			return err
		}
		err = fp.makeFeeTx(feeTx)
		if err != nil {
			updateErr := c.Wallet.UpdateVspTicketFeeToErrored(ctx, ticketHash, c.client.url, c.client.pub)
			if updateErr != nil {
				return stderrors.Join(err, updateErr)
			}
			return err
		}
		return fp.submitPayment()
	case udb.VSPFeeProcessPaid:
		// If a VSP ticket has been paid, but confirm payment.
		if len(vspTicket.Host) > 0 && !c.sameProvider(vspTicket.Host, vspTicket.PubKey) {
			// Cannot confirm a paid ticket that is already with another VSP.
			return fmt.Errorf("ticket already paid or confirmed with another vsp")
		}
		fp := c.feePayment(ticketHash, policy, true)
		if fp == nil {
			// Don't update VSPStatus to Errored if it was already paid or
			// confirmed.
			return fmt.Errorf("fee payment cannot be processed")
		}

		return fp.confirmPayment()
	case udb.VSPFeeProcessConfirmed:
		// VSPTicket has already been confirmed, there is nothing to process.
		return nil
	}
	return nil
}

// SetVoteChoice takes the provided consensus, tspend and treasury key voting
// preferences, and checks if they match the status of the specified ticket from
// the connected VSP. The status provides the current voting preferences so we
// can just update from there if need be.
func (c *Client) SetVoteChoice(ctx context.Context, hash *chainhash.Hash,
	choices []wallet.AgendaChoice, tspendPolicy map[string]string, treasuryPolicy map[string]string) error {

	// Retrieve current voting preferences from VSP.
	status, err := c.status(ctx, hash)
	if err != nil {
		if errors.Is(err, errors.Locked) {
			return err
		}
		log.Errorf("Could not check status of VSP ticket %s: %v", hash, err)
		return err
	}

	update := voteChoicesChanged(status, choices, tspendPolicy, treasuryPolicy)

	if !update {
		log.Debugf("VSP already has correct vote choices for ticket %s", hash)
		return nil
	}

	log.Debugf("Updating vote choices on VSP for ticket %s", hash)
	err = c.setVoteChoices(ctx, hash, choices, tspendPolicy, treasuryPolicy)
	if err != nil {
		return err
	}
	return nil
}

func voteChoicesChanged(status *ticketStatus, choices []wallet.AgendaChoice,
	tspendPolicy, treasuryPolicy map[string]string) bool {

	for _, newChoice := range choices {
		vspChoice, ok := status.VoteChoices[newChoice.AgendaID]
		if !ok || vspChoice != newChoice.ChoiceID {
			return true
		}
	}
	return votePolicyChanged(status.TSpendPolicy, tspendPolicy) ||
		votePolicyChanged(status.TreasuryPolicy, treasuryPolicy)
}

func votePolicyChanged(current, requested map[string]string) bool {
	for key, choice := range requested {
		value, ok := current[key]
		if !ok || value != choice {
			return true
		}
	}
	return false
}

// TicketInfo stores per-ticket info tracked by a VSP Client instance.
type TicketInfo struct {
	TicketHash     chainhash.Hash
	CommitmentAddr stdaddr.StakeAddress
	VotingAddr     stdaddr.StakeAddress
	State          uint32
	Fee            dcrutil.Amount
	FeeHash        chainhash.Hash
	Phase          string
	LastError      string
	NeedsUnlock    bool
	NextRetry      time.Time
}

// TrackedTickets returns information about all outstanding tickets tracked by
// a vsp.Client instance.
//
// Currently this returns only info about tickets which fee hasn't been paid or
// confirmed at enough depth to be considered committed to.
func (c *Client) TrackedTickets() []*TicketInfo {
	// Collect all jobs first, to avoid working under two different locks.
	c.mu.Lock()
	jobs := make([]*feePayment, 0, len(c.jobs))
	for _, job := range c.jobs {
		jobs = append(jobs, job)
	}
	c.mu.Unlock()

	tickets := make([]*TicketInfo, 0, len(jobs))
	for _, job := range jobs {
		job.mu.Lock()
		info := &TicketInfo{
			TicketHash:     job.ticketHash,
			CommitmentAddr: job.commitmentAddr,
			VotingAddr:     job.votingAddr,
			State:          uint32(job.state),
			Fee:            job.fee,
			FeeHash:        job.feeHash,
			NeedsUnlock:    errors.Is(job.err, errors.Locked),
		}
		if job.err != nil {
			info.LastError = job.err.Error()
		} else if job.blocked {
			info.LastError = errMissingFee.Error()
		}
		job.mu.Unlock()
		job.timerMu.Lock()
		info.Phase = job.phase
		info.NextRetry = job.nextRetry
		job.timerMu.Unlock()
		tickets = append(tickets, info)
	}

	return tickets
}
