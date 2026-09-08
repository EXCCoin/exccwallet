package wallet

import "sync"

// WalletStateNotifications reports changes to wallet or account key access.
// Notifications may be coalesced; subscribers must query the current state.
func (s *NotificationServer) WalletStateNotifications() (<-chan struct{}, func()) {
	c := make(chan struct{}, 1)
	s.walletStateMu.Lock()
	if s.walletStateClients == nil {
		s.walletStateClients = make(map[chan struct{}]struct{})
	}
	s.walletStateClients[c] = struct{}{}
	s.walletStateMu.Unlock()
	var once sync.Once
	return c, func() {
		once.Do(func() {
			s.walletStateMu.Lock()
			delete(s.walletStateClients, c)
			s.walletStateMu.Unlock()
		})
	}
}

func (s *NotificationServer) notifyWalletState() {
	s.walletStateMu.Lock()
	defer s.walletStateMu.Unlock()
	for c := range s.walletStateClients {
		select {
		case c <- struct{}{}:
		default:
		}
	}
}
