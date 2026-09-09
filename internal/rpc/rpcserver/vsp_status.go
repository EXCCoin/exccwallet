package rpcserver

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"sync"
	"time"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccwallet/v2/internal/loader"
	"github.com/EXCCoin/exccwallet/v2/internal/vsp"
	pb "github.com/EXCCoin/exccwallet/v2/rpc/walletrpc"
	"github.com/EXCCoin/exccwallet/v2/wallet/udb"
	"golang.org/x/sync/errgroup"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type vspSnapshot struct {
	records  map[chainhash.Hash]*udb.VSPTicket
	present  map[chainhash.Hash]bool
	hashes   []chainhash.Hash
	jobs     map[chainhash.Hash]*pb.GetTrackedVSPTicketsResponse_Ticket
	counts   [4]uint32
	revision []byte
	vsps     []*pb.GetTrackedVSPTicketsResponse_VSP
}

func (s *walletServer) vspFeeSnapshot(ctx context.Context) (*vspSnapshot, error) {
	records, present, err := s.wallet.VSPFeeRecords(ctx)
	if err != nil {
		return nil, err
	}
	snapshot := &vspSnapshot{records: records, present: present, jobs: make(map[chainhash.Hash]*pb.GetTrackedVSPTicketsResponse_Ticket)}
	for hash, record := range records {
		if host, err := vsp.CanonicalVSPHost(record.Host); err == nil {
			record.Host = host
		}
		snapshot.hashes = append(snapshot.hashes, hash)
		snapshot.counts[record.FeeTxStatus]++
	}
	sort.Slice(snapshot.hashes, func(i, j int) bool { return bytes.Compare(snapshot.hashes[i][:], snapshot.hashes[j][:]) < 0 })

	tip, _ := s.wallet.MainChainTip(ctx)
	snapshot.setRevision(tip)
	return snapshot, nil
}

func (s *walletServer) vspSnapshot(ctx context.Context) (*vspSnapshot, error) {
	snapshot, err := s.vspFeeSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	for host, client := range loader.AllVSPs(s.wallet) {
		jobs := marshalVSPTrackedTickets(client.TrackedTickets())
		snapshot.vsps = append(snapshot.vsps, &pb.GetTrackedVSPTicketsResponse_VSP{Host: host, Tickets: jobs})
		for _, job := range jobs {
			var hash chainhash.Hash
			copy(hash[:], job.TicketHash)
			if _, exists := snapshot.jobs[hash]; exists {
				return nil, status.Error(codes.FailedPrecondition, "a ticket has multiple VSP jobs")
			}
			snapshot.jobs[hash] = job
		}
	}
	return snapshot, nil
}

// Only durable records and the chain tip must stay unchanged while precise
// ticket lookups run. Job diagnostics are captured once and frozen with the result.
func (snapshot *vspSnapshot) setRevision(tip chainhash.Hash) {
	digest := sha256.New()
	writeBytes := func(value []byte) {
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(value)))
		digest.Write(size[:])
		digest.Write(value)
	}
	writeBytes(tip[:])
	for _, hash := range snapshot.hashes {
		record := snapshot.records[hash]
		writeBytes(hash[:])
		writeBytes(record.FeeHash[:])
		writeBytes([]byte{byte(record.FeeTxStatus)})
		writeBytes([]byte(record.Host))
		writeBytes(record.PubKey)
		if exists, checked := snapshot.present[hash]; checked {
			value := byte(0)
			if exists {
				value = 1
			}
			writeBytes([]byte{value})
		}
	}
	snapshot.revision = digest.Sum(nil)
}

// The revision is an opaque handle to a completed, immutable status read.
// A fresh first page always reads current state; continuation pages never mix it
// with later updates. At most four reads and 64 MiB of encoded results are retained.
const (
	vspStatusSnapshotTTL      = time.Minute
	maxVSPStatusSnapshots     = 4
	maxVSPStatusSnapshotBytes = 64 << 20
)

type cachedVSPStatus struct {
	response *pb.GetVSPStatusResponse
	expires  time.Time
	size     int
}

type vspStatusCache struct {
	mu        sync.Mutex
	snapshots []cachedVSPStatus
}

func (c *vspStatusCache) add(response *pb.GetVSPStatusResponse) error {
	size := proto.Size(response)
	if size > maxVSPStatusSnapshotBytes {
		return status.Error(codes.ResourceExhausted, "VSP status snapshot is too large")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	now := time.Now()
	kept := c.snapshots[:0]
	total := size
	for _, entry := range c.snapshots {
		if now.Before(entry.expires) {
			kept = append(kept, entry)
			total += entry.size
		}
	}
	clear(c.snapshots[len(kept):])
	c.snapshots = kept
	for len(c.snapshots) > 0 && (len(c.snapshots) >= maxVSPStatusSnapshots || total > maxVSPStatusSnapshotBytes) {
		total -= c.snapshots[0].size
		c.snapshots[0] = cachedVSPStatus{}
		c.snapshots = c.snapshots[1:]
	}
	c.snapshots = append(c.snapshots, cachedVSPStatus{response: response, expires: now.Add(vspStatusSnapshotTTL), size: size})
	return nil
}

func (c *vspStatusCache) get(revision []byte) (*pb.GetVSPStatusResponse, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, entry := range c.snapshots {
		if time.Now().Before(entry.expires) && bytes.Equal(entry.response.Revision, revision) {
			return entry.response, nil
		}
	}
	return nil, status.Error(codes.Aborted, "VSP status snapshot expired; restart from the first page")
}

func vspStatusPage(snapshot *pb.GetVSPStatusResponse, after []byte, size uint32) *pb.GetVSPStatusResponse {
	rows := snapshot.Registrations
	start := sort.Search(len(rows), func(i int) bool { return bytes.Compare(rows[i].TicketHash, after) > 0 })
	end := min(start+int(size), len(rows))
	page := &pb.GetVSPStatusResponse{
		Started: snapshot.Started, Paid: snapshot.Paid, Errored: snapshot.Errored, Confirmed: snapshot.Confirmed,
		Revision: snapshot.Revision, TrackingKnown: snapshot.TrackingKnown, Registrations: rows[start:end],
	}
	if len(after) == 0 {
		page.Vsps = snapshot.Vsps
	}
	if end < len(rows) {
		page.NextAfterTicketHash = rows[end-1].TicketHash
	}
	// Keep callers from mutating cached messages, including concurrent RPC encoders.
	return proto.Clone(page).(*pb.GetVSPStatusResponse)
}

func (s *walletServer) GetVSPStatus(ctx context.Context, req *pb.GetVSPStatusRequest) (*pb.GetVSPStatusResponse, error) {
	if err := ctx.Err(); err != nil {
		return nil, translateError(err)
	}
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = 200
	}
	if pageSize > 500 || (len(req.AfterTicketHash) != 0 && len(req.AfterTicketHash) != chainhash.HashSize) ||
		(len(req.Revision) != 0 && len(req.Revision) != sha256.Size) || (len(req.AfterTicketHash) != 0 && len(req.Revision) == 0) {
		return nil, status.Error(codes.InvalidArgument, "invalid VSP status page")
	}
	var snapshot *pb.GetVSPStatusResponse
	var err error
	if len(req.Revision) != 0 {
		snapshot, err = s.vspStatus.get(req.Revision)
	} else {
		snapshot, err = s.buildVSPStatus(ctx)
		if err == nil {
			err = s.vspStatus.add(snapshot)
		}
	}
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, translateError(err)
	}
	return vspStatusPage(snapshot, req.AfterTicketHash, pageSize), nil
}

func (s *walletServer) buildVSPStatus(ctx context.Context) (*pb.GetVSPStatusResponse, error) {
	before, err := s.vspSnapshot(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	response := &pb.GetVSPStatusResponse{
		Started: before.counts[0], Paid: before.counts[1], Errored: before.counts[2], Confirmed: before.counts[3],
		TrackingKnown: true, Vsps: before.vsps,
		Registrations: make([]*pb.VSPFeeRecord, len(before.hashes)),
	}
	group, lookupCtx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for i, hash := range before.hashes {
		group.Go(func() error {
			if err := lookupCtx.Err(); err != nil {
				return err
			}
			record := before.records[hash]
			entry := &pb.VSPFeeRecord{
				TicketHash: hash[:], FeeStatus: pb.GetVSPTicketsByFeeStatusRequest_FeeStatus(record.FeeTxStatus),
				FeeHash: record.FeeHash[:], Host: record.Host, Job: before.jobs[hash],
			}
			response.Registrations[i] = entry
			if record.FeeTxStatus == uint32(udb.VSPFeeProcessConfirmed) {
				return nil
			}
			if !before.present[hash] {
				entry.TicketMissing = true
				return nil
			}
			ticket, err := s.GetTicket(lookupCtx, &pb.GetTicketRequest{TicketHash: hash[:]})
			if err == nil {
				entry.TicketStatus = ticket.Ticket.TicketStatus
				entry.TicketStatusKnown = entry.TicketStatus != pb.GetTicketsResponse_TicketDetails_UNKNOWN
				return nil
			}
			return nil
		})
	}
	if err := group.Wait(); err != nil {
		return nil, translateError(err)
	}
	after, err := s.vspFeeSnapshot(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	if !bytes.Equal(before.revision, after.revision) {
		return nil, status.Error(codes.Aborted, "VSP status changed during the read")
	}
	response.Revision = make([]byte, sha256.Size)
	rand.Read(response.Revision)
	return response, nil
}
