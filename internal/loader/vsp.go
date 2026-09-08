package loader

import (
	"sync"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/internal/vsp"
	"github.com/EXCCoin/exccwallet/v2/wallet"
)

type cachedVSP struct {
	client *vsp.Client
	pubKey string
}

var vspClients = struct {
	mu      sync.Mutex
	wallets map[*wallet.Wallet]map[string]cachedVSP
}{wallets: make(map[*wallet.Wallet]map[string]cachedVSP)}

func registerVSPWallet(w *wallet.Wallet) {
	vspClients.mu.Lock()
	vspClients.wallets[w] = make(map[string]cachedVSP)
	vspClients.mu.Unlock()
	w.SetVSPTicketPruner(func(hash chainhash.Hash) {
		for _, client := range AllVSPs(w) {
			client.RemoveTicket(hash, "pruned")
		}
	})
}

func closeVSPWallet(w *wallet.Wallet) {
	vspClients.mu.Lock()
	clients := vspClients.wallets[w]
	delete(vspClients.wallets, w)
	vspClients.mu.Unlock()
	for _, cached := range clients {
		cached.client.Close()
	}
}

// VSP reuses clients only within the same loaded wallet and approved identity.
// Request-specific policies are passed separately to the client operations.
func VSP(cfg vsp.Config) (*vsp.Client, error) {
	host, pubKey, err := vsp.ParseVSPIdentity(cfg.URL, cfg.PubKey)
	if err != nil {
		return nil, err
	}
	vspClients.mu.Lock()
	defer vspClients.mu.Unlock()
	clients, ok := vspClients.wallets[cfg.Wallet]
	if !ok {
		return nil, errors.E(errors.Invalid, "VSP wallet is not loaded")
	}
	if cached, ok := clients[host]; ok {
		if cached.pubKey != string(pubKey) {
			return nil, errors.E(errors.Permission,
				"VSP public key changed; reopen the wallet before using the new identity")
		}
		return cached.client, nil
	}
	client, err := vsp.New(cfg)
	if err != nil {
		return nil, err
	}
	clients[host] = cachedVSP{client: client, pubKey: string(pubKey)}
	return client, nil
}

// LookupVSP returns a previously configured client for this loaded wallet.
func LookupVSP(w *wallet.Wallet, host string) (*vsp.Client, error) {
	host, err := vsp.CanonicalVSPHost(host)
	if err != nil {
		return nil, err
	}
	vspClients.mu.Lock()
	defer vspClients.mu.Unlock()
	cached, ok := vspClients.wallets[w][host]
	if !ok {
		return nil, errors.E(errors.NotExist,
			errors.Errorf("VSP client for %q not found", host))
	}
	return cached.client, nil
}

// AllVSPs returns a copy of this wallet's configured clients.
func AllVSPs(w *wallet.Wallet) map[string]*vsp.Client {
	vspClients.mu.Lock()
	defer vspClients.mu.Unlock()
	clients := vspClients.wallets[w]
	res := make(map[string]*vsp.Client, len(clients))
	for host, cached := range clients {
		res[host] = cached.client
	}
	return res
}
