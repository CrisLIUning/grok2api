package app

import (
	"context"
	"testing"
	"time"
)

// 启动时的配额补偿必须等共享签名预热落定,否则会把第三方签名服务打垮,
// 顺带饿死同一时刻的真实用户请求。
//
// 2026-07-19 线上实录:
//
//	{"msg":"account_bulk_completed","operation":"web_quota_startup_catchup",
//	 "total":100,"submitted":100,"succeeded":26,"failed":74,
//	 "duration_ms":120249,"canceled":true,"pool_limit":25}
//	{"msg":"web_statsig_fetch_failed","path":"/rest/rate-limits",
//	 "error":"Statsig 签名失败: Post \"https://grok.wodf.de/sign\": context deadline exceeded"}
//	{"msg":"web_statsig_fetch_failed","path":"/rest/app-chat/conversations/new",
//	 "error":"Statsig 签名失败: 签名服务返回 502"}
//	→ POST /v1/images/generations  status 403  duration 106595ms
//
// 签名缓存的键是 (baseURL, signerURL, method, path),**不含账号** —— 100 个账号
// 本该共用一份签名,singleflight 也会合并并发。设计没错,错在时序:预热与配额
// 补偿是两个独立 goroutine,谁先跑没有保证。缓存又只在内存里,容器一重启就全空,
// 于是冷启动 + 补偿抢跑 = 100 次缓存未命中砸向同一个第三方服务。
//
// 这不是假想的竞态:今天为了发版重启了四次容器,每次都重演一遍,而图片生成在
// 04:00(没重启)是好的。
//
// 修法是让补偿等预热落定 —— "落定"包含失败(unavailable)与无账号(disabled),
// 不能只等成功,否则签名服务真挂了会把启动卡死。
func TestStartupState_QuotaCatchupWaitsForStatsigToSettle(t *testing.T) {
	state := newStartupState(0)

	// 预热尚未落定时,等待应当超时返回 false,而不是立刻放行。
	ctx := context.Background()
	if state.awaitStatsigSettled(ctx, 30*time.Millisecond) {
		t.Fatal("预热还没落定就放行了 —— 100 个配额刷新会直接砸向冷缓存")
	}

	// 预热成功 → 立即放行。
	state.setStatsig("warm", "共享签名已预热", 3)
	if !state.awaitStatsigSettled(ctx, time.Second) {
		t.Error("预热成功后应立即放行")
	}
}

// 预热失败也算落定 —— 否则第三方签名服务宕机会把配额补偿永久卡住,
// 而按需重试本来就是设计好的兜底。
func TestStartupState_FailedWarmupStillReleasesTheGate(t *testing.T) {
	for _, terminal := range []string{"unavailable", "disabled"} {
		state := newStartupState(0)
		state.setStatsig(terminal, "落定", 0)
		if !state.awaitStatsigSettled(context.Background(), time.Second) {
			t.Errorf("%q 也是落定状态,必须放行,否则签名服务宕机会卡死启动", terminal)
		}
	}
}

// "warming" 是中间态,不能当成落定。
func TestStartupState_WarmingIsNotSettled(t *testing.T) {
	state := newStartupState(0)
	state.setStatsig("warming", "正在预热共享签名", 0)
	if state.awaitStatsigSettled(context.Background(), 30*time.Millisecond) {
		t.Error("warming 是中间态,不该放行")
	}
}

// 上下文取消时不能干等到超时。
func TestStartupState_AwaitRespectsContext(t *testing.T) {
	state := newStartupState(0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	done := make(chan bool, 1)
	go func() { done <- state.awaitStatsigSettled(ctx, time.Minute) }()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("上下文已取消,等待却没有立刻返回")
	}
}
