package client

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

type resourceRoundTrip func(*http.Request) (*http.Response, error)

func (f resourceRoundTrip) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

type repeatingJSON struct {
	remaining int64
	read      int64
}

func (r *repeatingJSON) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := int64(len(p))
	if n > r.remaining {
		n = r.remaining
	}
	for i := range p[:n] {
		p[i] = ' '
	}
	r.remaining -= n
	r.read += n
	return int(n), nil
}
func (*repeatingJSON) Close() error { return nil }

func TestReadResourceJSONBoundaries(t *testing.T) {
	c, err := New(Config{BaseURL: "http://127.0.0.1:1234", AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	id := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	base := "/api/v1/assets/" + id
	calls := 0
	c.api.Transport = resourceRoundTrip(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.Method != "GET" || r.Body != nil && r.Body != http.NoBody || r.Header.Get("Idempotency-Key") != "" {
			t.Fatal("not a plain read")
		}
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(`{}`)), Header: http.Header{}}, nil
	})
	for _, path := range []string{base + "?view=full", base + "/versions/" + id + "?view=full", base + "/versions/" + id + "/rights"} {
		if _, err = c.ReadResourceJSON(t.Context(), path); err != nil {
			t.Fatal(path, err)
		}
	}
	before := calls
	for _, path := range []string{base, base + "?view=full&view=full", base + "?view=%66ull", base + "?view=full#x", base + "/../" + id + "?view=full", strings.Replace(base, "assets/", "assets%2f", 1) + "?view=full", base + "/metadata?view=full", base + "/versions/bad?view=full", "https://elsewhere.test" + base + "?view=full", "//elsewhere.test" + base + "?view=full", base + "/versions/" + id + "/rights?", base + "/versions/" + id + "/rights?view=full"} {
		if _, err = c.ReadResourceJSON(t.Context(), path); err == nil {
			t.Fatal("unsafe path accepted", path)
		}
	}
	if calls != before {
		t.Fatal("invalid path sent network request")
	}
	for _, status := range []int{200, 403} {
		body := &repeatingJSON{remaining: MaxResourceJSONBytes + 100}
		c.api.Transport = resourceRoundTrip(func(r *http.Request) (*http.Response, error) {
			return &http.Response{StatusCode: status, Body: body, Header: http.Header{}}, nil
		})
		if _, err = c.ReadResourceJSON(t.Context(), base+"?view=full"); err == nil {
			t.Fatal("oversized response accepted")
		}
		limit := MaxResourceJSONBytes
		if status != 200 {
			limit = MaxJSONBytes
		}
		if body.read != limit+1 {
			t.Fatalf("unbounded read: %d, expected %d", body.read, limit+1)
		}
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	c.api.Transport = resourceRoundTrip(func(r *http.Request) (*http.Response, error) { return nil, r.Context().Err() })
	if _, err = c.ReadResourceJSON(ctx, base+"?view=full"); err == nil {
		t.Fatal("cancelled read succeeded")
	}
}

// A 16 MiB canonical JSON string can expand to almost 96 MiB on the wire.
// Generate the escaped wire form without constructing an unbounded fixture.
type escapedResourceJSON struct{ position, count int64 }

func (r *escapedResourceJSON) Read(p []byte) (int, error) {
	total := r.count*6 + 2
	if r.position == total {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > total-r.position {
		n = int(total - r.position)
	}
	for i := 0; i < n; i++ {
		pos := r.position + int64(i)
		if pos == 0 || pos == total-1 {
			p[i] = '"'
		} else {
			p[i] = `\u003c`[(pos-1)%6]
		}
	}
	r.position += int64(n)
	return n, nil
}
func (*escapedResourceJSON) Close() error { return nil }
func TestReadResourceJSONDefaultManifestExpansion(t *testing.T) {
	c, err := New(Config{BaseURL: "http://127.0.0.1:1234", AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.api.Transport = resourceRoundTrip(func(r *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: 200, Body: &escapedResourceJSON{count: (16 << 20) - 2}, Header: http.Header{}}, nil
	})
	result, err := c.ReadResourceJSON(t.Context(), "/api/v1/assets/01ARZ3NDEKTSV4RRFFQ69G5FAV/versions/01ARZ3NDEKTSV4RRFFQ69G5FAV?view=full")
	if err != nil || int64(len(result.Body)) != 6*((16<<20)-2)+2 {
		t.Fatal("default manifest expansion not covered", len(result.Body), err)
	}
}
