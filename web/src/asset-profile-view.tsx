import {
  profileSchema,
  profileStates,
  type ProfileDraft,
} from "./asset-profile";
export function ProfileFields({
  value,
  disabled,
  change,
}: {
  value: ProfileDraft;
  disabled: boolean;
  change: (key: keyof ProfileDraft, value: string) => void;
}) {
  return (
    <fieldset disabled={disabled} className="asset-editor-grid">
      <legend>结构化画像 · {profileSchema}</legend>
      <label>
        角色／人物标识（每行一项，最多64项／54字）
        <textarea
          value={value.characters}
          onChange={(e) => change("characters", e.target.value)}
        />
      </label>
      <label>
        来源（最多57字）
        <input
          maxLength={57}
          value={value.source}
          onChange={(e) => change("source", e.target.value)}
        />
      </label>
      <label>
        描述性业务状态
        <select
          value={value.business_state}
          onChange={(e) => change("business_state", e.target.value)}
        >
          {profileStates.map((state) => (
            <option key={state}>{state}</option>
          ))}
        </select>
      </label>
      <label>
        旧版状态原文（最多200字）
        <input
          maxLength={200}
          value={value.legacy_state}
          onChange={(e) => change("legacy_state", e.target.value)}
        />
      </label>
      <label>
        旧版版本标识（最多49字）
        <input
          maxLength={49}
          value={value.legacy_version}
          onChange={(e) => change("legacy_version", e.target.value)}
        />
      </label>
      <label>
        来源路径（每行一项，最多256项／2,048字）
        <textarea
          value={value.source_paths}
          onChange={(e) => change("source_paths", e.target.value)}
        />
        <small>只保存描述文字，不访问路径或 NAS。</small>
      </label>
      <label>
        备注（每行一项，最多64项／4,000字）
        <textarea
          value={value.notes}
          onChange={(e) => change("notes", e.target.value)}
        />
      </label>
      <p className="subtle">
        只更新当前著录的 extra.asset_profile，保留未知
        extra。角色、来源、状态、旧版版本同步为精确标签；人物追加到主题。active
        或 pending_review
        都不是人审、发布或用途许可。历史版本画像快照保持冻结。完整画像最多 1
        MiB。
      </p>
    </fieldset>
  );
}
