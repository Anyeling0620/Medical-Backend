// Command loadtest 是公开域读接口的轻量压测器（T4c 交付物之一）。
//
// 只用标准库实现，不需要 wrk/JMeter 等外部工具：后端与压测器同机、数据库跨公网，
// 因此口径必须写清楚（见 result/t4c-public-cache-benchmark.md）。
//
// 用法示例：
//
//	go run ./scripts/loadtest -base http://127.0.0.1:9080 -path /api/v1/public/departments -concurrency 32 -duration 20s
//
// 输出：RPS、成功/失败数、P50/P90/P95/P99/Max 延迟与状态码分布；同时打印一行 JSON 便于归档。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

func main() {
	base := flag.String("base", "http://127.0.0.1:9080", "被测服务地址（不含路径）")
	path := flag.String("path", "/api/v1/public/departments", "被测路径")
	concurrency := flag.Int("concurrency", 32, "并发数")
	duration := flag.Duration("duration", 20*time.Second, "测量阶段时长")
	warmup := flag.Duration("warmup", 5*time.Second, "预热阶段时长（不计入统计）")
	timeout := flag.Duration("timeout", 10*time.Second, "单请求超时")
	label := flag.String("label", "", "本次口径标签，仅用于输出")
	flag.Parse()

	if *concurrency < 1 {
		fmt.Fprintln(os.Stderr, "并发数必须大于 0")
		os.Exit(2)
	}
	url := *base + *path

	client := &http.Client{
		Timeout: *timeout,
		Transport: &http.Transport{
			MaxIdleConns:        *concurrency * 2,
			MaxIdleConnsPerHost: *concurrency * 2,
			MaxConnsPerHost:     *concurrency,
			IdleConnTimeout:     30 * time.Second,
		},
	}

	if *warmup > 0 {
		fmt.Printf("预热 %s（不计入统计）...\n", *warmup)
		run(context.Background(), client, url, *concurrency, *warmup, nil)
	}

	var counters requestCounters
	latencies := run(context.Background(), client, url, *concurrency, *duration, &counters)

	report := summarize(*label, url, *concurrency, *duration, latencies, counters.snapshot())
	printReport(report)
	encoded, err := json.Marshal(report)
	if err != nil {
		fmt.Fprintf(os.Stderr, "JSON 序列化失败: %v\n", err)
		return
	}
	fmt.Printf("\nJSON: %s\n", encoded)
}

// requestCounters 汇总压测期间的状态码与错误分类。
type requestCounters struct {
	total     atomic.Int64
	ok        atomic.Int64
	failed    atomic.Int64
	status2xx atomic.Int64
	status4xx atomic.Int64
	status5xx atomic.Int64
	transport atomic.Int64
}

func (c *requestCounters) snapshot() counterSnapshot {
	return counterSnapshot{
		Total:     c.total.Load(),
		OK:        c.ok.Load(),
		Failed:    c.failed.Load(),
		Status2xx: c.status2xx.Load(),
		Status4xx: c.status4xx.Load(),
		Status5xx: c.status5xx.Load(),
		Transport: c.transport.Load(),
	}
}

type counterSnapshot struct {
	Total     int64 `json:"total"`
	OK        int64 `json:"ok"`
	Failed    int64 `json:"failed"`
	Status2xx int64 `json:"status2xx"`
	Status4xx int64 `json:"status4xx"`
	Status5xx int64 `json:"status5xx"`
	Transport int64 `json:"transportErrors"`
}

// report 是一次压测的完整口径。
type report struct {
	Label       string          `json:"label,omitempty"`
	URL         string          `json:"url"`
	Concurrency int             `json:"concurrency"`
	DurationSec float64         `json:"durationSeconds"`
	RPS         float64         `json:"rps"`
	Counters    counterSnapshot `json:"counters"`
	P50Ms       float64         `json:"p50Ms"`
	P90Ms       float64         `json:"p90Ms"`
	P95Ms       float64         `json:"p95Ms"`
	P99Ms       float64         `json:"p99Ms"`
	MaxMs       float64         `json:"maxMs"`
}

// run 以固定并发持续发压：每轮测量单独收集延迟，预热轮次传 nil 丢弃样本。
func run(ctx context.Context, client *http.Client, url string, concurrency int, duration time.Duration, counters *requestCounters) []time.Duration {
	if duration <= 0 {
		return nil
	}
	deadline := time.Now().Add(duration)
	samples := make([][]time.Duration, concurrency)
	var wg sync.WaitGroup
	for worker := 0; worker < concurrency; worker++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			local := make([]time.Duration, 0, 1024)
			for time.Now().Before(deadline) {
				start := time.Now()
				status, err := request(ctx, client, url)
				elapsed := time.Since(start)
				if counters != nil {
					counters.total.Add(1)
					switch {
					case err != nil:
						counters.failed.Add(1)
						counters.transport.Add(1)
					case status >= 200 && status < 300:
						counters.ok.Add(1)
						counters.status2xx.Add(1)
					case status >= 400 && status < 500:
						counters.failed.Add(1)
						counters.status4xx.Add(1)
					default:
						counters.failed.Add(1)
						counters.status5xx.Add(1)
					}
				}
				if counters != nil {
					local = append(local, elapsed)
				}
			}
			samples[worker] = local
		}(worker)
	}
	wg.Wait()
	var merged []time.Duration
	for _, local := range samples {
		merged = append(merged, local...)
	}
	return merged
}

// request 发起一次 GET 并读空响应体，保证连接可复用。
func request(ctx context.Context, client *http.Client, url string) (int, error) {
	reqCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return resp.StatusCode, nil
}

// summarize 计算 RPS 与分位延迟。
func summarize(label, url string, concurrency int, duration time.Duration, latencies []time.Duration, counters counterSnapshot) report {
	sorted := make([]time.Duration, len(latencies))
	copy(sorted, latencies)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	seconds := duration.Seconds()
	result := report{
		Label:       label,
		URL:         url,
		Concurrency: concurrency,
		DurationSec: seconds,
		Counters:    counters,
	}
	if seconds > 0 {
		result.RPS = float64(counters.Total) / seconds
	}
	result.P50Ms = percentileMs(sorted, 0.50)
	result.P90Ms = percentileMs(sorted, 0.90)
	result.P95Ms = percentileMs(sorted, 0.95)
	result.P99Ms = percentileMs(sorted, 0.99)
	if len(sorted) > 0 {
		result.MaxMs = float64(sorted[len(sorted)-1].Microseconds()) / 1000
	}
	return result
}

// percentileMs 取最近秩分位（ceil(p*n)），n 为样本数，单位为毫秒。
func percentileMs(sorted []time.Duration, p float64) float64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted))*p+0.999999) - 1
	if index < 0 {
		index = 0
	}
	if index >= len(sorted) {
		index = len(sorted) - 1
	}
	return float64(sorted[index].Microseconds()) / 1000
}

// printReport 输出人类可读的口径与结果。
func printReport(r report) {
	fmt.Printf("\n===== 压测结果 =====\n")
	if r.Label != "" {
		fmt.Printf("口径标签    : %s\n", r.Label)
	}
	fmt.Printf("目标        : GET %s\n", r.URL)
	fmt.Printf("并发 / 时长 : %d / %.1fs\n", r.Concurrency, r.DurationSec)
	fmt.Printf("总请求      : %d（成功 %d，失败 %d）\n", r.Counters.Total, r.Counters.OK, r.Counters.Failed)
	fmt.Printf("状态码      : 2xx=%d 4xx=%d 5xx=%d 传输错误=%d\n",
		r.Counters.Status2xx, r.Counters.Status4xx, r.Counters.Status5xx, r.Counters.Transport)
	fmt.Printf("吞吐        : %.1f RPS\n", r.RPS)
	fmt.Printf("延迟        : P50=%.2fms P90=%.2fms P95=%.2fms P99=%.2fms Max=%.2fms\n",
		r.P50Ms, r.P90Ms, r.P95Ms, r.P99Ms, r.MaxMs)
}
