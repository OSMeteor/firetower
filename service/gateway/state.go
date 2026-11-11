package gateway

import (
	"fmt"
	"sync"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
	"github.com/holdno/snowFlakeByGo"
	"github.com/pelletier/go-toml"
)

// 这些变量接入方会直接赋值，类型不能改。包内的读写都经过这一把锁，
// 测试在协程还活着时换上新值才不会和正在读的协程撞上。
var gwMu sync.RWMutex

type gwSnap struct {
	tm     *TowerManager
	grpc   pb.TopicServiceClient
	manage *socket.TcpClient
	logger func(*FireTower, string, string)
	fire   func(*FireInfo, string, string)
	worker *snowFlakeByGo.Worker
	cfg    *toml.Tree
}

func snapshotGW() gwSnap {
	gwMu.RLock()
	defer gwMu.RUnlock()
	return gwSnap{
		tm:     TM,
		grpc:   topicManageGrpc,
		manage: topicManage,
		logger: TowerLogger,
		fire:   FireLogger,
		worker: IdWorker,
		cfg:    ConfigTree,
	}
}

func restoreGW(s gwSnap) {
	gwMu.Lock()
	TM = s.tm
	topicManageGrpc = s.grpc
	topicManage = s.manage
	TowerLogger = s.logger
	FireLogger = s.fire
	IdWorker = s.worker
	ConfigTree = s.cfg
	gwMu.Unlock()
}

func loadTM() *TowerManager {
	gwMu.RLock()
	defer gwMu.RUnlock()
	return TM
}

func storeTM(tm *TowerManager) {
	gwMu.Lock()
	TM = tm
	gwMu.Unlock()
}

func loadConfigTree() *toml.Tree {
	gwMu.RLock()
	defer gwMu.RUnlock()
	return ConfigTree
}

func storeConfigTree(tree *toml.Tree) {
	gwMu.Lock()
	ConfigTree = tree
	gwMu.Unlock()
}

func loadTopicManage() *socket.TcpClient {
	gwMu.RLock()
	defer gwMu.RUnlock()
	return topicManage
}

func storeTopicManage(c *socket.TcpClient) {
	gwMu.Lock()
	topicManage = c
	gwMu.Unlock()
}

func loadTopicGrpc() pb.TopicServiceClient {
	gwMu.RLock()
	defer gwMu.RUnlock()
	return topicManageGrpc
}

func storeTopicGrpc(c pb.TopicServiceClient) {
	gwMu.Lock()
	topicManageGrpc = c
	gwMu.Unlock()
}

func loadTopicClients() (*socket.TcpClient, pb.TopicServiceClient) {
	gwMu.RLock()
	defer gwMu.RUnlock()
	return topicManage, topicManageGrpc
}

func storeTopicClients(c *socket.TcpClient, rpc pb.TopicServiceClient) {
	gwMu.Lock()
	topicManage = c
	topicManageGrpc = rpc
	gwMu.Unlock()
}

func loadTowerLogger() func(*FireTower, string, string) {
	gwMu.RLock()
	defer gwMu.RUnlock()
	return TowerLogger
}

func storeTowerLogger(fn func(*FireTower, string, string)) {
	gwMu.Lock()
	TowerLogger = fn
	gwMu.Unlock()
}

// 日志函数可以被业务替换。它在各协程的 recover 里也会被调用，自己 panic 时不能再往外抛。
func callTowerLogger(t *FireTower, kind, info string) {
	if fn := loadTowerLogger(); fn != nil {
		defer func() {
			if err := recover(); err != nil {
				fmt.Printf("PANIC: tower logger recovered: %v\n", err)
			}
		}()
		fn(t, kind, info)
	}
}

func loadFireLogger() func(*FireInfo, string, string) {
	gwMu.RLock()
	defer gwMu.RUnlock()
	return FireLogger
}

func storeFireLogger(fn func(*FireInfo, string, string)) {
	gwMu.Lock()
	FireLogger = fn
	gwMu.Unlock()
}

func callFireLogger(f *FireInfo, kind, info string) {
	if fn := loadFireLogger(); fn != nil {
		defer func() {
			if err := recover(); err != nil {
				fmt.Printf("PANIC: fire logger recovered: %v\n", err)
			}
		}()
		fn(f, kind, info)
	}
}

func loadIDWorker() *snowFlakeByGo.Worker {
	gwMu.RLock()
	defer gwMu.RUnlock()
	return IdWorker
}

func storeIDWorker(worker *snowFlakeByGo.Worker) {
	gwMu.Lock()
	IdWorker = worker
	gwMu.Unlock()
}
