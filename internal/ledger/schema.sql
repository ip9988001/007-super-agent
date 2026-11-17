-- ledger.db 建表（设计方案 v1.5：五张表，全部由框架代码自动落盘）
-- 字段出处：triage_metrics / index_metrics / execution_ledger / expense_ledger /
-- queue_watermarks 五张表的用途见设计方案“可观测”一节；
-- execution_ledger 的命令/退出码/stdout 末尾 20 行/err_head_600 见第 7 轮证据包；
-- advisor_* 三字段见第 7 轮落位（NOTE ≤ 600 字）；水位未确认告警口径见第 4 轮
-- （seen>acked 且最久未确认 >1800 秒）。成本用整数毫分（0.001 元）存，杜绝浮点误差。

CREATE TABLE IF NOT EXISTS triage_metrics (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          TEXT    NOT NULL,  -- RFC3339 UTC，分诊发生时间
    session_id  TEXT    NOT NULL,
    task_id     TEXT    NOT NULL,
    route       TEXT    NOT NULL,  -- DIRECT / TOOL / META
    tier        INTEGER NOT NULL,  -- 0=规则直出，1=模型分诊
    tool_id     TEXT    NOT NULL DEFAULT '',
    tokens_in   INTEGER NOT NULL DEFAULT 0,
    tokens_out  INTEGER NOT NULL DEFAULT 0,
    latency_ms  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS index_metrics (
    id                    INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                    TEXT    NOT NULL,
    session_id            TEXT    NOT NULL,
    task_id               TEXT    NOT NULL,
    level                 TEXT    NOT NULL,  -- L1 / L2 / L3
    tool_id               TEXT    NOT NULL DEFAULT '',
    hit                   INTEGER NOT NULL DEFAULT 0,  -- 1=命中，0=未命中
    tokens_injected       INTEGER NOT NULL DEFAULT 0,
    tokens_saved_vs_full  INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS execution_ledger (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                  TEXT    NOT NULL,
    session_id          TEXT    NOT NULL,
    task_id             TEXT    NOT NULL,
    tool_id             TEXT    NOT NULL DEFAULT '',
    command             TEXT    NOT NULL,  -- 实际跑过的命令
    exit_code           INTEGER NOT NULL,
    stdout_tail         TEXT    NOT NULL DEFAULT '',  -- 标准输出末尾 20 行
    err_head            TEXT    NOT NULL DEFAULT '',  -- 错误头 600 字（按字符截断）
    duration_ms         INTEGER NOT NULL DEFAULT 0,
    status              TEXT    NOT NULL,  -- success / failure
    advisor_verdict     TEXT    NOT NULL DEFAULT 'NONE',  -- NONE / NOTE / CONCERN / HALT
    advisor_retry_count INTEGER NOT NULL DEFAULT 0,        -- 0..2
    advisor_note        TEXT    NOT NULL DEFAULT ''       -- 截断 ≤ 600 字（按字符截断）
);

CREATE TABLE IF NOT EXISTS expense_ledger (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                  TEXT    NOT NULL,
    session_id          TEXT    NOT NULL,
    task_id             TEXT    NOT NULL,
    model               TEXT    NOT NULL,
    prompt_tokens       INTEGER NOT NULL DEFAULT 0,
    cache_hit_tokens    INTEGER NOT NULL DEFAULT 0,
    cache_miss_tokens   INTEGER NOT NULL DEFAULT 0,
    completion_tokens   INTEGER NOT NULL DEFAULT 0,
    cost_cny_milli      INTEGER NOT NULL DEFAULT 0  -- 费用，单位 0.001 元
);

CREATE TABLE IF NOT EXISTS queue_watermarks (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                  TEXT    NOT NULL,
    box                 TEXT    NOT NULL,  -- 如 muse-link / pi-link
    seen_seq            INTEGER NOT NULL,
    acked_seq           INTEGER NOT NULL,
    oldest_unacked_age_s INTEGER NOT NULL DEFAULT 0,
    unacked_alert       INTEGER NOT NULL DEFAULT 0  -- 1=触发未确认告警
);

CREATE INDEX IF NOT EXISTS idx_execution_task ON execution_ledger(task_id);
CREATE INDEX IF NOT EXISTS idx_expense_session ON expense_ledger(session_id);
CREATE INDEX IF NOT EXISTS idx_triage_task ON triage_metrics(task_id);
