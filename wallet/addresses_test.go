// Copyright (c) 2018-2019 The Decred developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package wallet

import (
	"bytes"
	"context"
	"encoding/hex"
	"os"
	"testing"

	"github.com/EXCCoin/exccwallet/v2/wallet/walletdb"
	"github.com/EXCCoin/exccd/chaincfg/v3"
	"github.com/EXCCoin/exccd/dcrutil/v4"
	"github.com/EXCCoin/exccd/txscript/v4/stdaddr"
)

// expectedAddr is used to house the expected return values from a managed
// address.  Not all fields for used for all managed address types.
type expectedAddr struct {
	address     string
	addressHash []byte
	branch      uint32
	pubKey      []byte
}

// testContext is used to store context information about a running test which
// is passed into helper functions.
type testContext struct {
	t            *testing.T
	account      uint32
	watchingOnly bool
}

// hexToBytes is a wrapper around hex.DecodeString that panics if there is an
// error.  It MUST only be used with hard coded values in the tests.
func hexToBytes(origHex string) []byte {
	buf, err := hex.DecodeString(origHex)
	if err != nil {
		panic(err)
	}
	return buf
}

var (
	// seed is the master seed used throughout the tests.
	seed = []byte{
		0xb4, 0x6b, 0xc6, 0x50, 0x2a, 0x30, 0xbe, 0xb9, 0x2f,
		0x0a, 0xeb, 0xc7, 0x76, 0x40, 0x3c, 0x3d, 0xbf, 0x11,
		0xbf, 0xb6, 0x83, 0x05, 0x96, 0x7c, 0x36, 0xda, 0xc9,
		0xef, 0x8d, 0x64, 0x15, 0x67,
	}

	pubPassphrase  = []byte("_DJr{fL4H0O}*-0\n:V1izc)(6BomK")
	privPassphrase = []byte("81lUHXnOMZ@?XXd7O9xyDIWIbXX-lj")

	walletConfig = Config{
		PubPassphrase: pubPassphrase,
		GapLimit:      20,
		RelayFee:      dcrutil.Amount(1e5),
		Params:        chaincfg.SimNetParams(),
	}

	defaultAccount     = uint32(0)
	defaultAccountName = "default"

	waddrmgrBucketKey = []byte("waddrmgr")

	expectedInternalAddrs = []expectedAddr{
		{
			address:     "SskxCsXL8vNm7vJGKngsYN88VMADfAaGbS5",
			addressHash: hexToBytes("b61d627e15f0aa65e1fe00929c0c8232cfb30a33"),
			branch:      1,
			pubKey:      hexToBytes("02c20a0c03aead40e7c41be1f581ea200c180e7e24c1dae542b83b28390433b0de"),
		},
		{
			address:     "Ssrg2hVh7SHdmWdUD8XgpVZ2YQAjBJStAB5",
			addressHash: hexToBytes("f4defdfceb5f93a7d000ddc0754b6fd530d8a335"),
			branch:      1,
			pubKey:      hexToBytes("0257b3ec063ce28f9c9bfd8a0f81c7b34dc73a88aa65372477767f80f6e24d7f77"),
		},
		{
			address:     "SsqTAmS3esBArwufXxWoxcDj2KwCWzN2Lut",
			addressHash: hexToBytes("e778318e96948b283c5f41222d2d9508dfca4814"),
			branch:      1,
			pubKey:      hexToBytes("032b38dfebaac31fcdc05103c9a95a5bdd0a13f92a183265a45131a4039f04fe6e"),
		},
		{
			address:     "SsZxFzYhWs5WomAmZYjG9D84zRNUNoXyNzF",
			addressHash: hexToBytes("3d769b41f22ce5cc9fdf8825a76153e8ccd826d0"),
			branch:      1,
			pubKey:      hexToBytes("0322f71037a9f14734814508e373748f771dae08eabd8edb965bf6cba2891605c0"),
		},
		{
			address:     "SsseJmqAvGeU23vgZHtnXhs61VmQCCfDDJ1",
			addressHash: hexToBytes("ff83b45c2a042bd3834030b25c5c5935a0c9b571"),
			branch:      1,
			pubKey:      hexToBytes("02c46344dda4a84d0a657f5a4d3fb8138b3705e739c86ccf8519f53ef9e885cc51"),
		},
	}

	expectedExternalAddrs = []expectedAddr{
		{
			address:     "SsnezvUt3Lx5FoVRnAsW2Jv8mkdYH9PqTnR",
			addressHash: hexToBytes("c8cc97263cd035bfea3f9a7dc8e6d1d2d3096095"),
			pubKey:      hexToBytes("027a5fd32d39739ef49d003a38d3a4122411e0f977df2b285ea2465af1d692ae32"),
		},
		{
			address:     "SsrWXcfYNFs8X84roWwzVb6NSRJpJi6CDHr",
			addressHash: hexToBytes("f312f89283bd5bdb98b574cf129c2fe31e92f11c"),
			pubKey:      hexToBytes("02822dc0f2fd4fab28a18018dd0f738252c1b9a2806f9e37d64644f9a39895b803"),
		},
		{
			address:     "SsfLoxgzpgqEWwSUX5rTEShjEHb5686VD6n",
			addressHash: hexToBytes("78931273ff6520e9ef068f92c2c929a6192006db"),
			pubKey:      hexToBytes("027cfde63a36ef6e2bf134155ad2e8368704e6a6f58645101ab4a9286e2306a19c"),
		},
		{
			address:     "Ssf2wpUEbZ7VzH8vfJsxxRqX4ptnHFkunw8",
			addressHash: hexToBytes("753224ffc959e79062b6c9863b622cb97c4121ce"),
			pubKey:      hexToBytes("02a872aba38c083db55417c1d467630cdaf7be7c69845b96d317bea98c6908b376"),
		},
		{
			address:     "SsWxt2qbdLMyhwG8GrcYq6m1rP2ARqWusLV",
			addressHash: hexToBytes("1cac503a378f06d8e4a699c857a0015457950b1b"),
			pubKey:      hexToBytes("03601fee537616d0b7dd56d52ae98764c0c90bfdb293e468d3ded650630b310823"),
		},
	}
)

func setupWallet(t *testing.T, cfg *Config) (*Wallet, walletdb.DB, func()) {
	f, err := os.CreateTemp("", "testwallet.db")
	if err != nil {
		t.Fatal(err)
	}
	f.Close()

	db, err := walletdb.Create("bdb", f.Name())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	err = Create(ctx, opaqueDB{db}, pubPassphrase, privPassphrase, seed, cfg.Params)
	if err != nil {
		db.Close()
		os.Remove(f.Name())
		t.Fatal(err)
	}
	cfg.DB = opaqueDB{db}

	w, err := Open(ctx, cfg)
	if err != nil {
		db.Close()
		os.Remove(f.Name())
		t.Fatal(err)
	}

	teardown := func() {
		db.Close()
		os.Remove(f.Name())
	}

	return w, db, teardown
}

type newAddressFunc func(*Wallet, context.Context, uint32, ...NextAddressCallOption) (stdaddr.Address, error)

func testKnownAddresses(tc *testContext, prefix string, unlock bool, newAddr newAddressFunc, tests []expectedAddr) {
	w, db, teardown := setupWallet(tc.t, &walletConfig)
	defer teardown()

	ctx := context.Background()

	if unlock {
		err := w.Unlock(ctx, privPassphrase, nil)
		if err != nil {
			tc.t.Fatal(err)
		}
	}

	if tc.watchingOnly {
		err := walletdb.Update(ctx, db, func(tx walletdb.ReadWriteTx) error {
			ns := tx.ReadWriteBucket(waddrmgrBucketKey)
			return w.manager.ConvertToWatchingOnly(ns)
		})
		if err != nil {
			tc.t.Fatalf("%s: failed to convert wallet to watching only: %v",
				prefix, err)
		}
	}

	for i := 0; i < len(tests); i++ {
		addr, err := newAddr(w, context.Background(), defaultAccount)
		if err != nil {
			tc.t.Fatalf("%s: failed to generate external address: %v",
				prefix, err)
		}

		ka, err := w.KnownAddress(ctx, addr)
		if err != nil {
			tc.t.Errorf("Unexpected error: %v", err)
			continue
		}

		if ka.AccountName() != defaultAccountName {
			tc.t.Errorf("%s: expected account %v got %v", prefix,
				defaultAccount, ka.AccountName())
		}

		if ka.String() != tests[i].address {
			tc.t.Errorf("%s: expected address %v got %v", prefix,
				tests[i].address, ka)
		}
		a := ka.(BIP0044Address)
		if !bytes.Equal(a.PubKeyHash(), tests[i].addressHash) {
			tc.t.Errorf("%s: expected address hash %v got %v", prefix,
				hex.EncodeToString(tests[i].addressHash),
				hex.EncodeToString(a.PubKeyHash()))
		}

		if _, branch, _ := a.Path(); branch != tests[i].branch {
			tc.t.Errorf("%s: expected branch of %v got %v", prefix,
				tests[i].branch, branch)
		}

		pubKey := a.PubKey()
		if !bytes.Equal(pubKey, tests[i].pubKey) {
			tc.t.Errorf("%s: expected pubkey %v got %v",
				prefix, hex.EncodeToString(tests[i].pubKey),
				hex.EncodeToString(pubKey))
		}
	}
}

func TestAddresses(t *testing.T) {
	testAddresses(t, false)
	testAddresses(t, true)
}

func testAddresses(t *testing.T, unlock bool) {
	testKnownAddresses(&testContext{
		t:            t,
		account:      defaultAccount,
		watchingOnly: false,
	}, "testInternalAddresses", unlock, (*Wallet).NewInternalAddress, expectedInternalAddrs)

	testKnownAddresses(&testContext{
		t:            t,
		account:      defaultAccount,
		watchingOnly: true,
	}, "testInternalAddresses", unlock, (*Wallet).NewInternalAddress, expectedInternalAddrs)

	testKnownAddresses(&testContext{
		t:            t,
		account:      defaultAccount,
		watchingOnly: false,
	}, "testExternalAddresses", unlock, (*Wallet).NewExternalAddress, expectedExternalAddrs)

	testKnownAddresses(&testContext{
		t:            t,
		account:      defaultAccount,
		watchingOnly: true,
	}, "testExternalAddresses", unlock, (*Wallet).NewExternalAddress, expectedExternalAddrs)
}

func TestAccountIndexes(t *testing.T) {
	cfg := basicWalletConfig
	w, teardown := testWallet(t, &cfg)
	defer teardown()

	w.SetNetworkBackend(mockNetwork{})

	tests := []struct {
		f       func(t *testing.T, w *Wallet)
		indexes accountIndexes
	}{
		{nil, accountIndexes{{^uint32(0), 0}, {^uint32(0), 0}}},
		{nextAddresses(1), accountIndexes{{^uint32(0), 1}, {^uint32(0), 0}}},
		{nextAddresses(19), accountIndexes{{^uint32(0), 20}, {^uint32(0), 0}}},
		{watchFutureAddresses, accountIndexes{{^uint32(0), 20}, {^uint32(0), 0}}},
		{useAddress(10), accountIndexes{{10, 9}, {^uint32(0), 0}}},
		{nextAddresses(1), accountIndexes{{10, 10}, {^uint32(0), 0}}},
		{nextAddresses(10), accountIndexes{{10, 20}, {^uint32(0), 0}}},
		{useAddress(30), accountIndexes{{30, 0}, {^uint32(0), 0}}},
		{useAddress(31), accountIndexes{{31, 0}, {^uint32(0), 0}}},
	}
	for i, test := range tests {
		if test.f != nil {
			test.f(t, w)
		}
		w.addressBuffersMu.Lock()
		b := w.addressBuffers[0]
		t.Logf("ext last=%d, ext cursor=%d, int last=%d, int cursor=%d",
			b.albExternal.lastUsed, b.albExternal.cursor, b.albInternal.lastUsed, b.albInternal.cursor)
		check := func(what string, a, b uint32) {
			if a != b {
				t.Fatalf("%d: %s do not match: %d != %d", i, what, a, b)
			}
		}
		check("external last indexes", b.albExternal.lastUsed, test.indexes[0].last)
		check("external cursors", b.albExternal.cursor, test.indexes[0].cursor)
		check("internal last indexes", b.albInternal.lastUsed, test.indexes[1].last)
		check("internal cursors", b.albInternal.cursor, test.indexes[1].cursor)
		w.addressBuffersMu.Unlock()
	}
}

type accountIndexes [2]struct {
	last, cursor uint32
}

func nextAddresses(n int) func(t *testing.T, w *Wallet) {
	return func(t *testing.T, w *Wallet) {
		for i := 0; i < n; i++ {
			_, err := w.NewExternalAddress(context.Background(), 0)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
}

func watchFutureAddresses(t *testing.T, w *Wallet) {
	ctx := context.Background()
	n, _ := w.NetworkBackend()
	_, err := w.watchHDAddrs(ctx, false, n)
	if err != nil {
		t.Fatal(err)
	}
}

func useAddress(child uint32) func(t *testing.T, w *Wallet) {
	ctx := context.Background()
	return func(t *testing.T, w *Wallet) {
		w.addressBuffersMu.Lock()
		xbranch := w.addressBuffers[0].albExternal.branchXpub
		w.addressBuffersMu.Unlock()
		addr, err := deriveChildAddress(xbranch, child, basicWalletConfig.Params)
		if err != nil {
			t.Fatal(err)
		}
		err = walletdb.Update(ctx, w.db, func(dbtx walletdb.ReadWriteTx) error {
			ns := dbtx.ReadWriteBucket(waddrmgrBucketKey)
			ma, err := w.manager.Address(ns, addr)
			if err != nil {
				return err
			}
			return w.markUsedAddress("", dbtx, ma)
		})
		if err != nil {
			t.Fatal(err)
		}
		watchFutureAddresses(t, w)
	}
}
