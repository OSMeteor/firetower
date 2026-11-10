// 真实拉起 go-sql-driver 的 startWatcher。
// 业务仓库不在这台机器上。这里用驱动自己的连接池：TCP 连上后驱动会为每条连接起一个 watcher，
// 连接关闭后这些协程必须退出。
package main

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"runtime"
	"strings"
	"sync"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

func main() {
	const want = 4
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		exit(err)
	}
	var mu sync.Mutex
	var held []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			held = append(held, c)
			mu.Unlock()
		}
	}()

	dsn := fmt.Sprintf("root:root@tcp(%s)/db?timeout=20s", ln.Addr().String())
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		exit(err)
	}
	db.SetMaxOpenConns(want)
	db.SetMaxIdleConns(want)

	var wg sync.WaitGroup
	for i := 0; i < want; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			_ = db.PingContext(ctx)
		}()
	}

	deadline := time.Now().Add(5 * time.Second)
	got := 0
	for time.Now().Before(deadline) {
		got = countWatchers()
		if got >= want {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	fmt.Printf("DB01 pool_open=%d startWatcher=%d\n", want, got)

	_ = ln.Close()
	mu.Lock()
	for _, c := range held {
		_ = c.Close()
	}
	mu.Unlock()
	wg.Wait()
	_ = db.Close()

	left := want
	for i := 0; i < 30; i++ {
		left = countWatchers()
		if left == 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	fmt.Printf("DB01 after_close startWatcher=%d\n", left)
	if got < want {
		exit(fmt.Errorf("连接池打开后 watcher 数量是 %d，期望 %d", got, want))
	}
	if left != 0 {
		exit(fmt.Errorf("连接池关闭后还剩 %d 个 startWatcher", left))
	}
	fmt.Println("DB01 PASS driver-pool")
}

func countWatchers() int {
	buf := make([]byte, 2<<20)
	n := runtime.Stack(buf, true)
	return strings.Count(string(buf[:n]), "startWatcher.func1")
}

func exit(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
