package manager

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"

	pb "github.com/OSMeteor/firetower/grpc/manager"
)

// FuzzRegisterRequests 用任意字段组合调用 Manager 的登记、退订、发布、查询接口。
// gRPC 处理函数里有 recover，panic 不会让进程退出，所以这里把记到日志里的 PANIC 当作失败。
func FuzzRegisterRequests(f *testing.F) {
	f.Add("10.0.0.1:1", "a,b", int64(1), uint64(1), uint64(0), true, true, "a")
	f.Add("", "", int64(-1), uint64(0), uint64(3), false, true, "")
	f.Add("x", "_firetower_epoch,_firetower_handshake,a\x1f3", int64(5), uint64(9), uint64(9), true, false, "a")
	f.Fuzz(func(t *testing.T, ip, topics string, count int64, ver, round uint64, begin, end bool, query string) {
		var panics atomic.Int32
		prev := currentLogger()
		setLogger(func(level, _ string) {
			if level == "PANIC" {
				panics.Add(1)
			}
		})
		defer setLogger(prev)
		svc := &topicGrpcService{}
		ctx := context.Background()
		list := strings.Split(topics, ",")
		counts := make([]int64, len(list))
		versions := make([]uint64, len(list))
		for i := range list {
			counts[i] = count + int64(i)
			versions[i] = ver + uint64(i)
		}
		// 长度对得上的和对不上的都要试。
		_, _ = svc.SubscribeTopic(ctx, &pb.SubscribeTopicRequest{Ip: ip, Topic: list, Count: counts, Version: versions, Round: round, RoundBegin: begin, RoundEnd: end})
		_, _ = svc.SubscribeTopic(ctx, &pb.SubscribeTopicRequest{Ip: ip, Topic: list, Count: counts[:len(counts)/2], Version: versions})
		_, _ = svc.SubscribeTopic(ctx, &pb.SubscribeTopicRequest{Ip: ip, Topic: list})
		_, _ = svc.UnSubscribeTopic(ctx, &pb.UnSubscribeTopicRequest{Ip: ip, Topic: list, Count: counts, Version: versions})
		_, _ = svc.UnSubscribeTopic(ctx, &pb.UnSubscribeTopicRequest{Ip: ip, Topic: list})
		_, _ = svc.Publish(ctx, &pb.PublishRequest{Topic: query, Data: []byte(topics), MessageId: ip, Source: query})
		_, _ = svc.GetConnectNum(ctx, &pb.GetConnectNumRequest{Topic: query})
		_, _ = svc.CheckTopicExist(ctx, &pb.CheckTopicExistRequest{Topic: query})
		_, _ = svc.SubscribeTopic(ctx, nil)
		_, _ = svc.UnSubscribeTopic(ctx, nil)
		// 收尾：把这个键下的全部登记清掉，不让一次输入影响下一次。
		topicMu.Lock()
		forgetIPLocked(ip)
		delete(deadKeys, ip)
		delete(keyAlias, ip)
		topicMu.Unlock()
		if n := panics.Load(); n > 0 {
			t.Fatalf("%d recovered panic(s) for ip=%q topics=%q", n, ip, topics)
		}
	})
}
