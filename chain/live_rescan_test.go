// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package chain

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/EXCCoin/exccd/chaincfg/chainhash"
	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccd/gcs/v3"
	"github.com/EXCCoin/exccd/gcs/v3/blockcf2"
	"github.com/EXCCoin/exccd/wire"
	"github.com/EXCCoin/exccwallet/v2/rpc/client/dcrd"
	"github.com/EXCCoin/exccwallet/v2/wallet"
	_ "github.com/EXCCoin/exccwallet/v2/wallet/drivers/bdb"
	"github.com/gorilla/websocket"
)

type rescanCaller func(context.Context, string, interface{}, ...interface{}) error

func (f rescanCaller) Call(ctx context.Context, method string, result interface{}, args ...interface{}) error {
	return f(ctx, method, result, args...)
}

func liveRescanWallet(t *testing.T) (*wallet.Wallet, *gcs.FilterV2) {
	t.Helper()
	ctx := context.Background()
	db, err := wallet.CreateDB("bdb", filepath.Join(t.TempDir(), "wallet.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	params := chaincfg.SimNetParams()
	pub := []byte(wallet.InsecurePubPassphrase)
	if err := wallet.Create(ctx, db, pub, []byte("private"), bytes.Repeat([]byte{1}, 32), params); err != nil {
		t.Fatal(err)
	}
	w, err := wallet.Open(ctx, &wallet.Config{DB: db, PubPassphrase: pub, Params: params, GapLimit: 20})
	if err != nil {
		t.Fatal(err)
	}
	filter, err := gcs.FromBytesV2(blockcf2.B, blockcf2.M, nil)
	if err != nil {
		t.Fatal(err)
	}
	return w, filter
}

func rescanTestBlock(w *wallet.Wallet, filter *gcs.FilterV2, parent *wire.BlockHeader, nonce uint32, known bool) *wallet.BlockNode {
	header := &wire.BlockHeader{PrevBlock: parent.BlockHash(), Height: parent.Height + 1,
		Bits: w.ChainParams().PowLimitBits, StakeRoot: filter.Hash(), VoteBits: 1,
		Timestamp: parent.Timestamp.Add(time.Second), Nonce: nonce}
	hash := header.BlockHash()
	var relevant []*wire.MsgTx
	if known {
		relevant = []*wire.MsgTx{}
	}
	return wallet.NewBlockNode(header, &hash, filter, relevant)
}

func attachRescanTestBlocks(t *testing.T, s *Syncer, blocks ...*wallet.BlockNode) {
	t.Helper()
	for _, b := range blocks {
		s.sidechains.AddBlockNode(b)
	}
	best, err := s.wallet.EvaluateBestChain(context.Background(), &s.sidechains)
	if err != nil {
		t.Fatal(err)
	}
	if len(best) == 0 {
		t.Fatal("test blocks did not extend the best chain")
	}
	if _, err := s.wallet.ChainSwitch(context.Background(), &s.sidechains, best); err != nil {
		t.Fatal(err)
	}
}

func blockNotification(t *testing.T, node *wallet.BlockNode) json.RawMessage {
	t.Helper()
	var header bytes.Buffer
	if err := node.Header.Serialize(&header); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal([]interface{}{hex.EncodeToString(header.Bytes()), []string{}})
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestLiveRPCRescansHeaderOnlyGapBeforeReady(t *testing.T) {
	for _, failure := range []bool{false, true} {
		t.Run(fmt.Sprintf("rescan_failure_%v", failure), func(t *testing.T) {
			ctx := context.Background()
			w, filter := liveRescanWallet(t)
			s := NewSyncer(w, nil)
			s.notifier = &notifier{syncer: s, ctx: ctx}
			one := rescanTestBlock(w, filter, &w.ChainParams().GenesisBlock.Header, 1, true)
			old := rescanTestBlock(w, filter, one.Header, 2, true)
			attachRescanTestBlocks(t, s, one, old)
			missing := rescanTestBlock(w, filter, one.Header, 3, false)
			tip := rescanTestBlock(w, filter, missing.Header, 4, true)
			s.sidechains.AddBlockNode(missing)
			address, err := w.NewExternalAddress(ctx, 0)
			if err != nil {
				t.Fatal(err)
			}
			version, script := address.(wallet.Address).PaymentScript()
			credit := wire.NewMsgTx()
			credit.AddTxIn(wire.NewTxIn(&wire.OutPoint{Hash: chainhash.Hash{8}}, 1000, nil))
			credit.AddTxOut(&wire.TxOut{Value: 900, Version: version, PkScript: script})
			creditBytes, err := credit.Bytes()
			if err != nil {
				t.Fatal(err)
			}

			var events []string
			var mu sync.Mutex
			event := func(value string) { mu.Lock(); events = append(events, value); mu.Unlock() }
			s.cb = &Callbacks{
				Synced:         func(ready bool) { event(fmt.Sprintf("synced:%v", ready)) },
				RescanStarted:  func() { event("start") },
				RescanProgress: func(height int32) { event(fmt.Sprintf("progress:%d", height)) },
				RescanFinished: func() { event("finish") },
			}
			atomic.StoreUint32(&s.atomicWalletSynced, 1)
			notifications := w.NtfnServer.MainTipChangedNotifications()
			defer notifications.Done()
			tipObserved := make(chan struct{})
			go func() {
				<-notifications.C
				if atomic.LoadUint32(&s.atomicWalletSynced) != 0 {
					t.Error("new tip announced while wallet still reported ready")
				}
				event("tip")
				close(tipObserved)
			}()
			rescanCalls := 0
			s.rpc = dcrd.New(rescanCaller(func(_ context.Context, method string, result interface{}, args ...interface{}) error {
				switch method {
				case "getcfilterv2":
					return json.Unmarshal([]byte(`{"data":"","proofindex":0,"proofhashes":[]}`), result)
				case "loadtxfilter":
					return nil
				case "rescan":
					<-tipObserved
					rescanCalls++
					encoded, err := json.Marshal(args[0])
					if err != nil {
						return err
					}
					var hashes []string
					if err := json.Unmarshal(encoded, &hashes); err != nil {
						return err
					}
					if !reflect.DeepEqual(hashes, []string{missing.Hash.String(), tip.Hash.String()}) {
						t.Errorf("wrong rescan range: %v", hashes)
					}
					if failure {
						return context.DeadlineExceeded
					}
					response, _ := json.Marshal(map[string]interface{}{"discovereddata": []interface{}{map[string]interface{}{"hash": missing.Hash.String(), "transactions": []string{hex.EncodeToString(creditBytes)}}}})
					return json.Unmarshal(response, result)
				default:
					return fmt.Errorf("unexpected RPC %s", method)
				}
			}))
			err = s.notifier.Notify("blockconnected", blockNotification(t, tip))
			<-tipObserved
			point, pointErr := w.RescanPoint(ctx)
			if pointErr != nil {
				t.Fatal(pointErr)
			}
			if failure {
				if !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("first live block swallowed rescan failure: %v", err)
				}
				if point == nil || *point != *missing.Hash || atomic.LoadUint32(&s.atomicWalletSynced) != 0 {
					t.Fatal("failed rescan lost its gap or restored readiness")
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				if point != nil || atomic.LoadUint32(&s.atomicWalletSynced) != 1 {
					t.Fatalf("rescan did not finish: point=%v", point)
				}
				hash := credit.TxHash()
				block, height, err := w.TxBlock(ctx, &hash)
				if err != nil || block != *missing.Hash || height != 2 {
					t.Fatalf("missing transaction not recovered: %v %d %v", block, height, err)
				}
			}
			if rescanCalls != 1 {
				t.Fatalf("rescan calls = %d", rescanCalls)
			}
			want := []string{"synced:false", "tip", "start"}
			if !failure {
				want = append(want, "progress:3", "finish", "synced:true")
			}
			mu.Lock()
			defer mu.Unlock()
			if !reflect.DeepEqual(events, want) {
				t.Errorf("events = %v, want %v", events, want)
			}
		})
	}
}

func TestStartupReadyIsSerializedWithLiveChainSwitch(t *testing.T) {
	ctx := context.Background()
	w, filter := liveRescanWallet(t)
	s := NewSyncer(w, nil)
	s.notifier = &notifier{syncer: s, ctx: ctx}
	one := rescanTestBlock(w, filter, &w.ChainParams().GenesisBlock.Header, 1, true)
	old := rescanTestBlock(w, filter, one.Header, 2, true)
	attachRescanTestBlocks(t, s, one, old)
	missing := rescanTestBlock(w, filter, one.Header, 3, false)
	tip := rescanTestBlock(w, filter, missing.Header, 4, true)
	s.sidechains.AddBlockNode(missing)

	loading := make(chan struct{})
	allowLoad := make(chan struct{})
	filterRequested := make(chan struct{})
	var loadCalls atomic.Int32
	s.rpc = dcrd.New(rescanCaller(func(_ context.Context, method string, result interface{}, args ...interface{}) error {
		switch method {
		case "loadtxfilter":
			if loadCalls.Add(1) == 1 {
				close(loading)
				<-allowLoad
			}
			return nil
		case "getcfilterv2":
			close(filterRequested)
			return json.Unmarshal([]byte(`{"data":"","proofindex":0,"proofhashes":[]}`), result)
		case "rescan":
			return context.DeadlineExceeded
		default:
			return fmt.Errorf("unexpected RPC %s", method)
		}
	}))
	var events []bool
	s.cb = &Callbacks{Synced: func(ready bool) {
		events = append(events, ready)
		if ready {
			hash, _ := w.MainChainTip(ctx)
			point, err := w.RescanPoint(ctx)
			if hash != *old.Hash || point != nil || err != nil {
				t.Error("startup announced readiness after a live gap was attached")
			}
		}
	}}
	startupResult := make(chan error, 1)
	go func() { startupResult <- s.finishStartupSync(ctx) }()
	<-loading
	if s.sidechainsMu.TryLock() {
		s.sidechainsMu.Unlock()
		t.Error("startup final filter/ready stage does not serialize chain changes")
	}
	liveResult := make(chan error, 1)
	params := blockNotification(t, tip)
	go func() { liveResult <- s.notifier.Notify("blockconnected", params) }()
	<-filterRequested
	close(allowLoad)
	if err := <-startupResult; err != nil {
		t.Fatal(err)
	}
	if err := <-liveResult; !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("notification waiting for startup swallowed live catch-up failure: %v", err)
	}
	if !reflect.DeepEqual(events, []bool{true, false}) {
		t.Fatalf("readiness transitions = %v", events)
	}
	point, err := w.RescanPoint(ctx)
	if err != nil || point == nil || *point != *missing.Hash {
		t.Fatalf("live gap after startup = %v, %v", point, err)
	}
}

func TestRPCStartupLoadsFilterBeforeBlockNotifications(t *testing.T) {
	w, _ := liveRescanWallet(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var subscribed atomic.Bool
	upgrader := websocket.Upgrader{}
	server := httptest.NewServer(http.HandlerFunc(func(rw http.ResponseWriter, req *http.Request) {
		ws, err := upgrader.Upgrade(rw, req, nil)
		if err != nil {
			t.Error(err)
			return
		}
		defer ws.Close()
		filtered := false
		for {
			var request struct {
				ID     uint32 `json:"id"`
				Method string `json:"method"`
			}
			if err := ws.ReadJSON(&request); err != nil {
				return
			}
			var result interface{}
			switch request.Method {
			case "getcurrentnet":
				result = w.ChainParams().Net
			case "version":
				result = map[string]interface{}{"exccdjsonrpcapi": map[string]int{"major": 7, "minor": 0, "patch": 0}}
			case "loadtxfilter":
				filtered = true
			case "notifyblocks":
				// exccd permits subscriptions without a loaded filter, but then
				// sends block notifications with no relevant transactions.
				if !filtered {
					t.Error("subscribed to blocks before loading wallet filter")
				}
				subscribed.Store(true)
				cancel()
			case "getrawmempool", "getheaders":
				result = []string{}
			case "notifytspend", "rebroadcastwinners", "rebroadcastmissed":
			default:
				t.Errorf("unexpected RPC %s", request.Method)
				cancel()
				return
			}
			if err := ws.WriteJSON(map[string]interface{}{"id": request.ID, "result": result, "error": nil}); err != nil {
				return
			}
		}
	}))
	defer server.Close()
	s := NewSyncer(w, &RPCOptions{Address: strings.TrimPrefix(server.URL, "http://"), Insecure: true})
	err := s.Run(ctx)
	if err == nil || ctx.Err() != context.Canceled {
		t.Fatalf("startup did not stop after subscribing: %v", err)
	}
	if !subscribed.Load() {
		t.Fatal("block notifications were never registered")
	}
}
