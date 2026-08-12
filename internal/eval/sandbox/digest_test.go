package sandbox

import (
	"crypto/sha256"
	"encoding/hex"
)

func digestBytes(value []byte) string {
	digest := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(digest[:])
}
