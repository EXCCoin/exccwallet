/*
 * Copyright (c) 2015-2016 The Decred developers
 * Copyright (c) 2018 The ExchangeCoin team
 *
 * Permission to use, copy, modify, and distribute this software for any
 * purpose with or without fee is hereby granted, provided that the above
 * copyright notice and this permission notice appear in all copies.
 *
 * THE SOFTWARE IS PROVIDED "AS IS" AND THE AUTHOR DISCLAIMS ALL WARRANTIES
 * WITH REGARD TO THIS SOFTWARE INCLUDING ALL IMPLIED WARRANTIES OF
 * MERCHANTABILITY AND FITNESS. IN NO EVENT SHALL THE AUTHOR BE LIABLE FOR
 * ANY SPECIAL, DIRECT, INDIRECT, OR CONSEQUENTIAL DAMAGES OR ANY DAMAGES
 * WHATSOEVER RESULTING FROM LOSS OF USE, DATA OR PROFITS, WHETHER IN AN
 * ACTION OF CONTRACT, NEGLIGENCE OR OTHER TORTIOUS ACTION, ARISING OUT OF
 * OR IN CONNECTION WITH THE USE OR PERFORMANCE OF THIS SOFTWARE.
 */

package pgpwordlist

import (
	"crypto/sha512"
	"golang.org/x/crypto/pbkdf2"
	"golang.org/x/text/unicode/norm"
	"strings"
)

// DecodeMnemonics returns the decoded seed that is encoded by words and password.  Any
// words that are whitespace are empty are skipped.
func DecodeMnemonics(mnemonic, password string) []byte {
	words := strings.Fields(strings.ToLower(norm.NFKD.String(mnemonic)))

	mnemonic = strings.Join(words, " ")
	salt := norm.NFKD.String("mnemonic" + password)
	return pbkdf2.Key([]byte(mnemonic), []byte(salt), 2048, 64, sha512.New)
}
