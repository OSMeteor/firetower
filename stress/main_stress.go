package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"net/url"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gorilla/websocket"
)

var (
	stableUsers   = flag.Int("stable", 50, "Number of stable long-connection users")
	chaosUsers    = flag.Int("chaos", 50, "Number of churn users (frequent connect/disconnect)")
	slowUsers     = flag.Int("slow", 10, "Number of slow-reading users")
	duration      = flag.Duration("d", 30*time.Second, "Test duration")
	serverAddr    = flag.String("addr", "127.0.0.1:9999", "Server address") // No scheme, just host:port
	
	sentCount     int64
	recvCount     int64
	connectCount  int64
	errorCount    int64
)

type FireInfo struct {
	Topic string          `json:"topic"`
	Data  json.RawMessage `json:"data"`
	Type  string          `json:"type"`
}

func main() {
	flag.Parse()
	log.Printf("🔥 STARTING ADVANCED STRESS TEST 🔥")
	log.Printf("Stable Users: %d | Chaos Users: %d | Slow Users: %d", *stableUsers, *chaosUsers, *slowUsers)
	log.Printf("Target: ws://%s/ws", *serverAddr)
	log.Printf("Duration: %v", *duration)

	interrupt := make(chan os.Signal, 1)
	signal.Notify(interrupt, os.Interrupt, syscall.SIGTERM)

	start := time.Now()
	var wg sync.WaitGroup

	// Channel to signal all goroutines to stop
	stopChan := make(chan struct{})

	// 1. Stable Users
	for i := 0; i < *stableUsers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			runStableClient(id, stopChan)
		}(i)
	}

	// 2. Chaos Users
	for i := 0; i < *chaosUsers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			runChaosClient(id, stopChan)
		}(i)
	}

	// 3. Slow Users
	for i := 0; i < *slowUsers; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			runSlowClient(id, stopChan)
		}(i)
	}

	// Wait for duration or interrupt
	select {
	case <-time.After(*duration):
		log.Println("⏰ Duration reached. Stopping...")
	case <-interrupt:
		log.Println("🛑 Interrupted by user.")
	}
	close(stopChan)

	log.Println("Waiting for clients to finish...")
	wg.Wait()

	log.Printf("=========== RESULTS ===========")
	log.Printf("Total Connections Attempted: %d", atomic.LoadInt64(&connectCount))
	log.Printf("Total Messages Sent: %d", atomic.LoadInt64(&sentCount))
	log.Printf("Total Messages Received: %d", atomic.LoadInt64(&recvCount))
	log.Printf("Total Errors: %d", atomic.LoadInt64(&errorCount))
	log.Printf("Test Duration: %s", time.Since(start))
	log.Printf("================================")
}

func getUrl() string {
	u := url.URL{Scheme: "ws", Host: *serverAddr, Path: "/ws"}
	return u.String()
}

func runStableClient(id int, stop <-chan struct{}) {
	c, _, err := websocket.DefaultDialer.Dial(getUrl(), nil)
	if err != nil {
		atomic.AddInt64(&errorCount, 1)
		// Assuming stable clients failing to connect is bad, but we retry or just return
		return 
	}
	defer c.Close()
	atomic.AddInt64(&connectCount, 1)

	// Sub to "stable_chat"
	subMsg := FireInfo{Type: "subscribe", Topic: "stable_chat"}
	c.WriteJSON(subMsg)

	// Reader
	go func() {
		for {
			_, _, err := c.ReadMessage()
			if err != nil {
				return
			}
			atomic.AddInt64(&recvCount, 1)
		}
	}()

	ticker := time.NewTicker(1 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			// Publish something
			msg := FireInfo{
				Type: "publish",
				Topic: "stable_chat",
				Data: json.RawMessage(fmt.Sprintf(`{"msg":"stable %d"}`, id)),
			}
			err := c.WriteJSON(msg)
			if err != nil {
				atomic.AddInt64(&errorCount, 1)
				return
			}
			atomic.AddInt64(&sentCount, 1)
		}
	}
}

func runChaosClient(id int, stop <-chan struct{}) {
	// Loop until stop
	for {
		select {
		case <-stop:
			return
		default:
			// Connect
			c, _, err := websocket.DefaultDialer.Dial(getUrl(), nil)
			if err != nil {
				atomic.AddInt64(&errorCount, 1)
				time.Sleep(time.Duration(rand.Intn(1000)) * time.Millisecond)
				continue
			}
			atomic.AddInt64(&connectCount, 1)

			// Random behavior
			action := rand.Intn(3)
			switch action {
			case 0:
				// Sub then Disconnect immediately
				c.WriteJSON(FireInfo{Type: "subscribe", Topic: "chaos_room"})
			case 1:
				// Pub then Disconnect
				c.WriteJSON(FireInfo{
					Type: "publish", 
					Topic: "chaos_room", 
					Data: json.RawMessage(`{"msg":"chaos"}`),
				})
				atomic.AddInt64(&sentCount, 1)
			case 2:
				// Stay valid for a bit
				time.Sleep(time.Duration(rand.Intn(500)) * time.Millisecond)
			}

			c.Close()
			
			// Sleep before next iteration to avoid overwhelming OS ports
			time.Sleep(time.Duration(rand.Intn(2000)) * time.Millisecond)
		}
	}
}

func runSlowClient(id int, stop <-chan struct{}) {
	c, _, err := websocket.DefaultDialer.Dial(getUrl(), nil)
	if err != nil {
		atomic.AddInt64(&errorCount, 1)
		return
	}
	defer c.Close()
	atomic.AddInt64(&connectCount, 1)

	// Subscribe to a busy topic
	c.WriteJSON(FireInfo{Type: "subscribe", Topic: "stable_chat"})

	// Read loop BUT SLOW
	for {
		select {
		case <-stop:
			return
		default:
			_, _, err := c.ReadMessage()
			if err != nil {
				return
			}
			atomic.AddInt64(&recvCount, 1)
			// Sleep to block the TCP window?
			// Blocking here means we don't read from the socket buffer.
			// Eventually socket buffer fills.
			// Server write will block or timeout.
			time.Sleep(500 * time.Millisecond) 
		}
	}
}
