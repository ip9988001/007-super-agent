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
    role                TEXT    NOT NULL DEFAULT '',  -- 规划 v2：front_plan / front_judge / executor
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

-- inbox_queue 是网关的持久化入队队列表（第 14 步 B-02）：收信先落盘
-- 再回执，进程崩溃也不丢任务；启动时重放 PENDING/RUNNING 行。
-- status：PENDING 待处理 / RUNNING 已取走未确认 / DONE 已确认 / FAILED 已失败
-- / REJECTED 前置校验拒绝（线 A）/ TIMEOUT 看门狗超时置位（线 A）。
-- deadline：RUNNING 行的终结期限（RFC3339 UTC，线 A 看门狗用；
-- 为空表示旧行，用 updated_ts + 1800 秒兜底）。
-- (task_id, inbound_seq) 只在 seq>0 时唯一（发信方给了序号才防重放；
-- seq=0 表示发信方没给序号，每条都是独立投递，不做去重）。
CREATE TABLE IF NOT EXISTS inbox_queue (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          TEXT    NOT NULL,  -- RFC3339 UTC，入队时间
    session_id  TEXT    NOT NULL,
    task_id     TEXT    NOT NULL,
    inbound_seq INTEGER NOT NULL DEFAULT 0,
    source      TEXT    NOT NULL DEFAULT '',
    text        TEXT    NOT NULL,
    status      TEXT    NOT NULL,  -- PENDING / RUNNING / DONE / FAILED / REJECTED / TIMEOUT
    retry_count INTEGER NOT NULL DEFAULT 0,
    deadline    TEXT    NOT NULL DEFAULT '',
    created_ts  TEXT    NOT NULL,
    updated_ts  TEXT    NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_inbox_dedup ON inbox_queue(task_id, inbound_seq) WHERE inbound_seq > 0;
CREATE INDEX IF NOT EXISTS idx_inbox_status ON inbox_queue(status, updated_ts);
CREATE INDEX IF NOT EXISTS idx_inbox_deadline ON inbox_queue(status, deadline);

-- dispatch_events 是派单预算事件表（线 A）：软告警 / 分级裁减 / 超限拒绝 / 看门狗超时。
-- 只记事件摘要（任务号、派单号、种类、派单估算 token 数、人话说明），不记派单全文、不记密钥。
-- dispatch_est_tokens 是 EstimateDispatchTokens 保守口径的估算值，刻意与账本
-- 费用 token 数区分开（P0 修复：旧列名 tokens 已由 Open 迁移改名）。
CREATE TABLE IF NOT EXISTS dispatch_events (
    id          INTEGER PRIMARY KEY AUTOINCREMENT,
    ts          TEXT    NOT NULL,  -- RFC3339 UTC
    session_id  TEXT    NOT NULL,
    task_id     TEXT    NOT NULL,
    dispatch_id TEXT    NOT NULL DEFAULT '',
    kind        TEXT    NOT NULL,  -- soft_alert / trimmed / overflow_rejected / watchdog_timeout
    dispatch_est_tokens INTEGER NOT NULL DEFAULT 0,
    detail      TEXT    NOT NULL DEFAULT ''  -- 人话摘要（截断 ≤600 字）
);

CREATE INDEX IF NOT EXISTS idx_dispatch_events_task ON dispatch_events(task_id);

-- selfevolve_proposals 是自进化闭环（第 20 步）的提议表：同一问题签名
-- 7 天内出现 3 次及以上才产生一条提议；status：pending 待人批 /
-- applied 已应用 / rejected 应用失败。只记提议内容与状态，不记任何密钥。
CREATE TABLE IF NOT EXISTS selfevolve_proposals (
    id             INTEGER PRIMARY KEY AUTOINCREMENT,
    proposal_id    TEXT    NOT NULL,  -- se-YYYY-MM-DD-NNN，全局唯一
    date           TEXT    NOT NULL,  -- 自查日期（北京时间 YYYY-MM-DD）
    signature      TEXT    NOT NULL,  -- 问题签名，如 exec_fail:cap-code-run-audit
    evidence_count INTEGER NOT NULL DEFAULT 0,  -- 7 天内该签名的出现次数
    proposal_text  TEXT    NOT NULL,  -- 提议内容（模型原文，截断 ≤2000 字）
    status         TEXT    NOT NULL DEFAULT 'pending',  -- pending / applied / rejected
    created_ts     TEXT    NOT NULL   -- RFC3339 UTC，创建时间
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_proposal_id ON selfevolve_proposals(proposal_id);
CREATE INDEX IF NOT EXISTS idx_proposal_sig ON selfevolve_proposals(signature, created_ts);

-- plan_runs 是多步骤规划器（第 21 步）的计划表：一次 PlanAndVerify
-- 记一行（规划器输出的步骤表原文、核验结论、修改轮数、预估费用）。
-- verdict：APPROVE / REVISE / REJECT；plan_id 全局唯一 pl-YYYY-MM-DD-NNN。
CREATE TABLE IF NOT EXISTS plan_runs (
    id                   INTEGER PRIMARY KEY AUTOINCREMENT,
    plan_id              TEXT    NOT NULL,  -- pl-YYYY-MM-DD-NNN，全局唯一
    task_id              TEXT    NOT NULL,
    task_text            TEXT    NOT NULL,  -- 任务原话（截断 ≤2000 字）
    steps                INTEGER NOT NULL DEFAULT 0,
    plan_json            TEXT    NOT NULL DEFAULT '',  -- 规划器输出的步骤表原文
    verdict              TEXT    NOT NULL DEFAULT '',  -- APPROVE / REVISE / REJECT
    revise_rounds        INTEGER NOT NULL DEFAULT 0,
    estimated_cost_milli INTEGER NOT NULL DEFAULT 0,  -- 预估费用（毫分，上限口径）
    created_ts           TEXT    NOT NULL   -- RFC3339 UTC，创建时间
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_plan_id ON plan_runs(plan_id);
CREATE INDEX IF NOT EXISTS idx_plan_task ON plan_runs(task_id);

CREATE INDEX IF NOT EXISTS idx_execution_task ON execution_ledger(task_id);
CREATE INDEX IF NOT EXISTS idx_expense_session ON expense_ledger(session_id);
CREATE INDEX IF NOT EXISTS idx_triage_task ON triage_metrics(task_id);

-- recall_log 是线 B 第一阶段的召回账本（v2.1 第七节）：每次召回记查询、
-- 候选、各路名次、最终卡片与耗时；写入前全部过脱敏中间件（见
-- ledger.RecordRecall），账本里绝不出现凭证原文。查询截断 ≤2000 字，
-- 候选串截断 ≤8000 字，名次与交付串各截断 ≤4000 字（按字符截断）。
-- 选 ledger.db 新表而不是独立文件：复用现有 ledger.Open 建表/迁移与
-- WAL + busy_timeout + 单连接串行写，不另起一套备份与并发规矩；
-- 评测 harness 与生产 memorySummary 共用 RecordRecall 写入。
CREATE TABLE IF NOT EXISTS recall_log (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    ts            TEXT    NOT NULL,  -- RFC3339 UTC，召回发生时间
    session_id    TEXT    NOT NULL DEFAULT '',
    task_id       TEXT    NOT NULL DEFAULT '',
    query         TEXT    NOT NULL,  -- 已脱敏的查询原文
    candidates    TEXT    NOT NULL DEFAULT '',  -- 前 40 候选：source=score 逗号串（已脱敏）
    lexical_rank  TEXT    NOT NULL DEFAULT '',  -- 词法路名次：source 逗号串（已脱敏）
    semantic_rank TEXT    NOT NULL DEFAULT '',  -- 语义路名次：source 逗号串（已脱敏）
    delivered     TEXT    NOT NULL DEFAULT '',  -- 最终交付卡片 source 逗号串（已脱敏）
    tokens_est    INTEGER NOT NULL DEFAULT 0,   -- 交付卡片合计 token 估算
    latency_ms    INTEGER NOT NULL DEFAULT 0,
    hits          INTEGER NOT NULL DEFAULT 0    -- 候选总数
);

CREATE INDEX IF NOT EXISTS idx_recall_task ON recall_log(task_id);
CREATE INDEX IF NOT EXISTS idx_recall_ts ON recall_log(ts);

-- patrol_log 是线 B 第二阶段的巡检账本（v2.1 §6）：每次巡检记时间、
-- 增量/全量、各计数与细节；写入前细节过脱敏中间件（见
-- ledger.RecordPatrol），账本里只有路径与计数，不含文件内容、
-- 不含凭证原文。细节截断 ≤8000 字（按字符截断）。
CREATE TABLE IF NOT EXISTS patrol_log (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    ts                 TEXT    NOT NULL,  -- RFC3339 UTC，巡检发生时间
    session_id         TEXT    NOT NULL DEFAULT '',
    task_id            TEXT    NOT NULL DEFAULT '',
    full               INTEGER NOT NULL DEFAULT 0,  -- 1=全量，0=增量
    scanned            INTEGER NOT NULL DEFAULT 0,  -- 扫过的源文件数
    new_count          INTEGER NOT NULL DEFAULT 0,  -- 新增未入库
    modified           INTEGER NOT NULL DEFAULT 0,  -- 源哈希变了
    renamed            INTEGER NOT NULL DEFAULT 0,  -- 改名继承
    deleted            INTEGER NOT NULL DEFAULT 0,  -- 源删同步删
    stale_marked       INTEGER NOT NULL DEFAULT 0,  -- 改标 stale
    pending_marked     INTEGER NOT NULL DEFAULT 0,  -- 改标 pending
    skipped_generating INTEGER NOT NULL DEFAULT 0,  -- 跳过的 generating
    secret_files       INTEGER NOT NULL DEFAULT 0,  -- 含凭证串的文件数
    secret_hits        INTEGER NOT NULL DEFAULT 0,  -- 凭证命中次数
    detail             TEXT    NOT NULL DEFAULT ''   -- 细节行（已脱敏）
);

CREATE INDEX IF NOT EXISTS idx_patrol_ts ON patrol_log(ts);
