package rpcserver

import (
	"github.com/EXCCoin/exccwallet/v2/internal/vsp"
	pb "github.com/EXCCoin/exccwallet/v2/rpc/walletrpc"
)

func marshalVSPProcessResults(results []vsp.ManagedTicketResult) []*pb.VSPProcessResult {
	response := make([]*pb.VSPProcessResult, len(results))
	for i, result := range results {
		entry := &pb.VSPProcessResult{TicketHash: result.TicketHash[:]}
		switch result.Status {
		case vsp.ManagedTicketStarted:
			entry.Status = pb.VSPProcessResult_STARTED
		case vsp.ManagedTicketAlreadyTracked:
			entry.Status = pb.VSPProcessResult_ALREADY_TRACKED
		case vsp.ManagedTicketConfirmed:
			entry.Status = pb.VSPProcessResult_CONFIRMED
		case vsp.ManagedTicketUnknownToVSP:
			entry.Status = pb.VSPProcessResult_UNKNOWN_TO_PROVIDER
		case vsp.ManagedTicketNotManaged:
			entry.Status = pb.VSPProcessResult_NOT_MANAGED
		case vsp.ManagedTicketDifferentVSP:
			entry.Status = pb.VSPProcessResult_DIFFERENT_PROVIDER
		case vsp.ManagedTicketFailed:
			entry.Status = pb.VSPProcessResult_FAILED
		}
		if result.Err != nil {
			entry.Error = result.Err.Error()
		}
		response[i] = entry
	}
	return response
}
