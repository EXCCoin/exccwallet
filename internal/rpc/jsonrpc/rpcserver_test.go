// Copyright (c) 2013-2014 The btcsuite developers
// Copyright (c) 2015 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package jsonrpc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	dcrdtypes "github.com/EXCCoin/exccd/rpc/jsonrpc/types/v3"
	"github.com/EXCCoin/exccwallet/v2/internal/loader"
	"github.com/EXCCoin/exccwallet/v2/version"
)

func TestThrottle(t *testing.T) {
	const threshold = 1
	busy := make(chan struct{})

	srv := httptest.NewServer(throttledFn(threshold,
		func(w http.ResponseWriter, r *http.Request) {
			<-busy
		}),
	)

	type resp struct {
		resp *http.Response
		err  error
	}
	responses := make(chan resp, 2)
	for i := 0; i < cap(responses); i++ {
		go func() {
			r, err := http.Get(srv.URL)
			responses <- resp{r, err}
		}()
	}

	got := make(map[int]int, cap(responses))
	for i := 0; i < cap(responses); i++ {
		r := <-responses
		if r.err != nil {
			t.Fatal(r.err)
		}
		got[r.resp.StatusCode]++

		if i == 0 {
			close(busy)
		}
	}

	want := map[int]int{200: 1, 429: 1}
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("status codes: want: %v, got: %v", want, got)
	}
}

func TestVersion(t *testing.T) {
	s := &Server{walletLoader: new(loader.Loader)}
	result, err := s.version(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	versions := result.(map[string]dcrdtypes.VersionResult)

	walletVersion, ok := versions["exccwallet"]
	if !ok {
		t.Fatal("missing exccwallet version")
	}
	if walletVersion.VersionString != version.String() ||
		walletVersion.Major != version.Major ||
		walletVersion.Minor != version.Minor ||
		walletVersion.Patch != version.Patch ||
		walletVersion.Prerelease != version.PreRelease ||
		walletVersion.BuildMetadata != version.BuildMetadata {
		t.Fatalf("exccwallet version = %+v", walletVersion)
	}

	apiVersion, ok := versions["exccwalletjsonrpcapi"]
	if !ok {
		t.Fatal("missing exccwallet JSON-RPC API version")
	}
	if apiVersion.VersionString != jsonrpcSemverString {
		t.Fatalf("JSON-RPC API version = %q, want %q",
			apiVersion.VersionString, jsonrpcSemverString)
	}
}
