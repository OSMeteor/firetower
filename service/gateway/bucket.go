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
	hot            map[string]chan *socket.SendMessage
}

// hotTopicMin 大于 0 时，本桶订阅者达到这个数的 Topic 走自己的队列，不再占用共用的 BuffChan。0 表示关闭。
var hotTopicMin atomic.Int64

var (
	holderMu sync.Mutex
	holders  = map[string]map[*Bucket]struct{}{}
)

func buildBuckets() {
	if loadConfigTree() == nil {
		fmt.Println("config is nil, skip buckets")
		return
	}
	bucketNum := configInt("bucket.Num", 1)
	central := configInt("bucket.CentralChanCount", 1024)
	tm := &TowerManager{
		bucket:      make([]*Bucket, bucketNum),
		centralChan: make(chan *socket.SendMessage, central),
	}
	storeTM(tm)

	for i := 0; i < bucketNum; i++ {
		tm.bucket[i] = newBucket()
	}
	go tm.dispatch(bucketNum)
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
		if cap(t.centralChan) > 0 {
			line := int64(cap(t.centralChan) * configInt("warn.queuePercent", 70) / 100)
			warnCapacity("centralChan", int64(len(t.centralChan)), line, "check one topic before growing the queue")
		}
		// 采样日志在入队之前打。入队之后测试会改写原消息，用来确认订阅者拿到的是副本。
		if socket.NoteHot() {
			message.Info("Sended")
		}
		// 本机发布的帧已经带着片键。业务后端和管理命令按房间名发来的帧在这里展开成本机的各片。
		keys := []string{message.Topic}
		if topicShards(message.Topic) >= 2 {
			keys = publishKeys(message.Topic)
		}
		for _, key := range keys {
			targets := bucketsFor(key)
			if message.Type == socket.OfflineUserKey {
				targets = t.bucket
			}
			for _, b := range targets {
				if b == nil {
					continue
				}
				copied := cloneSendMessage(message)
				if copied != nil {
					copied.Topic = key
				}
				b.enqueue(copied)
			}
		}
		message.Recycling()
	}
}

func deliverOrDrop(b *Bucket, index int, message *socket.SendMessage) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("PANIC: bucket deliver recovered: %v\n", err)
		}
	}()
	if b == nil || b.BuffChan == nil || message == nil {
		return
	}
	timer := time.NewTimer(100 * time.Millisecond)
	defer timer.Stop()
	select {
	case b.BuffChan <- message:
	case <-timer.C:
		noteBucketDrop(index)
	}
}

var (
	bucketDrops       atomic.Int64
	lastBucketDropLog atomic.Int64
)

func noteBucketDrop(index int) {
	bucketDrops.Add(1)
	now := time.Now().UnixNano()
	prev := lastBucketDropLog.Load()
	if now-prev < int64(time.Second) || !lastBucketDropLog.CompareAndSwap(prev, now) {
		return
	}
	fmt.Printf("bucket %d full, drop message\n", index)
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
	return atomic.AddInt64(&bucketId, 1)
}

func getConnId() uint64 {
	return atomic.AddUint64(&connId, 1)
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
			case socket.PublishKey, socket.StateKey:
				callBeforePush(message)
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
	noteHolder(topic, b, true)
	b.mu.Unlock()
}

// DelSubscribe 删除当前实例中的topic->conn的订阅关系。只删除仍指向该连接的记录。
func (b *Bucket) DelSubscribe(topic string, bt *FireTower) {
	if b == nil || bt == nil {
		return
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if m, ok := b.topicRelevance[topic]; ok {
		if cur, exists := m[bt.ClientId]; exists && cur == bt {
			delete(m, bt.ClientId)
		}
		if len(m) == 0 {
			delete(b.topicRelevance, topic)
			noteHolder(topic, b, false)
		}
	}
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
	if message.Type == socket.StateKey {
		kept := targets[:0]
		for _, v := range targets {
			if !v.passiveTopic(message.Topic) {
				kept = append(kept, v)
			}
		}
		targets = kept
	}
	if len(targets) == 0 {
		return ErrorTopicEmpty
	}
	for _, v := range targets {
		deliverTo(v, message)
	}
	return nil
}

// deliverTo 把一条消息交给一个连接。一个连接 panic 只丢掉它这一份，桶里的其他连接继续收。
func deliverTo(v *FireTower, message *socket.SendMessage) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("PANIC: deliver recovered: %v\n", err)
		}
	}()
	if v == nil {
		return
	}
	_ = v.Send(message)
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
	handler := target.unSubscribeHandler
	safeCall("unsubscribe handler", func() {
		if handler != nil {
			handler(nil, []string{parentTopic(message.Topic)})
		}
	})
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
		handler := v.onSystemRemove
		topic := parentTopic(message.Topic)
		safeCall("system remove", func() {
			if handler != nil {
				handler(topic)
			}
		})
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
		user := v
		safeCall("offline user", func() { user.Close() })
	}
	return nil
}

func (b *Bucket) enqueue(message *socket.SendMessage) {
	if b == nil || message == nil {
		return
	}
	if ch := b.hotChan(message); ch != nil {
		select {
		case ch <- message:
		default:
			noteBucketDrop(int(b.id))
		}
		return
	}
	if b.BuffChan == nil {
		return
	}
	select {
	case b.BuffChan <- message:
	default:
		// 不等待。dispatch 只有一个协程，在这里等会让别的 Topic 一起排在 centralChan 后面被丢掉。
		noteBucketDrop(int(b.id))
	}
}

func (b *Bucket) hotChan(message *socket.SendMessage) chan *socket.SendMessage {
	limit := int(hotTopicMin.Load())
	if limit <= 0 || message == nil || message.Topic == "" || message.Type == socket.OfflineUserKey {
		return nil
	}
	b.mu.Lock()
	ch := b.hot[message.Topic]
	var start chan *socket.SendMessage
	if ch == nil {
		n := 0
		if m := b.topicRelevance[message.Topic]; m != nil {
			n = len(m)
		}
		if n >= limit {
			ch = make(chan *socket.SendMessage, 8)
			if b.hot == nil {
				b.hot = map[string]chan *socket.SendMessage{}
			}
			b.hot[message.Topic] = ch
			start = ch
		}
	}
	b.mu.Unlock()
	if start != nil {
		go b.hotLoop(start)
	}
	return ch
}

// testBeforePush 只给测试注入一次 panic。正式运行保持 nil。
type pushFn func(*socket.SendMessage)

var testBeforePush atomic.Pointer[pushFn]

func callBeforePush(message *socket.SendMessage) {
	if fn := testBeforePush.Load(); fn != nil {
		(*fn)(message)
	}
}

func (b *Bucket) hotLoop(ch chan *socket.SendMessage) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("PANIC: hot topic recovered: %v\n", err)
			time.Sleep(100 * time.Millisecond)
			go b.hotLoop(ch)
		}
	}()
	for message := range ch {
		if message == nil {
			continue
		}
		callBeforePush(message)
		_ = b.push(message)
	}
}

func safeCall(name string, fn func()) {
	if fn == nil {
		return
	}
	defer func() {
		if err := recover(); err != nil {
			fmt.Printf("PANIC: %s recovered: %v\n", name, err)
		}
	}()
	fn()
}

func noteHolder(topic string, b *Bucket, add bool) {
	if topic == "" || b == nil {
		return
	}
	holderMu.Lock()
	defer holderMu.Unlock()
	set := holders[topic]
	if add {
		if set == nil {
			set = map[*Bucket]struct{}{}
			holders[topic] = set
		}
		set[b] = struct{}{}
		return
	}
	if set == nil {
		return
	}
	delete(set, b)
	if len(set) == 0 {
		delete(holders, topic)
	}
}

func bucketsFor(topic string) []*Bucket {
	holderMu.Lock()
	defer holderMu.Unlock()
	set := holders[topic]
	out := make([]*Bucket, 0, len(set))
	for b := range set {
		out = append(out, b)
	}
	return out
}
