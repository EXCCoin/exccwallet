// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package rpcserver

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"net"

	"github.com/gorilla/websocket"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func validateSyncCertificate(pem []byte) error {
	if len(pem) != 0 && !x509.NewCertPool().AppendCertsFromPEM(pem) {
		return status.Error(codes.InvalidArgument, "RPC certificate must contain a valid PEM certificate")
	}
	return nil
}

func transportErrorCode(err error) codes.Code {
	var verification *tls.CertificateVerificationError
	var authority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var certificate x509.CertificateInvalidError
	var roots *x509.SystemRootsError
	var tlsRecord tls.RecordHeaderError
	if errors.As(err, &verification) || errors.As(err, &authority) ||
		errors.As(err, &hostname) || errors.As(err, &certificate) ||
		errors.As(err, &roots) || errors.As(err, &tlsRecord) ||
		errors.Is(err, websocket.ErrBadHandshake) {
		return codes.FailedPrecondition
	}
	var address *net.AddrError
	if errors.As(err, &address) {
		return codes.InvalidArgument
	}
	var network net.Error
	if errors.As(err, &network) || errors.Is(err, net.ErrClosed) {
		return codes.Unavailable
	}
	return codes.Unknown
}

func syncError(ctx context.Context, err error) error {
	if err == nil {
		return nil
	}
	if ctx.Err() != nil {
		return translateError(ctx.Err())
	}
	return translateError(fmt.Errorf("wallet synchronization stopped: %w", err))
}
