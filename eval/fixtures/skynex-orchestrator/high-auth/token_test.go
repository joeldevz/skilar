package highauth

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"testing"
)

func TestVerifyTokenRejectsNoneAlgorithm(t *testing.T) {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"none"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"admin"}`))
	if err := VerifyToken(header+"."+payload+".", []byte("secret")); err == nil {
		t.Fatal("alg=none token was accepted")
	}
}

func TestVerifyTokenRejectsTampering(t *testing.T) {
	valid := signedToken([]byte("secret"), `{"sub":"reader"}`)
	if err := VerifyToken(valid+"tampered", []byte("secret")); err == nil {
		t.Fatal("tampered token was accepted")
	}
}

func TestVerifyTokenAcceptsValidHS256(t *testing.T) {
	if err := VerifyToken(signedToken([]byte("secret"), `{"sub":"reader"}`), []byte("secret")); err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
}

func signedToken(secret []byte, payloadJSON string) string {
	header := base64.RawURLEncoding.EncodeToString([]byte(`{"alg":"HS256"}`))
	payload := base64.RawURLEncoding.EncodeToString([]byte(payloadJSON))
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(header + "." + payload))
	return header + "." + payload + "." + base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}
