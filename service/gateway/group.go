package gateway

import (
	"hash/fnv"
	"strconv"
	"sync"

	"github.com/pelletier/go-toml"
)

// groupSep 把房间名和分片号拼进 Manager 的 Topic。客户端看不到这个键。
const groupSep = "\x1f"

const maxTopicShards = 256

var (
	groupMu     sync.Mutex
	topicGroups = map[string]int{}
)

// SetTopicShards 给一个房间设分片数。小于 2 表示关闭。默认没有任何房间分组，订阅和发布仍用客户端传来的 Topic。
// 打开之后，这个连接只登记自己的一片，一次发布会写到每一片。多台 Manager 时各片会落到不同的发布目标。
func SetTopicShards(topic string, shards int) {
	if topic == "" {
		return
	}
	if shards > maxTopicShards {
		shards = maxTopicShards
	}
	groupMu.Lock()
	defer groupMu.Unlock()
	if shards < 2 {
		delete(topicGroups, topic)
		return
	}
	topicGroups[topic] = shards
}

func topicShards(topic string) int {
	groupMu.Lock()
	n := topicGroups[topic]
	groupMu.Unlock()
	return n
}

func groupsSnapshot() map[string]int {
	groupMu.Lock()
	defer groupMu.Unlock()
	out := make(map[string]int, len(topicGroups))
	for topic, n := range topicGroups {
		out[topic] = n
	}
	return out
}

func restoreGroups(saved map[string]int) {
	groupMu.Lock()
	defer groupMu.Unlock()
	topicGroups = make(map[string]int, len(saved))
	for topic, n := range saved {
		topicGroups[topic] = n
	}
}

func loadGroups(tree *toml.Tree) {
	if tree == nil {
		return
	}
	var rows []*toml.Tree
	switch raw := tree.Get("group").(type) {
	case []*toml.Tree:
		rows = raw
	case []interface{}:
		for _, row := range raw {
			if item, ok := row.(*toml.Tree); ok {
				rows = append(rows, item)
			}
		}
	}
	for _, row := range rows {
		if row == nil {
			continue
		}
		topic, _ := row.Get("topic").(string)
		n, _ := row.Get("shards").(int64)
		SetTopicShards(topic, int(n))
	}
}

func shardUser(t *FireTower) string {
	if t == nil {
		return ""
	}
	if t.UserId != "" {
		return t.UserId
	}
	return t.ClientId
}

func registerKey(topic, user string) string {
	n := topicShards(topic)
	if n < 2 {
		return topic
	}
	h := fnv.New32a()
	_, _ = h.Write([]byte(user))
	return topic + groupSep + strconv.Itoa(int(h.Sum32()%uint32(n)))
}

func publishKeys(topic string) []string {
	n := topicShards(topic)
	if n < 2 {
		return []string{topic}
	}
	out := make([]string, n)
	for i := 0; i < n; i++ {
		out[i] = topic + groupSep + strconv.Itoa(i)
	}
	return out
}

func parentTopic(key string) string {
	for i := 0; i < len(key); i++ {
		if key[i] != groupSep[0] {
			continue
		}
		parent := key[:i]
		if topicShards(parent) >= 2 {
			return parent
		}
	}
	return key
}
