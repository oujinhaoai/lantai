package query

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"

	"golang.org/x/text/unicode/norm"

	"github.com/oujinhaoai/lantai/internal/catalog/manifest"
	"github.com/oujinhaoai/lantai/internal/commands"
	"github.com/oujinhaoai/lantai/internal/contract/errcode"
	"github.com/oujinhaoai/lantai/internal/contract/event"
	"github.com/oujinhaoai/lantai/internal/contract/ids"
	"github.com/oujinhaoai/lantai/internal/events"
)

type relation struct {
	SourceAsset   ids.ID
	SourceVersion ids.ID
	Use           manifest.Use
}
type projection struct {
	Item      Item
	Relations []relation
}
type revisionKey struct {
	Stream string
	ID     ids.ID
}

func searchText(i Item) string {
	return strings.ToLower(norm.NFC.String(strings.Join(append([]string{i.Slug, i.Title, i.Summary}, append(i.Tags, i.Subjects...)...), "\n")))
}

// scan 从权威版本登记枚举，不扫描未 committed 的磁盘目录。每个文件由 catalog
// 校验摘要；任何文件缺失/损坏都终止构建，不能发布一份悄悄少了对象的“成功”索引。
func (s *Service) scan(ctx context.Context) (map[ids.ID]projection, error) {
	out := map[ids.ID]projection{}
	var after ids.ID
	for {
		versions, err := s.reader.Versions(ctx, after, 256)
		if err != nil {
			return nil, err
		}
		if len(versions) == 0 {
			break
		}
		for _, v := range versions {
			if v.VersionID <= after {
				return nil, errors.New("query: authoritative version enumeration did not advance")
			}
			after = v.VersionID
			p, ok := out[v.AssetID]
			if !ok {
				a, err := s.catalog.ReadProjection(ctx, v.AssetID)
				if err != nil {
					return nil, err
				}
				p.Item = itemOf(a)
			}
			doc, err := s.catalog.ReadProjectionVersion(ctx, v.AssetID, v.VersionID)
			if err != nil {
				return nil, err
			}
			for _, use := range doc.Manifest.Content.Uses {
				p.Relations = append(p.Relations, relation{v.AssetID, v.VersionID, use})
			}
			out[v.AssetID] = p
		}
	}
	return out, nil
}

// refresh 重读整个资产；事件 payload 只是定位提示，绝不作为可见事实。M1 的
// Reader 只提供全库版本枚举，关联刷新因此是线性扫描；不建立第二份权威清单。
func (s *Service) refresh(ctx context.Context, asset ids.ID) (projection, error) {
	a, err := s.catalog.ReadProjection(ctx, asset)
	if err != nil {
		return projection{}, err
	}
	p := projection{Item: itemOf(a)}
	var after ids.ID
	for {
		versions, err := s.reader.Versions(ctx, after, 256)
		if err != nil {
			return projection{}, err
		}
		if len(versions) == 0 {
			break
		}
		for _, v := range versions {
			if v.VersionID <= after {
				return projection{}, errors.New("query: authoritative version enumeration did not advance")
			}
			after = v.VersionID
			if v.AssetID != asset {
				continue
			}
			doc, err := s.catalog.ReadProjectionVersion(ctx, v.AssetID, v.VersionID)
			if err != nil {
				return projection{}, err
			}
			for _, use := range doc.Manifest.Content.Uses {
				p.Relations = append(p.Relations, relation{v.AssetID, v.VersionID, use})
			}
		}
	}
	return p, nil
}

// affected 返回需要对账的资产；不同事件族的修订分别记录，因为项目登记与
// 说明修订并非同一修订序列，不能仅凭 aggregate_id/global_seq 推断业务顺序。
func affected(e event.Envelope) (ids.ID, error) {
	var p struct {
		AssetID    ids.ID `json:"asset_id"`
		TargetID   ids.ID `json:"target_id"`
		TargetKind string `json:"target_kind"`
	}
	relevant := e.AggregateType == "asset" || e.EventType == "version.committed" || (e.EventType == "ledger.metadata_committed" && e.AggregateType == "asset")
	if !relevant {
		return "", nil
	}
	if e.SchemaVersion != 1 {
		return "", errcode.New(errcode.SchemaInvalid, "unsupported projection event schema")
	}
	if err := json.Unmarshal(e.Payload, &p); err != nil {
		return "", errcode.Wrap(errcode.SchemaInvalid, "invalid projection event payload", err)
	}
	if e.AggregateType == "asset" {
		return e.AggregateID, nil
	}
	if !p.AssetID.Valid() {
		return "", errcode.New(errcode.SchemaInvalid, "projection event is missing asset_id")
	}
	return p.AssetID, nil
}

func streamKey(e event.Envelope) revisionKey {
	return revisionKey{e.EventType + ":" + e.AggregateType, e.AggregateID}
}

// readThrough 固定终点，避免持续有新事件时构建无法结束。事件缺失窗口必须
// 由 events 返回 CURSOR_EXPIRED；不能跳过丢失历史后声称快照已覆盖该水位。
func (s *Service) readThrough(ctx context.Context, after, through int64, apply func(events.Entry) error) error {
	for after < through {
		p, err := s.events.Read(ctx, after, 256)
		if err != nil {
			return err
		}
		if p.HighWater <= after {
			return errors.New("query: event scan did not advance")
		}
		for _, e := range p.Entries {
			if e.GlobalSeq > through {
				break
			}
			if err := apply(e); err != nil {
				return err
			}
		}
		after = p.HighWater
	}
	return nil
}

// Rebuild 先固定事件起点 S，再扫权威文件/台账，重放 (S,E] 后在 index 的一次
// 事务里替换全部投影与消费水位。读者要么看到上一代，要么看到完整新代。
// 文件扫描期间的新提交（含排在已扫 ID 前的预留版本）由重放补入。
func (s *Service) Rebuild(ctx context.Context) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.registerRetention(ctx); err != nil {
		return State{}, err
	}
	lctx, h, err := s.gate.Acquire(ctx, commands.Request{})
	if err != nil {
		return State{}, err
	}
	defer h.Release()
	start, err := s.events.HighWater(lctx)
	if err != nil {
		return State{}, err
	}
	all, err := s.scan(lctx)
	if err != nil {
		return State{}, err
	}
	end, err := s.events.HighWater(lctx)
	if err != nil {
		return State{}, err
	}
	revisions := map[revisionKey]int64{}
	var seen []events.Entry
	err = s.readThrough(lctx, start, end, func(e events.Entry) error {
		asset, err := affected(e.Envelope)
		if err != nil {
			return err
		}
		k := streamKey(e.Envelope)
		if revisions[k] < e.Envelope.AggregateRevision {
			if asset != "" {
				p, err := s.refresh(lctx, asset)
				if errcode.CodeOf(err) == errcode.NotFound {
					delete(all, asset)
				} else if err != nil {
					return err
				} else {
					all[asset] = p
				}
			}
			revisions[k] = e.Envelope.AggregateRevision
		}
		seen = append(seen, e)
		return nil
	})
	if err != nil {
		return State{}, err
	}
	tx, err := s.db.BeginTx(lctx, nil)
	if err != nil {
		return State{}, err
	}
	defer tx.Rollback()
	old, err := readState(lctx, tx)
	if err != nil {
		return State{}, err
	}
	for _, table := range []string{"query_assets", "query_relations", "query_revisions", "query_processed_events"} {
		if _, err = tx.ExecContext(lctx, `DELETE FROM `+table); err != nil {
			return State{}, err
		}
	}
	for _, p := range all {
		if err = putProjection(lctx, tx, p); err != nil {
			return State{}, err
		}
	}
	for k, r := range revisions {
		if err = putRevision(lctx, tx, k, r); err != nil {
			return State{}, err
		}
	}
	for _, e := range seen {
		if err = putSeen(lctx, tx, e); err != nil {
			return State{}, err
		}
	}
	st := State{Generation: old.Generation + 1, HighWater: end, RebuildStart: start}
	if _, err = tx.ExecContext(lctx, `UPDATE query_state SET generation=?,high_water=?,rebuild_start=? WHERE singleton=1`, st.Generation, st.HighWater, st.RebuildStart); err != nil {
		return State{}, err
	}
	if err = tx.Commit(); err != nil {
		return State{}, err
	}
	h.Release()
	return st, s.acknowledgeRetention(ctx, st)
}

// CatchUp 消费至调用时的事件高水位。投影、按事件 ID 去重、对象修订与消费
// 水位在 index 内同事务提交。重试可以重复读权威事实，但不会跳过未写入状态。
func (s *Service) CatchUp(ctx context.Context) (State, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.registerRetention(ctx); err != nil {
		return State{}, err
	}
	lctx, h, err := s.gate.Acquire(ctx, commands.Request{})
	if err != nil {
		return State{}, err
	}
	defer h.Release()
	st, err := s.State(lctx)
	if err != nil {
		return State{}, err
	}
	if st.Generation == 0 {
		return st, expired("index requires a full rebuild")
	}
	end, err := s.events.HighWater(lctx)
	if err != nil {
		return State{}, err
	}
	for st.HighWater < end {
		page, err := s.events.Read(lctx, st.HighWater, 256)
		if err != nil {
			return st, err
		}
		if page.HighWater <= st.HighWater {
			return st, errors.New("query: event scan did not advance")
		}
		tx, err := s.db.BeginTx(lctx, nil)
		if err != nil {
			return st, err
		}
		err = func() error {
			defer tx.Rollback()
			for _, e := range page.Entries {
				if e.GlobalSeq > end {
					break
				}
				var seen int
				if err := tx.QueryRowContext(lctx, `SELECT count(*) FROM query_processed_events WHERE event_id=?`, e.Envelope.EventID).Scan(&seen); err != nil {
					return err
				}
				if seen != 0 {
					continue
				}
				asset, err := affected(e.Envelope)
				if err != nil {
					return err
				}
				k := streamKey(e.Envelope)
				var rev int64
				err = tx.QueryRowContext(lctx, `SELECT revision FROM query_revisions WHERE aggregate_type=? AND aggregate_id=?`, k.Stream, k.ID).Scan(&rev)
				if err != nil && !errors.Is(err, sql.ErrNoRows) {
					return err
				}
				if rev < e.Envelope.AggregateRevision {
					if asset != "" {
						p, err := s.refresh(lctx, asset)
						if errcode.CodeOf(err) == errcode.NotFound {
							if err = deleteProjection(lctx, tx, asset); err != nil {
								return err
							}
						} else if err != nil {
							return err
						} else if err = putProjection(lctx, tx, p); err != nil {
							return err
						}
					}
					if err = putRevision(lctx, tx, k, e.Envelope.AggregateRevision); err != nil {
						return err
					}
				}
				if err = putSeen(lctx, tx, e); err != nil {
					return err
				}
			}
			next := min(page.HighWater, end)
			if _, err := tx.ExecContext(lctx, `UPDATE query_state SET high_water=? WHERE singleton=1`, next); err != nil {
				return err
			}
			return tx.Commit()
		}()
		if err != nil {
			return st, err
		}
		st.HighWater = min(page.HighWater, end)
	}
	h.Release()
	return st, s.acknowledgeRetention(ctx, st)
}

func putRevision(ctx context.Context, tx *sql.Tx, k revisionKey, r int64) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO query_revisions(aggregate_type,aggregate_id,revision) VALUES(?,?,?) ON CONFLICT(aggregate_type,aggregate_id) DO UPDATE SET revision=max(revision,excluded.revision)`, k.Stream, k.ID, r)
	return err
}
func putSeen(ctx context.Context, tx *sql.Tx, e events.Entry) error {
	_, err := tx.ExecContext(ctx, `INSERT INTO query_processed_events(event_id,global_seq) VALUES(?,?) ON CONFLICT(event_id) DO NOTHING`, e.Envelope.EventID, e.GlobalSeq)
	return err
}
func deleteProjection(ctx context.Context, tx *sql.Tx, asset ids.ID) error {
	if _, err := tx.ExecContext(ctx, `DELETE FROM query_relations WHERE source_asset_id=?`, asset); err != nil {
		return err
	}
	_, err := tx.ExecContext(ctx, `DELETE FROM query_assets WHERE asset_id=?`, asset)
	return err
}
func putProjection(ctx context.Context, tx *sql.Tx, p projection) error {
	raw, err := json.Marshal(p.Item)
	if err != nil {
		return err
	}
	if err = deleteProjection(ctx, tx, p.Item.AssetID); err != nil {
		return err
	}
	if _, err = tx.ExecContext(ctx, `INSERT INTO query_assets(asset_id,project_id,latest_version_id,document_json,search_text) VALUES(?,?,?,?,?)`, p.Item.AssetID, p.Item.ProjectID, p.Item.VersionID, raw, searchText(p.Item)); err != nil {
		return err
	}
	for _, r := range p.Relations {
		if _, err = tx.ExecContext(ctx, `INSERT INTO query_relations(source_asset_id,source_version_id,target_instance_id,target_asset_id,target_version_id,relation) VALUES(?,?,?,?,?,?)`, r.SourceAsset, r.SourceVersion, r.Use.InstanceID, r.Use.AssetID, r.Use.VersionID, r.Use.Relation); err != nil {
			return err
		}
	}
	return nil
}
