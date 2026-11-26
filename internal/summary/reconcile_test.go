package summary

import (
	"strings"
	"testing"
)

// TestManifestReconcile：防丢核对（线 B 修复轮第 4 棒）。
// 第 1 批 20 篇只入库 14 篇、静默漏 6 篇：接入 CLI 与回填对
// pending/skipped 照常 exit 0，没有任何一道关卡核对总数。
// Reconcile 要求写了多少就入库多少，pending>0 就报错。
func TestManifestReconcile(t *testing.T) {
	ok := &IngestManifest{Total: 20, Accepted: 20, Pending: 0, Degraded: 0}
	if err := ok.Reconcile(); err != nil {
		t.Fatalf("20/20 should reconcile: %v", err)
	}
	okDegraded := &IngestManifest{Total: 20, Accepted: 17, Pending: 0, Degraded: 3}
	if err := okDegraded.Reconcile(); err != nil {
		t.Fatalf("17 ok + 3 degraded_ok should reconcile: %v", err)
	}
	// 第 1 批原状：20 篇写好，14 篇入库，6 篇 pending，必须报错。
	batch01 := &IngestManifest{Total: 20, Accepted: 14, Pending: 6, Degraded: 0}
	err := batch01.Reconcile()
	if err == nil {
		t.Fatalf("20/14/6 should NOT reconcile")
	}
	if !strings.Contains(err.Error(), "input=20") || !strings.Contains(err.Error(), "stored=14") || !strings.Contains(err.Error(), "pending=6") {
		t.Fatalf("reconcile error should carry counts, got: %v", err)
	}
	// 内部计数不一致也要报错。
	bad := &IngestManifest{Total: 20, Accepted: 14, Pending: 5, Degraded: 0}
	if err := bad.Reconcile(); err == nil {
		t.Fatalf("inconsistent counts should NOT reconcile")
	}
	if err := (*IngestManifest)(nil).Reconcile(); err == nil {
		t.Fatalf("nil manifest should NOT reconcile")
	}
}

// TestOrphanBatch01TriggerRewrite：第 1 批 6 篇孤儿的根因复现与修复。
// 根因：6 篇的触发词只有规则②一条不过（整词重合率 57%–86% > 50%），
// 其余规则①③④⑤⑥⓪全过；不是文件名映射、哈希校验或格式解析问题
// （20 篇文件全在、Parse 全过、source_hash 全算出；
// 用真实 OverlapRate 在原文件上重算，与 store2 manifest 的比率
// 逐篇一致）。6 篇触发词是照简介原文关键词抄的，
// 如 1303207ceb97 篇 7 个触发词里 6 个原样出现在简介里。
// 修复：只改触发词行（简介正文不动），换成口语/同义/症状说法，
// 使至少一半触发词不在简介里；旧表必须仍判失败、新表必须通过。
func TestOrphanBatch01TriggerRewrite(t *testing.T) {
	cases := []struct {
		rel     string
		summary string
		oldTrg  []string
		newTrg  []string
	}{
		{
			rel:     "l3/anti-patterns/bash-instead-of-recall-memory.md",
			summary: "反模式警示：不要把 recall_memory 当 bash 命令执行，它是 pi 内置工具函数而非终端命令，用 bash 调只会得到 command not found，查记忆应直接调用工具函数。",
			oldTrg:  []string{"recall_memory", "command not found", "bash调用", "工具函数", "记忆检索", "反模式", "工具映射表"},
			newTrg:  []string{"recall_memory", "查记忆怎么调", "command not found", "bash调用", "记忆检索", "工具映射表", "终端里执行"},
		},
		{
			rel:     "l3/cases/2026-06-21-fact-1303207ceb97.md",
			summary: "记录钢铁意志四档模式的命名与规模：A档零号约 6k、B档独行约 12k、C档阵斩约 29k、C档加进化加约 36k，配置档位或对名称时查。",
			oldTrg:  []string{"钢铁意志", "四档模式", "零号", "独行", "阵斩", "进化加", "档位命名"},
			newTrg:  []string{"钢铁意志", "四档模式", "档位命名", "模式名字对不上", "档位怎么分", "6k还是12k", "规模对照表"},
		},
		{
			rel:     "l3/patterns/2026-06-21-pattern-58e52fe17a9a.md",
			summary: "历史记忆重载的可复用流程：遍历 L1/L3 记忆文件，按 2000 字符切块后向量化存入 Vectra，重建或迁移记忆库时照此执行。",
			oldTrg:  []string{"记忆重载", "向量化", "Vectra", "2000字符切块", "遍历L1L3", "重建记忆库", "迁移记忆"},
			newTrg:  []string{"记忆重载", "转向量入库", "Vectra", "2000字符切块", "遍历L1L3", "重建记忆库", "记忆库搬家"},
		},
		{
			rel:     "l3/patterns/2026-06-22-pattern-da91345d95e6.md",
			summary: "Pi 启动批处理脚本的集成模式：后台拉起 L1 watcher、自动开浏览器访问 WebUI、再启动 Next.js 服务，做一键启动或改启动脚本时参考。",
			oldTrg:  []string{"启动脚本", "批处理", "L1 watcher", "自动开浏览器", "Next.js启动", "一键启动", "WebUI入口"},
			newTrg:  []string{"开机一键拉起", "bat脚本", "L1 watcher", "自动开浏览器", "Next.js启动", "一键开全套", "WebUI入口"},
		},
		{
			rel:     "wiki/entities/browseract-安装部署方案.md",
			summary: "BrowserAct 浏览器自动化 CLI 的安装部署方案，支持反爬、验证码、人类接力和多浏览器并发，推荐用 uv tool install 配 Python 3.12，适合替代 Playwright 做深度网页操控时参考。",
			oldTrg:  []string{"BrowserAct", "浏览器自动化", "反爬", "验证码自动解", "uv tool install", "Playwright替代", "人类接力"},
			newTrg:  []string{"BrowserAct", "浏览器操控", "过反爬验证", "验证码自动解", "uv tool install", "Playwright替代", "人工接管"},
		},
		{
			rel:     "wiki/synthesis/123ip-地址卡字段规范-美式顺序-与电话-城市名生成规则.md",
			summary: "规定 address.123ip.cc 地址卡 10 字段的美式顺序与复制按钮，电话按州区号加 555-01XX 示例段生成，城市名走中英双语策略，改地址卡页面或加城市译名时用。",
			oldTrg:  []string{"地址卡", "美式顺序", "555-01XX", "区号生成", "城市译名", "CITY_ZH", "address.123ip.cc", "复制按钮"},
			newTrg:  []string{"地址卡片", "美国式排序", "555-01XX", "区号生成", "城市译名", "CITY_ZH", "address.123ip.cc", "一键复制"},
		},
	}
	for _, c := range cases {
		oldRate := OverlapRate(c.summary, c.oldTrg)
		if oldRate <= 0.50 {
			t.Errorf("%s: old triggers should still fail rule 2, rate=%.0f%%", c.rel, oldRate*100)
		}
		newRate := OverlapRate(c.summary, c.newTrg)
		if newRate > 0.50 {
			t.Errorf("%s: new triggers should pass rule 2, rate=%.0f%%", c.rel, newRate*100)
		}
		if got := CheckOverlap(c.summary, c.newTrg); !got.Pass {
			t.Errorf("%s: CheckOverlap(new) should pass: %s", c.rel, got.Reason)
		}
		// 新触发词仍须 3–8 个、无禁用泛词。
		if n := len(c.newTrg); n < MinTriggers || n > MaxTriggers {
			t.Errorf("%s: new triggers count=%d, want 3-8", c.rel, n)
		}
		if got := CheckGenericWords(c.newTrg); !got.Pass {
			t.Errorf("%s: new triggers hit generic words: %s", c.rel, got.Reason)
		}
	}
}
