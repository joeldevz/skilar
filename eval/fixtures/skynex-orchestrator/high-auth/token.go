package highauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"strings"
)

var ErrInvalidToken = errors.New("invalid token")

type header struct {
	Algorithm string `json:"alg"`
}

// VerifyToken verifies a compact test token. The intentionally vulnerable
// baseline accepts alg=none; the evaluation task must remove that bypass while
// preserving HS256 verification.
func VerifyToken(raw string, secret []byte) error {
	parts := strings.Split(raw, ".")
	if len(parts) != 3 {
		return ErrInvalidToken
	}
	headerBytes, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return ErrInvalidToken
	}
	var parsed header
	if json.Unmarshal(headerBytes, &parsed) != nil {
		return ErrInvalidToken
	}
	if parsed.Algorithm == "none" {
		return nil
	}
	if parsed.Algorithm != "HS256" {
		return ErrInvalidToken
	}
	signature, err := base64.RawURLEncoding.DecodeString(parts[2])
	if err != nil {
		return ErrInvalidToken
	}
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(parts[0] + "." + parts[1]))
	if !hmac.Equal(signature, mac.Sum(nil)) {
		return ErrInvalidToken
	}
	return nil
}
