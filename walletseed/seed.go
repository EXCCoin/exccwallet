// Copyright (c) 2016 The Decred developers
// Copyright (c) 2018 The ExchangeCoin team
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package walletseed

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"

	"crypto/rand"
	"fmt"
	"github.com/EXCCoin/exccwallet/pgpwordlist"
	"strconv"
)

var (
	MinEntBytes       = uint(16)
	MaxEntBytes       = uint(32)
	RecommendedEntLen = uint(32)
	ErrInvalidEntLen  = fmt.Errorf("entropy length must be between %d and %d bits", MinEntBytes*8, MaxEntBytes*8)
)

// GenerateRandomEntropy returns a new seed created from a cryptographically-secure
// random source. If the seed size is unacceptable,
// ErrInvalidEntLen is returned.
func GenerateRandomEntropy(size uint) ([]byte, error) {
	// Per [BIP32], entropy must be in range [MinEntBytes, MaxEntBytes].
	if size < MinEntBytes || size > MaxEntBytes {
		return nil, ErrInvalidEntLen
	}

	buf := make([]byte, size)
	_, err := rand.Read(buf)
	if err != nil {
		return nil, err
	}

	return buf, nil
}

// ------------------------------------------------------------------------

func bytesToBits(bytes []byte) []byte {
	length := len(bytes)
	bits := make([]byte, length*8)
	for i := 0; i < length; i++ {
		b := bytes[i]
		for j := 0; j < 8; j++ {
			mask := byte(1 << uint8(j))
			bit := b & mask
			if bit == 0 {
				bits[(i*8)+8-(j+1)] = '0'
			} else {
				bits[(i*8)+8-(j+1)] = '1'
			}
		}
	}
	return bits
}

// CheckSummed returns a bit slice of entropy with an appended check sum
func CheckSummed(ent []byte) []byte {
	cs := CheckSum(ent)
	bits := bytesToBits(ent)
	return append(bits, cs...)
}

// CheckSum returns a slice of bits from the given entropy
func CheckSum(ent []byte) []byte {
	h := sha256.New()
	h.Write(ent) // nolint: errcheck
	cs := h.Sum(nil)
	hashBits := bytesToBits(cs)
	num := len(ent) * 8 / 32
	return hashBits[:num]
}

// EncodeMnemonicSlice encodes a entropy as a mnemonic word list.
func EncodeMnemonicSlice(ent []byte) ([]string, error) {
	const chunkSize = 11
	bits := CheckSummed(ent)
	length := len(bits)
	words := make([]string, length/11)
	for i := 0; i < length; i += chunkSize {
		stringVal := string(bits[i : chunkSize+i])
		intVal, err := strconv.ParseInt(stringVal, 2, 64)
		if err != nil {
			return nil, fmt.Errorf("could not convert %s to word index", stringVal)
		}
		word := pgpwordlist.WordList[intVal]
		words[(chunkSize+i)/11-1] = word
	}

	return words, nil
}

// EncodeMnemonic encodes a entropy as a mnemonic word list separated by spaces.
func EncodeMnemonic(ent []byte) (string, error) {
	words, err := EncodeMnemonicSlice(ent)
	if err != nil {
		return "", err
	}

	return strings.Join(words, " "), nil
}

// DecodeUserInput decodes a seed in either hexadecimal or mnemonic word list
// encoding back into its binary form.
func DecodeUserInput(input, password string) ([]byte, error) {
	input = strings.TrimSpace(input)

	var seed []byte
	var err error
	if strings.Contains(input, " ") {
		// Assume mnemonic
		seed, err = pgpwordlist.DecodeMnemonics(input, password)
	} else {
		// Assume hex
		seed, err = hex.DecodeString(input)
	}
	if err != nil {
		return nil, err
	}

	return seed, nil
}

// DecodeMnemonicSlice decodes a seed in mnemonic word list
// encoding back into its binary form.
func DecodeMnemonicSlice(input []string, password string) ([]byte, error) {
	return DecodeUserInput(strings.Join(input, " "), password)
}
