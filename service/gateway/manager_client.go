package gateway

import (
	"fmt"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"

	"google.golang.org/grpc"
)

// buildManagerClient 实例化一个与topicManager连接的tcp链接
func buildManagerClient() {
	go func() {
		defer func() {
			if err := recover(); err != nil {
				fmt.Printf("PANIC: manager client recovered: %v\n", err)
			}
		}()
		sleepTime := time.Second
	Retry:
		var err error
		conn, err := grpc.Dial(ConfigTree.Get("grpc.address").(string), grpc.WithInsecure())
		if err != nil {
			fmt.Printf("[manager client] grpc connect error: %v, retrying in %v\n", err, sleepTime)
			time.Sleep(sleepTime)
			sleepTime *= 2
			if sleepTime > 30*time.Second {
				sleepTime = 30 * time.Second
			}
			goto Retry
		}
		topicManageGrpc = pb.NewTopicServiceClient(conn)
		topicManage = socket.NewClient(ConfigTree.Get("topicServiceAddr").(string))

		topicManage.OnPush(func(sendMessage *socket.SendMessage) {
			TM.centralChan <- sendMessage
		})
		
		// Reset sleep time for next phase
		sleepTime = time.Second
	ConnectTcp:
		err = topicManage.Connect()
		if err != nil {
			fmt.Printf("[manager client] tcp connect error: %v, retrying in %v\n", err, sleepTime)
			time.Sleep(sleepTime)
			sleepTime *= 2
			if sleepTime > 30*time.Second {
				sleepTime = 30 * time.Second
			}
			goto ConnectTcp
		} else {
			fmt.Println("[manager client] connected:", ConfigTree.Get("topicServiceAddr").(string))
		}
	}()
}
