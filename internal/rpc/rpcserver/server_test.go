// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package rpcserver

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	pb "github.com/EXCCoin/exccwallet/v2/rpc/walletrpc"
)

func TestPurchaseTicketsClearsPassphraseOnValidationError(t *testing.T) {
	req := &pb.PurchaseTicketsRequest{
		Passphrase: []byte("private"),
		SpendLimit: -1,
	}
	_, err := new(walletServer).PurchaseTickets(context.Background(), req)
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, b := range req.Passphrase {
		if b != 0 {
			t.Fatal("passphrase was not cleared")
		}
	}
}
