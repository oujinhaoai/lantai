package mcpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/oujinhaoai/lantai/internal/client"
)

func TestDownloadToolsRegisterPublicSchemas(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"api_version":"v1","capabilities":["read_grants","backup_management"]}`))
	}))
	defer srv.Close()
	c, err := client.New(client.Config{BaseURL: srv.URL, AllowHTTP: true})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = New(t.Context(), Config{Client: c}); err != nil {
		t.Fatal(err)
	}
}
