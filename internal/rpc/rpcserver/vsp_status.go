package rpcserver

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sort"

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

func (s *walletServer) vspSnapshot(ctx context.Context) (*vspSnapshot, error) {
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
	jobHosts := make(map[chainhash.Hash]string)
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
			jobHosts[hash] = host
		}
	}
	tip, _ := s.wallet.MainChainTip(ctx)
	if err := snapshot.setRevision(tip, jobHosts); err != nil {
		return nil, err
	}
	return snapshot, nil
}

func (snapshot *vspSnapshot) setRevision(tip chainhash.Hash, jobHosts map[chainhash.Hash]string) error {
	jobHashes := make([]chainhash.Hash, 0, len(jobHosts))
	for hash := range jobHosts {
		jobHashes = append(jobHashes, hash)
	}
	sort.Slice(jobHashes, func(i, j int) bool { return bytes.Compare(jobHashes[i][:], jobHashes[j][:]) < 0 })
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
	for _, hash := range jobHashes {
		writeBytes(hash[:])
		writeBytes([]byte(jobHosts[hash]))
		payload, err := (proto.MarshalOptions{Deterministic: true}).Marshal(snapshot.jobs[hash])
		if err != nil {
			return err
		}
		writeBytes(payload)
	}
	snapshot.revision = digest.Sum(nil)
	return nil
}

func (s *walletServer) GetVSPStatus(ctx context.Context, req *pb.GetVSPStatusRequest) (*pb.GetVSPStatusResponse, error) {
	pageSize := req.PageSize
	if pageSize == 0 {
		pageSize = 200
	}
	if pageSize > 500 || (len(req.AfterTicketHash) != 0 && len(req.AfterTicketHash) != chainhash.HashSize) || (len(req.Revision) != 0 && len(req.Revision) != sha256.Size) {
		return nil, status.Error(codes.InvalidArgument, "invalid VSP status page")
	}
	before, err := s.vspSnapshot(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	if len(req.Revision) != 0 && !bytes.Equal(req.Revision, before.revision) {
		return nil, status.Error(codes.Aborted, "VSP status changed between pages")
	}
	start := sort.Search(len(before.hashes), func(i int) bool { return bytes.Compare(before.hashes[i][:], req.AfterTicketHash) > 0 })
	end := min(start+int(pageSize), len(before.hashes))
	response := &pb.GetVSPStatusResponse{
		Started: before.counts[0], Paid: before.counts[1], Errored: before.counts[2], Confirmed: before.counts[3],
		Revision: before.revision, TrackingKnown: true,
		Registrations: make([]*pb.VSPFeeRecord, end-start),
	}
	if len(req.AfterTicketHash) == 0 {
		response.Vsps = before.vsps
	}
	if end < len(before.hashes) {
		response.NextAfterTicketHash = append([]byte(nil), before.hashes[end-1][:]...)
	}
	group, lookupCtx := errgroup.WithContext(ctx)
	group.SetLimit(8)
	for i, hash := range before.hashes[start:end] {
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
	after, err := s.vspSnapshot(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	if !bytes.Equal(before.revision, after.revision) {
		return nil, status.Error(codes.Aborted, "VSP status changed during the read")
	}
	return response, nil
}
