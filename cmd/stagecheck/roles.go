package main

import (
	"fmt"
	"net/http"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"time"

	"github.com/OSMeteor/firetower/service/gateway"
	"github.com/OSMeteor/firetower/service/manager"
	"github.com/gorilla/websocket"
)

// 子进程角色。同一个可执行文件按环境变量 SC_ROLE 变成 Manager 或 Gateway，
// 这样多台 Gateway 可以各自带不同的 ClusterId，不用改 example。

func runManager() {
	grpcPort := os.Getenv("SC_GRPC")
	tcpPort := os.Getenv("SC_TCP")
	if grpcPort == "" || tcpPort == "" {
		fmt.Println("manager role needs SC_GRPC and SC_TCP")
		os.Exit(2)
	}
	if mb, _ := strconv.Atoi(os.Getenv("SC_MEMMB")); mb > 0 {
		manager.EnableMemoryGuard(mb, 0, 0)
	}
	if addr := os.Getenv("SC_MEMHTTP"); addr != "" {
		mux := http.NewServeMux()
		mux.HandleFunc("/mem", func(w http.ResponseWriter, r *http.Request) {
			// 和产品代码一样只读运行时指标，不用会暂停程序的 ReadMemStats。
			s := []metrics.Sample{{Name: "/gc/heap/live:bytes"}}
			metrics.Read(s)
			fmt.Fprintf(w, "%d %d %d", s[0].Value.Uint64(), manager.MemoryGuardDropped(), manager.MemoryGuardLimit())
		})
		go func() { _ = http.ListenAndServe(addr, mux) }()
	}
	m := &manager.Manager{}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Println("grpc service panic:", r)
			}
		}()
		if err := m.StartGrpcService(":" + grpcPort); err != nil {
			fmt.Println("grpc service:", err)
		}
	}()
	m.StartSocketService("127.0.0.1:" + tcpPort)
}

var upgrader = websocket.Upgrader{CheckOrigin: func(*http.Request) bool { return true }}

func runGateway() {
	id, _ := strconv.ParseInt(os.Getenv("SC_CLUSTER"), 10, 64)
	if id <= 0 {
		id = 1
	}
	gateway.ClusterId = id
	gateway.Init()
	probe := &gateway.FireTower{}

	http.HandleFunc("/ws", func(w http.ResponseWriter, r *http.Request) {
		ws, err := upgrader.Upgrade(w, r, nil)
		if err != nil || ws == nil {
			return
		}
		uid := r.URL.Query().Get("uid")
		bug := r.URL.Query().Get("bug")
		tower := gateway.BuildTower(ws, uid+"-"+strconv.FormatInt(time.Now().UnixNano(), 36))
		if tower == nil {
			_ = ws.Close()
			return
		}
		tower.UserId = uid
		if r.URL.Query().Get("gap") == "1" {
			tower.EnableGapHints()
		}
		if topic := r.URL.Query().Get("passive"); topic != "" {
			tower.SetPassive(topic, true)
		}
		tower.SetReadHandler(func(fire *gateway.FireInfo) bool {
			if bug == "read" {
				panic("stagecheck: business bug in read handler")
			}
			_ = tower.Publish(fire)
			return true
		})
		tower.SetSubscribeHandler(func(*gateway.FireLife, []string) bool {
			if bug == "sub" {
				panic("stagecheck: business bug in subscribe handler")
			}
			return true
		})
		tower.SetUnSubscribeHandler(func(*gateway.FireLife, []string) bool {
			if bug == "unsub" {
				panic("stagecheck: business bug in unsubscribe handler")
			}
			return true
		})
		tower.SetOnOfflineHandler(func() {
			if bug == "offline" {
				panic("stagecheck: business bug in offline handler")
			}
		})
		tower.SetOnConnectHandler(func() bool {
			if bug == "connect" {
				panic("stagecheck: business bug in connect handler")
			}
			return true
		})
		if bug == "gorun" {
			// 业务用 go tower.Run() 的写法，回调 panic 时没有 net/http 兜底。
			tower.SetOnConnectHandler(func() bool { panic("stagecheck: business bug in connect handler (go Run)") })
			done := make(chan struct{})
			go func() {
				defer close(done)
				tower.Run()
			}()
			<-done
			return
		}
		if bug == "goclose" {
			// 业务协程直接调 Close，下线回调 panic。
			tower.SetOnOfflineHandler(func() { panic("stagecheck: business bug in offline handler (go Close)") })
			go func() {
				time.Sleep(200 * time.Millisecond)
				tower.Close()
			}()
		}
		tower.Run()
	})
	http.HandleFunc("/num", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%d", probe.GetConnectNum(r.URL.Query().Get("topic")))
	})
	http.HandleFunc("/exists", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%t", probe.CheckTopicExist(r.URL.Query().Get("topic")))
	})
	http.HandleFunc("/key", func(w http.ResponseWriter, r *http.Request) {
		// 这台 Gateway 在第一台 Manager 上登记用的键。对账用例要用它在 Manager 上制造偏差。
		if cl := gateway.GetTopicManage(); cl != nil {
			fmt.Fprint(w, cl.RouteKey())
		}
	})
	http.HandleFunc("/mem", func(w http.ResponseWriter, r *http.Request) {
		limit, live, heap, rejecting, closed, trimmed := gateway.MemoryGuardStats()
		fmt.Fprintf(w, "%d %d %d %t %d %d", limit, live, heap, rejecting, closed, trimmed)
	})
	http.HandleFunc("/goroutines", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "%d", runtime.NumGoroutine())
	})
	http.HandleFunc("/drain", func(w http.ResponseWriter, r *http.Request) {
		gateway.Drain(16, 20*time.Millisecond)
		fmt.Fprint(w, "ok")
	})
	addr := os.Getenv("SC_HTTP")
	fmt.Println("stagecheck gateway", id, "listening", addr)
	if err := http.ListenAndServe(addr, nil); err != nil {
		fmt.Println("http:", err)
		os.Exit(2)
	}
}
