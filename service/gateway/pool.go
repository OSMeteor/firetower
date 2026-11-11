package gateway

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/OSMeteor/firetower/grpc/manager"
	"github.com/OSMeteor/firetower/socket"
	"github.com/pelletier/go-toml"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// managerLink 是连接池里的一台 Manager。register 为真才接收订阅，publish 为真才接收发布。
type managerLink struct {
	name     string
	tcp      *socket.TcpClient
	rpc      pb.TopicServiceClient
	register bool
	publish  bool
	forceUp  bool
}

type managerSpec struct {
	name     string
	tcp      string
	grpc     string
	register bool
	publish  bool
}

var (
	poolMu       sync.RWMutex
	pool         []*managerLink
	pickMu       sync.Mutex
	lastPick     = map[string]string{}
	ManagerFlaps atomic.Int64
	lastFlapLog  atomic.Int64
)

func setPool(links []*managerLink) {
	poolMu.Lock()
	pool = links
	poolMu.Unlock()
	pickMu.Lock()
	lastPick = map[string]string{}
	pickMu.Unlock()
}

func addLink(link *managerLink) {
	if link == nil {
		return
	}
	poolMu.Lock()
	pool = append(pool, link)
	first := len(pool) == 1
	poolMu.Unlock()
	if first {
		storeTopicClients(link.tcp, link.rpc)
	}
	watchLink(link)
}

func watchLink(link *managerLink) {
	if link == nil || link.tcp == nil {
		return
	}
	link.tcp.SetOnConnect(func() {
		announceRouteClient(link.tcp)
		replayLink(link)
	})
	link.tcp.OnPush(func(message *socket.SendMessage) {
		forwardPush(message)
	})
}

var centralDrops atomic.Int64

func forwardPush(message *socket.SendMessage) {
	tm := loadTM()
	if tm == nil || tm.centralChan == nil || message == nil {
		return
	}
	select {
	case tm.centralChan <- message:
	default:
		centralDrops.Add(1)
	}
}

func activeLinks() []*managerLink {
	poolMu.RLock()
	links := append([]*managerLink(nil), pool...)
	poolMu.RUnlock()
	if len(links) > 0 {
		return links
	}
	manage, grpcClient := loadTopicClients()
	if manage == nil && grpcClient == nil {
		return nil
	}
	return []*managerLink{{
		name:     "default",
		tcp:      manage,
		rpc:      grpcClient,
		register: true,
		publish:  true,
	}}
}

func (l *managerLink) connected() bool {
	if l == nil {
		return false
	}
	if l.forceUp {
		return true
	}
	if l.tcp == nil {
		return false
	}
	return l.tcp.RouteKey() != "" || l.tcp.CurrentAddr() != ""
}

func (l *managerLink) endpoint() (string, error) {
	if l == nil || l.tcp == nil {
		return "", errors.New("topicManage is nil")
	}
	if key := l.tcp.RouteKey(); key != "" {
		return key, nil
	}
	addr := l.tcp.CurrentAddr()
	if addr == "" {
		return "", errors.New("topicManage.Conn is nil")
	}
	return addr, nil
}

func reachableRegisters() []*managerLink {
	var out []*managerLink
	for _, l := range activeLinks() {
		if l != nil && l.register && l.rpc != nil && l.connected() {
			out = append(out, l)
		}
	}
	return out
}

func pickPublisher(topic string) *managerLink {
	ready := selectable(true)
	if len(ready) == 0 {
		ready = selectable(false)
	}
	chosen := hrw(topic, ready)
	if chosen != nil && len(ready) > 1 {
		rememberPick(topic, chosen.name)
	}
	return chosen
}

func selectable(requireReady bool) []*managerLink {
	var candidates []*managerLink
	for _, l := range activeLinks() {
		if l == nil || !l.publish || l.tcp == nil || !l.connected() {
			continue
		}
		if requireReady && !l.tcp.Publishable() {
			continue
		}
		candidates = append(candidates, l)
	}
	return candidates
}

func hrw(topic string, links []*managerLink) *managerLink {
	var best *managerLink
	var bestScore uint64
	for _, l := range links {
		h := fnv.New64a()
		_, _ = h.Write([]byte(topic))
		_, _ = h.Write([]byte{0})
		_, _ = h.Write([]byte(l.name))
		score := h.Sum64()
		if best == nil || score > bestScore || (score == bestScore && l.name < best.name) {
			best = l
			bestScore = score
		}
	}
	return best
}

const maxRememberedPicks = 8192

func rememberPick(topic, name string) {
	pickMu.Lock()
	if len(lastPick) >= maxRememberedPicks {
		lastPick = map[string]string{}
	}
	prev, ok := lastPick[topic]
	lastPick[topic] = name
	pickMu.Unlock()
	if !ok || prev == name {
		return
	}
	ManagerFlaps.Add(1)
	now := time.Now().UnixNano()
	last := lastFlapLog.Load()
	if now-last < int64(time.Second) || !lastFlapLog.CompareAndSwap(last, now) {
		return
	}
	callTowerLogger(nil, "WARN", fmt.Sprintf("publish for %s moved from %s to %s", topic, prev, name))
}

func subscribeAll(topics []string, counts []int64, versions []uint64) error {
	targets := reachableRegisters()
	if len(targets) == 0 {
		return errors.New("no reachable manager")
	}
	for _, l := range targets {
		ip, err := l.endpoint()
		if err != nil {
			return err
		}
		req := &pb.SubscribeTopicRequest{Topic: topics, Ip: ip, Count: counts, Version: versions}
		err = callRegister(l, func(ctx context.Context) error {
			_, err := l.rpc.SubscribeTopic(ctx, req)
			return err
		})
		if err != nil {
			// Manager 连着、只是一直答不上来（登记排队、gRPC 在重连）：订阅已经记进本机的整表，
			// 交给后台重放补登记，重试到它收下为止。不能因为 Manager 慢就把这次订阅丢掉。
			// Manager 明确拒绝、或者这台已经断开时，仍然整次失败。
			if retryableRegister(err) && l.connected() {
				replayLinkSoon(l)
				continue
			}
			return err
		}
	}
	return nil
}

// 登记请求带绝对人数和版本号，重发是幂等的。Manager 只是一时忙（登记在排队）或者 gRPC 正在重连时，
// 超时不代表失败，按递增的超时再发；还不行就交给后台重放（见 subscribeAll）。
// Manager 明确返回错误、或者这台的 TCP 已经断开时不重试，照旧整次失败。
var registerTimeouts = []time.Duration{managerRPCTimeout, 2 * managerRPCTimeout, 4 * managerRPCTimeout}

func callRegister(l *managerLink, call func(ctx context.Context) error) error {
	var err error
	for i, d := range registerTimeouts {
		ctx, cancel := context.WithTimeout(context.Background(), d)
		err = call(ctx)
		cancel()
		if err == nil {
			return nil
		}
		if i == len(registerTimeouts)-1 || !retryableRegister(err) || !l.connected() {
			return err
		}
	}
	return err
}

func retryableRegister(err error) bool {
	switch status.Code(err) {
	case codes.DeadlineExceeded, codes.Unavailable:
		return true
	}
	return errors.Is(err, context.DeadlineExceeded)
}

func unsubscribeAll(topics []string, counts []int64, versions []uint64) error {
	targets := reachableRegisters()
	if len(targets) == 0 {
		return errors.New("no reachable manager")
	}
	var failed error
	for _, l := range targets {
		ip, err := l.endpoint()
		if err != nil {
			if failed == nil {
				failed = err
			}
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), managerRPCTimeout)
		_, err = l.rpc.UnSubscribeTopic(ctx, &pb.UnSubscribeTopicRequest{
			Topic: topics, Ip: ip, Count: counts, Version: versions,
		})
		cancel()
		if err != nil {
			// 退订在 Close 里调用，不在这里重试，免得关连接变慢。Manager 上的人数先多着，
			// 后台马上整表重放一次把它盖回去，不用等 3 分钟的对账。
			replayLinkSoon(l)
		}
		if err != nil && failed == nil {
			failed = err
		}
	}
	return failed
}

func managerSpecs(tree *toml.Tree) []managerSpec {
	if tree == nil {
		return nil
	}
	if specs := specsFromValue(tree.Get("manager")); len(specs) > 0 {
		return specs
	}
	tcp, _ := tree.Get("topicServiceAddr").(string)
	grpcAddr := ""
	if grpcTree, ok := tree.Get("grpc").(*toml.Tree); ok && grpcTree != nil {
		grpcAddr, _ = grpcTree.Get("address").(string)
	}
	if tcp == "" && grpcAddr == "" {
		return nil
	}
	return []managerSpec{{name: "default", tcp: tcp, grpc: grpcAddr, register: true, publish: true}}
}

func specsFromValue(raw interface{}) []managerSpec {
	switch rows := raw.(type) {
	case []*toml.Tree:
		return specsFromTrees(rows)
	case []interface{}:
		var trees []*toml.Tree
		for _, row := range rows {
			if t, ok := row.(*toml.Tree); ok {
				trees = append(trees, t)
			}
		}
		return specsFromTrees(trees)
	default:
		return nil
	}
}

func specsFromTrees(rows []*toml.Tree) []managerSpec {
	var specs []managerSpec
	for _, row := range rows {
		if row == nil {
			continue
		}
		tcp, _ := row.Get("tcp").(string)
		grpcAddr, _ := row.Get("grpc").(string)
		if tcp == "" && grpcAddr == "" {
			continue
		}
		name, _ := row.Get("name").(string)
		if name == "" {
			name = tcp
		}
		specs = append(specs, managerSpec{
			name:     name,
			tcp:      tcp,
			grpc:     grpcAddr,
			register: boolOr(row, "register", true),
			publish:  boolOr(row, "publish", true),
		})
	}
	return specs
}

func boolOr(row *toml.Tree, key string, fallback bool) bool {
	value, ok := row.Get(key).(bool)
	if !ok {
		return fallback
	}
	return value
}
