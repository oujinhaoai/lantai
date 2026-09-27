package query

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/oujinhaoai/lantai/internal/catalog"
	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/authz"
	"github.com/oujinhaoai/lantai/internal/contract/digest"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
)

// Filter 固定一个可见范围。Text 是 NFC、不分大小写的字面子串匹配；不执行
// 用户提供的 SQL/FTS 表达式。M1 只检索著录，不把来源路径/声明混入全文。
type Filter struct {
	ProjectID ids.ID             `json:"project_id,omitempty"`
	Text      string             `json:"text,omitempty"`
	AssetType manifest.AssetType `json:"asset_type,omitempty"`
}

type SearchRequest struct {
	Who    authz.Context
	Filter Filter
	Limit  int
	Cursor string
}
type Page struct {
	Items      []Item `json:"items"`
	NextCursor string `json:"next_cursor,omitempty"`
	State      State  `json:"state"`
}
type cursorData struct {
	Generation int64
	HighWater  int64
	After      ids.ID
	Filter     digest.Digest
	Principal  ids.ID
	Session    ids.ID
}

func filterDigest(f Filter) digest.Digest { raw, _ := json.Marshal(f); return digest.Of(raw) }
func (s *Service) sealCursor(c cursorData) (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	nonce := make([]byte, s.cursor.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return "", err
	}
	sealed := s.cursor.Seal(nonce, nonce, raw, []byte("lantai.query.cursor/v1"))
	return base64.RawURLEncoding.EncodeToString(sealed), nil
}
func (s *Service) openCursor(raw string) (cursorData, error) {
	var c cursorData
	b, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || len(b) < s.cursor.NonceSize() {
		return c, expired("invalid query cursor; replace the visible scope")
	}
	n := s.cursor.NonceSize()
	plain, err := s.cursor.Open(nil, b[:n], b[n:], []byte("lantai.query.cursor/v1"))
	if err != nil {
		return c, expired("query cursor is no longer available")
	}
	if err = json.Unmarshal(plain, &c); err != nil {
		return c, expired("invalid query cursor")
	}
	return c, nil
}

func cleanFilter(f Filter) (Filter, error) {
	if f.ProjectID != "" && !f.ProjectID.Valid() {
		return f, errcode.New(errcode.SchemaInvalid, "invalid project filter")
	}
	if f.AssetType != "" && !f.AssetType.Valid() {
		return f, errcode.New(errcode.SchemaInvalid, "invalid asset type filter")
	}
	f.Text = strings.ToLower(norm.NFC.String(strings.TrimSpace(f.Text)))
	if len(f.Text) > 4096 {
		return f, errcode.New(errcode.SchemaInvalid, "query text is too long")
	}
	return f, nil
}

// visible 从权威登记得项目，再查当前身份和来源。索引中的项目/版本字段只
// 用于定位，不能代替最终检查。拒绝不返回对象存在性或隐藏来源的数量。
func (s *Service) visible(ctx context.Context, who authz.Context, asset, version ids.ID) (bool, error) {
	v, err := s.reader.Version(ctx, asset, version)
	if errcode.CodeOf(err) == errcode.NotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	d, err := s.authz.Authorize(ctx, who, catalog.ActionRead, authz.Resource{ProjectID: v.ProjectID, Kind: "asset", ID: asset})
	if err != nil {
		return false, err
	}
	if !d.Allowed {
		switch d.Code {
		case errcode.AuthRequired, errcode.TokenExpired, errcode.TokenRevoked:
			return false, d.Err()
		}
		return false, nil
	}
	a, err := s.catalog.ReadProjection(ctx, asset)
	if errcode.CodeOf(err) == errcode.NotFound {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if a.Description.Sensitivity == "personal" {
		p, err := s.authz.Authorize(ctx, who, authz.Action("personal.read"), authz.Resource{ProjectID: v.ProjectID, Kind: "asset", ID: asset})
		if err != nil {
			return false, err
		}
		if !p.Allowed {
			switch p.Code {
			case errcode.AuthRequired, errcode.TokenExpired, errcode.TokenRevoked:
				return false, p.Err()
			}
			return false, nil
		}
	}
	r, err := s.rights.EvaluateUse(ctx, who, v.Ref(s.instance), authz.PurposeArchiveReview)
	if err != nil {
		return false, err
	}
	return r.Allowed, nil
}

// Search 在 security_guard 共享锁内组装响应；已完成的撤权使新读取立即过滤。
// 游标用 AEAD 加密，避免把最后扫描到的无权对象 ID 暴露给调用者；绑定主体、
// 会话、查询范围、构建代次和水位。投影变化/进程重启后必须重新开始分页。
func (s *Service) Search(ctx context.Context, req SearchRequest) (Page, error) {
	f, err := cleanFilter(req.Filter)
	if err != nil {
		return Page{}, err
	}
	if req.Limit == 0 {
		req.Limit = 50
	}
	if req.Limit < 1 || req.Limit > 200 {
		return Page{}, errcode.New(errcode.SchemaInvalid, "page limit must be between 1 and 200")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lctx, h, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return Page{}, err
	}
	defer h.Release()
	st, err := s.State(lctx)
	if err != nil {
		return Page{}, err
	}
	if st.Generation == 0 {
		return Page{}, expired("index requires a full rebuild")
	}
	var after ids.ID
	if req.Cursor != "" {
		c, err := s.openCursor(req.Cursor)
		if err != nil {
			return Page{}, err
		}
		if c.Generation != st.Generation || c.HighWater != st.HighWater || c.Filter != filterDigest(f) || c.Principal != req.Who.PrincipalID || c.Session != req.Who.SessionID {
			return Page{}, expired("query cursor scope or projection has changed")
		}
		after = c.After
	}
	items, last, more, err := s.selectItems(lctx, req.Who, f, after, req.Limit)
	if err != nil {
		return Page{}, err
	}
	out := Page{Items: items, State: st}
	if more {
		out.NextCursor, err = s.sealCursor(cursorData{st.Generation, st.HighWater, last, filterDigest(f), req.Who.PrincipalID, req.Who.SessionID})
	}
	return out, err
}

func (s *Service) selectItems(ctx context.Context, who authz.Context, f Filter, after ids.ID, limit int) ([]Item, ids.ID, bool, error) {
	out := []Item{}
	// Scan bound limits the work per ordinary page even if all candidates are hidden.
	// Snapshot passes limit=0 and reads the complete replacement scope.
	scanned := 0
	for {
		rows, err := s.db.QueryContext(ctx, `SELECT asset_id,document_json FROM query_assets WHERE asset_id>? AND (?='' OR project_id=?) AND (?='' OR instr(search_text,?)>0) ORDER BY asset_id LIMIT 256`, after, f.ProjectID, f.ProjectID, f.Text, f.Text)
		if err != nil {
			return nil, after, false, err
		}
		var batch []Item
		for rows.Next() {
			var id ids.ID
			var raw []byte
			if err = rows.Scan(&id, &raw); err != nil {
				rows.Close()
				return nil, after, false, err
			}
			var i Item
			if err = json.Unmarshal(raw, &i); err != nil {
				rows.Close()
				return nil, after, false, err
			}
			batch = append(batch, i)
		}
		err = rows.Err()
		rows.Close()
		if err != nil {
			return nil, after, false, err
		}
		if len(batch) == 0 {
			return out, after, false, nil
		}
		for n, i := range batch {
			after = i.AssetID
			scanned++
			if f.AssetType == "" || f.AssetType == i.AssetType {
				ok, err := s.visible(ctx, who, i.AssetID, i.VersionID)
				if err != nil {
					return nil, after, false, err
				}
				if ok {
					out = append(out, i)
				}
			}
			if limit > 0 && (len(out) == limit || scanned == 1024) {
				return out, after, n+1 < len(batch) || len(batch) == 256, nil
			}
		}
	}
}

// Snapshot 是内部全量重同步模型，ReplaceScope 恒为 true：客户端必须先
// 删除该 Filter 范围的旧条目再安装 Items，不得做仅追加的合并。先捕获 S，
// 再消费至至少 S；返回同一代的完整当前授权视图，后续增量从 HighWater 开始。
type Snapshot struct {
	Items        []Item `json:"items"`
	Filter       Filter `json:"filter"`
	ReplaceScope bool   `json:"replace_scope"`
	Start        int64  `json:"start"`
	State        State  `json:"state"`
}

func (s *Service) Snapshot(ctx context.Context, who authz.Context, f Filter) (Snapshot, error) {
	f, err := cleanFilter(f)
	if err != nil {
		return Snapshot{}, err
	}
	start, err := s.events.HighWater(ctx)
	if err != nil {
		return Snapshot{}, err
	}
	if _, err = s.CatchUp(ctx); err != nil {
		return Snapshot{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lctx, h, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return Snapshot{}, err
	}
	defer h.Release()
	st, err := s.State(lctx)
	if err != nil {
		return Snapshot{}, err
	}
	if st.HighWater < start {
		return Snapshot{}, expired("projection has not reached snapshot start")
	}
	items, _, _, err := s.selectItems(lctx, who, f, "", 0)
	return Snapshot{Items: items, Filter: f, ReplaceScope: true, Start: start, State: st}, err
}

// Related 是当前可读取的关联。不返回隐藏目标的 ID、路径、计数或占位详情。
type Related struct {
	Source   ids.PermanentRef `json:"source"`
	Target   ids.PermanentRef `json:"target"`
	Relation string           `json:"relation"`
}

func (s *Service) Relations(ctx context.Context, who authz.Context, ref ids.PermanentRef, incoming bool) ([]Related, error) {
	if ref.InstanceID != s.instance {
		return nil, errcode.New(errcode.NotFound, "")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	lctx, h, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return nil, err
	}
	defer h.Release()
	ok, err := s.visible(lctx, who, ref.AssetID, ref.VersionID)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, errcode.New(errcode.NotFound, "")
	}
	query := `SELECT source_asset_id,source_version_id,target_instance_id,target_asset_id,target_version_id,relation FROM query_relations WHERE source_asset_id=? AND source_version_id=? ORDER BY target_asset_id,target_version_id,relation`
	if incoming {
		query = `SELECT source_asset_id,source_version_id,target_instance_id,target_asset_id,target_version_id,relation FROM query_relations WHERE target_asset_id=? AND target_version_id=? ORDER BY source_asset_id,source_version_id,relation`
	}
	rows, err := s.db.QueryContext(lctx, query, ref.AssetID, ref.VersionID)
	if err != nil {
		return nil, err
	}
	var candidates []Related
	for rows.Next() {
		r := Related{Source: ids.PermanentRef{InstanceID: s.instance}}
		if err = rows.Scan(&r.Source.AssetID, &r.Source.VersionID, &r.Target.InstanceID, &r.Target.AssetID, &r.Target.VersionID, &r.Relation); err != nil {
			rows.Close()
			return nil, err
		}
		candidates = append(candidates, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return nil, err
	}
	out := []Related{}
	for _, r := range candidates {
		if r.Target.InstanceID != s.instance {
			continue
		}
		a, err := s.visible(lctx, who, r.Source.AssetID, r.Source.VersionID)
		if err != nil {
			return nil, err
		}
		if !a {
			continue
		}
		b, err := s.visible(lctx, who, r.Target.AssetID, r.Target.VersionID)
		if err != nil {
			return nil, err
		}
		if b {
			out = append(out, r)
		}
	}
	return out, nil
}

// Change 只返回重新核对后的目录摘要，不转发原始事件 payload。
type Change struct {
	Item Item `json:"item"`
}
type Changes struct {
	Items          []Change `json:"items"`
	HighWater      int64    `json:"high_water"`
	ResyncRequired bool     `json:"resync_required"`
}

// Changes 是 M1 内部增量读取。权限/限制/生命周期事件要求重新替换可见范围，
// 因为旧客户端条目可能已不可见，不能靠“过滤掉撤权事件”来移除旧条目。
// M2 的 HTTP 事件查询和长轮询不由本实现开放。
func (s *Service) Changes(ctx context.Context, who authz.Context, after int64, limit int) (Changes, error) {
	if limit < 1 || limit > 1000 {
		return Changes{}, errcode.New(errcode.SchemaInvalid, "change limit must be between 1 and 1000")
	}
	page, err := s.events.Read(ctx, after, limit)
	if err != nil {
		return Changes{}, err
	}
	lctx, h, err := s.gate.Coordinator().Acquire(ctx, commands.Request{Security: commands.ModeShared})
	if err != nil {
		return Changes{}, err
	}
	defer h.Release()
	out := Changes{Items: []Change{}, HighWater: page.HighWater}
	seen := map[ids.ID]bool{}
	for _, e := range page.Entries {
		prefix, _, _ := strings.Cut(e.Envelope.EventType, ".")
		switch prefix {
		case "principal", "session", "project", "policy", "rights", "trash":
			out.ResyncRequired = true
		}
		asset, err := affected(e.Envelope)
		if err != nil {
			return Changes{}, err
		}
		if asset == "" || seen[asset] {
			continue
		}
		seen[asset] = true
		a, err := s.catalog.ReadProjection(lctx, asset)
		if errcode.CodeOf(err) == errcode.NotFound {
			out.ResyncRequired = true
			continue
		}
		if err != nil {
			return Changes{}, err
		}
		ok, err := s.visible(lctx, who, asset, a.Latest.VersionID)
		if err != nil {
			return Changes{}, err
		}
		if ok {
			out.Items = append(out.Items, Change{itemOf(a)})
		} else {
			// 不暴露被移除对象的 ID；要求调用者替换范围以清除旧条目。
			out.ResyncRequired = true
		}
	}
	return out, nil
}
