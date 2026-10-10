package client

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func privateTestTLS(t *testing.T) (string, tls.Certificate) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	root, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), NotBefore: now.Add(-time.Minute), NotAfter: now.Add(time.Hour), BasicConstraintsValid: true, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "ca.pem")
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: root}), 0o600); err != nil {
		t.Fatal(err)
	}
	return path, tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestCAFileVerifiesAPITransferAndIP(t *testing.T) {
	ca, cert := privateTestTLS(t)
	var requests atomic.Int32
	s := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		if r.Header.Get("Authorization") != "Bearer synthetic-tls-session" {
			t.Error("missing session")
		}
		if strings.HasPrefix(r.URL.Path, "/xfer/") {
			_, _ = io.WriteString(w, "bytes")
			return
		}
		_, _ = io.WriteString(w, `{}`)
	}))
	s.TLS = &tls.Config{Certificates: []tls.Certificate{cert}}
	s.StartTLS()
	t.Cleanup(s.Close)
	c, err := New(Config{BaseURL: s.URL, CAFile: ca, SessionToken: "synthetic-tls-session"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(c.Close)
	if _, err := c.Do(t.Context(), "GET", "/api/v1/whoami", nil, Options{}); err != nil {
		t.Fatal(err)
	}
	r, err := c.transferRequest(t.Context(), "GET", "/xfer/test", nil, 0, nil)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(r.Body)
	r.Body.Close()
	if err != nil || string(body) != "bytes" || requests.Load() != 2 {
		t.Fatal("API/transfer failed", err)
	}
	otherCA, _ := privateTestTLS(t)
	for _, config := range []Config{
		{BaseURL: s.URL},
		{BaseURL: s.URL, CAFile: otherCA},
		{BaseURL: strings.Replace(s.URL, "127.0.0.1", "localhost", 1), CAFile: ca},
	} {
		config.SessionToken = "synthetic-tls-session"
		bad, err := New(config)
		if err != nil {
			t.Fatal(err)
		}
		_, err = bad.Do(context.Background(), "GET", "/api/v1/whoami", nil, Options{})
		bad.Close()
		if err == nil {
			t.Fatal("untrusted certificate or wrong IP accepted")
		}
	}
	if requests.Load() != 2 {
		t.Fatal("credential reached an unverified endpoint")
	}
}

func TestCAFileRejectsMalformedAndAmbiguousConfiguration(t *testing.T) {
	ca, _ := privateTestTLS(t)
	for _, contents := range []string{"", "not-pem", "-----BEGIN PRIVATE KEY-----\nAA==\n-----END PRIVATE KEY-----\n", strings.Repeat("x", (1<<20)+1)} {
		name := filepath.Join(t.TempDir(), "bad.pem")
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := New(Config{BaseURL: "https://127.0.0.1", CAFile: name}); err == nil {
			t.Fatal("invalid bundle accepted")
		}
	}
	for _, c := range []Config{
		{BaseURL: "https://127.0.0.1", CAFile: ca, TLSConfig: &tls.Config{}},
		{BaseURL: "http://127.0.0.1", AllowHTTP: true, CAFile: ca},
		{BaseURL: "https://127.0.0.1", CAFile: filepath.Join(t.TempDir(), "missing")},
		{BaseURL: "https://127.0.0.1", CAFile: t.TempDir()},
	} {
		if _, err := New(c); err == nil {
			t.Fatal("invalid CA configuration accepted")
		}
	}
}
