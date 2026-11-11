// stagecheck 用真实进程验证阶段 2（多 Gateway）、阶段 3（多 Manager）、阶段 4（热点分组）。
// 每个用例都按「找出问题」来写：恰好一次、没订阅的不能收到、人数必须精确、
// 杀进程之后其余进程必须活着、日志里不能有未恢复的 panic 和竞态报告。
//
//	go build -race -o stagecheck ./cmd/stagecheck
//	./stagecheck -stage all
package main

import (
	"flag"
	"fmt"
	"os"
	"strings"
	"time"
)

func main() {
	switch os.Getenv("SC_ROLE") {
	case "manager":
		runManager()
		return
	case "gateway":
		runGateway()
		return
	}
	stage := flag.String("stage", "all", "2, 3, 4, net, load, opt, cfg, mem, reconcile 或 all")
	gateways := flag.Int("gateways", 4, "阶段 2 的 Gateway 台数")
	big := flag.Int("big", 20, "阶段 2 Manager 故障恢复用的 Gateway 台数，0 表示跳过")
	flag.DurationVar(&reconcileWait, "reconcile-wait", 200*time.Second, "人数对不上时，最多等多久的定期对账")
	keep := flag.Bool("keep", false, "结束后保留各进程的目录和日志")
	flag.Parse()
	start := time.Now()
	run := func(name string, fn func(keep bool)) {
		if *stage == "all" || *stage == name {
			fmt.Printf("\n===== 阶段 %s =====\n", name)
			fn(*keep)
		}
	}
	run("2", func(k bool) { stage2(*gateways, k) })
	if *big > 0 {
		run("2", func(k bool) { stage2Big(*big, k) })
	}
	run("3", stage3)
	run("4", stage4)
	run("net", stageNet)
	run("load", stageLoad)
	run("opt", stageOptions)
	run("cfg", stageConfig)
	run("mem", stageMemory)
	run("mem", stageManagerMemory)
	run("reconcile", stageReconcile)

	fmt.Printf("\n===== 汇总 =====\n通过 %d，失败 %d，用时 %s\n", rep.passed, rep.failed, time.Since(start).Round(time.Second))
	if rep.failed > 0 {
		fmt.Println(strings.Join(rep.lines, "\n"))
		os.Exit(1)
	}
}

var reconcileWait time.Duration

func finish(c *cluster, id string, keep bool) {
	rep.check(id, "进程都活着，没有未恢复的 panic，没有竞态报告", c.health())
	c.stop()
	if keep || rep.failed > 0 {
		note("日志保留在 %s", c.root)
		return
	}
	_ = os.RemoveAll(c.root)
}

func must(id, what string, err error) bool {
	if err != nil {
		rep.check(id, what, err)
		return false
	}
	return true
}
