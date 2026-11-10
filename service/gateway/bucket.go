package gateway

import (
	"fmt"
	"sync"
	"sync/atomic"
	"time"

	"github.com/OSMeteor/firetower/socket"
)

var (
	// TM 是一个实例的管理中心
	TM *TowerManager
)

// TowerManager 包含中心处理队列和多个bucket
// bucket的作用是将一个实例的连接均匀的分布在多个bucket中来达到并发推送的目的
type TowerManager struct {
	bucket      []*Bucket
	centralChan chan *socket.SendMessage // 中心处理队列
}

// Bucket 的作用是将一个实例的连接均匀的分布在多个bucket中来达到并发推送的目的
type Bucket struct {
	mu             sync.RWMutex // 读写锁，可并发读不可并发读写
	id             int64
	len            int64
	topicRelevance map[string]map[string]*FireTower // topic -> websocket clientid -> websocket conn
	BuffChan       chan *socket.SendMessage         // bucket的消息处理队列
}

func buildBuckets() {
	if ConfigTree == nil {
		fmt.Println("config is nil, skip buckets")
		return
	}
	bucketNum := configInt("bucket.Num", 1)
	central := configInt("bucket.CentralChanCount", 1024)
	TM = &TowerManager{
		bucket:      make([]*Bucket, bucketNum),
		centralChan: make(chan *socket.SendMessage, central),
	}

	for i := 0; i < bucketNum; i++ {
		TM.bucket[i] = newBucket()
	}
	go TM.dispatch(bucketNum)
}

func (t *TowerManager) dispatch(bucketNum int) {
	if t == nil || t.centralChan == nil {
		return
	}
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("PANIC: towerManager central processor recovered: %v\n", err)
			time.Sleep(100 * time.Millisecond)
			go t.dispatch(bucketNum)
		}
	}()
	for message := range t.centralChan {
		if message == nil {
			continue
		}
		for i := 0; i < bucketNum && i < len(t.bucket); i++ {
			b := t.bucket[i]
			if b == nil || b.BuffChan == nil {
				continue
			}
			select {
			case b.BuffChan <- message:
			default:
				fmt.Printf("bucket %d full, drop message\n", i)
			}
		}
		message.Info("Sended")
		message.Recycling()
	}
}

func newBucket() *Bucket {
	b := &Bucket{
		id:             getNewBucketId(),
		len:            0,
		topicRelevance: make(map[string]map[string]*FireTower),
		BuffChan:       make(chan *socket.SendMessage, configInt("bucket.BuffChanCount", 8)),
	}

	ConsumerNum := configInt("bucket.ConsumerNum", 1)
	if ConsumerNum == 0 {
		ConsumerNum = 1
	}
	// 每个bucket启动ConsumerNum个消费者(并发处理)
	for i := 0; i < ConsumerNum; i++ {
		go b.consumer()
	}
	return b
}

var (
	bucketId int64
	connId   uint64
)

func getNewBucketId() int64 {
	atomic.AddInt64(&bucketId, 1)
	return bucketId
}

func getConnId() uint64 {
	atomic.AddUint64(&connId, 1)
	return connId
}

// GetBucket 获取一个可以分配当前连接的bucket
func (t *TowerManager) GetBucket(bt *FireTower) (bucket *Bucket) {
	if t == nil || bt == nil || len(t.bucket) == 0 {
		return nil
	}
	return t.bucket[bt.connId%uint64(len(t.bucket))]
}

func (b *Bucket) consumer() {
	if b == nil || b.BuffChan == nil {
		return
	}
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("PANIC: bucket consumer recovered: %v\n", err)
			time.Sleep(100 * time.Millisecond)
			go b.consumer()
		}
	}()
	for {
		select {
		case message := <-b.BuffChan:
			if message == nil {
				continue
			}
			switch message.Type {
			case socket.PublishKey:
				b.push(message)
			case socket.OfflineTopicByUserIdKey:
				// 需要退订的topic和user_id
				b.unSubscribeByUserId(message)
			case socket.OfflineTopicKey:
				b.unSubscribeAll(message)
			case socket.OfflineUserKey:
				b.offlineUsers(message)
			}
			if message.Type == "push" {

			}

		}
	}
}

// AddSubscribe 添加当前实例中的topic->conn的订阅关系
func (b *Bucket) AddSubscribe(topic string, bt *FireTower) {
	if b == nil || bt == nil || topic == "" {
		return
	}
	b.mu.Lock()
	if b.topicRelevance == nil {
		b.topicRelevance = make(map[string]map[string]*FireTower)
	}
	if m, ok := b.topicRelevance[topic]; ok {
		m[bt.ClientId] = bt
	} else {
		b.topicRelevance[topic] = make(map[string]*FireTower)
		b.topicRelevance[topic][bt.ClientId] = bt
	}
	b.mu.Unlock()
}

// DelSubscribe 删除当前实例中的topic->conn的订阅关系。只删除仍指向该连接的记录。
func (b *Bucket) DelSubscribe(topic string, bt *FireTower) {
	if b == nil || bt == nil {
		return
	}
	b.mu.Lock()
	if m, ok := b.topicRelevance[topic]; ok {
		if cur, exists := m[bt.ClientId]; exists && cur == bt {
			delete(m, bt.ClientId)
		}
		if len(m) == 0 {
			delete(b.topicRelevance, topic)
		}
	}
	b.mu.Unlock()
}

// Push 桶内进行遍历push
// 每个bucket有一个Push方法
// 在推送时每个bucket同时调用Push方法 来达到并发推送
// 该方法主要通过遍历桶中的topic->conn订阅关系来进行websocket写入
func (b *Bucket) push(message *socket.SendMessage) error {
	if b == nil || message == nil {
		return ErrorTopicEmpty
	}
	b.mu.RLock()
	var targets []*FireTower
	if m, ok := b.topicRelevance[message.Topic]; ok {
		for _, v := range m {
			if v != nil {
				targets = append(targets, v)
			}
		}
	}
	b.mu.RUnlock()
	if len(targets) == 0 {
		return ErrorTopicEmpty
	}
	for _, v := range targets {
		_ = v.Send(message)
	}
	return nil
}

// UnSubscribeByUserId 服务端指定某个用户退订某个topic
func (b *Bucket) unSubscribeByUserId(message *socket.SendMessage) error {
	if b == nil || message == nil {
		return ErrorTopicEmpty
	}
	var target *FireTower
	b.mu.RLock()
	if m, ok := b.topicRelevance[message.Topic]; ok {
		userId := string(message.Data)
		for _, v := range m {
			if v != nil && v.UserId == userId {
				target = v
				break
			}
		}
	}
	b.mu.RUnlock()
	if target == nil {
		return ErrorTopicEmpty
	}
	_, err := target.unbindTopic([]string{message.Topic})
	_ = target.ToSelf([]byte("{}"))
	if target.unSubscribeHandler != nil {
		target.unSubscribeHandler(nil, []string{message.Topic})
	}
	return err
}

// UnSubscribeAll 移除所有该topic的订阅关系
func (b *Bucket) unSubscribeAll(message *socket.SendMessage) error {
	if b == nil || message == nil {
		return ErrorTopicEmpty
	}
	var targets []*FireTower
	b.mu.RLock()
	if m, ok := b.topicRelevance[message.Topic]; ok {
		for _, v := range m {
			if v != nil {
				targets = append(targets, v)
			}
		}
	}
	b.mu.RUnlock()
	if len(targets) == 0 {
		return ErrorTopicEmpty
	}
	var first error
	for _, v := range targets {
		if _, err := v.unbindTopic([]string{message.Topic}); err != nil && first == nil {
			first = err
		}
		if v.onSystemRemove != nil {
			v.onSystemRemove(message.Topic)
		}
	}
	if first != nil {
		return first
	}
	return nil
}

func (b *Bucket) offlineUsers(message *socket.SendMessage) error {
	if b == nil || message == nil {
		return ErrorTopicEmpty
	}
	var targets []*FireTower
	b.mu.RLock()
	if m, ok := b.topicRelevance[message.Topic]; ok {
		userId := string(message.Data)
		for _, v := range m {
			if v != nil && v.UserId == userId {
				targets = append(targets, v)
			}
		}
	}
	b.mu.RUnlock()
	if len(targets) == 0 {
		return ErrorTopicEmpty
	}
	for _, v := range targets {
		v.Close()
	}
	return nil
}
