package web

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"encoding/binary"
	"math/big"
	"testing"
	"time"
)

func TestEncodeRdCleanPath_Empty(t *testing.T) {
	buf := encodeRdCleanPath(nil)
	if len(buf) != 4 {
		t.Fatalf("expected 4-byte header for empty chain, got %d", len(buf))
	}
	size := binary.BigEndian.Uint32(buf[0:4])
	if size != 0 {
		t.Errorf("expected payload size 0, got %d", size)
	}
}

func TestEncodeRdCleanPath_OneCert(t *testing.T) {
	cert := generateTestCert(t)

	buf := encodeRdCleanPath([]*x509.Certificate{cert})

	totalLen := binary.BigEndian.Uint32(buf[0:4])
	expectedTotal := uint32(4 + len(cert.Raw))
	if totalLen != expectedTotal {
		t.Errorf("total length = %d, want %d", totalLen, expectedTotal)
	}

	certLen := binary.BigEndian.Uint32(buf[4:8])
	if certLen != uint32(len(cert.Raw)) {
		t.Errorf("cert length = %d, want %d", certLen, len(cert.Raw))
	}

	if string(buf[8:]) != string(cert.Raw) {
		t.Error("cert DER data mismatch")
	}
}

func TestEncodeRdCleanPath_TwoCerts(t *testing.T) {
	cert1 := generateTestCert(t)
	cert2 := generateTestCert(t)

	buf := encodeRdCleanPath([]*x509.Certificate{cert1, cert2})

	totalLen := binary.BigEndian.Uint32(buf[0:4])
	expected := uint32(4 + len(cert1.Raw) + 4 + len(cert2.Raw))
	if totalLen != expected {
		t.Errorf("total length = %d, want %d", totalLen, expected)
	}

	cert1Len := binary.BigEndian.Uint32(buf[4:8])
	if cert1Len != uint32(len(cert1.Raw)) {
		t.Errorf("cert1 length = %d, want %d", cert1Len, len(cert1.Raw))
	}

	off := 8 + len(cert1.Raw)
	cert2Len := binary.BigEndian.Uint32(buf[off : off+4])
	if cert2Len != uint32(len(cert2.Raw)) {
		t.Errorf("cert2 length = %d, want %d", cert2Len, len(cert2.Raw))
	}
}

func generateTestCert(t *testing.T) *x509.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("GenerateKey: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Hour),
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("CreateCertificate: %v", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatalf("ParseCertificate: %v", err)
	}
	return cert
}
