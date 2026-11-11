package gateway

import (
	"fmt"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"

	"google.golang.org/grpc"
	"google.golang.org/grpc/backoff"
)

// buildManagerClient 实例化一个与topicManager连接的tcp链接
func buildManagerClient() {
	go func() {
		for {
			panicked := false
			func() {
				defer func() {
					if err := recover(); err != nil {
						panicked = true
						fmt.Printf("PANIC: manager client recovered: %v\n", err)
					}
				}()
				dialManager()
			}()
			if !panicked {
				return
			}
			time.Sleep(time.Second)
		}
	}()
}

func dialManager() {
	specs := managerSpecs(loadConfigTree())
	if len(specs) == 0 {
		fmt.Println("[manager client] no manager address configured")
		return
	}
	startPeriodicReplay()
	for _, spec := range specs {
		go connectSpec(spec)
	}
}

func connectSpec(spec managerSpec) {
	added := false
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("PANIC: manager connect %s recovered: %v\n", spec.name, err)
			if !added {
				time.Sleep(time.Second)
				go connectSpec(spec)
			}
		}
	}()
	sleepTime := time.Second
	var rpc pb.TopicServiceClient
	for rpc == nil {
		// gRPC 默认的重连退避最长 120 秒。Manager 停得久一点再回来时，TCP 几秒内就连上了，
		// 订阅和重放却要等 gRPC 的退避走完。这里把上限压到 3 秒。
		conn, err := grpc.Dial(spec.grpc, grpc.WithInsecure(), grpc.WithConnectParams(grpc.ConnectParams{
			Backoff: backoff.Config{
				BaseDelay:  200 * time.Millisecond,
				Multiplier: 1.6,
				Jitter:     0.2,
				MaxDelay:   3 * time.Second,
			},
			MinConnectTimeout: 3 * time.Second,
		}))
		if err != nil {
			fmt.Printf("[manager client] grpc connect %s error: %v, retrying in %v\n", spec.name, err, sleepTime)
			time.Sleep(sleepTime)
			if sleepTime < 30*time.Second {
				sleepTime *= 2
			}
			continue
		}
		rpc = pb.NewTopicServiceClient(conn)
	}
	tcp := socket.NewClient(spec.tcp)
	addLink(&managerLink{
		name:     spec.name,
		tcp:      tcp,
		rpc:      rpc,
		register: spec.register,
		publish:  spec.publish,
	})
	added = true
	sleepTime = time.Second
	for {
		err := tcp.Connect()
		if err == nil {
			fmt.Println("[manager client] connected:", spec.name, spec.tcp)
			return
		}
		fmt.Printf("[manager client] tcp connect %s error: %v, retrying in %v\n", spec.name, err, sleepTime)
		time.Sleep(sleepTime)
		if sleepTime < 30*time.Second {
			sleepTime *= 2
		}
	}
}
