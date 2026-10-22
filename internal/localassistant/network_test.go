package localassistant

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/client"
	"github.com/oujinhaoai/lantai/internal/contract/canonjson"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

type tlsWorkset struct {
	t          *testing.T
	mu         sync.Mutex
	server     *httptest.Server
	instance   string
	entry      client.WorksetEntry
	content    map[string]any
	manifest   map[string]any
	data       []byte
	spec       client.InputFile
	requests   []string
	ranges     []string
	deny       bool
	shortOnce  bool
	onTransfer func()
	grantURL   string
}

func newTLSWorkset(t *testing.T) *tlsWorkset {
	f := &tlsWorkset{t: t, instance: string(ids.New()), data: []byte("synthetic content with a resumable prefix and complete digest\n")}
	f.entry = client.WorksetEntry{AssetID: string(ids.New()), VersionID: string(ids.New()), Destination: "scene"}
	f.spec = client.InputFile{Path: "selected.txt", Role: "primary", Size: int64(len(f.data)), SHA256: hash(f.data)}
	f.content = map[string]any{"asset_type": "doc", "files": []client.InputFile{f.spec}}
	raw := raw(t, f.content)
	canonical, err := canonjson.Canonicalize(raw)
	if err != nil {
		t.Fatal(err)
	}
	d := digest.Of(canonical)
	f.manifest = map[string]any{"version": map[string]any{"asset_id": f.entry.AssetID, "version_id": f.entry.VersionID, "manifest_digest": d}, "manifest": map[string]any{"instance_id": f.instance, "asset_id": f.entry.AssetID, "version_id": f.entry.VersionID, "manifest_digest": d, "content": f.content}}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	return f
}

func (f *tlsWorkset) serve(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, r.Method+" "+r.URL.Path)
	write := func(v any) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(v)
	}
	if r.Header.Get("Authorization") != "Bearer synthetic-private-session" {
		w.WriteHeader(http.StatusUnauthorized)
		write(errcode.New(errcode.AuthRequired, "synthetic rejected").Envelope("synthetic"))
		return
	}
	path := "/api/v1/assets/" + f.entry.AssetID + "/versions/" + f.entry.VersionID
	switch {
	case r.Method == "GET" && r.URL.Path == "/api/v1/meta":
		write(map[string]any{"api_version": "v1", "instance_id": f.instance})
	case r.Method == "GET" && r.URL.Path == path:
		if r.URL.Query().Get("view") != "full" {
			f.t.Error("manifest was not read as full fixed version")
		}
		write(f.manifest)
	case r.Method == "POST" && r.URL.Path == path+"/read-grants":
		if f.deny {
			w.WriteHeader(http.StatusForbidden)
			write(errcode.New(errcode.Forbidden, "synthetic revocation").Envelope("synthetic"))
			return
		}
		var in map[string]string
		if json.NewDecoder(r.Body).Decode(&in) != nil || in["path"] != f.spec.Path || in["purpose"] != "archive_review" {
			f.t.Error("read grant changed selected path or purpose")
		}
		url := "/xfer/v1/reads/" + string(ids.New())
		if f.grantURL != "" {
			url = f.grantURL
		}
		write(client.DownloadGrant{URL: url, SHA256: f.spec.SHA256, Size: f.spec.Size, Path: f.spec.Path})
	case r.Method == "GET" && strings.HasPrefix(r.URL.Path, "/xfer/v1/reads/"):
		if f.onTransfer != nil {
			f.onTransfer()
		}
		f.ranges = append(f.ranges, r.Header.Get("Range"))
		start := 0
		if r.Header.Get("Range") != "" {
			_, _ = fmt.Sscanf(r.Header.Get("Range"), "bytes=%d-", &start)
			w.Header().Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, len(f.data)-1, len(f.data)))
		}
		w.Header().Set("Content-Length", fmt.Sprint(len(f.data)-start))
		if start > 0 {
			w.WriteHeader(http.StatusPartialContent)
		}
		data := f.data[start:]
		if f.shortOnce {
			f.shortOnce = false
			data = data[:len(data)/2]
		}
		_, _ = w.Write(data)
	default:
		f.t.Errorf("unexpected synthetic request %s %s", r.Method, r.URL.Path)
		w.WriteHeader(http.StatusNotFound)
	}
}

func (f *tlsWorkset) job(t *testing.T, root string) (Job, string, Options) {
	job := Job{Schema: JobSchema, JobID: "synthetic-workset-pull", Action: "workset.pull", Origin: f.server.URL, Input: raw(t, client.WorksetInput{Schema: client.WorksetSchema, Entries: []client.WorksetEntry{f.entry}, Entrypoints: []string{"scene/selected.txt"}}), Directory: "working/fixed", Output: "reports/workset.json", Purpose: "archive_review"}
	path := writeJob(t, root, job)
	ca := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: f.server.Certificate().Raw})
	if err := client.WritePrivate(filepath.Join(root, "credentials/ca.pem"), ca); err != nil {
		t.Fatal(err)
	}
	if err := client.SaveState(filepath.Join(root, "credentials/session.json"), map[string]string{"schema": "lantai.client-session/v1", "origin": f.server.URL, "token": "synthetic-private-session"}); err != nil {
		t.Fatal(err)
	}
	inspection, err := InspectFile(path, root)
	if err != nil {
		t.Fatal(err)
	}
	return job, path, Options{Workspace: root, ApprovalDigest: inspection.ApprovalDigest, SessionFile: "credentials/session.json", CAFile: "credentials/ca.pem"}
}

func TestAssistantStrictHTTPSWorksetResumeAndRevocationUseOriginalClient(t *testing.T) {
	root, f := workspace(t), newTLSWorkset(t)
	job, path, opts := f.job(t, root)
	f.shortOnce = true
	first, err := RunFile(t.Context(), path, opts)
	if err == nil || first.Status == "completed" || first.ExitCode == 0 {
		t.Fatalf("short transfer reported complete: %+v %v", first, err)
	}
	if _, err := os.Stat(filepath.Join(root, "working/fixed/scene/selected.txt")); !os.IsNotExist(err) {
		t.Fatal("partial transfer was published")
	}
	second, err := RunFile(t.Context(), path, opts)
	if err != nil || second.Status != "completed" || second.ExitCode != 0 {
		t.Fatalf("explicit original job resume: %+v %v", second, err)
	}
	got, _ := os.ReadFile(filepath.Join(root, "working/fixed/scene/selected.txt"))
	if string(got) != string(f.data) || second.ApprovalDigest != first.ApprovalDigest {
		t.Fatal("fixed working copy or original job binding changed")
	}
	f.mu.Lock()
	if len(f.ranges) != 2 || f.ranges[1] == "" {
		t.Errorf("original Range resume was not used: %v", f.ranges)
	}
	f.deny = true
	f.mu.Unlock()
	third, err := RunFile(t.Context(), path, opts)
	if err == nil || third.Status == "completed" || third.ExitCode == 0 || third.ErrorCode != "FORBIDDEN" {
		t.Fatal("existing local file bypassed current read permission")
	}
	var saved Result
	if client.LoadState(filepath.Join(root, job.Output), &saved) != nil || saved.Status == "completed" {
		t.Fatal("revocation retained stale successful result")
	}
	serialized := string(raw(t, third))
	if strings.Contains(serialized, "synthetic-private-session") || strings.Contains(serialized, "/xfer/") || strings.Contains(serialized, "Bearer ") {
		t.Fatal("result leaked a credential or short-lived read URL")
	}
}

func TestAssistantRejectsWrongOriginAndUntrustedTLSBeforeTransfer(t *testing.T) {
	for _, mode := range []string{"session-origin", "session-env", "missing-ca", "cross-origin-grant"} {
		t.Run(mode, func(t *testing.T) {
			root, f := workspace(t), newTLSWorkset(t)
			_, path, opts := f.job(t, root)
			switch mode {
			case "session-env":
				t.Setenv("LANTAI_SESSION_TOKEN", "synthetic-private-session")
				opts.SessionFile = ""
			case "session-origin":
				if err := client.SaveState(filepath.Join(root, "credentials/session.json"), map[string]string{"schema": "lantai.client-session/v1", "origin": "https://other.example", "token": "synthetic-private-session"}); err != nil {
					t.Fatal(err)
				}
			case "missing-ca":
				opts.CAFile = ""
			case "cross-origin-grant":
				f.grantURL = "https://other.example/xfer/v1/reads/synthetic"
			}
			out, err := RunFile(t.Context(), path, opts)
			if err == nil || out.Status == "completed" {
				t.Fatalf("unsafe network path succeeded: %+v %v", out, err)
			}
			f.mu.Lock()
			defer f.mu.Unlock()
			if len(f.ranges) != 0 || ((mode == "session-origin" || mode == "session-env") && len(f.requests) != 0) {
				t.Fatalf("rejected credentials/TLS issued transfers: requests=%v ranges=%v", f.requests, f.ranges)
			}
		})
	}
}

func TestAssistantStrictTLSChecksHostnameNotOnlyTrustedCertificate(t *testing.T) {
	root, f := workspace(t), newTLSWorkset(t)
	f.server.Close()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	cert := &x509.Certificate{SerialNumber: big.NewInt(42), Subject: pkix.Name{CommonName: "synthetic-invalid-host"}, DNSNames: []string{"localhost"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, cert, cert, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	f.server = httptest.NewUnstartedServer(http.HandlerFunc(f.serve))
	f.server.Config.ErrorLog = log.New(io.Discard, "", 0)
	f.server.TLS = &tls.Config{Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, MinVersion: tls.VersionTLS12}
	f.server.StartTLS()
	t.Cleanup(f.server.Close)
	_, path, opts := f.job(t, root)
	result, err := RunFile(t.Context(), path, opts)
	if err == nil || result.Status == "completed" {
		t.Fatal("trusted DNS-only certificate accepted for an IP origin")
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.requests) != 0 {
		t.Fatal("hostname failure reached authenticated HTTP")
	}
}

func TestAssistantDownloadCancellationPreservesPartialAndSource(t *testing.T) {
	root, f := workspace(t), newTLSWorkset(t)
	_, path, opts := f.job(t, root)
	ctx, cancel := context.WithCancel(t.Context())
	f.onTransfer = cancel
	out, err := RunFile(ctx, path, opts)
	if err == nil || out.Status != "cancelled" || out.ExitCode != 130 || out.SourceDeleted || out.AutomaticRetry {
		t.Fatalf("cancelled download result=%+v error=%v", out, err)
	}
	f.mu.Lock()
	f.onTransfer = nil
	f.mu.Unlock()
	resumed, err := RunFile(t.Context(), path, opts)
	if err != nil || resumed.Status != "completed" {
		t.Fatalf("cancelled job could not resume original working copy: %+v %v", resumed, err)
	}
}
