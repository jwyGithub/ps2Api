// trial_refresh_test.go —— 账号卡片「刷新」按钮的组合动作（RefreshAccountQuotaWithTrial）
// 判定矩阵回归：额度未耗尽 → none；耗尽 + ext 可开 → started；耗尽 + base 活跃 →
// skipped-base-active；耗尽 + ext 已用过 → skipped-used。
package router

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ps2api/internal/provider"
	"ps2api/internal/store"
)

// stubTrialTransport 按请求 path 分发 stub 响应：ws/proxy 转发体的 path 字段区分
// /api/trials 查询与 /api/users/<uid>/operations 额度直查；POST /api/trials（开通）
// 返回标准 start trial 成功体。
func stubTrialTransport(t *testing.T, trialResp func(path string) string, quotaJSON string) *Router {
	t.Helper()
	r := newTestRouter(t)
	r.Provider.Client = &http.Client{Transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		var body struct {
			Method string `json:"method"`
			Path   string `json:"path"`
		}
		_ = json.NewDecoder(req.Body).Decode(&body)
		resp := "not found"
		if strings.Contains(body.Path, "/api/trials") {
			if body.Method == "POST" {
				resp = `{"result":"success","account_id":"a","team_id":"t","trial_id":"solo-7-days-extension-trial","trial_period_end":"2026-10-22 07:00:00"}`
			} else {
				resp = trialResp(body.Path)
			}
		} else {
			resp = quotaJSON
		}
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(resp)),
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Request:    req,
		}, nil
	})}
	return r
}

func trialStatusJSON(isAllowed bool, count int, start, end string) string {
	b, _ := json.Marshal(map[string]interface{}{
		"result":               "success",
		"id":                   "solo-7-days-extension-trial",
		"duration":             7,
		"accountTrialDetails":  map[string]interface{}{"isAllowed": isAllowed, "trialActivationCount": count, "trialStart": start, "trialEnd": end},
	})
	return string(b)
}

// 账号置为耗尽（exhausted），探测直查返回额度耗尽 usage。
func setExhausted(t *testing.T, r *Router, id int64) {
	t.Helper()
	if err := r.Store.SetAccountStatus(id, "exhausted", "test"); err != nil {
		t.Fatal(err)
	}
}

// billing operations 响应（fetchBillingOpsResult 主路径解析的形状）。
func billingOpsJSON(usage float64) string {
	b, _ := json.Marshal(map[string]interface{}{
		"operations": []map[string]interface{}{{
			"name": "ai_millicredits", "limit": 400000, "usage": usage,
			"overage": 0, "spillage": 0, "plan": "solo", "is_pooled": "0",
		}},
	})
	return string(b)
}

const exhaustedQuotaJSON = `{"operations":[{"name":"ai_millicredits","limit":400000,"usage":400000,"overage":0,"spillage":0,"plan":"solo","is_pooled":"0"}]}`

func TestRefreshWithTrialNotExhausted(t *testing.T) {
	// 额度充足 → 不判断试用（action=none），不向上游发 trial 查询。
	queried := false
	r := stubTrialTransport(t, func(path string) string {
		if strings.Contains(path, "extension_trial") {
			queried = true
		}
		return trialStatusJSON(true, 0, "", "")
	}, billingOpsJSON(100))
	acc, _ := r.Store.GetAccount(1)
	pr, action, err := r.RefreshAccountQuotaWithTrial(t.Context(), acc.ID)
	if err != nil || !pr.OK {
		t.Fatalf("probe failed: %v %+v", err, pr)
	}
	if action != "none" {
		t.Fatalf("healthy account must be action=none, got %s", action)
	}
	if queried {
		t.Fatal("trial status must not be queried for healthy account")
	}
}

func TestRefreshWithTrialStartsExtension(t *testing.T) {
	r := stubTrialTransport(t, func(path string) string {
		if strings.Contains(path, "extension_trial") {
			return trialStatusJSON(true, 0, "", "")
		}
		// base 槽位：未开过（空）
		return trialStatusJSON(true, 0, "", "")
	}, exhaustedQuotaJSON)
	setExhausted(t, r, 1)
	acc, _ := r.Store.GetAccount(1)
	_, action, err := r.RefreshAccountQuotaWithTrial(t.Context(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if action != "started" {
		t.Fatalf("exhausted + fresh ext slot must start trial, got %s", action)
	}
}

func TestRefreshWithTrialBaseActiveSkips(t *testing.T) {
	future := time.Now().Add(3 * 24 * time.Hour).Format(trialTimeLayout)
	r := stubTrialTransport(t, func(path string) string {
		if strings.Contains(path, "extension_trial") {
			return trialStatusJSON(true, 0, "", "")
		}
		// base：活跃期内
		return trialStatusJSON(true, 1, "2026-10-10 07:00:00", future)
	}, exhaustedQuotaJSON)
	setExhausted(t, r, 1)
	acc, _ := r.Store.GetAccount(1)
	_, action, err := r.RefreshAccountQuotaWithTrial(t.Context(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if action != "skipped-base-active" {
		t.Fatalf("base active must skip ext (would be truncated), got %s", action)
	}
}

func TestRefreshWithTrialAlreadyUsedSkips(t *testing.T) {
	r := stubTrialTransport(t, func(path string) string {
		// ext：已激活过
		return trialStatusJSON(false, 1, "2026-09-20 07:00:00", "2026-09-27 07:00:00")
	}, exhaustedQuotaJSON)
	setExhausted(t, r, 1)
	acc, _ := r.Store.GetAccount(1)
	_, action, err := r.RefreshAccountQuotaWithTrial(t.Context(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if action != "skipped-used" {
		t.Fatalf("used ext slot must skip, got %s", action)
	}
}

func TestRefreshWithTrialDisabledAccountSkips(t *testing.T) {
	// 禁用号不自动开 trial（恢复是人工动作）。
	r := stubTrialTransport(t, func(path string) string {
		return trialStatusJSON(true, 0, "", "")
	}, exhaustedQuotaJSON)
	acc, _ := r.Store.GetAccount(1)
	setExhausted(t, r, acc.ID)
	_ = r.Store.SetAccountEnabled(acc.ID, false)
	_, action, err := r.RefreshAccountQuotaWithTrial(t.Context(), acc.ID)
	if err != nil {
		t.Fatal(err)
	}
	if action != "none" {
		t.Fatalf("disabled account must not trigger trial, got %s", action)
	}
}

// 编译期保证 account 字段存在（防止重构踩空）。
var _ = store.Account{}
var _ = provider.Tokens{}
