// Copyright (c) 2026 The EXCCoin developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package dcrd

import (
	"encoding/json"
	"testing"
)

func TestHashSlicesRejectMalformedJSON(t *testing.T) {
	tests := []struct {
		name   string
		target any
	}{
		{"hash pointers", new(hashes)},
		{"contiguous hashes", new(hashesContiguous)},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			err := json.Unmarshal([]byte(`{"not":"an array"}`), test.target)
			if err == nil {
				t.Fatal("malformed hash array decoded without error")
			}
		})
	}
}
