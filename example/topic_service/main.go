package main

import (
	"fmt"

	"github.com/OSMeteor/firetower/service/manager"

	"github.com/pelletier/go-toml"
)

// ConfigTree 配置信息
var ConfigTree *toml.Tree

func init() {
	var (
		err error
	)
	if ConfigTree, err = toml.LoadFile("./topicmanage.toml"); err != nil {
		fmt.Println("config load failed:", err)
	}
}
func main() {
	grpcPort, ok := configPort("grpc.port")
	socketPort, okSocket := configPort("socket.port")
	if !ok || !okSocket {
		fmt.Println("config load failed")
		return
	}
	m := &manager.Manager{}
	go func() {
		defer func() {
			if r := recover(); r != nil {
				fmt.Println("grpc service panic:", r)
			}
		}()
		if err := m.StartGrpcService(fmt.Sprintf(":%d", grpcPort)); err != nil {
			fmt.Println("grpc service:", err)
		}
	}()
	m.StartSocketService(fmt.Sprintf("0.0.0.0:%d", socketPort))
}

func configPort(key string) (int64, bool) {
	if ConfigTree == nil {
		return 0, false
	}
	port, ok := ConfigTree.Get(key).(int64)
	if !ok || port <= 0 {
		return 0, false
	}
	return port, true
}
