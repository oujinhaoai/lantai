package client

import (
	"bytes"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"io"
	"os"
)

// Trust is scoped to the two client transports. Hostname/IP verification remains
// enabled; malformed bundles never fall back to system roots.
func tlsConfigFromCAFile(name string) (*tls.Config, error) {
	f, err := os.Open(name)
	if err != nil {
		return nil, errors.New("client: CA bundle unavailable")
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, errors.New("client: CA bundle must be a regular file")
	}
	const maxBundle = 1 << 20
	data, err := io.ReadAll(io.LimitReader(f, maxBundle+1))
	if err != nil || len(data) > maxBundle {
		return nil, errors.New("client: CA bundle unreadable or too large")
	}
	pool := x509.NewCertPool()
	count := 0
	for len(bytes.TrimSpace(data)) != 0 {
		data = bytes.TrimSpace(data)
		if !bytes.HasPrefix(data, []byte("-----BEGIN CERTIFICATE-----")) {
			return nil, errors.New("client: CA bundle must contain only PEM CA certificates")
		}
		block, rest := pem.Decode(data)
		if block == nil || block.Type != "CERTIFICATE" || len(block.Headers) != 0 {
			return nil, errors.New("client: invalid CA certificate PEM")
		}
		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil || !cert.IsCA || !cert.BasicConstraintsValid {
			return nil, errors.New("client: invalid CA certificate")
		}
		pool.AddCert(cert)
		count++
		data = rest
	}
	if count == 0 {
		return nil, errors.New("client: empty CA bundle")
	}
	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}
