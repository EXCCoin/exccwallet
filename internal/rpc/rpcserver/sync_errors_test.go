// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package rpcserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	walletErrors "github.com/EXCCoin/exccwallet/v2/errors"
	pb "github.com/EXCCoin/exccwallet/v2/rpc/walletrpc"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	"github.com/gorilla/websocket"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
	"google.golang.org/grpc/test/bufconn"
)

func TestTranslateSyncErrors(t *testing.T) {
	cert := &x509.Certificate{}
	for _, test := range []struct {
		name string
		err  error
		code codes.Code
	}{
		{"nil", nil, codes.OK},
		{"canceled", context.Canceled, codes.Canceled},
		{"deadline", context.DeadlineExceeded, codes.DeadlineExceeded},
		{"nested canceled", walletErrors.E(walletErrors.Invalid, fmt.Errorf("connection: %w", context.Canceled)), codes.Canceled},
		{"nested deadline", walletErrors.E(walletErrors.IO, fmt.Errorf("connection: %w", context.DeadlineExceeded)), codes.DeadlineExceeded},
		{"TLS verification", &tls.CertificateVerificationError{Err: x509.UnknownAuthorityError{Cert: cert}}, codes.FailedPrecondition},
		{"unknown certificate", x509.UnknownAuthorityError{Cert: cert}, codes.FailedPrecondition},
		{"certificate hostname", x509.HostnameError{Certificate: cert, Host: "node.invalid"}, codes.FailedPrecondition},
		{"expired certificate", x509.CertificateInvalidError{Cert: cert, Reason: x509.Expired}, codes.FailedPrecondition},
		{"TLS protocol", tls.RecordHeaderError{Msg: "not TLS"}, codes.FailedPrecondition},
		{"websocket handshake", websocket.ErrBadHandshake, codes.FailedPrecondition},
		{"invalid address", &net.AddrError{Err: "invalid port", Addr: "node.invalid:bad"}, codes.InvalidArgument},
		{"DNS", &net.DNSError{Err: "lookup failed", Name: "node.invalid"}, codes.Unavailable},
		{"connection", &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("refused")}, codes.Unavailable},
		{"closed connection", net.ErrClosed, codes.Unavailable},
		{"wallet passphrase", walletErrors.E(walletErrors.Passphrase, "invalid passphrase"), codes.InvalidArgument},
		{"unknown", errors.New("unexpected failure"), codes.Unknown},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := test.err
			if err != nil {
				err = fmt.Errorf("sync: %w", err)
			}
			if got := status.Code(translateError(err)); got != test.code {
				t.Fatalf("got %v, want %v", got, test.code)
			}
		})
	}
	s, err := status.New(codes.Unauthenticated, "authentication rejected").WithDetails(&errdetails.ErrorInfo{Reason: "RPC_AUTH"})
	if err != nil {
		t.Fatal(err)
	}
	got := status.Convert(translateError(fmt.Errorf("sync: %w", s.Err())))
	if got.Code() != codes.Unauthenticated || len(got.Details()) != 1 || got.Details()[0].(*errdetails.ErrorInfo).Reason != "RPC_AUTH" {
		t.Fatalf("wrapped gRPC status/details lost: %v", got)
	}
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	if code := status.Code(syncError(ctx, errors.New("connection stopped"))); code != codes.DeadlineExceeded {
		t.Fatalf("sync context deadline changed to %v", code)
	}
}

func TestValidateSyncCertificate(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	defer server.Close()
	valid := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: server.Certificate().Raw})
	for _, certificate := range [][]byte{nil, valid} {
		if err := validateSyncCertificate(certificate); err != nil {
			t.Fatal(err)
		}
	}
	if code := status.Code(validateSyncCertificate([]byte("not a certificate"))); code != codes.InvalidArgument {
		t.Fatalf("invalid certificate code: %v", code)
	}
}

func TestQuietNotificationStreamsPublishReadinessHeaders(t *testing.T) {
	listener := bufconn.Listen(1 << 20)
	server := grpc.NewServer()
	w := &wallet.Wallet{NtfnServer: &wallet.NotificationServer{}}
	pb.RegisterWalletServiceServer(server, &walletServer{wallet: w})
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	t.Cleanup(func() { listener.Close() })
	conn, err := grpc.NewClient("passthrough:///wallet-test", grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	client := pb.NewWalletServiceClient(conn)
	for _, account := range []bool{false, true} {
		name := "transactions"
		if account {
			name = "accounts"
		}
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			var stream grpc.ClientStream
			var err error
			if account {
				stream, err = client.AccountNotifications(ctx, &pb.AccountNotificationsRequest{})
			} else {
				stream, err = client.TransactionNotifications(ctx, &pb.TransactionNotificationsRequest{})
			}
			if err != nil {
				t.Fatal(err)
			}
			headers, err := stream.Header()
			if err != nil {
				t.Fatalf("quiet stream never acknowledged subscription: %v", err)
			}
			ready := headers.Get("exccwallet-subscription-ready")
			if len(ready) != 1 || ready[0] != "true" {
				t.Fatalf("missing readiness marker: %v", headers)
			}
			cancel()
			var response interface{} = &pb.TransactionNotificationsResponse{}
			if account {
				response = &pb.AccountNotificationsResponse{}
			}
			if code := status.Code(stream.RecvMsg(response)); code != codes.Canceled {
				t.Fatalf("quiet stream emitted a fabricated notification or lost cancellation: %v", code)
			}
		})
	}
}
