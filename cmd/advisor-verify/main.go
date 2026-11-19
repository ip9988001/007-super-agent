package main

import (
	"007-super-agent/internal/advisor"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"time"
)

func main() {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var p struct {
			Messages []struct{ Content string } `json:"messages"`
		}
		_ = json.NewDecoder(r.Body).Decode(&p)
		v, why := "NOTE", "干净：测试覆盖了改动的字段"
		for _, m := range p.Messages {
			if strings.Contains(m.Content, "does_not_assert_fixed_field") {
				v, why = "HALT", "假测试：改了 amount 校验，测试没断言该字段"
			}
		}
		fmt.Fprintf(w, `{"choices":[{"message":{"content":%q}}]}`,
			fmt.Sprintf(`{"verdict":%q,"reason":%q,"fix_directive":"给测试补上对该字段的断言"}`, v, why))
	}))
	defer srv.Close()
	cfg := advisor.AdvisorConfig{Endpoint: srv.URL, APIKey: "test-key", Model: "stub", Timeout: 10 * time.Second}
	fake := advisor.EvidenceBundle{TaskAndCriteria: "修 amount 校验", GitDiff: "does_not_assert_fixed_field"}
	d1, e1 := advisor.RunAdvisorCheck(context.Background(), cfg, fake, 0, "")
	fmt.Println("fake-test verdict:", d1.Verdict, "| block:", d1.ShouldBlock, "| err:", e1)
	clean := advisor.EvidenceBundle{TaskAndCriteria: "修 amount 校验", GitDiff: "test asserts amount field"}
	d2, e2 := advisor.RunAdvisorCheck(context.Background(), cfg, clean, 0, "")
	fmt.Println("clean verdict:", d2.Verdict, "| block:", d2.ShouldBlock, "| err:", e2)
}
