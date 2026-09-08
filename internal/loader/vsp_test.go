package loader

import (
	"encoding/base64"
	"testing"

	"github.com/EXCCoin/exccwallet/v2/errors"
	"github.com/EXCCoin/exccwallet/v2/internal/vsp"
	"github.com/EXCCoin/exccwallet/v2/wallet"
)

func TestVSPClientsStayWithLoadedWallet(t *testing.T) {
	newWallet := func(name string) (*Loader, *wallet.Wallet) {
		db, err := wallet.CreateDB("bdb", t.TempDir()+"/"+name)
		if err != nil {
			t.Fatal(err)
		}
		l, w := new(Loader), new(wallet.Wallet)
		l.onLoaded(w, db)
		t.Cleanup(func() {
			if l.wallet != nil {
				_ = l.UnloadWallet()
			}
		})
		return l, w
	}
	a, wa := newWallet("a.db")
	_, wb := newWallet("b.db")
	cfg := vsp.Config{URL: "https://vsp.example", PubKey: base64.StdEncoding.EncodeToString(make([]byte, 32)), Wallet: wa, Policy: vsp.Policy{FeeAcct: 1}}
	ca, err := VSP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Policy.FeeAcct = 2
	cfg.URL = "https://VSP.example:443/"
	reused, err := VSP(cfg)
	if err != nil || reused != ca {
		t.Fatalf("same wallet must retain its active client: %v", err)
	}
	if ca.Policy.FeeAcct != 1 {
		t.Fatal("request changed cached default fee policy")
	}
	cfg.Wallet = wb
	cb, err := VSP(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if ca == cb || cb.Wallet != wb {
		t.Fatal("VSP client leaked between wallets")
	}
	for w, want := range map[*wallet.Wallet]*vsp.Client{wa: ca, wb: cb} {
		got, err := LookupVSP(w, cfg.URL)
		if err != nil || got != want || len(AllVSPs(w)) != 1 {
			t.Fatalf("wrong wallet's client: %v", err)
		}
	}
	cfg.Wallet = wa
	cfg.PubKey = base64.StdEncoding.EncodeToString(append([]byte{1}, make([]byte, 31)...))
	if _, err := VSP(cfg); !errors.Is(err, errors.Permission) {
		t.Fatalf("changed identity accepted: %v", err)
	}
	if len(AllVSPs(wa)) != 1 {
		t.Fatal("identity change replaced active client")
	}
	if err := a.UnloadWallet(); err != nil {
		t.Fatal(err)
	}
	if len(AllVSPs(wa)) != 0 || len(AllVSPs(wb)) != 1 {
		t.Fatal("unload did not isolate wallet cache cleanup")
	}
	if _, err := LookupVSP(wa, cfg.URL); !errors.Is(err, errors.NotExist) {
		t.Fatalf("unloaded wallet client remains: %v", err)
	}
	if _, err := VSP(cfg); !errors.Is(err, errors.Invalid) {
		t.Fatalf("stale wallet reference recreated client: %v", err)
	}
}
