-- 旧版本的管理员因子重置保留历史口令，setup_pending 不能证明该口令属于
-- 当前设置操作。升级时统一要求未完成设置者重新设置口令；已登记账户和
-- 经口令+恢复码开始的自助恢复（reset_pending）保留口令。
DELETE FROM identity_passwords
WHERE principal_id IN (
    SELECT principal_id FROM identity_human_accounts WHERE factor_state = 'setup_pending'
);
