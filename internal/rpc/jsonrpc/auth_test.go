// Copyright (c) 2026 The ExchangeCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package jsonrpc

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"testing"

	"github.com/EXCCoin/exccd/dcrjson/v4"
)

// authRequest builds an "authenticate" websocket request with the given params.
func authRequest(t *testing.T, params string) *dcrjson.Request {
	t.Helper()
	var req dcrjson.Request
	body := `{"jsonrpc":"1.0","id":1,"method":"authenticate","params":` + params + `}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal request: %v", err)
	}
	return &req
}

// serverWithCreds returns a Server configured with the given basic auth creds.
func serverWithCreds(user, pass string) *Server {
	login := user + ":" + pass
	auth := "Basic " + base64.StdEncoding.EncodeToString([]byte(login))
	h := sha256.Sum256([]byte(auth))
	return &Server{authsha: &h}
}

// TestInvalidAuthFailsClosed ensures the authenticate RPC accepts only exact
// credential matches and rejects malformed or incomplete requests.
func TestInvalidAuthFailsClosed(t *testing.T) {
	s := serverWithCreds("user", "pass")

	tests := []struct {
		name   string
		params string
		want   bool
	}{
		{"correct credentials", `["user","pass"]`, false},
		{"wrong passphrase", `["user","wrong"]`, true},
		{"wrong username", `["wrong","pass"]`, true},
		{"empty credentials", `["",""]`, true},
		{"too few params", `["user"]`, true},
		{"no params", `[]`, true},
		{"wrong param types", `[1,2]`, true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := s.invalidAuth(authRequest(t, test.params))
			if got != test.want {
				t.Fatalf("invalidAuth = %v, want %v", got, test.want)
			}
		})
	}
}

// TestInvalidAuthNoBasicAuthConfigured ensures the authenticate RPC cannot be
// used at all when basic auth is disabled (TLS client cert auth only).
func TestInvalidAuthNoBasicAuthConfigured(t *testing.T) {
	s := &Server{authsha: nil}
	if !s.invalidAuth(authRequest(t, `["user","pass"]`)) {
		t.Fatal("invalidAuth = false, want true when basic auth is disabled")
	}
}
