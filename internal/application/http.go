package application

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/oujinhaoai/lantai/internal/operations"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
	"github.com/oujinhaoai/lantai/internal/transport/httpapi"
)

type Listeners struct {
	API        string `json:"api"`
	Transfer   string `json:"transfer"`
	Operations string `json:"operations"`
	Merged     bool   `json:"merged"`
}

// HTTPServers 拥有监听与 HTTP 连接；先 Shutdown，再关闭 App 的库和数据根锁。
type HTTPServers struct {
	Addresses Listeners
	servers   []*http.Server
	errors    chan error
	wg        sync.WaitGroup
}

func validateAddress(addr string, loopback bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return errors.New("application: listener requires host:port")
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 0 || n > 65535 {
		return errors.New("application: invalid listener port")
	}
	// 运维面禁止可变 DNS、通配符与网卡地址，不依赖外围防火墙保证本机可达。
	if loopback {
		ip := net.ParseIP(host)
		if ip == nil || !ip.IsLoopback() {
			return errors.New("application: operations listener must use a numeric loopback address")
		}
	}
	return nil
}

// StartHTTP 先绑定所有内部监听，失败时全部释放。网关、TLS 与真实部署仍由
// T08 的部署方案负责；传输地址由领域层返回为同一外部 origin 的路径。
func (a *App) StartHTTP(ctx context.Context, cfg operations.Config) (*HTTPServers, error) {
	if !a.Ready() {
		return nil, errors.New("application: instance is not ready")
	}
	if err := validateAddress(cfg.Listen.API, false); err != nil {
		return nil, err
	}
	if !cfg.Listen.Merged {
		if err := validateAddress(cfg.Listen.Transfer, false); err != nil {
			return nil, err
		}
	}
	if err := validateAddress(cfg.Listen.Operations, true); err != nil {
		return nil, err
	}
	h, err := httpapi.New(httpapi.Deps{Identity: a.Identity, Catalog: a.Catalog, Storage: a.Storage, Query: a.Query, Operations: a}, httpapi.Config{InstanceID: a.Instance.InstanceID(), AllowedOrigins: cfg.HTTP.AllowedOrigins, MaxJSONBytes: cfg.HTTP.MaxJSONBytes, APITimeout: time.Duration(cfg.HTTP.APITimeoutSeconds) * time.Second})
	if err != nil {
		return nil, err
	}
	t := cfg.Transfer
	scheduler := transfer.New(transfer.Limits{InteractiveSlots: t.InteractiveSlots, BatchSlots: t.BatchSlots, BatchPerPrincipal: t.BatchPerPrincipal, BatchBytesPerSecond: t.BatchBytesPerSecond, BatchBytesPerSecondWhileInteractive: t.BatchBytesPerSecondWhileInteractive})
	xfer := h.Transfer(a.Storage, scheduler)
	r := &HTTPServers{errors: make(chan error, 3)}
	api := h.APIConfig(cfg.Listen.API)
	if cfg.Listen.Merged {
		api = httpapi.TransferConfig(cfg.Listen.API, h.Merged(xfer))
	}
	r.servers = append(r.servers, api)
	if !cfg.Listen.Merged {
		r.servers = append(r.servers, httpapi.TransferConfig(cfg.Listen.Transfer, xfer))
	}
	ops := http.NewServeMux()
	ops.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]bool{"live": a.Instance.Readiness().Live})
	})
	ops.HandleFunc("GET /readyz", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		ready := a.Ready()
		if !ready {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ready": ready, "transfer": scheduler.Stats()})
	})
	r.servers = append(r.servers, httpapi.APIConfig(cfg.Listen.Operations, ops))
	var listeners []net.Listener
	for _, srv := range r.servers {
		lc := net.ListenConfig{}
		ln, e := lc.Listen(ctx, "tcp", srv.Addr)
		if e != nil {
			for _, existing := range listeners {
				_ = existing.Close()
			}
			return nil, e
		}
		listeners = append(listeners, ln)
		// 不将 panic 堆栈或请求信息写入可能带签名 URL 的默认 HTTP 日志。
		srv.ErrorLog = log.New(io.Discard, "", 0)
		srv.BaseContext = func(net.Listener) context.Context { return ctx }
	}
	r.Addresses = Listeners{API: listeners[0].Addr().String(), Operations: listeners[len(listeners)-1].Addr().String(), Merged: cfg.Listen.Merged}
	if cfg.Listen.Merged {
		r.Addresses.Transfer = r.Addresses.API
	} else {
		r.Addresses.Transfer = listeners[1].Addr().String()
	}
	for idx, srv := range r.servers {
		r.wg.Add(1)
		go func(s *http.Server, ln net.Listener) {
			defer r.wg.Done()
			if e := s.Serve(ln); e != nil && !errors.Is(e, http.ErrServerClosed) {
				r.errors <- e
			}
		}(srv, listeners[idx])
	}
	return r, nil
}

func (r *HTTPServers) Wait(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case err := <-r.errors:
		return err
	}
}

func (r *HTTPServers) Shutdown(ctx context.Context) error {
	var wg sync.WaitGroup
	errs := make(chan error, len(r.servers))
	for _, srv := range r.servers {
		wg.Add(1)
		go func(s *http.Server) {
			defer wg.Done()
			if err := s.Shutdown(ctx); err != nil {
				errs <- err
				_ = s.Close()
			}
		}(srv)
	}
	wg.Wait()
	r.wg.Wait()
	close(errs)
	var all []error
	for err := range errs {
		all = append(all, err)
	}
	return errors.Join(all...)
}
