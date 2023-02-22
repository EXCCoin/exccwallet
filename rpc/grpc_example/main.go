package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"fmt"
	"io/ioutil"
	"path/filepath"

	pb "github.com/EXCCoin/exccwallet/v2/rpc/walletrpc"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
	"google.golang.org/protobuf/encoding/prototext"

	"github.com/EXCCoin/exccd/dcrutil/v4"
)

var (
	certificateFile      = filepath.Join(dcrutil.AppDataDir("exccwallet", false), "rpc.cert")
	walletClientCertFile = "client.pem" // must be part of ~/.dcrwallet/clients.pem
	walletClientKeyFile  = "client-key.pem"
)

func main() {
	serverCAs := x509.NewCertPool()
	serverCert, err := ioutil.ReadFile(certificateFile)
	if err != nil {
		fmt.Println(err)
		return
	}
	if !serverCAs.AppendCertsFromPEM(serverCert) {
		fmt.Printf("no certificates found in %s\n", certificateFile)
		return
	}
	keypair, err := tls.LoadX509KeyPair(walletClientCertFile, walletClientKeyFile)
	if err != nil {
		fmt.Println(err)
		return
	}
	creds := credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{keypair},
		RootCAs:      serverCAs,
	})
	conn, err := grpc.Dial("localhost:19111", grpc.WithTransportCredentials(creds))
	if err != nil {
		fmt.Println(err)
		return
	}
	defer conn.Close()
	c := pb.NewWalletServiceClient(conn)

	balanceRequest := &pb.BalanceRequest{
		AccountNumber:         0,
		RequiredConfirmations: 1,
	}
	balanceResponse, err := c.Balance(context.Background(), balanceRequest)
	if err != nil {
		fmt.Println(err)
		return
	}

	fmt.Println("Spendable balance: ", dcrutil.Amount(balanceResponse.Spendable))

	decodedTx := func(tx string) {
		rawTx, _ := hex.DecodeString(tx)
		dmClient := pb.NewDecodeMessageServiceClient(conn)
		decodeRequest := &pb.DecodeRawTransactionRequest{
			SerializedTransaction: rawTx,
		}
		decodeResponse, err := dmClient.DecodeRawTransaction(context.Background(), decodeRequest)
		if err != nil {
			fmt.Println(err)
			return
		}

		// tj, _ := json.MarshalIndent(decodeResponse.Transaction, "", "   ")
		// fmt.Println(string(tj))
		fmt.Println(prototext.MarshalOptions{Multiline: true}.Format(decodeResponse.Transaction))
	}

	for _, tx := range txns {
		decodedTx(tx)
	}

	wsClient := pb.NewWalletServiceClient(conn)

	for _, script := range importScripts {
		// NOTE: These scripts will forever be imported into your
		// testing wallet.
		scriptB, err := hex.DecodeString(script)
		if err != nil {
			fmt.Println(err)
			return
		}
		importScriptRequest := &pb.ImportScriptRequest{
			Script: scriptB,
		}
		_, err = wsClient.ImportScript(context.Background(), importScriptRequest)
		if err != nil {
			fmt.Println(err)
			return
		}
	}

	// Add an owned address to validated addresses.
	nextAddressResp, err := wsClient.NextAddress(context.Background(), new(pb.NextAddressRequest))
	if err != nil {
		fmt.Println(err)
		return
	}
	validateAddrs = append(validateAddrs, nextAddressResp.Address)

	for _, addr := range validateAddrs {
		validateAddrRequest := &pb.ValidateAddressRequest{
			Address: addr,
		}
		validateAddrResp, err := wsClient.ValidateAddress(context.Background(), validateAddrRequest)
		if err != nil {
			fmt.Println(err)
			return
		}
		fmt.Println(validateAddrResp.ScriptType)
		fmt.Println(prototext.MarshalOptions{Multiline: true}.Format(validateAddrResp))
	}
}

var txns = []string{
	// vote (stakegen)
	"01000000020000000000000000000000000000000000000000000000000000000000000000ffffffff00ffffffff24c5e81c429df60b4bfd33b44b9535a7874a9d06053a815e1ee9d0d1a121b2290000000001ffffffff0300000000000000000000266a248cfb198b12330d1c36ed45a84d41ab0e661793aa6560b6504f0b53670000000058720c0000000000000000000000086a06010009000000da72c2620200000000001abb76a91454e2d8ebbad00e0609c5c5e8e8890848b23126f388ac000000000000000002319f2b000000000000000000ffffffff020000a9d396620200000003720c00060000006b483045022100c7c78d30bda7dd1b95f675f26e16adb7d3a086958ad312d64b21005b6d10293102205d39dc2846452d941ab47bb0e3ccec82b493630d5fdbaa06edb15396e1c55e0a0121037bb6c86704276d6753e348f85f48c53fc47a4fe34fa3ce81d31356ae7e8f7643",
	// 1 pubkeyhash and 1 pubkey output
	"01000000016e341707db12587ec25d5ea5f8ed0014c52d529ca31f380bf1be5f1d4bcf4bdf0100000000ffffffff02afef35070000000000002321022ce8072be4a73c268d6330fa6455628c6ff54fb7ece550ebc0d8f0746a702291ac950e97160000000000001976a91425bccf8268059f1ef9f7767f05fa7ded8195674588ac0000000000000000010065cd1d0000000003ad0200030000006b483045022100fa94b17b70e748f5ef0436350dd82736f9f1ca1c45e2476d3ddf0ba9db402a1a022052349cca845b8bd1df9320078b115f3ce324e3892b37ca1cc6361b6df470fb8a0121021c80d0573a302b239ed2b4db8f9324e9411e26abdb47661092daf664ddc311bd",
	// treasurebase with a tadd
	"03000000010000000000000000000000000000000000000000000000000000000000000000ffffffff00ffffffff02fdb3480000000000000001c1000000000000000000000e6a0c56720c00093c28052bb4dd16000000000000000001fdb348000000000000000000ffffffff00",
	// treasuryspend with a tgen
	"03000000010000000000000000000000000000000000000000000000000000000000000000ffffffff00ffffffff0200000000000000000000226a20f6eaf505000000000c9ef1c885c62d089ff9988eb4ca4c2c620614eb75c620bc00e1f5050000000000001ac376a914bd15503ed7d24fc5b36ceba70ee8741a36fe3dca88ac00000000f28e080001f6eaf5050000000000000000ffffffff644054a36eb67457facef5a075f8ebba5d7a9342dd13b45c6d3dd43b22bead35428603fd805eea93674aa7cd120e46e3fa7a2c563926686909c8eb1b06b40237f3a52103beca9bbd227ca6bb5a58e03a36ba2b52fff09093bd7a50aee1193bccd257fb8ac2",

	// coinbase (1 nulldata and 1 pubkeyhash)
	"03000000010000000000000000000000000000000000000000000000000000000000000000ffffffff00ffffffff02000000000000000000000e6a0c55720c00e88a9c9e5c49d15b8021b5010000000000001976a914d251921c9857791815e97750afcea48fbd30568a88ac000000000000000001f237b4010000000000000000ffffffff0800002f646372642f",
	// legacy stakepool pool ticket (note that gRPC's DecodeRawTransaction calls the sstxcommitment/nulldata outputs stakesubmission instead)
	"0100000002793d7447b9b4463cb069c684afadc757225b6d21cb395208cb8740244bf8598a0200000000ffffffff793d7447b9b4463cb069c684afadc757225b6d21cb395208cb8740244bf8598a0300000000ffffffff057aee520600000000000018baa914c8148420843952f6085ce051c66c66e538eed5cd8700000000000000000000206a1ec667be7328c77abff2f9f35d2aad91cc80a9c4fdc2880300000000000058000000000000000000001abd76a914000000000000000000000000000000000000000088ac00000000000000000000206a1e2b51ac564b85c04d874f1075960fc17d86e3777de47a4f06000000000058000000000000000000001abd76a914000000000000000000000000000000000000000088ac000000002011050002c2880300000000000c110500070000006a4730440220227d7b5cec71179918fc982a8b05bb5e4fe2267f8a7f584322cfdbd989943267022046a921072177590120d7a2365952d151ecc89dc627b65da43056f20646383002012103130967a676b0762272729278a78afbeb234b0a1831c51e857395ab565baee24ce47a4f06000000000c110500070000006a47304402205e5b12a9f11b5bd65e56ec7d21e306dedf79a717527567028121a326d5ccc02d02202f58a2e6ec9d29aa4a3bcd2b25e00aa1844f05abf3a3e2e55bad05f96b30c96e012103130967a676b0762272729278a78afbeb234b0a1831c51e857395ab565baee24c",
	// solo ticket
	"01000000016db59a8f8e1c9db0a963b08a30f02f1cea95ddd430303a8d9859ee730c01d8650000000000ffffffff03a9d396620200000000001aba76a914fd5e20128a2e5c4bd9abf93386912b627951e5c488ac00000000000000000000206a1e899eb23ed3b51cfe839f708678af833daa5f28794ddf966202000000004e000000000000000000001abd76a914000000000000000000000000000000000000000088ac0000000090720c00014ddf966202000000ffffff7fffffffff6b483045022100e04b03b2844ade2ab84847902dac3ccd6022bc23dc012a11390a59046c59186902202c0f6bddfa4b4c2494f40bfc8a7ec07ab570c4be6d66fa10b48555deb66148d101210248be5d2501e3a9ed3079138a48e943c12e78da56ed752caf590c510f1ce3c929",
}

var importScripts = []string{
	// TSPEND
	"c376a914d17043c104a57393aa7353e1510e39eab811e3db88ac",
	// 2 of 2 multisig
	"522103d484eb60ad03549e731ae9045281f8ee14ff6ea11b697f32cde3d8a18992261b210342b0b9c0ecb53cb9761beb0d010bbf08b5049d2a4d3bea5d3a1d95eb664931cb52ae",
	// NonStandard
	"01",
}

var validateAddrs = []string{
	"TcpEWwGdCN3RCNAQUhBn8f2Xdko2JzcQSQs",

	"TkKkYvSrnu8orwhtedcJGkD7guarvZUbUAtjr4iKqD9Y8pNEf8iHu", // PubKeyEcdsaSecp256k1V0
	"Tsp18L8qTcjzigYXrD5GSdwDmhVYBpKmfUL",                   // PubKeyHashEcdsaSecp256k1V0
	"TkKnVfd6EvzEYAqiELWstkASHgVyYH8JK3gNvAxUX79C9CrnsV8W6", // PubKeyEd25519V0
	"TedZCnJ5uQ8z7VzKqdBhP1WP2RBYaaoCiUe",                   // PubKeyHashEd25519V0
	"TkKpSQoKgxqfDPyXp3RTWk7ktTR69zn19vU1zHCdD18r9bMTvDKT3", // PubKeySchnorrSecp256k1V0
	"TSs3jHQMbbZGPyftUh4cgaALzgDZhfXGtxn",                   // PubKeyHashSchnorrSecp256k1V0
	"TcvVou7ooM4rJRWNeJYwehJ9fQq1HTc5pbK",                   // ScriptHashV0

	// These are owned imported scripts.
	"TcfdqCrK2fiFJBZnGj5N6xs6rMsbQBsJBYf", // TSPEND
	"TcrzaAVMbFURm1PpukWru8yE2uBTjvQePoa", // 2 of 2 multisig
	"TckSpBht36nMZgnDDjv7xaHUrgCyJpxQiLA", // NonStandard
}
