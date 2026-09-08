package rpcserver

import (
	"bytes"
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccd/gcs/v3"
	"github.com/EXCCoin/exccd/gcs/v3/blockcf2"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/rpc/client/dcrd"
	pb "github.com/EXCCoin/exccwallet/v2/rpc/walletrpc"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

func TestVSPStatusPagesUseConsistentFeeRecords(t *testing.T) {
	ctx := context.Background()
	db, err := wallet.CreateDB("bdb", t.TempDir()+"/wallet.db")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	pubPass := []byte(wallet.InsecurePubPassphrase)
	params := chaincfg.SimNetParams()
	if err := wallet.Create(ctx, db, pubPass, []byte("test-private"), bytes.Repeat([]byte{1}, 32), params); err != nil {
		t.Fatal(err)
	}
	w, err := wallet.Open(ctx, &wallet.Config{DB: db, PubPassphrase: pubPass, Params: params, GapLimit: 20})
	if err != nil {
		t.Fatal(err)
	}
	s := &walletServer{wallet: w}
	rawDB := db.(walletdb.DB)
	_, store, _, err := udb.Open(ctx, rawDB, params, pubPass)
	if err != nil {
		t.Fatal(err)
	}
	addr, err := stdaddr.NewAddressPubKeyHashEcdsaSecp256k1V0(make([]byte, 20), params)
	if err != nil {
		t.Fatal(err)
	}
	tx := wire.NewMsgTx()
	tx.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{77}}, 100, nil))
	version, script := addr.VotingRightsScript()
	tx.AddTxOut(&wire.TxOut{Value: 100, Version: version, PkScript: script})
	version, script = addr.RewardCommitmentScript(100, 0, 0)
	tx.AddTxOut(&wire.TxOut{Version: version, PkScript: script})
	version, script = addr.StakeChangeScript()
	tx.AddTxOut(&wire.TxOut{Version: version, PkScript: script})
	rec, err := udb.NewTxRecordFromMsgTx(tx, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	key := bytes.Repeat([]byte{2}, 32)
	const savedHost = "https://VSP.example:443/"
	err = walletdb.Update(ctx, rawDB, func(dbtx walletdb.ReadWriteTx) error {
		if err := store.InsertMemPoolTx(dbtx, rec); err != nil {
			return err
		}
		for i := 0; i < 4; i++ {
			hash := chainhash.Hash{byte(i + 1)}
			if i == 0 {
				hash = rec.Hash
			}
			if err := udb.SetVSPTicket(dbtx, &hash, &udb.VSPTicket{FeeTxStatus: uint32(i), Host: savedHost, PubKey: key}); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	var revision, cursor []byte
	seen := make(map[string]bool)
	for {
		page, err := s.GetVSPStatus(ctx, &pb.GetVSPStatusRequest{PageSize: 2, Revision: revision, AfterTicketHash: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if page.Started != 1 || page.Paid != 1 || page.Errored != 1 || page.Confirmed != 1 || !page.TrackingKnown {
			t.Fatalf("wrong totals: %v", page)
		}
		if len(revision) != 0 && !bytes.Equal(revision, page.Revision) {
			t.Fatal("revision changed")
		}
		revision = page.Revision
		for _, entry := range page.Registrations {
			if entry.Host != "https://vsp.example" {
				t.Fatalf("provider alias was not normalized: %q", entry.Host)
			}
			if seen[string(entry.TicketHash)] {
				t.Fatal("ticket repeated across pages")
			}
			seen[string(entry.TicketHash)] = true
			if bytes.Equal(entry.TicketHash, rec.Hash[:]) {
				if !entry.TicketStatusKnown || entry.TicketStatus != pb.GetTicketsResponse_TicketDetails_UNMINED || entry.TicketMissing {
					t.Fatalf("unmined ticket misclassified: %v", entry)
				}
			} else if entry.FeeStatus != pb.GetVSPTicketsByFeeStatusRequest_VSP_FEE_PROCESS_CONFIRMED && (!entry.TicketMissing || entry.TicketStatusKnown) {
				t.Fatalf("missing transaction misclassified: %v", entry)
			}
		}
		cursor = page.NextAfterTicketHash
		if len(cursor) == 0 {
			break
		}
	}
	if len(seen) != 4 {
		t.Fatalf("got %d records", len(seen))
	}
	info, err := w.VSPTicketInfo(ctx, &rec.Hash)
	if err != nil || info.Host != savedHost {
		t.Fatalf("reading status changed the saved provider: %+v, %v", info, err)
	}
	tip, _ := w.MainChainTip(ctx)
	if err := w.AbandonTransaction(ctx, &rec.Hash); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetVSPStatus(ctx, &pb.GetVSPStatusRequest{Revision: revision}); status.Code(err) != codes.Aborted {
		t.Fatalf("same-tip removal accepted the old revision: %v", err)
	}
	missingPage, err := s.GetVSPStatus(ctx, &pb.GetVSPStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range missingPage.Registrations {
		if bytes.Equal(entry.TicketHash, rec.Hash[:]) && (!entry.TicketMissing || entry.TicketStatusKnown) {
			t.Fatalf("removed ticket was not marked missing: %v", entry)
		}
	}
	err = walletdb.Update(ctx, rawDB, func(dbtx walletdb.ReadWriteTx) error {
		return store.InsertMemPoolTx(dbtx, rec)
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetVSPStatus(ctx, &pb.GetVSPStatusRequest{Revision: missingPage.Revision}); status.Code(err) != codes.Aborted {
		t.Fatalf("same-tip restoration accepted the old revision: %v", err)
	}
	if currentTip, _ := w.MainChainTip(ctx); currentTip != tip {
		t.Fatal("presence regression changed the chain tip")
	}
	t.Run("failed precise lookup remains unknown", func(t *testing.T) {
		filter, err := gcs.NewFilterV2(blockcf2.B, blockcf2.M, [gcs.KeySize]byte{}, nil)
		if err != nil {
			t.Fatal(err)
		}
		err = walletdb.Update(ctx, rawDB, func(dbtx walletdb.ReadWriteTx) error {
			previous := tip
			for height := uint32(1); height <= uint32(params.TicketMaturity)+2; height++ {
				header := &wire.BlockHeader{Height: height, PrevBlock: previous, VoteBits: 1}
				if err := store.ExtendMainChain(dbtx.ReadWriteBucket([]byte("wtxmgr")), header, filter); err != nil {
					return err
				}
				previous = header.BlockHash()
				if height == 1 {
					if err := store.InsertMinedTx(dbtx, rec, &previous); err != nil {
						return err
					}
				}
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
		caller := new(failingVSPStatusCaller)
		w.SetNetworkBackend(dcrd.New(caller))
		defer w.SetNetworkBackend(nil)
		page, err := s.GetVSPStatus(ctx, &pb.GetVSPStatusRequest{})
		if err != nil {
			t.Fatal(err)
		}
		if caller.calls.Load() == 0 {
			t.Fatal("test did not attempt a precise node lookup")
		}
		for _, entry := range page.Registrations {
			if bytes.Equal(entry.TicketHash, rec.Hash[:]) && (entry.TicketStatusKnown || entry.TicketMissing || entry.TicketStatus != pb.GetTicketsResponse_TicketDetails_UNKNOWN) {
				t.Fatalf("failed node lookup was presented as known: %v", entry)
			}
		}
	})
	err = walletdb.Update(ctx, rawDB, func(dbtx walletdb.ReadWriteTx) error {
		return udb.SetVSPTicket(dbtx, &rec.Hash, &udb.VSPTicket{FeeTxStatus: uint32(udb.VSPFeeProcessConfirmed), Host: "https://vsp.example", PubKey: key})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetVSPStatus(ctx, &pb.GetVSPStatusRequest{Revision: revision}); status.Code(err) != codes.Aborted {
		t.Fatalf("accepted stale revision: %v", err)
	}
	for _, req := range []*pb.GetVSPStatusRequest{{PageSize: 501}, {AfterTicketHash: []byte{1}}, {Revision: []byte{1}}} {
		if _, err := s.GetVSPStatus(ctx, req); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("accepted invalid page: %v", err)
		}
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := s.GetVSPStatus(canceled, &pb.GetVSPStatusRequest{}); status.Code(err) != codes.Canceled {
		t.Fatalf("lost cancellation: %v", err)
	}
	err = walletdb.Update(ctx, rawDB, func(dbtx walletdb.ReadWriteTx) error {
		return dbtx.ReadWriteBucket([]byte("vsp")).Put(rec.Hash[:], []byte{1})
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetVSPStatus(ctx, &pb.GetVSPStatusRequest{}); err == nil {
		t.Fatal("corrupt record accepted")
	}
}

type failingVSPStatusCaller struct{ calls atomic.Int32 }

func (c *failingVSPStatusCaller) Call(context.Context, string, interface{}, ...interface{}) error {
	c.calls.Add(1)
	return errors.New("test node unavailable")
}

func TestVSPStatusRevisionIncludesJobDiagnostics(t *testing.T) {
	hash := chainhash.Hash{1}
	hosts := map[chainhash.Hash]string{hash: "https://vsp.example"}
	base := &pb.GetTrackedVSPTicketsResponse_Ticket{TicketHash: hash[:]}
	revision := func(job *pb.GetTrackedVSPTicketsResponse_Ticket) []byte {
		snapshot := &vspSnapshot{jobs: map[chainhash.Hash]*pb.GetTrackedVSPTicketsResponse_Ticket{hash: job}}
		if err := snapshot.setRevision(chainhash.Hash{}, hosts); err != nil {
			t.Fatal(err)
		}
		return snapshot.revision
	}
	before := revision(base)
	for name, change := range map[string]func(*pb.GetTrackedVSPTicketsResponse_Ticket){
		"phase":      func(job *pb.GetTrackedVSPTicketsResponse_Ticket) { job.Phase = "confirm payment" },
		"failure":    func(job *pb.GetTrackedVSPTicketsResponse_Ticket) { job.LastError = "wallet is locked" },
		"unlock":     func(job *pb.GetTrackedVSPTicketsResponse_Ticket) { job.NeedsUnlock = true },
		"retry":      func(job *pb.GetTrackedVSPTicketsResponse_Ticket) { job.NextRetryUnix = 123 },
		"fee":        func(job *pb.GetTrackedVSPTicketsResponse_Ticket) { job.Fee = 456 },
		"fee hash":   func(job *pb.GetTrackedVSPTicketsResponse_Ticket) { job.FeeHash = []byte{2} },
		"state":      func(job *pb.GetTrackedVSPTicketsResponse_Ticket) { job.State = 2 },
		"voting":     func(job *pb.GetTrackedVSPTicketsResponse_Ticket) { job.VotingAddress = "test voting address" },
		"commitment": func(job *pb.GetTrackedVSPTicketsResponse_Ticket) { job.CommitmentAddress = "test commitment address" },
	} {
		t.Run(name, func(t *testing.T) {
			job := proto.Clone(base).(*pb.GetTrackedVSPTicketsResponse_Ticket)
			change(job)
			if bytes.Equal(before, revision(job)) {
				t.Fatal("changed job retained the old revision")
			}
			if !bytes.Equal(revision(job), revision(job)) {
				t.Fatal("unchanged job revision is unstable")
			}
		})
	}
}
