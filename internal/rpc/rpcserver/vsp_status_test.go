package rpcserver

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
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
		// Change a later record after the first page. Continuation must retain
		// the original values and counts, even though a fresh read sees it.
		err = walletdb.Update(ctx, rawDB, func(dbtx walletdb.ReadWriteTx) error {
			hash := chainhash.Hash{3}
			return udb.SetVSPTicket(dbtx, &hash, &udb.VSPTicket{FeeTxStatus: uint32(udb.VSPFeeProcessPaid), Host: savedHost, PubKey: key})
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	err = walletdb.Update(ctx, rawDB, func(dbtx walletdb.ReadWriteTx) error {
		hash := chainhash.Hash{3}
		return udb.SetVSPTicket(dbtx, &hash, &udb.VSPTicket{FeeTxStatus: uint32(udb.VSPFeeProcessErrored), Host: savedHost, PubKey: key})
	})
	if err != nil {
		t.Fatal(err)
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
	cached, err := s.GetVSPStatus(ctx, &pb.GetVSPStatusRequest{Revision: revision})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range cached.Registrations {
		if bytes.Equal(entry.TicketHash, rec.Hash[:]) && entry.TicketMissing {
			t.Fatal("cached read changed after removal")
		}
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
	restoredPage, err := s.GetVSPStatus(ctx, &pb.GetVSPStatusRequest{})
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range restoredPage.Registrations {
		if bytes.Equal(entry.TicketHash, rec.Hash[:]) && (entry.TicketMissing || !entry.TicketStatusKnown) {
			t.Fatal("fresh read missed same-tip restoration")
		}
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
	confirmedPage, err := s.GetVSPStatus(ctx, &pb.GetVSPStatusRequest{})
	if err != nil || confirmedPage.Confirmed != 2 {
		t.Fatalf("fresh read missed fee confirmation: %v, %v", confirmedPage, err)
	}
	if _, err := s.GetVSPStatus(ctx, &pb.GetVSPStatusRequest{Revision: bytes.Repeat([]byte{99}, 32)}); status.Code(err) != codes.Aborted {
		t.Fatalf("accepted unknown snapshot: %v", err)
	}
	for _, req := range []*pb.GetVSPStatusRequest{{PageSize: 501}, {AfterTicketHash: []byte{1}}, {Revision: []byte{1}}, {AfterTicketHash: make([]byte, 32)}} {
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

func TestVSPStatusSnapshotFreezesChangingJobsAcrossPages(t *testing.T) {
	const count = 1200
	hash := func(i int) []byte {
		h := make([]byte, 32)
		binary.BigEndian.PutUint32(h, uint32(i+1))
		return h
	}
	live := &pb.GetVSPStatusResponse{Started: count, TrackingKnown: true, Revision: hash(9000)}
	provider := &pb.GetTrackedVSPTicketsResponse_VSP{Host: "https://vsp.example"}
	live.Vsps = []*pb.GetTrackedVSPTicketsResponse_VSP{provider}
	for i := range count {
		job := &pb.GetTrackedVSPTicketsResponse_Ticket{TicketHash: hash(i), Phase: "confirm payment", NextRetryUnix: 1}
		provider.Tickets = append(provider.Tickets, job)
		live.Registrations = append(live.Registrations, &pb.VSPFeeRecord{TicketHash: hash(i), Host: provider.Host, Job: job})
	}
	snapshot := proto.Clone(live).(*pb.GetVSPStatusResponse)
	s := new(walletServer)
	if err := s.vspStatus.add(snapshot); err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	done := make(chan struct{})
	var updates atomic.Int32
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-done:
				return
			default:
			}
			mu.Lock()
			for _, job := range provider.Tickets {
				job.Phase = "retry payment"
				job.NextRetryUnix++
				job.LastError = "context deadline exceeded"
			}
			mu.Unlock()
			updates.Add(1)
			time.Sleep(time.Millisecond)
		}
	}()
	defer func() { close(done); wg.Wait() }()
	var cursor []byte
	var seen int
	for {
		page, err := s.GetVSPStatus(context.Background(), &pb.GetVSPStatusRequest{PageSize: 137, Revision: snapshot.Revision, AfterTicketHash: cursor})
		if err != nil {
			t.Fatal(err)
		}
		if page.Started != count || !bytes.Equal(page.Revision, snapshot.Revision) {
			t.Fatal("snapshot totals changed")
		}
		if seen == 0 && len(page.Vsps[0].Tickets) != count {
			t.Fatal("first page omitted jobs")
		}
		if seen != 0 && len(page.Vsps) != 0 {
			t.Fatal("later page repeated jobs")
		}
		for _, row := range page.Registrations {
			if !bytes.Equal(row.TicketHash, hash(seen)) || row.Job.NextRetryUnix != 1 || row.Job.LastError != "" {
				t.Fatal("mixed, omitted, or duplicated records")
			}
			seen++
			row.Job.LastError = "caller mutated its own page"
		}
		cursor = page.NextAfterTicketHash
		if len(cursor) == 0 {
			break
		}
		time.Sleep(2 * time.Millisecond)
	}
	if seen != count || updates.Load() == 0 {
		t.Fatalf("records %d, updates %d", seen, updates.Load())
	}
	mu.Lock()
	fresh := proto.Clone(live).(*pb.GetVSPStatusResponse)
	mu.Unlock()
	fresh.Revision = hash(9001)
	if err := s.vspStatus.add(fresh); err != nil {
		t.Fatal(err)
	}
	page, err := s.GetVSPStatus(context.Background(), &pb.GetVSPStatusRequest{Revision: fresh.Revision})
	if err != nil || page.Registrations[0].Job.LastError != "context deadline exceeded" {
		t.Fatalf("fresh snapshot did not expose updates: %v", err)
	}
	old, err := s.GetVSPStatus(context.Background(), &pb.GetVSPStatusRequest{Revision: snapshot.Revision})
	if err != nil || old.Registrations[0].Job.LastError != "" {
		t.Fatal("refresh or caller mutation changed an active read")
	}
}

func TestVSPStatusCacheExpiryAndBounds(t *testing.T) {
	s := new(walletServer)
	var first []byte
	for i := range maxVSPStatusSnapshots + 1 {
		revision := bytes.Repeat([]byte{byte(i + 1)}, 32)
		if i == 0 {
			first = revision
		}
		if err := s.vspStatus.add(&pb.GetVSPStatusResponse{Revision: revision}); err != nil {
			t.Fatal(err)
		}
	}
	if len(s.vspStatus.snapshots) != maxVSPStatusSnapshots {
		t.Fatal("cache is unbounded")
	}
	if _, err := s.vspStatus.get(first); status.Code(err) != codes.Aborted {
		t.Fatal("evicted snapshot remained usable")
	}
	entry := &s.vspStatus.snapshots[0]
	entry.expires = time.Now().Add(-time.Second)
	if _, err := s.vspStatus.get(entry.response.Revision); status.Code(err) != codes.Aborted {
		t.Fatal("expired snapshot remained usable")
	}
	// Exercise the byte budget without allocating a large fixture.
	for i := range s.vspStatus.snapshots {
		s.vspStatus.snapshots[i].size = maxVSPStatusSnapshotBytes / 2
	}
	if err := s.vspStatus.add(&pb.GetVSPStatusResponse{Revision: bytes.Repeat([]byte{9}, 32)}); err != nil {
		t.Fatal(err)
	}
	total := 0
	for _, entry := range s.vspStatus.snapshots {
		total += entry.size
	}
	if total > maxVSPStatusSnapshotBytes {
		t.Fatal("cache exceeded its byte budget")
	}
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.GetVSPStatus(cancelled, &pb.GetVSPStatusRequest{Revision: s.vspStatus.snapshots[0].response.Revision}); status.Code(err) != codes.Canceled {
		t.Fatal("cached read ignored cancellation")
	}
}

func TestVSPStatusDurableRevisionIgnoresJobDiagnostics(t *testing.T) {
	hash := chainhash.Hash{1}
	snapshot := &vspSnapshot{hashes: []chainhash.Hash{hash}, records: map[chainhash.Hash]*udb.VSPTicket{hash: {Host: "https://vsp.example"}}, present: map[chainhash.Hash]bool{hash: true}, jobs: map[chainhash.Hash]*pb.GetTrackedVSPTicketsResponse_Ticket{hash: {TicketHash: hash[:]}}}
	snapshot.setRevision(chainhash.Hash{})
	before := bytes.Clone(snapshot.revision)
	snapshot.jobs[hash].NextRetryUnix = 42
	snapshot.jobs[hash].LastError = "temporary failure"
	snapshot.setRevision(chainhash.Hash{})
	if !bytes.Equal(before, snapshot.revision) {
		t.Fatal("job diagnostics invalidated the durable read")
	}
	snapshot.present[hash] = false
	snapshot.setRevision(chainhash.Hash{})
	if bytes.Equal(before, snapshot.revision) {
		t.Fatal("transaction presence change was ignored")
	}
	snapshot.present[hash] = true
	snapshot.records[hash].FeeTxStatus = uint32(udb.VSPFeeProcessConfirmed)
	snapshot.setRevision(chainhash.Hash{})
	if bytes.Equal(before, snapshot.revision) {
		t.Fatal("fee status change was ignored")
	}
	snapshot.records[hash].FeeTxStatus = 0
	snapshot.setRevision(chainhash.Hash{2})
	if bytes.Equal(before, snapshot.revision) {
		t.Fatal("chain tip change was ignored")
	}
}
