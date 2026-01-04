package main

import (
	"flag"
	"fmt"
	"log"
	"time"

	"github.com/OSMeteor/firetower/socket"
)

var (
	targetAddr = flag.String("addr", "127.0.0.1:6666", "Topic Service TCP address")
	topic      = flag.String("topic", "stable_chat", "Topic to broadcast to")
	rate       = flag.Int("rate", 1000, "Messages per second")
	duration   = flag.Duration("d", 30*time.Second, "Test duration")
)

func main() {
	flag.Parse()
	log.Printf("📢 STARTING BROADCAST SENDER 📢")
	log.Printf("Target: %s | Topic: %s | Rate: %d/s", *targetAddr, *topic, *rate)

	client := socket.NewClient(*targetAddr)
	if err := client.Connect(); err != nil {
		log.Fatalf("Failed to connect to topic service: %v", err)
	}
	defer client.Close()

	log.Println("Connected. Starting broadcast...")

	interval := time.Second / time.Duration(*rate)
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	stop := time.After(*duration)
	count := 0

	for {
		select {
		case <-stop:
			log.Printf("Finished. Total Sent: %d", count)
			return
		case <-ticker.C:
			data := []byte(fmt.Sprintf(`{"msg":"broadcast %d", "ts":%d}`, count, time.Now().UnixNano()))
			// Publish(id, type, topic, data)
			// id is arbitrary, "broadcast_bot"
			// type "user" is standard
			err := client.Publish("broadcast_bot", "user", *topic, data)
			if err != nil {
				log.Printf("Publish error: %v", err)
			} else {
				count++
			}
			
			if count%1000 == 0 {
				fmt.Printf("\rSent: %d", count)
			}
		}
	}
}
