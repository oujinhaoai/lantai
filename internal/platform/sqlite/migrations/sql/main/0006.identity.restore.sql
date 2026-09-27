-- 恢复运行的凭据收敛属于 identity；run/epoch 防止重试重复吊销新凭据。
CREATE TABLE identity_restore_runs (
    run_id         TEXT PRIMARY KEY,
    recovery_epoch INTEGER NOT NULL UNIQUE CHECK (recovery_epoch >= 2),
    invalidated_at INTEGER NOT NULL
) STRICT;

-- 只记录离线重置已完成的身份/因子引用，不保存密码、TOTP 或恢复码。
-- 与新口令、新因子和离线重置审计事件在同一个 main 事务内写入。
CREATE TABLE identity_restore_admin_resets (
    run_id       TEXT NOT NULL,
    principal_id TEXT NOT NULL,
    auth_epoch   INTEGER NOT NULL CHECK (auth_epoch >= 1),
    factor_id    TEXT NOT NULL,
    reset_at     INTEGER NOT NULL,
    PRIMARY KEY (run_id, principal_id)
) STRICT;
