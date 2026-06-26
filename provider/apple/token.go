// SPDX-License-Identifier: MIT

package apple

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
)

// tokenBytes is the entropy of a generated K3S_TOKEN. 32 bytes = 256 bits, hex-encoded
// to a 64-char string. k3s accepts an arbitrary shared secret here.
const tokenBytes = 32

// generateToken returns a cryptographically random K3S_TOKEN. Used when the caller did
// not supply one in ClusterConfig.Token. The Talos sibling had no equivalent (Talos's
// cluster secret is baked into the generated machine config); k3s's join secret is a
// plain shared string, so the launcher owns it.
func generateToken() (string, error) {
	b := make([]byte, tokenBytes)
	if _, err := rand.Read(b); err != nil {
		return "", fmt.Errorf("generating K3S_TOKEN: %w", err)
	}

	return hex.EncodeToString(b), nil
}
