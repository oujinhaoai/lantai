package storage

import (
	"encoding/json"
	"errors"
	"mime"
	"net/http"
	"path"
	"strconv"
	"time"

	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/platform/sqlite"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

// 传输面路径（由服务端在上传会话与读取授权中给出，客户端不自行拼接）：
//
//	PUT  /xfer/v1/uploads/{upload_id}/files/{sha256}/parts/{part}   写入分片
//	GET  /xfer/v1/reads/{grant_id}?sig=…                            下载（支持 Range）
//	HEAD /xfer/v1/reads/{grant_id}?sig=…
//
// 分片请求必须带 Content-Length 与 Lantai-Part-Sha256（本分片字节的 SHA-256）。
// 每个请求都要带本人的会话凭据；创建上传会话、完成文件与签发读取授权属于
// 接口面（T07 接线）。
const (
	UploadPathPrefix = "/xfer/v1/uploads/"
	PartDigestHeader = "Lantai-Part-Sha256"
)

// PartsURL 返回上传会话写入分片的相对地址前缀：PUT {PartsURL}{sha256}/parts/{n}。
func PartsURL(upload ids.ID) string { return UploadPathPrefix + string(upload) + "/files/" }

// Authenticator 从请求中认证调用者并返回可信上下文（由 identity/httpauth 的
// Guard 适配），与接口面使用同一套判定。
type Authenticator interface {
	Authenticate(r *http.Request) (authz.Context, error)
}

// AuthenticatorFunc 让普通函数实现 Authenticator。
type AuthenticatorFunc func(r *http.Request) (authz.Context, error)

// Authenticate 实现 Authenticator。
func (f AuthenticatorFunc) Authenticate(r *http.Request) (authz.Context, error) { return f(r) }

// TransferHandler 是传输面的 HTTP 处理器，由 transport（T07）挂到传输监听上。
// 它认证调用者、按可信档位排队准入并限速，然后调用存储服务；每个下载请求都
// 重新做完整的授权检查。
type TransferHandler struct {
	svc   *Service
	auth  Authenticator
	sched *transfer.Scheduler
	mux   *http.ServeMux
}

// NewTransferHandler 创建传输面处理器。
func NewTransferHandler(svc *Service, auth Authenticator, sched *transfer.Scheduler) *TransferHandler {
	h := &TransferHandler{svc: svc, auth: auth, sched: sched, mux: http.NewServeMux()}
	h.mux.HandleFunc("PUT "+UploadPathPrefix+"{upload}/files/{sha}/parts/{part}", h.putPart)
	h.mux.HandleFunc("GET "+ReadPathPrefix+"{grant}", h.read)
	h.mux.HandleFunc("HEAD "+ReadPathPrefix+"{grant}", h.read)
	return h
}

// ServeHTTP 实现 http.Handler。
func (h *TransferHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) { h.mux.ServeHTTP(w, r) }

func (h *TransferHandler) putPart(w http.ResponseWriter, r *http.Request) {
	who, err := h.auth.Authenticate(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	part, err := strconv.Atoi(r.PathValue("part"))
	if err != nil {
		writeError(w, r, invalid("part number must be an integer"))
		return
	}
	if r.ContentLength < 0 {
		writeError(w, r, invalid("Content-Length is required for part uploads"))
		return
	}
	tk, err := h.sched.Admit(r.Context(), who)
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer tk.Release()
	res, err := h.svc.PutPart(r.Context(), PartRequest{
		Who: who, UploadID: ids.ID(r.PathValue("upload")), SHA256: r.PathValue("sha"), PartNumber: part,
		PartSHA256: r.Header.Get(PartDigestHeader), Size: r.ContentLength, Body: tk.Reader(r.Context(), r.Body),
	})
	if err != nil {
		writeError(w, r, err)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(res)
}

func (h *TransferHandler) read(w http.ResponseWriter, r *http.Request) {
	who, err := h.auth.Authenticate(r)
	if err != nil {
		writeError(w, r, err)
		return
	}
	tk, err := h.sched.Admit(r.Context(), who)
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer tk.Release()
	handle, err := h.svc.OpenRead(r.Context(), who, ids.ID(r.PathValue("grant")), r.URL.Query().Get("sig"))
	if err != nil {
		writeError(w, r, err)
		return
	}
	defer handle.Close()
	hdr := w.Header()
	hdr.Set("Content-Type", "application/octet-stream")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("Cache-Control", "private, no-store")
	hdr.Set("ETag", `"sha256:`+handle.Grant.SHA256+`"`)
	hdr.Set("Content-Disposition", mime.FormatMediaType("attachment", map[string]string{"filename": path.Base(handle.Grant.Path)}))
	http.ServeContent(w, r, "", time.Time{}, tk.ReadSeeker(r.Context(), handle.File))
}

// writeError 以错误信封写出错误；存储锁竞争为可重试的 STORAGE_UNAVAILABLE，
// 其他非结构化错误一律为 INTERNAL，不带内部细节。
func writeError(w http.ResponseWriter, r *http.Request, err error) {
	var e *errcode.Error
	switch {
	case errors.Is(err, transfer.ErrInvalidCaller):
		e = errcode.New(errcode.AuthRequired, "")
	case errors.Is(err, r.Context().Err()) && r.Context().Err() != nil:
		// 客户端已断开或取消：状态码不会被看到，写一个可重试的维护外错误即可。
		e = errcode.New(errcode.StorageUnavailable, "the transfer was cancelled")
	default:
		e = sqlite.Structured(err)
	}
	errcode.Observe(r.Context(), e.Code, err)
	if e.RetryAfter > 0 {
		w.Header().Set("Retry-After", strconv.Itoa(int(max(1, e.RetryAfter/time.Second))))
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(e.HTTPStatus())
	json.NewEncoder(w).Encode(e.Envelope(r.Header.Get("X-Request-Id")))
}
