// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package vsp

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"encoding/base64"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
	"github.com/EXCCoin/exccwallet/v2/wallet"
)

type vspTestSigner struct{}

func (vspTestSigner) SignMessage(context.Context, string, stdaddr.Address) ([]byte, error) {
	return []byte("test request signature"), nil
}

type vspTestTransport func(*http.Request) (*http.Response, error)

func (f vspTestTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

func vspTestKeys() (ed25519.PublicKey, ed25519.PrivateKey) {
	key := ed25519.NewKeyFromSeed(make([]byte, ed25519.SeedSize))
	return key.Public().(ed25519.PublicKey), key
}

func vspTestClient(t *testing.T, server *httptest.Server, pub ed25519.PublicKey) *client {
	t.Helper()
	c := newClient(server.URL, pub, vspTestSigner{})
	c.Transport = server.Client().Transport
	t.Cleanup(c.CloseIdleConnections)
	return c
}

func writeVSPTestResponse(w http.ResponseWriter, key ed25519.PrivateKey, status int, body []byte) {
	w.Header().Set("VSP-Server-Signature", base64.StdEncoding.EncodeToString(ed25519.Sign(key, body)))
	w.WriteHeader(status)
	w.Write(body)
}

func TestVSPClientIdentityValidation(t *testing.T) {
	pub, _ := vspTestKeys()
	encoded := base64.StdEncoding.EncodeToString(pub)
	for _, host := range []string{
		"http://example.com", "https://", "https://user:password@example.com",
		"https://example.com?query=1", "https://example.com?", "https://example.com#fragment", "https://example.com#",
		"https://example.com:", "https://example.com:0", "https://example.com:65536",
	} {
		if _, err := New(Config{URL: host, PubKey: encoded, Wallet: &wallet.Wallet{}}); err == nil {
			t.Errorf("accepted invalid URL %q", host)
		}
	}
	for _, key := range []string{
		"", "not-base64", base64.StdEncoding.EncodeToString(pub[:31]),
		base64.StdEncoding.EncodeToString(append(pub, 0)), encoded + "\n",
		strings.TrimRight(encoded, "="),
	} {
		if _, err := New(Config{URL: "https://example.com", PubKey: key, Wallet: &wallet.Wallet{}}); err == nil {
			t.Errorf("accepted invalid public key %q", key)
		}
	}
	c, err := New(Config{URL: "HTTPS://EXAMPLE.COM/vsp/", PubKey: encoded, Wallet: &wallet.Wallet{}})
	if err != nil {
		t.Fatal(err)
	}
	if c.url != "https://example.com/vsp" || c.Timeout != vspRequestTimeout || c.CheckRedirect == nil {
		t.Fatalf("missing canonical identity or HTTP policy: %+v", c.client)
	}
}

func TestVSPClientRequestDeadline(t *testing.T) {
	pub, _ := vspTestKeys()
	for _, limit := range []time.Duration{vspRequestTimeout, time.Second} {
		ctx := context.Background()
		if limit < vspRequestTimeout {
			var cancel context.CancelFunc
			ctx, cancel = context.WithTimeout(ctx, limit)
			defer cancel()
		}
		c := newClient("https://example.com", pub, vspTestSigner{})
		c.Transport = vspTestTransport(func(req *http.Request) (*http.Response, error) {
			deadline, ok := req.Context().Deadline()
			if !ok || time.Until(deadline) > limit || time.Until(deadline) <= 0 {
				t.Errorf("request deadline missing or exceeds %v: %v", limit, deadline)
			}
			return nil, context.Canceled
		})
		if err := c.get(ctx, "/api/v3/vspinfo", nil); !errors.Is(err, context.Canceled) {
			t.Fatalf("request cancellation lost: %v", err)
		}
	}
}

func TestVSPClientRejectsUnauthenticatedErrors(t *testing.T) {
	pub, _ := vspTestKeys()
	for _, signature := range []string{"", base64.StdEncoding.EncodeToString(make([]byte, ed25519.SignatureSize))} {
		server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("VSP-Server-Signature", signature)
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"code":2,"message":"untrusted error"}`)
		}))
		c := vspTestClient(t, server, pub)
		err := c.get(context.Background(), "/api/v3/vspinfo", nil)
		server.Close()
		var apiError *BadRequestError
		if err == nil || errors.As(err, &apiError) {
			t.Fatalf("unauthenticated response became an API error: %v", err)
		}
	}
}

func TestVSPClientSignedResponsesAndBounds(t *testing.T) {
	pub, key := vspTestKeys()
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			body := []byte(`{"code":2,"message":"test failure"}`)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeVSPTestResponse(w, key, status, body)
			}))
			defer server.Close()
			c := vspTestClient(t, server, pub)
			err := c.get(context.Background(), "/api/v3/vspinfo", nil)
			var apiError *BadRequestError
			if status == http.StatusOK && err != nil {
				t.Fatal(err)
			}
			if status == http.StatusBadRequest && (!errors.As(err, &apiError) || apiError.Code != 2) {
				t.Fatalf("lost authenticated API error: %v", err)
			}
		})
	}
	for _, status := range []int{http.StatusOK, http.StatusBadRequest} {
		t.Run("oversized "+http.StatusText(status), func(t *testing.T) {
			body := bytes.Repeat([]byte{'x'}, maxVSPResponseBytes+1)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				writeVSPTestResponse(w, key, status, body)
			}))
			defer server.Close()
			err := vspTestClient(t, server, pub).get(context.Background(), "/api/v3/vspinfo", nil)
			if err == nil || !strings.Contains(err.Error(), "response exceeds") {
				t.Fatalf("oversized response accepted: %v", err)
			}
		})
	}
}

func TestVSPClientTimeoutAndCancellation(t *testing.T) {
	pub, key := vspTestKeys()
	for _, stalledBody := range []bool{false, true} {
		name := "headers"
		if stalledBody {
			name = "body"
		}
		t.Run(name, func(t *testing.T) {
			cancelled := make(chan struct{})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if stalledBody {
					writeVSPTestResponse(w, key, http.StatusOK, []byte("{"))
					w.(http.Flusher).Flush()
				}
				<-r.Context().Done()
				close(cancelled)
			}))
			defer server.Close()
			c := vspTestClient(t, server, pub)
			c.Timeout = 100 * time.Millisecond
			err := c.get(context.Background(), "/api/v3/vspinfo", nil)
			var timeout net.Error
			if !errors.As(err, &timeout) || !timeout.Timeout() {
				t.Fatalf("stalled %s did not time out: %v", name, err)
			}
			select {
			case <-cancelled:
			case <-time.After(time.Second):
				t.Fatal("timed out request left a connection active")
			}
		})
	}
	started := make(chan struct{})
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
	}))
	defer server.Close()
	c := vspTestClient(t, server, pub)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.get(ctx, "/api/v3/vspinfo", nil) }()
	<-started
	cancel()
	if err := <-done; !errors.Is(err, context.Canceled) {
		t.Fatalf("caller cancellation lost: %v", err)
	}
}

func TestVSPClientRedirectDoesNotLeakSignedRequest(t *testing.T) {
	pub, key := vspTestKeys()
	var foreignRequests atomic.Int32
	foreign := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		foreignRequests.Add(1)
	}))
	defer foreign.Close()
	var target atomic.Value
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/final" {
			if r.Header.Get("VSP-Client-Signature") == "" {
				t.Error("same-provider redirect lost signature")
			}
			body, _ := io.ReadAll(r.Body)
			if string(body) != `{"secret":"test voting material"}` {
				t.Errorf("same-provider redirect changed body: %s", body)
			}
			writeVSPTestResponse(w, key, http.StatusOK, []byte("{}"))
			return
		}
		http.Redirect(w, r, target.Load().(string), http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	c := vspTestClient(t, server, pub)
	request := map[string]string{"secret": "test voting material"}
	for _, redirect := range []string{foreign.URL + "/final", strings.Replace(server.URL, "https:", "http:", 1) + "/final"} {
		target.Store(redirect)
		err := c.post(context.Background(), "/api/v3/payfee", nil, nil, request)
		if err == nil || !strings.Contains(err.Error(), "same HTTPS provider") {
			t.Fatalf("unsafe redirect was accepted: %v", err)
		}
	}
	if foreignRequests.Load() != 0 {
		t.Fatal("signed request reached a different provider")
	}
	target.Store(server.URL + "/final")
	if err := c.post(context.Background(), "/api/v3/payfee", nil, nil, request); err != nil {
		t.Fatalf("same-provider redirect failed: %v", err)
	}
	target.Store(server.URL + "/loop")
	if err := c.get(context.Background(), "/loop", nil); err == nil || !strings.Contains(err.Error(), "too many VSP redirects") {
		t.Fatalf("redirect loop did not stop: %v", err)
	}
}

func TestVSPClientPrivateHTTPSCustomDialer(t *testing.T) {
	pub, key := vspTestKeys()
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeVSPTestResponse(w, key, http.StatusOK, []byte(`{"feepercentage":1}`))
	}))
	defer server.Close()
	var dialed atomic.Bool
	dialer := func(ctx context.Context, network, addr string) (net.Conn, error) {
		dialed.Store(true)
		return (&net.Dialer{}).DialContext(ctx, network, addr)
	}
	c, err := New(Config{URL: server.URL, PubKey: base64.StdEncoding.EncodeToString(pub), Wallet: &wallet.Wallet{}, Dialer: dialer})
	if err != nil {
		t.Fatal(err)
	}
	transport := c.Transport.(*http.Transport)
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	defer c.CloseIdleConnections()
	fee, err := c.FeePercentage(context.Background())
	if err != nil || fee != 1 || !dialed.Load() {
		t.Fatalf("private HTTPS/custom dialer failed: fee=%v, dialed=%v, err=%v", fee, dialed.Load(), err)
	}
}

func TestVSPClientBoundsAllTicketRequests(t *testing.T) {
	pub, key := vspTestKeys()
	c := newClient("https://vsp.example", pub, vspTestSigner{})
	const count = 400
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	started := make(chan struct{}, count)
	release := make(chan struct{})
	var active, peak atomic.Int32
	c.Transport = vspTestTransport(func(req *http.Request) (*http.Response, error) {
		current := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); current > old; old = peak.Load() {
			if peak.CompareAndSwap(old, current) {
				break
			}
		}
		started <- struct{}{}
		select {
		case <-release:
		case <-req.Context().Done():
			return nil, req.Context().Err()
		}
		body := []byte("{}")
		return &http.Response{StatusCode: http.StatusOK,
			Header: http.Header{"Vsp-Server-Signature": []string{base64.StdEncoding.EncodeToString(ed25519.Sign(key, body))}},
			Body:   io.NopCloser(bytes.NewReader(body))}, nil
	})
	done := make(chan error, count)
	for i := range count {
		go func() {
			if i%2 == 0 {
				done <- c.get(ctx, "/api/v3/vspinfo", nil)
			} else {
				done <- c.post(ctx, "/api/v3/ticketstatus", nil, nil, struct{}{})
			}
		}()
	}
	for range maxConcurrentManagedTickets {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("requests did not start")
		}
	}
	select {
	case <-started:
	case <-time.After(100 * time.Millisecond):
	}
	beforeRelease := peak.Load()
	close(release)
	for range count {
		if err := <-done; err != nil {
			t.Fatal(err)
		}
	}
	if beforeRelease > maxConcurrentManagedTickets || peak.Load() > maxConcurrentManagedTickets {
		t.Fatalf("provider received %d concurrent requests, limit %d", peak.Load(), maxConcurrentManagedTickets)
	}
	if active.Load() != 0 {
		t.Fatal("request remained active after completion")
	}
}

func TestVSPClientQueuedRequestsRespectCancellationAndDeadline(t *testing.T) {
	pub, key := vspTestKeys()
	c := newClient("https://vsp.example", pub, vspTestSigner{})
	for range maxConcurrentManagedTickets {
		c.requests <- struct{}{}
	}
	var calls atomic.Int32
	c.Transport = vspTestTransport(func(req *http.Request) (*http.Response, error) {
		calls.Add(1)
		deadline, ok := req.Context().Deadline()
		if !ok || time.Until(deadline) < vspRequestTimeout-100*time.Millisecond {
			t.Error("queue wait consumed the request timeout")
		}
		body := []byte("{}")
		return &http.Response{StatusCode: http.StatusOK,
			Header: http.Header{"Vsp-Server-Signature": []string{base64.StdEncoding.EncodeToString(ed25519.Sign(key, body))}},
			Body:   io.NopCloser(bytes.NewReader(body))}, nil
	})
	for _, cancelled := range []bool{false, true} {
		ctx, cancel := context.WithTimeout(context.Background(), 25*time.Millisecond)
		if cancelled {
			cancel()
		}
		err := c.get(ctx, "/api/v3/vspinfo", nil)
		cancel()
		if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("queued request lost cancellation: %v", err)
		}
	}
	if calls.Load() != 0 {
		t.Fatal("cancelled queued request contacted the provider")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- c.get(ctx, "/api/v3/vspinfo", nil) }()
	select {
	case err := <-done:
		t.Fatalf("queued request ended early: %v", err)
	case <-time.After(150 * time.Millisecond):
	}
	<-c.requests
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if calls.Load() != 1 {
		t.Fatal("queued request did not complete exactly once")
	}
	if len(c.requests) != maxConcurrentManagedTickets-1 {
		t.Fatal("request leaked a slot")
	}
}

type vspSilentConn struct {
	net.Conn
	silent atomic.Bool
}

func (c *vspSilentConn) Write(p []byte) (int, error) {
	if c.silent.Load() {
		return len(p), nil
	}
	return c.Conn.Write(p)
}

type vspSilentListener struct {
	net.Listener
	accepted atomic.Int32
}

func (l *vspSilentListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	l.accepted.Add(1)
	return &vspSilentConn{Conn: c}, nil
}

func TestVSPClientIsolatesSilentHTTPConnection(t *testing.T) {
	pub, key := vspTestKeys()
	type connKey struct{}
	stalled, disconnected := make(chan struct{}), make(chan struct{})
	started := make(chan struct{}, 32)
	release := make(chan struct{})
	var released atomic.Bool
	defer func() {
		if released.CompareAndSwap(false, true) {
			close(release)
		}
	}()
	server := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 || r.TLS.NegotiatedProtocol == "h2" {
			t.Error("VSP request did not use HTTP/1.1")
		}
		if r.URL.Path == "/stall" {
			r.Context().Value(connKey{}).(*vspSilentConn).silent.Store(true)
			writeVSPTestResponse(w, key, http.StatusOK, []byte(`{"feepercentage":1}`))
			w.(http.Flusher).Flush()
			close(stalled)
			<-r.Context().Done()
			close(disconnected)
			return
		}
		if r.URL.Path == "/hold" {
			started <- struct{}{}
			select {
			case <-release:
			case <-r.Context().Done():
				return
			}
		}
		writeVSPTestResponse(w, key, http.StatusOK, []byte(`{"feepercentage":1}`))
	}))
	listener := &vspSilentListener{Listener: server.Listener}
	server.Listener = listener
	server.EnableHTTP2 = true
	server.Config.ConnContext = func(ctx context.Context, conn net.Conn) context.Context {
		return context.WithValue(ctx, connKey{}, conn.(*tls.Conn).NetConn().(*vspSilentConn))
	}
	server.StartTLS()
	defer server.Close()
	c, err := New(Config{URL: server.URL,
		PubKey: base64.StdEncoding.EncodeToString(pub), Wallet: &wallet.Wallet{}})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	transport := c.Transport.(*http.Transport)
	transport.TLSClientConfig = server.Client().Transport.(*http.Transport).TLSClientConfig.Clone()
	transport.ForceAttemptHTTP2 = true
	c.sign = (vspTestSigner{}).SignMessage
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, err := c.FeePercentage(ctx); err != nil {
		t.Fatal(err)
	}
	stallCtx, stopStall := context.WithCancel(ctx)
	defer stopStall()
	stallDone := make(chan error, 1)
	go func() { stallDone <- c.get(stallCtx, "/stall", nil) }()
	select {
	case <-stalled:
	case <-ctx.Done():
		t.Fatal("request did not stall")
	}
	const count = 2 * (maxConcurrentManagedTickets - 1)
	done := make(chan error, count)
	for i := range count {
		go func() {
			if i%2 == 0 {
				done <- c.get(ctx, "/hold", nil)
			} else {
				done <- c.post(ctx, "/hold", nil, nil, struct{}{})
			}
		}()
	}
	for range maxConcurrentManagedTickets - 1 {
		select {
		case <-started:
		case <-ctx.Done():
			t.Fatal("other requests blocked behind silent connection")
		}
	}
	if got := listener.accepted.Load(); got != maxConcurrentManagedTickets {
		t.Fatalf("unexpected connection count: %d", got)
	}
	if released.CompareAndSwap(false, true) {
		close(release)
	}
	for range count {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-ctx.Done():
			t.Fatal("healthy requests did not complete")
		}
	}
	select {
	case err := <-stallDone:
		t.Fatalf("silent request ended before cancellation: %v", err)
	default:
	}
	stopStall()
	select {
	case err := <-stallDone:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("silent request lost cancellation: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("silent request did not cancel")
	}
	select {
	case <-disconnected:
	case <-ctx.Done():
		t.Fatal("cancelled connection remained open")
	}
	fee, err := c.FeePercentage(ctx)
	if err != nil || fee != 1 {
		t.Fatalf("request after cancellation: fee=%v err=%v", fee, err)
	}
	if got := listener.accepted.Load(); got != maxConcurrentManagedTickets {
		t.Fatalf("healthy connections were not reused: %d", got)
	}
}
