package main

import (
	"fmt"
	"net/http"
	"strconv"
	"hash/fnv"

	"github.com/OSMeteor/firetower/service/gateway"

	"github.com/gorilla/websocket"
	"github.com/holdno/snowFlakeByGo"
)

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true
	},
}

// GlobalIdWorker 全局唯一id生成器
var GlobalIdWorker *snowFlakeByGo.Worker

func main() {
	// 全局唯一id生成器
	GlobalIdWorker, _ = snowFlakeByGo.NewWorker(2) // Cluster ID 2
	gateway.ClusterId = 2
	gateway.Init()
	http.HandleFunc("/ws", Websocket)
	fmt.Println("Topic Bucketing Demo service start: 0.0.0.0:9998")
	if err := http.ListenAndServe("0.0.0.0:9998", nil); err != nil {
		fmt.Printf("server start error: %v\n", err)
	}
}

// Websocket http转websocket连接 并实例化firetower
func Websocket(w http.ResponseWriter, r *http.Request) {
	ws, _ := upgrader.Upgrade(w, r, nil)
	id := GlobalIdWorker.GetId()
	userID := strconv.FormatInt(id, 10)
	tower := gateway.BuildTower(ws, userID) // ClientID/UserID

	// -------------------------------------------------------------
	// 核心特性：超级热点分桶 (Bucketing Middleware)
	// -------------------------------------------------------------
	tower.SetBeforeSubscribeHandler(func(context *gateway.FireLife, topics []string) ([]string, bool) {
		var newTopics []string
		for _, topic := range topics {
			// 假设 "live_stream" 是一个超级热门的直播间 Topic
			if topic == "live_stream" {
				// 我们将其拆分为 100 个子 Bucket (live_stream_0 ... live_stream_99)
				// 使用简单的 FNV Hash 算法对 UserID 进行分流
				bucketID := hash(userID) % 100
				
				// 将原始 Topic 重写为具体的 bucket topic
				targetTopic := fmt.Sprintf("%s_%d", topic, bucketID)
				newTopics = append(newTopics, targetTopic)
				
				fmt.Printf("[Bucketing] User %s subscribed to 'live_stream', redirected to '%s'\n", userID, targetTopic)
			} else {
				// 普通 Topic 不做处理
				newTopics = append(newTopics, topic)
			}
		}
		// 返回修改后的 Topic 列表，允许订阅
		return newTopics, true
	})
	// -------------------------------------------------------------

	tower.Run()
}

func hash(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}
