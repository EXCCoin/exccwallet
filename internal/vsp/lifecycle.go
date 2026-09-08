package vsp

import "github.com/EXCCoin/exccd/chaincfg/chainhash"

// RemoveTicket stops this ticket's scheduled work without waiting for a
// database transaction which may be held by the caller pruning the ticket.
func (c *Client) RemoveTicket(hash chainhash.Hash, reason string) {
	c.mu.Lock()
	if c.jobGeneration == nil {
		c.jobGeneration = make(map[chainhash.Hash]uint64)
	}
	c.jobGeneration[hash]++
	fp := c.jobs[hash]
	delete(c.jobs, hash)
	c.mu.Unlock()
	if fp != nil {
		fp.stop()
		log.Infof("ticket %v is %s; removing from VSP client", &hash, reason)
	}
}

// Close prevents new fee jobs and cancels this wallet's outstanding work.
func (c *Client) Close() {
	c.mu.Lock()
	c.closed = true
	jobs := c.jobs
	c.jobs = make(map[chainhash.Hash]*feePayment)
	if c.cancel != nil {
		c.cancel()
	}
	c.mu.Unlock()
	for _, fp := range jobs {
		fp.stop()
	}
	c.CloseIdleConnections()
}
