package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/oujinhaoai/lantai/internal/apiv1"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/commit"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/contract/install"
	"github.com/oujinhaoai/lantai/internal/contract/rights"
	"github.com/oujinhaoai/lantai/internal/storage/transfer"
)

type readGateFunc func(context.Context, authz.Context, authz.Action, authz.Resource, func(context.Context, authz.Decision) error) error

func (f readGateFunc) BeginRead(ctx context.Context, who authz.Context, action authz.Action, res authz.Resource, open func(context.Context, authz.Decision) error) error {
	return f(ctx, who, action, res, open)
}

type useEvaluatorFunc func(context.Context, authz.Context, ids.PermanentRef, authz.Purpose) (rights.Decision, error)

func (f useEvaluatorFunc) EvaluateUse(ctx context.Context, who authz.Context, ref ids.PermanentRef, purpose authz.Purpose) (rights.Decision, error) {
	return f(ctx, who, ref, purpose)
}

func TestReadGrantRechecksRevocationAtFinalReadBoundary(t *testing.T) {
	for _, expire := range []bool{false, true} {
		t.Run(fmt.Sprint("expire=", expire), func(t *testing.T) {
			f := newFixture(t, testConfig(), nil)
			c, _ := f.readable()
			g, err := f.svc.IssueReadGrant(t.Context(), ReadRequest{Who: f.who, AssetID: c.AssetID, VersionID: c.VersionID, Path: "README.md", Purpose: authz.PurposeReference})
			if err != nil {
				t.Fatal(err)
			}
			gate := f.svc.reads
			f.svc.reads = readGateFunc(func(ctx context.Context, who authz.Context, action authz.Action, res authz.Resource, open func(context.Context, authz.Decision) error) error {
				if expire {
					f.clk.Advance(16 * time.Minute)
				} else if err := f.svc.RevokeReadGrant(ctx, who, g.GrantID); err != nil {
					return err
				}
				return gate.BeginRead(ctx, who, action, res, open)
			})
			h, err := f.svc.OpenRead(t.Context(), f.who, g.GrantID, strings.Split(g.URL, "?sig=")[1])
			if h != nil {
				h.Close()
				t.Fatal("grant changed while waiting for the final boundary but content was opened")
			}
			code := errcode.TokenRevoked
			if expire {
				code = errcode.TokenExpired
			}
			wantCode(t, err, code)
		})
	}
}

func TestSourceGrantsPreservePurposeAndCurrentAcceptance(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	c, _ := f.readable()
	u := f.createUpload(f.who, f.project)
	req := SourceGrantRequest{Who: f.who, UploadID: u.UploadID, Source: ids.PermanentRef{AssetID: c.AssetID, VersionID: c.VersionID}, Path: "README.md", Purpose: authz.PurposeReference}
	first, err := f.svc.GrantFromSource(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	again, err := f.svc.GrantFromSource(t.Context(), req)
	if err != nil || again.GrantID != first.GrantID {
		t.Fatalf("same purpose replay: %+v %v", again, err)
	}
	ir := install.Request{OperationID: u.OperationID, ProjectID: u.ProjectID, Files: []install.File{{Path: "README.md", SHA256: first.SHA256, Size: first.Size}}}
	if err := f.svc.VerifyGrantAcceptance(t.Context(), f.who, ir, authz.PurposeReference); err != nil {
		t.Fatal(err)
	}
	// 未记录 Uses 的版本也不能借参考用途授权接受生产用途的内容。
	wantCode(t, f.svc.VerifyGrantAcceptance(t.Context(), f.who, ir, authz.PurposeProduction), errcode.BlobGrantRequired)
	req.Purpose = authz.PurposeProduction
	second, err := f.svc.GrantFromSource(t.Context(), req)
	if err != nil || second.GrantID == first.GrantID || second.Purpose != req.Purpose {
		t.Fatalf("source grants conflated different purposes: %+v %+v %v", first, second, err)
	}
	if err := f.svc.VerifyGrantAcceptance(t.Context(), f.who, ir, authz.PurposeProduction); err != nil {
		t.Fatal(err)
	}
	f.rights.Restrict(c.VersionID, authz.PurposeProduction)
	wantCode(t, f.svc.VerifyGrantAcceptance(t.Context(), f.who, ir, authz.PurposeProduction), errcode.UseRestricted)
	if err := f.svc.VerifyGrantAcceptance(t.Context(), f.who, ir, authz.PurposeReference); err != nil {
		t.Fatalf("restriction for production must not be applied to reference use: %v", err)
	}
	f.rights.Clear(c.VersionID)
	other := f.agent(f.project)
	wantCode(t, f.svc.VerifyGrantAcceptance(t.Context(), other, ir, authz.PurposeProduction), errcode.BlobGrantRequired)
	// 授权已消费只免到期；来源限制收紧仍在最终接受时拒绝。
	if _, err := f.svc.db.ExecContext(t.Context(), `UPDATE storage_blob_grants SET consumed_at = 1, expires_at = 1 WHERE operation_id = ?`, u.OperationID); err != nil {
		t.Fatal(err)
	}
	f.rights.Restrict(c.VersionID, "")
	wantCode(t, f.svc.VerifyGrantAcceptance(t.Context(), f.who, ir, authz.PurposeProduction), errcode.UseRestricted)
	f.rights.Clear(c.VersionID)
	fresh := f.az.OpenSession(f.who.PrincipalID, time.Hour)
	if err := f.svc.VerifyGrantAcceptance(t.Context(), fresh, ir, authz.PurposeProduction); err != nil {
		t.Fatalf("same principal must be able to continue with a fresh session: %v", err)
	}
	f.az.Revoke(f.who.PrincipalID, f.project)
	wantCode(t, f.svc.VerifyGrantAcceptance(t.Context(), fresh, ir, authz.PurposeProduction), errcode.TokenRevoked)
}

func TestSourceGrantRejectsCloseDuringIssuance(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	c, _ := f.readable()
	u := f.createUpload(f.who, f.project)
	f.svc.rights = useEvaluatorFunc(func(ctx context.Context, who authz.Context, ref ids.PermanentRef, purpose authz.Purpose) (rights.Decision, error) {
		if err := f.svc.CancelUpload(t.Context(), f.who, u.UploadID); err != nil {
			return rights.Decision{}, err
		}
		return f.rights.EvaluateUse(ctx, who, ref, purpose)
	})
	_, err := f.svc.GrantFromSource(t.Context(), SourceGrantRequest{Who: f.who, UploadID: u.UploadID,
		Source: ids.PermanentRef{AssetID: c.AssetID, VersionID: c.VersionID}, Path: "README.md", Purpose: authz.PurposeReference})
	wantReason(t, err, errcode.InvalidStateTransition, "upload_closed")
	var count int
	if err := f.svc.db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM storage_blob_grants WHERE operation_id = ?`, u.OperationID).Scan(&count); err != nil || count != 0 {
		t.Fatalf("closed upload received a new grant: %d %v", count, err)
	}
}

// testAuth 把 "Bearer <session_id>" 当作会话令牌，按授权桩核对会话。
func (f *fixture) testAuth() Authenticator {
	return AuthenticatorFunc(func(r *http.Request) (authz.Context, error) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if !ok {
			return authz.Context{}, errcode.New(errcode.AuthRequired, "")
		}
		return f.az.VerifySession(r.Context(), ids.ID(tok))
	})
}

func (f *fixture) server(sched *transfer.Scheduler) *httptest.Server {
	f.t.Helper()
	if sched == nil {
		sched = transfer.New(transfer.Limits{})
	}
	srv := httptest.NewServer(NewTransferHandler(f.svc, f.testAuth(), sched))
	f.t.Cleanup(srv.Close)
	return srv
}

type reply struct {
	status int
	body   []byte
	header http.Header
}

func do(t *testing.T, method, url string, who *authz.Context, header map[string]string, body []byte) reply {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if who != nil {
		req.Header.Set("Authorization", "Bearer "+string(who.SessionID))
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if body == nil {
		req.ContentLength = 0
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return reply{status: resp.StatusCode, body: b, header: resp.Header}
}

func (r reply) code(t *testing.T) errcode.Code {
	t.Helper()
	var env errcode.Envelope
	if err := json.Unmarshal(r.body, &env); err != nil {
		t.Fatalf("status %d body %q is not an error envelope", r.status, r.body)
	}
	return env.Error.Code
}

// readable 提交一个两文件版本，返回提交结果与其中大文件的内容。
func (f *fixture) readable() (commit.Committed, []byte) {
	f.t.Helper()
	big := synthetic("download", 200_000)
	c := f.commitVersion(f.who, f.project, "", "reads/asset", "", map[string][]byte{"data/big.bin": big, "README.md": []byte("hi\n")})
	return c, big
}

func TestReadGrantDownloadAndRange(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	c, big := f.readable()
	srv := f.server(nil)
	g, err := f.svc.IssueReadGrant(t.Context(), ReadRequest{Who: f.who, AssetID: c.AssetID, VersionID: c.VersionID, Path: "data/big.bin", Purpose: authz.PurposeProduction})
	if err != nil {
		t.Fatal(err)
	}
	if g.SHA256 != shaOf(big) || g.Size != int64(len(big)) || !g.ExpiresAt.Equal(g.IssuedAt.Add(15*time.Minute)) || !strings.HasPrefix(g.URL, ReadPathPrefix) {
		t.Fatalf("grant = %+v", g)
	}
	full := do(t, "GET", srv.URL+g.URL, &f.who, nil, nil)
	if full.status != 200 || !bytes.Equal(full.body, big) || full.header.Get("Accept-Ranges") != "bytes" {
		t.Fatalf("full download: status %d, %d bytes", full.status, len(full.body))
	}
	part := do(t, "GET", srv.URL+g.URL, &f.who, map[string]string{"Range": "bytes=1000-1999"}, nil)
	if part.status != 206 || !bytes.Equal(part.body, big[1000:2000]) {
		t.Fatalf("range download: status %d, %d bytes", part.status, len(part.body))
	}
	resume := do(t, "GET", srv.URL+g.URL, &f.who, map[string]string{"Range": fmt.Sprintf("bytes=%d-", len(big)-10)}, nil)
	if resume.status != 206 || !bytes.Equal(resume.body, big[len(big)-10:]) {
		t.Fatalf("resume download: status %d", resume.status)
	}
	head := do(t, "HEAD", srv.URL+g.URL, &f.who, nil, nil)
	if head.status != 200 || head.header.Get("Content-Length") != fmt.Sprint(len(big)) || head.header.Get("ETag") != `"sha256:`+shaOf(big)+`"` {
		t.Fatalf("head: %d %v", head.status, head.header)
	}
	// NFD 形式的路径请求同一文件；不存在的路径 NOT_FOUND。
	if _, err := f.svc.IssueReadGrant(t.Context(), ReadRequest{Who: f.who, AssetID: c.AssetID, VersionID: c.VersionID, Path: "nope.bin", Purpose: authz.PurposeProduction}); errcode.CodeOf(err) != errcode.NotFound {
		t.Fatalf("missing path: %v", err)
	}
}

// 仅持 URL、别人的会话、同一主体的另一会话、篡改的签名都不能取件；
// 返回与不存在的授权一样的结果，不泄露存在性。
func TestReadGrantIsBoundToTheCallersSession(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	c, _ := f.readable()
	srv := f.server(nil)
	g, err := f.svc.IssueReadGrant(t.Context(), ReadRequest{Who: f.who, AssetID: c.AssetID, VersionID: c.VersionID, Path: "README.md", Purpose: authz.PurposeReference})
	if err != nil {
		t.Fatal(err)
	}
	other := f.agent(f.project) // 同项目、同样有读取权限的另一个主体
	sameOwnerOtherSession := f.az.OpenSession(f.who.PrincipalID, time.Hour)
	cases := map[string]struct {
		who  *authz.Context
		url  string
		want int
	}{
		"url only":             {nil, g.URL, 401},
		"forwarded to another": {&other, g.URL, 404},
		"another session":      {&sameOwnerOtherSession, g.URL, 404},
		"tampered signature":   {&f.who, g.URL[:len(g.URL)-2] + "xx", 404},
		"no signature":         {&f.who, strings.Split(g.URL, "?")[0], 404},
		"unknown grant":        {&f.who, ReadPathPrefix + string(ids.New()) + "?sig=x", 404},
	}
	for name, c := range cases {
		if r := do(t, "GET", srv.URL+c.url, c.who, nil, nil); r.status != c.want {
			t.Errorf("%s: status %d, want %d", name, r.status, c.want)
		}
	}
}

// 撤销后的新请求立即拒绝，不等签名到期；每个请求都重新核验。
func TestRevocationAppliesToTheNextRequest(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	c, big := f.readable()
	srv := f.server(nil)
	issue := func(who authz.Context) ReadGrant {
		t.Helper()
		g, err := f.svc.IssueReadGrant(t.Context(), ReadRequest{Who: who, AssetID: c.AssetID, VersionID: c.VersionID, Path: "data/big.bin", Purpose: authz.PurposeProduction})
		if err != nil {
			t.Fatal(err)
		}
		return g
	}
	g := issue(f.who)
	if r := do(t, "GET", srv.URL+g.URL, &f.who, map[string]string{"Range": "bytes=0-99"}, nil); r.status != 206 {
		t.Fatalf("before revocation: %d", r.status)
	}
	calls := f.rights.Calls()
	// 用途限制在签发后收紧：下一个 Range 请求即被拒绝。
	f.rights.Restrict(c.VersionID, authz.PurposeProduction)
	if r := do(t, "GET", srv.URL+g.URL, &f.who, map[string]string{"Range": "bytes=100-199"}, nil); r.status != 403 || r.code(t) != errcode.UseRestricted {
		t.Fatalf("after restriction: %d", r.status)
	}
	if f.rights.Calls() <= calls {
		t.Fatal("restrictions must be evaluated on every request")
	}
	f.rights.Pending(c.VersionID, authz.PurposeProduction)
	if r := do(t, "GET", srv.URL+g.URL, &f.who, nil, nil); r.status != 409 || r.code(t) != errcode.RightsPending {
		t.Fatalf("pending restriction: %d", r.status)
	}
	f.rights.Clear(c.VersionID)
	if r := do(t, "GET", srv.URL+g.URL, &f.who, nil, nil); r.status != 200 || len(r.body) != len(big) {
		t.Fatalf("after clearing: %d", r.status)
	}
	// 持有者撤销授权。
	if err := f.svc.RevokeReadGrant(t.Context(), f.who, ids.ID(strings.TrimPrefix(strings.Split(g.URL, "?")[0], ReadPathPrefix))); err != nil {
		t.Fatal(err)
	}
	if r := do(t, "GET", srv.URL+g.URL, &f.who, nil, nil); r.status != 401 || r.code(t) != errcode.TokenRevoked {
		t.Fatalf("revoked grant: %d", r.status)
	}
	// 角色被撤销：签名仍有效的授权也立即失效。
	g2 := issue(f.who)
	f.az.Revoke(f.who.PrincipalID, f.project)
	if r := do(t, "GET", srv.URL+g2.URL, &f.who, nil, nil); r.status < 400 {
		t.Fatalf("download after role revocation: %d", r.status)
	}
	// 到期。
	reader := f.agent(f.project)
	g3 := issue(reader)
	f.clk.Advance(16 * time.Minute)
	if r := do(t, "GET", srv.URL+g3.URL, &reader, nil, nil); r.status != 401 || r.code(t) != errcode.TokenExpired {
		t.Fatalf("expired grant: %d", r.status)
	}
	// 整馆从备份恢复（recovery_epoch 推进）：之前签发的授权与会话一律失效。
	survivor := f.agent(f.project)
	g4 := issue(survivor)
	f.az.Restore()
	if r := do(t, "GET", srv.URL+g4.URL, &survivor, nil, nil); r.status != 401 || r.code(t) != errcode.TokenRevoked {
		t.Fatalf("grant issued before a restore: %d", r.status)
	}
}

// 无权主体得到的回答与对象不存在时相同；未提交的版本不能签发读取授权。
func TestReadGrantDoesNotLeakExistence(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	c, _ := f.readable()
	stranger := f.agent(ids.New())
	for _, req := range []ReadRequest{
		{Who: stranger, AssetID: c.AssetID, VersionID: c.VersionID, Path: "README.md", Purpose: authz.PurposeReference},
		{Who: stranger, AssetID: ids.New(), VersionID: ids.New(), Path: "README.md", Purpose: authz.PurposeReference},
	} {
		_, err := f.svc.IssueReadGrant(t.Context(), req)
		if code := errcode.CodeOf(err); code != errcode.NotFound && code != errcode.Forbidden {
			t.Fatalf("stranger: %v", err)
		}
	}
	u := f.uploadAll(f.who, f.project, []byte("pending"))
	fs := files(map[string][]byte{"p.txt": []byte("pending")})
	p, _ := f.prepareInstall(f.who, u, f.project, "", "reads/pending", "", fs)
	_, err := f.svc.IssueReadGrant(t.Context(), ReadRequest{Who: f.who, AssetID: p.AssetID, VersionID: p.VersionID, Path: "p.txt", Purpose: authz.PurposeReference})
	wantCode(t, err, errcode.NotFound)
	// 读取授权不超过会话有效期。
	short := f.az.OpenSession(f.who.PrincipalID, 5*time.Minute)
	g, err := f.svc.IssueReadGrant(t.Context(), ReadRequest{Who: short, AssetID: c.AssetID, VersionID: c.VersionID, Path: "README.md", Purpose: authz.PurposeReference})
	if err != nil || !g.ExpiresAt.Equal(short.ExpiresAt.Truncate(time.Millisecond)) {
		t.Fatalf("grant must not outlive the session: %+v %v", g, err)
	}
}

func TestPartUploadOverHTTP(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	srv := f.server(nil)
	content := synthetic("http", 100_000) // 2 个分片
	u := f.createUpload(f.who, f.project, content)
	uf := u.Files[0]
	url := func(n int) string { return fmt.Sprintf("%s%s%s/parts/%d", srv.URL, u.PartsURL, uf.SHA256, n) }
	for n := 1; n <= uf.PartCount; n++ {
		b := partBytes(content, uf, n)
		r := do(t, "PUT", url(n), &f.who, map[string]string{PartDigestHeader: shaOf(b)}, b)
		if r.status != 200 {
			t.Fatalf("part %d: %d %s", n, r.status, r.body)
		}
		// 响应与 OpenAPI 契约生成的传输类型一致。
		var pr apiv1.PartResult
		dec := json.NewDecoder(bytes.NewReader(r.body))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&pr); err != nil || pr.PartNumber != n || pr.PartCount != uf.PartCount || pr.ReceivedParts != n {
			t.Fatalf("part response %s does not match the contract: %v", r.body, err)
		}
	}
	bad := partBytes(content, uf, 1)
	if r := do(t, "PUT", url(1), &f.who, map[string]string{PartDigestHeader: shaOf([]byte("other"))}, bad); r.status != 422 || r.code(t) != errcode.HashMismatch {
		t.Fatalf("conflicting part: %d", r.status)
	}
	if r := do(t, "PUT", url(1), nil, map[string]string{PartDigestHeader: shaOf(bad)}, bad); r.status != 401 {
		t.Fatalf("unauthenticated part: %d", r.status)
	}
	if _, err := f.svc.CompleteFile(t.Context(), f.who, u.UploadID, uf.SHA256); err != nil {
		t.Fatal(err)
	}
}

// 传输档位只取自可信会话：客户端在请求里自报的优先级不起作用。
func TestTransferClassIgnoresClientHints(t *testing.T) {
	f := newFixture(t, testConfig(), nil)
	c, _ := f.readable()
	sched := transfer.New(transfer.Limits{BatchSlots: 1, BatchPerPrincipal: 1, InteractiveSlots: 1})
	srv := f.server(sched)
	g, err := f.svc.IssueReadGrant(t.Context(), ReadRequest{Who: f.who, AssetID: c.AssetID, VersionID: c.VersionID, Path: "README.md", Purpose: authz.PurposeReference})
	if err != nil {
		t.Fatal(err)
	}
	hold, err := sched.Admit(t.Context(), f.agent(f.project)) // 占满批量档
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan int, 1)
	go func() {
		r := do(t, "GET", srv.URL+g.URL, &f.who, map[string]string{"Priority": "u=0", "X-Lantai-Transfer-Class": "interactive"}, nil)
		done <- r.status
	}()
	select {
	case st := <-done:
		t.Fatalf("an agent's request claiming interactive priority was admitted past a full batch pool (status %d)", st)
	case <-time.After(150 * time.Millisecond):
	}
	hold.Release()
	if st := <-done; st != 200 {
		t.Fatalf("after the batch slot was released: %d", st)
	}
	stats := sched.Stats()
	if stats.AdmittedInteract != 0 {
		t.Fatalf("stats = %+v", stats)
	}
}
