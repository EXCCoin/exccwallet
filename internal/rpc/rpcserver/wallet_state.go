package rpcserver

import (
	"context"

	pb "github.com/EXCCoin/exccwallet/v2/rpc/walletrpc"
)

func (s *walletServer) WalletState(context.Context, *pb.WalletStateRequest) (*pb.WalletStateResponse, error) {
	return &pb.WalletStateResponse{
		Locked: s.wallet.Locked(), WatchingOnly: s.wallet.WatchingOnly(),
	}, nil
}

func (s *walletServer) WalletStateNotifications(req *pb.WalletStateRequest, stream pb.WalletService_WalletStateNotificationsServer) error {
	changed, done := s.wallet.NtfnServer.WalletStateNotifications()
	defer done()
	for {
		state, err := s.WalletState(stream.Context(), req)
		if err != nil {
			return err
		}
		if err := stream.Send(state); err != nil {
			return translateError(err)
		}
		select {
		case <-changed:
		case <-stream.Context().Done():
			return translateError(stream.Context().Err())
		}
	}
}
