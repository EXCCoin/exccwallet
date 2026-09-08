package spv

import (
	"bytes"
	"context"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/addrmgr/v2"
	"github.com/EXCCoin/exccd/blockchain/stake/v4"
	blockchain "github.com/EXCCoin/exccd/blockchain/standalone/v2"
	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccd/txscript/v4"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/p2p"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	_ "github.com/EXCCoin/exccwallet/v2/wallet/drivers/bdb"
)

// wireTestConn gives an in-memory connection the address type the real peer
// handshake expects. All wire messages stay within this test process.
type wireTestConn struct{ net.Conn }

func (c wireTestConn) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 19108}
}
func (c wireTestConn) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(198, 51, 100, 42), Port: 19108}
}

func validationPeer(t *testing.T, services wire.ServiceFlag, block *wire.MsgBlock, tx *wire.MsgTx) (*p2p.LocalPeer, *p2p.RemotePeer) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	local, remote := net.Pipe()
	amgr := addrmgr.New(t.TempDir(), net.LookupIP)
	lp := p2p.NewLocalPeer(chaincfg.SimNetParams(), nil, amgr)
	lp.SetDialFunc(func(context.Context, string, string) (net.Conn, error) { return wireTestConn{local}, nil })
	done := make(chan struct{})
	go func() {
		defer close(done)
		defer remote.Close()
		for {
			msg, _, err := wire.ReadMessage(remote, p2p.Pver, wire.SimNet)
			if err != nil {
				return
			}
			var replies []wire.Message
			switch msg := msg.(type) {
			case *wire.MsgVersion:
				addr := wire.NewNetAddressIPPort(net.IPv4(127, 0, 0, 1), 19108, services)
				v := wire.NewMsgVersion(addr, addr, 1, 0)
				v.ProtocolVersion = int32(p2p.Pver)
				v.Services = services
				replies = append(replies, v)
			case *wire.MsgGetAddr:
				replies = append(replies, wire.NewMsgAddr())
			case *wire.MsgGetData:
				for _, inv := range msg.InvList {
					if inv.Type == wire.InvTypeBlock && block != nil {
						replies = append(replies, block)
					}
					if inv.Type == wire.InvTypeTx && tx != nil {
						replies = append(replies, tx)
					}
				}
			}
			for _, reply := range replies {
				if err := wire.WriteMessage(remote, reply, p2p.Pver, wire.SimNet); err != nil {
					return
				}
			}
		}
	}()
	t.Cleanup(func() { cancel(); local.Close(); remote.Close(); <-done })
	rp, err := lp.ConnectOutbound(ctx, "198.51.100.42:19108", 0)
	if err != nil {
		t.Fatal(err)
	}
	return lp, rp
}

func merkleTestBlock(combined bool) *wire.MsgBlock {
	tx := func(value int64) *wire.MsgTx {
		tx := wire.NewMsgTx()
		tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{byte(value)}}, value, []byte{1, 1}))
		tx.AddTxOut(wire.NewTxOut(value, []byte{txscript.OP_TRUE}))
		return tx
	}
	b := &wire.MsgBlock{Transactions: []*wire.MsgTx{tx(100)}, STransactions: []*wire.MsgTx{tx(200)}}
	b.Header.MerkleRoot = blockchain.CalcTxTreeMerkleRoot(b.Transactions)
	b.Header.StakeRoot = blockchain.CalcTxTreeMerkleRoot(b.STransactions)
	if combined {
		b.Header.MerkleRoot = blockchain.CalcCombinedTxTreeMerkleRoot(b.Transactions, b.STransactions)
	}
	return b
}

func TestBlocksFromPeerValidatesTransactionTrees(t *testing.T) {
	for _, name := range []string{"separate roots", "combined root", "regular body changed", "stake body changed", "regular witness changed", "combined stake changed"} {
		t.Run(name, func(t *testing.T) {
			combined := name == "combined root" || name == "combined stake changed"
			block := merkleTestBlock(combined)
			hash := block.BlockHash()
			invalid := false
			switch name {
			case "regular body changed":
				block.Transactions[0].TxOut[0].Value++
				invalid = true
			case "stake body changed", "combined stake changed":
				block.STransactions[0].TxOut[0].Value++
				invalid = true
			case "regular witness changed":
				block.Transactions[0].TxIn[0].SignatureScript = []byte{1, 2}
				invalid = true
			}
			if block.BlockHash() != hash {
				t.Fatal("test unexpectedly changed header hash")
			}
			_, rp := validationPeer(t, wire.SFNodeNetwork, block, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			blocks, err := blocksFromPeer(ctx, rp, []*chainhash.Hash{&hash})
			if invalid {
				if !errors.Is(err, errors.Consensus) || len(blocks) != 0 {
					t.Fatalf("tampered block accepted: %v", err)
				}
				if !errors.Is(rp.Err(), errors.Consensus) {
					t.Fatal("invalid block peer remains connected")
				}
			} else if err != nil || len(blocks) != 1 {
				t.Fatalf("valid block rejected: %v", err)
			}
		})
	}
}

func TestBlockRetrievalPathsRejectTamperedBodies(t *testing.T) {
	for _, path := range []string{"network backend", "block announcement"} {
		t.Run(path, func(t *testing.T) {
			block := merkleTestBlock(false)
			hash := block.BlockHash()
			block.STransactions[0].TxOut[0].Value++
			_, rp := validationPeer(t, wire.SFNodeNetwork, block, nil)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s := &Syncer{remotes: map[string]*p2p.RemotePeer{"test": rp}}
			if path == "network backend" {
				removed := make(chan struct{})
				go func() { rp.Err(); s.remotesMu.Lock(); delete(s.remotes, "test"); s.remotesMu.Unlock(); close(removed) }()
				blocks, err := s.Blocks(ctx, []*chainhash.Hash{&hash})
				if err == nil || len(blocks) != 0 {
					t.Fatal("network backend returned tampered block")
				}
				<-removed
			} else {
				if err := rp.SendHeaders(ctx); err != nil {
					t.Fatal(err)
				}
				if err := s.handleBlockInvs(ctx, rp, []*chainhash.Hash{&hash}); !errors.Is(err, errors.Consensus) {
					t.Fatalf("announcement error = %v", err)
				}
			}
			if !errors.Is(rp.Err(), errors.Consensus) {
				t.Fatal("invalid block peer remains connected")
			}
		})
	}
}

func validationWallet(t *testing.T) (*wallet.Wallet, wire.OutPoint) {
	t.Helper()
	ctx := context.Background()
	db, err := wallet.CreateDB("bdb", filepath.Join(t.TempDir(), "wallet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	params := chaincfg.SimNetParams()
	pub, priv := []byte(wallet.InsecurePubPassphrase), []byte("test-password")
	if err := wallet.Create(ctx, db, pub, priv, bytes.Repeat([]byte{2}, 32), params); err != nil {
		t.Fatal(err)
	}
	w, err := wallet.Open(ctx, &wallet.Config{DB: db, PubPassphrase: pub, Params: params, GapLimit: 20})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.Unlock(ctx, priv, nil); err != nil {
		t.Fatal(err)
	}
	addr, err := w.NewExternalAddress(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	version, script := addr.PaymentScript()
	funding := wire.NewMsgTx()
	funding.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{88}}, 100000, nil))
	funding.AddTxOut(&wire.TxOut{Value: 100000, Version: version, PkScript: script})
	if err := w.AddTransaction(ctx, funding, nil); err != nil {
		t.Fatal(err)
	}
	return w, wire.OutPoint{Hash: funding.TxHash(), Tree: wire.TxTreeRegular}
}

func TestTxAnnouncementsRejectForgedWalletSpends(t *testing.T) {
	for _, name := range []string{"full node", "SPV peer", "valid full node"} {
		t.Run(name, func(t *testing.T) {
			w, out := validationWallet(t)
			tx := wire.NewMsgTx()
			tx.AddTxIn(wire.NewTxIn(&out, 100000, nil))
			tx.AddTxOut(wire.NewTxOut(90000, []byte{txscript.OP_TRUE}))
			valid := name == "valid full node"
			if valid {
				errs, err := w.SignTransaction(context.Background(), tx, txscript.SigHashAll, nil, nil, nil)
				if err != nil || len(errs) != 0 {
					t.Fatalf("sign: %v %v", err, errs)
				}
				tx.TxIn[0].ValueIn = wire.NullValueIn
			}
			services := wire.SFNodeNetwork
			if name == "SPV peer" {
				services = 0
			}
			lp, rp := validationPeer(t, services, nil, tx)
			s := NewSyncer(w, lp)
			s.rescanFilter.AddUnspentOutPoint(&out)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			hash := tx.TxHash()
			s.handleTxInvs(ctx, rp, []*chainhash.Hash{&hash})
			stored, missing, err := w.GetTransactionsByHashes(ctx, []*chainhash.Hash{&hash})
			if valid {
				if err != nil || len(stored) != 1 {
					t.Fatalf("valid announcement rejected: %v", err)
				}
			} else if !errors.Is(err, errors.NotExist) || len(missing) != 1 {
				t.Fatalf("forged announcement stored: %v", err)
			}
			if name == "full node" {
				if !errors.Is(rp.Err(), errors.ScriptFailure) {
					t.Fatal("forging full node not disconnected")
				}
			} else {
				// Successful transmission proves the valid/full or unvalidated
				// SPV peer was not disconnected by our rejection policy.
				if err := rp.SendHeaders(ctx); err != nil {
					t.Fatalf("peer disconnected: %v", err)
				}
			}
		})
	}
}

func TestForgedWitnessDoesNotSuppressValidAnnouncement(t *testing.T) {
	for _, services := range []wire.ServiceFlag{0, wire.SFNodeNetwork} {
		t.Run(services.String(), func(t *testing.T) {
			w, out := validationWallet(t)
			valid := wire.NewMsgTx()
			valid.AddTxIn(wire.NewTxIn(&out, 100000, nil))
			valid.AddTxOut(wire.NewTxOut(90000, []byte{txscript.OP_TRUE}))
			errs, err := w.SignTransaction(context.Background(), valid, txscript.SigHashAll, nil, nil, nil)
			if err != nil || len(errs) != 0 {
				t.Fatalf("sign: %v %v", err, errs)
			}
			forged := valid.Copy()
			forged.TxIn[0].SignatureScript = nil
			hash := valid.TxHash()
			if forged.TxHash() != hash {
				t.Fatal("test changed transaction id")
			}
			lp, badPeer := validationPeer(t, services, nil, forged)
			_, goodPeer := validationPeer(t, wire.SFNodeNetwork, nil, valid)
			s := NewSyncer(w, lp)
			s.rescanFilter.AddUnspentOutPoint(&out)
			var notified []*wire.MsgTx
			s.notifications = &Notifications{MempoolTxs: func(txs []*wire.MsgTx) { notified = append(notified, txs...) }}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			s.handleTxInvs(ctx, badPeer, []*chainhash.Hash{&hash})
			if s.seenTxs.Contains(hash) || len(notified) != 0 {
				t.Fatal("forged witness was cached or announced to wallet clients")
			}
			s.handleTxInvs(ctx, goodPeer, []*chainhash.Hash{&hash})
			if !s.seenTxs.Contains(hash) || len(notified) != 1 || len(notified[0].TxIn[0].SignatureScript) == 0 {
				t.Fatal("valid later witness was suppressed")
			}
			stored, _, err := w.GetTransactionsByHashes(ctx, []*chainhash.Hash{&hash})
			if err != nil || len(stored) != 1 || len(stored[0].TxIn[0].SignatureScript) == 0 {
				t.Fatalf("valid witness not stored: %v", err)
			}
		})
	}
}

func TestAutomaticRevocationAnnouncementIsDeferred(t *testing.T) {
	w, _ := validationWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	addr, err := w.NewExternalAddress(ctx, 0)
	if err != nil {
		t.Fatal(err)
	}
	a := addr.(stdaddr.StakeAddress)
	ticket := wire.NewMsgTx()
	ticket.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{39}}, 100000, nil))
	version, script := a.VotingRightsScript()
	ticket.AddTxOut(&wire.TxOut{Value: 100000, Version: version, PkScript: script})
	version, script = a.RewardCommitmentScript(100000, 0, 0)
	ticket.AddTxOut(&wire.TxOut{Version: version, PkScript: script})
	version, script = a.StakeChangeScript()
	ticket.AddTxOut(&wire.TxOut{Version: version, PkScript: script})
	if err := w.AddTransaction(ctx, ticket, nil); err != nil {
		t.Fatal(err)
	}
	ticketHash := ticket.TxHash()
	header, err := w.ChainParams().GenesisBlock.Header.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	revocation, err := stake.CreateRevocationFromTicket(&ticketHash, stake.ConvertToMinimalOutputs(ticket), 0, stake.TxVersionAutoRevocations, w.ChainParams(), header, true)
	if err != nil {
		t.Fatal(err)
	}
	lp, rp := validationPeer(t, wire.SFNodeNetwork, nil, revocation)
	s := NewSyncer(w, lp)
	out := wire.OutPoint{Hash: ticketHash, Tree: wire.TxTreeStake}
	s.rescanFilter.AddUnspentOutPoint(&out)
	count := 0
	s.notifications = &Notifications{MempoolTxs: func(txs []*wire.MsgTx) { count += len(txs) }}
	hash := revocation.TxHash()
	s.handleTxInvs(ctx, rp, []*chainhash.Hash{&hash})
	if count != 0 {
		t.Fatal("deferred revocation reported as accepted")
	}
	if _, _, err := w.GetTransactionsByHashes(ctx, []*chainhash.Hash{&hash}); !errors.Is(err, errors.NotExist) {
		t.Fatalf("deferred revocation recorded: %v", err)
	}
	if err := rp.SendHeaders(ctx); err != nil {
		t.Fatalf("honest automatic-revocation peer disconnected: %v", err)
	}
}
