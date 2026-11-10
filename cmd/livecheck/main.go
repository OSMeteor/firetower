// 真实进程验收：同时启动 TopicManager、Gateway 和 WebSocket 客户端。
// 不进入普通 go test。用法：go run ./cmd/livecheck -mode all
package main

import (
	"bytes"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

func main() {
	mode := flag.String("mode", "all", "nw04, lg, or all")
	conns := flag.Int("conns", 1000, "LG 连接数")
	hold := flag.Duration("hold", 60*time.Second, "LG 稳态保持时间")
	topicBin := flag.String("topic", "", "topic service 可执行文件")
	gwBin := flag.String("gateway", "", "gateway 可执行文件")
	flag.Parse()

	root, err := os.Getwd()
	if err != nil {
		fatal(err)
	}
	if *topicBin == "" || *gwBin == "" {
		fatal(fmt.Errorf("需要 -topic 和 -gateway"))
	}
	fmt.Printf("livecheck root=%s mode=%s conns=%d hold=%s\n", root, *mode, *conns, *hold)

	var failed bool
	if *mode == "all" || *mode == "nw04" {
		if err := runNW04(*topicBin, *gwBin); err != nil {
			failed = true
			fmt.Printf("NW04 FAIL: %v\n", err)
		} else {
			fmt.Println("NW04 PASS")
		}
	}
	if *mode == "all" || *mode == "lg" {
		if err := runLG(*topicBin, *gwBin, *conns, *hold); err != nil {
			failed = true
			fmt.Printf("LG FAIL: %v\n", err)
		} else {
			fmt.Println("LG PASS")
		}
	}
	if failed {
		os.Exit(1)
	}
}

type stack struct {
	dir     string
	topic   *exec.Cmd
	gateway *exec.Cmd
	tmLog   string
	gwLog   string
	http    string
	grpc    int
	tcp     int
}

func (s *stack) stop() {
	kill(s.gateway)
	kill(s.topic)
}

func startStack(topicBin, gwBin string) (*stack, error) {
	grpcPort, err := freePort()
	if err != nil {
		return nil, err
	}
	tcpPort, err := freePort()
	if err != nil {
		return nil, err
	}
	httpPort, err := freePort()
	if err != nil {
		return nil, err
	}
	dir, err := os.MkdirTemp("", "firetower-live-")
	if err != nil {
		return nil, err
	}
	tmToml := fmt.Sprintf("heartbeat = 5\nservicetimeout = 8\n\n[grpc]\nport = %d\n\n[socket]\nport = %d\n", grpcPort, tcpPort)
	if err := os.WriteFile(filepath.Join(dir, "topicmanage.toml"), []byte(tmToml), 0o644); err != nil {
		return nil, err
	}
	gwToml := fmt.Sprintf(`chanLens = 256
heartbeat = 30
topicServiceAddr = "127.0.0.1:%d"

[grpc]
address = "127.0.0.1:%d"

[bucket]
Num = 4
CentralChanCount = 8192
BuffChanCount = 1024
ConsumerNum = 4
`, tcpPort, grpcPort)
	if err := os.WriteFile(filepath.Join(dir, "fireTower.toml"), []byte(gwToml), 0o644); err != nil {
		return nil, err
	}

	s := &stack{
		dir:   dir,
		tmLog: filepath.Join(dir, "topic.log"),
		gwLog: filepath.Join(dir, "gateway.log"),
		http:  fmt.Sprintf("127.0.0.1:%d", httpPort),
		grpc:  grpcPort,
		tcp:   tcpPort,
	}
	s.topic, err = startLogged(topicBin, dir, s.tmLog, nil)
	if err != nil {
		return nil, err
	}
	if err := waitPort(fmt.Sprintf("127.0.0.1:%d", tcpPort), 10*time.Second); err != nil {
		s.stop()
		return nil, fmt.Errorf("topic tcp 没听上: %w\n%s", err, tail(s.tmLog))
	}
	if err := waitPort(fmt.Sprintf("127.0.0.1:%d", grpcPort), 10*time.Second); err != nil {
		s.stop()
		return nil, fmt.Errorf("topic grpc 没听上: %w\n%s", err, tail(s.tmLog))
	}
	env := append(os.Environ(), "FIRETOWER_HTTP="+s.http)
	s.gateway, err = startLogged(gwBin, dir, s.gwLog, env)
	if err != nil {
		s.stop()
		return nil, err
	}
	if err := waitPort(s.http, 10*time.Second); err != nil {
		s.stop()
		return nil, fmt.Errorf("gateway 没听上: %w\n%s", err, tail(s.gwLog))
	}
	if err := waitFileContains(s.gwLog, "connected:", 15*time.Second); err != nil {
		s.stop()
		return nil, fmt.Errorf("gateway 没有连上 topic manager: %w\n%s", err, tail(s.gwLog))
	}
	fmt.Printf("stack up http=%s grpc=%d tcp=%d gatewayPID=%d topicPID=%d\n", s.http, s.grpc, s.tcp, s.gateway.Process.Pid, s.topic.Process.Pid)
	return s, nil
}

func runNW04(topicBin, gwBin string) error {
	fmt.Println("--- NW04 拉起 Gateway + TopicManager + 两个客户端 ---")
	s, err := startStack(topicBin, gwBin)
	if err != nil {
		return err
	}
	defer s.stop()
	defer os.RemoveAll(s.dir)

	a, err := dialClient(s.http)
	if err != nil {
		return fmt.Errorf("客户端 A 连接失败: %w", err)
	}
	defer a.close()
	b, err := dialClient(s.http)
	if err != nil {
		return fmt.Errorf("客户端 B 连接失败: %w", err)
	}
	defer b.close()
	if err := a.subscribe("nw04-room"); err != nil {
		return err
	}
	if err := b.subscribe("nw04-room"); err != nil {
		return err
	}
	if err := a.waitContains("onSubscribe", 5*time.Second); err != nil {
		return fmt.Errorf("A 订阅没有走完真实 RPC: %w\n%s", err, tail(s.gwLog))
	}
	if err := b.waitContains("onSubscribe", 5*time.Second); err != nil {
		return fmt.Errorf("B 订阅没有走完真实 RPC: %w\n%s", err, tail(s.gwLog))
	}
	if err := a.publish("nw04-room", map[string]string{"msg": "nw04-payload"}); err != nil {
		return err
	}
	if err := b.waitContains("nw04-payload", 5*time.Second); err != nil {
		return fmt.Errorf("跨 TopicManager 的推送没有到达另一个客户端: %w\n%s", err, tail(s.tmLog))
	}
	fmt.Println("NW04 基线: 订阅和跨连接推送到达")

	fmt.Println("NW04 故障: 杀掉 TopicManager")
	kill(s.topic)
	waitExit(s.topic)
	s.topic = nil

	started := time.Now()
	a.close()
	closeWait := time.Since(started)
	fmt.Printf("NW04 客户端关闭耗时: %s\n", closeWait)
	if closeWait > 2*time.Second {
		return fmt.Errorf("关掉客户端用了 %s，关闭路径被堵住", closeWait)
	}

	probe, err := dialClient(s.http)
	if err != nil {
		return fmt.Errorf("TopicManager 挂了之后 Gateway 不再接受连接: %w", err)
	}
	probe.close()

	time.Sleep(3 * time.Second)
	stuck, profile, err := stuckGoroutines(s.http)
	if err != nil {
		return fmt.Errorf("读 goroutine 栈失败: %w", err)
	}
	fmt.Printf("NW04 故障 3 秒后的阻塞栈:\n%s\n", summarize(profile))
	if stuck != "" {
		return fmt.Errorf("TopicManager 挂掉 3 秒后仍有协程停在分钟级等待:\n%s", stuck)
	}
	if strings.Contains(profile, "unbindTopic") || strings.Contains(profile, ".SubscribeTopic") {
		return fmt.Errorf("TopicManager 挂掉 3 秒后关闭或订阅仍停在 RPC:\n%s", clip(profile))
	}

	fmt.Println("NW04 恢复: 重新拉起 TopicManager")
	restarted := time.Now()
	s.topic, err = startLogged(topicBin, s.dir, s.tmLog, nil)
	if err != nil {
		return err
	}
	if err := waitPort(fmt.Sprintf("127.0.0.1:%d", s.tcp), 10*time.Second); err != nil {
		return fmt.Errorf("TopicManager 没有重新听上: %w\n%s", err, tail(s.tmLog))
	}
	var recovered time.Duration
	var last error
	for time.Since(restarted) < 20*time.Second {
		c, dialErr := dialClient(s.http)
		if dialErr != nil {
			last = dialErr
			time.Sleep(300 * time.Millisecond)
			continue
		}
		if subErr := c.subscribe("nw04-recover"); subErr != nil {
			c.close()
			last = subErr
			time.Sleep(300 * time.Millisecond)
			continue
		}
		last = c.waitContains("onSubscribe", 2*time.Second)
		c.close()
		if last == nil {
			recovered = time.Since(restarted)
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	if recovered == 0 {
		return fmt.Errorf("TopicManager 恢复后 20 秒内新订阅没有成功: %v\n%s", last, tail(s.gwLog))
	}
	fmt.Printf("NW04 恢复耗时: %s\n", recovered)
	return nil
}

func runLG(topicBin, gwBin string, n int, hold time.Duration) error {
	fmt.Printf("--- LG 拉起 %d 连接，保持 %s，然后注入 TopicManager 故障 ---\n", n, hold)
	s, err := startStack(topicBin, gwBin)
	if err != nil {
		return err
	}
	defer s.stop()
	defer os.RemoveAll(s.dir)

	baseG, err := goroutineCount(s.http)
	if err != nil {
		return err
	}
	baseRSS := workingSet(s.gateway.Process.Pid)
	fmt.Printf("LG 基线 goroutine=%d rss=%dKB\n", baseG, baseRSS/1024)

	slowN := n / 20
	chaosN := n / 50
	if chaosN < 1 {
		chaosN = 1
	}
	if chaosN > 20 {
		chaosN = 20
	}
	stableN := n - slowN - chaosN
	if stableN < 1 {
		stableN = 1
	}
	var clients []*wsClient
	var mu sync.Mutex
	sem := make(chan struct{}, 40)
	var wg sync.WaitGroup
	var dialFail atomic.Int64
	for i := 0; i < stableN+slowN; i++ {
		wg.Add(1)
		sem <- struct{}{}
		slow := i >= stableN
		go func(slow bool) {
			defer wg.Done()
			defer func() { <-sem }()
			c, err := dialClient(s.http)
			if err != nil {
				dialFail.Add(1)
				return
			}
			if err := c.subscribe("lg-room"); err != nil {
				dialFail.Add(1)
				c.close()
				return
			}
			if slow {
				c.slow.Store(true)
			}
			mu.Lock()
			clients = append(clients, c)
			mu.Unlock()
		}(slow)
	}
	wg.Wait()
	fmt.Printf("LG 已连接 stable+slow=%d 失败=%d chaos计划=%d\n", len(clients), dialFail.Load(), chaosN)
	if len(clients) < n*8/10 {
		return fmt.Errorf("只连上 %d/%d", len(clients), n)
	}

	stopChaos := make(chan struct{})
	var chaosDial atomic.Int64
	var chaosFail atomic.Int64
	for i := 0; i < chaosN; i++ {
		go func() {
			for {
				select {
				case <-stopChaos:
					return
				default:
				}
				c, err := dialClient(s.http)
				if err != nil {
					chaosFail.Add(1)
					time.Sleep(200 * time.Millisecond)
					continue
				}
				_ = c.subscribe("lg-chaos")
				chaosDial.Add(1)
				time.Sleep(100 * time.Millisecond)
				c.close()
				time.Sleep(200 * time.Millisecond)
			}
		}()
	}

	pubStop := make(chan struct{})
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		i := 0
		for {
			select {
			case <-pubStop:
				return
			case <-t.C:
				if len(clients) == 0 {
					continue
				}
				_ = clients[0].publish("lg-room", map[string]string{"msg": fmt.Sprintf("lg-%d", i)})
				i++
			}
		}
	}()

	sampleAt := []time.Duration{hold / 3, hold * 2 / 3, hold}
	if hold < 3*time.Second {
		sampleAt = []time.Duration{hold}
	}
	start := time.Now()
	var midG int
	var midRSS uint64
	for _, at := range sampleAt {
		wait := at - time.Since(start)
		if wait > 0 {
			time.Sleep(wait)
		}
		g, gerr := goroutineCount(s.http)
		rss := workingSet(s.gateway.Process.Pid)
		midG, midRSS = g, rss
		fmt.Printf("LG t=%s goroutine=%d rss=%dKB\n", time.Since(start).Round(time.Second), g, rss/1024)
		if gerr != nil {
			close(pubStop)
			close(stopChaos)
			return fmt.Errorf("采样失败: %w", gerr)
		}
	}
	close(pubStop)
	close(stopChaos)
	time.Sleep(300 * time.Millisecond)

	fmt.Println("LG03 注入: 杀掉 TopicManager")
	killAt := time.Now()
	kill(s.topic)
	waitExit(s.topic)
	s.topic = nil
	probe, err := dialClient(s.http)
	if err != nil {
		return fmt.Errorf("故障期间 Gateway 不接受新连接: %w", err)
	}
	probeStarted := time.Now()
	probe.close()
	fmt.Printf("LG03 故障期间关闭耗时: %s\n", time.Since(probeStarted))
	stuck, profile, err := stuckGoroutines(s.http)
	if err != nil {
		return err
	}
	if stuck != "" {
		return fmt.Errorf("故障注入后出现分钟级卡住:\n%s", stuck)
	}
	fmt.Printf("LG03 故障栈摘要: %s\n", summarize(profile))

	s.topic, err = startLogged(topicBin, s.dir, s.tmLog, nil)
	if err != nil {
		return err
	}
	if err := waitPort(fmt.Sprintf("127.0.0.1:%d", s.tcp), 10*time.Second); err != nil {
		return fmt.Errorf("故障后 TopicManager 没起来: %w", err)
	}
	recovered := time.Duration(0)
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		c, dialErr := dialClient(s.http)
		if dialErr != nil {
			time.Sleep(300 * time.Millisecond)
			continue
		}
		_ = c.subscribe("lg-recover")
		err = c.waitContains("onSubscribe", 2*time.Second)
		c.close()
		if err == nil {
			recovered = time.Since(killAt)
			break
		}
	}
	if recovered == 0 {
		return fmt.Errorf("故障注入后 20 秒内没有恢复订阅\n%s", tail(s.gwLog))
	}
	fmt.Printf("LG03 从杀掉 TopicManager 到新订阅成功: %s\n", recovered.Round(time.Millisecond))

	for _, c := range clients {
		c.close()
	}
	time.Sleep(3 * time.Second)
	afterG, err := goroutineCount(s.http)
	if err != nil {
		return err
	}
	afterRSS := workingSet(s.gateway.Process.Pid)
	fmt.Printf("LG 关掉 %d 连接后 goroutine=%d (基线 %d) rss=%dKB (峰值采样 %dKB) chaos连接=%d chaos失败=%d\n",
		len(clients), afterG, baseG, afterRSS/1024, midRSS/1024, chaosDial.Load(), chaosFail.Load())
	if afterG > baseG+200 {
		return fmt.Errorf("连接关掉后 goroutine 从 %d 留到 %d", baseG, afterG)
	}
	if midG > baseG+len(clients)*8 {
		return fmt.Errorf("稳态 goroutine %d 相对 %d 个连接过高", midG, len(clients))
	}
	fmt.Printf("LG 稳态窗口是 %s，不是两小时。两小时门禁不能据此判通过。\n", hold)
	return nil
}

type wsClient struct {
	c    *websocket.Conn
	mu   sync.Mutex
	seen [][]byte
	slow atomic.Bool
	dead atomic.Bool
}

func dialClient(httpAddr string) (*wsClient, error) {
	d := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	conn, _, err := d.Dial("ws://"+httpAddr+"/ws", nil)
	if err != nil {
		return nil, err
	}
	cl := &wsClient{c: conn}
	go cl.read()
	return cl, nil
}

func (c *wsClient) read() {
	for {
		_ = c.c.SetReadDeadline(time.Now().Add(90 * time.Second))
		_, msg, err := c.c.ReadMessage()
		if err != nil {
			c.dead.Store(true)
			return
		}
		c.mu.Lock()
		c.seen = append(c.seen, append([]byte(nil), msg...))
		if len(c.seen) > 20 {
			c.seen = c.seen[len(c.seen)-20:]
		}
		c.mu.Unlock()
		if c.slow.Load() {
			time.Sleep(300 * time.Millisecond)
		}
	}
}

func (c *wsClient) subscribe(topic string) error {
	return c.write(map[string]any{"type": "subscribe", "topic": topic, "data": map[string]any{}})
}

func (c *wsClient) publish(topic string, data any) error {
	raw, err := json.Marshal(data)
	if err != nil {
		return err
	}
	return c.write(map[string]any{"type": "publish", "topic": topic, "data": json.RawMessage(raw)})
}

func (c *wsClient) write(v any) error {
	_ = c.c.SetWriteDeadline(time.Now().Add(3 * time.Second))
	return c.c.WriteJSON(v)
}

func (c *wsClient) waitContains(part string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		c.mu.Lock()
		ok := false
		for _, m := range c.seen {
			if bytes.Contains(m, []byte(part)) {
				ok = true
				break
			}
		}
		c.mu.Unlock()
		if ok {
			return nil
		}
		if c.dead.Load() {
			return fmt.Errorf("连接已断开，没看到 %q", part)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("超时，没看到 %q", part)
}

func (c *wsClient) close() {
	_ = c.c.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
	_ = c.c.Close()
}

func stuckGoroutines(httpAddr string) (string, string, error) {
	body, err := getText("http://"+httpAddr+"/debug/pprof/goroutine?debug=2", 8*time.Second)
	if err != nil {
		return "", "", err
	}
	if !strings.Contains(body, "goroutine ") {
		return "", body, fmt.Errorf("pprof debug=2 没有返回栈: %s", clip(body))
	}
	blocks := strings.Split(body, "\n\n")
	var stuck []string
	for _, b := range blocks {
		if !strings.Contains(b, " minutes") && !strings.Contains(b, " minute]") {
			continue
		}
		if strings.Contains(b, "firetower") || strings.Contains(b, "grpc") {
			stuck = append(stuck, b)
		}
	}
	if len(stuck) == 0 {
		return "", body, nil
	}
	return strings.Join(stuck, "\n\n"), body, nil
}

func summarize(profile string) string {
	n := strings.Count(profile, "\ngoroutine ")
	if strings.HasPrefix(profile, "goroutine ") {
		n++
	}
	head := profile
	if len(head) > 180 {
		head = head[:180]
	}
	return fmt.Sprintf("goroutines=%d head=%q", n, head)
}

func goroutineCount(httpAddr string) (int, error) {
	body, err := getText("http://"+httpAddr+"/debug/pprof/goroutine?debug=1", 5*time.Second)
	if err != nil {
		return 0, err
	}
	if i := strings.Index(body, "goroutine profile: total "); i >= 0 {
		rest := body[i+len("goroutine profile: total "):]
		var n int
		if _, err := fmt.Sscanf(rest, "%d", &n); err == nil && n > 0 {
			return n, nil
		}
	}
	n := strings.Count(body, "\ngoroutine ")
	if strings.HasPrefix(body, "goroutine ") {
		n++
	}
	if n == 0 {
		return 0, fmt.Errorf("pprof 没有返回协程栈: %s", clip(body))
	}
	return n, nil
}

func getText(url string, d time.Duration) (string, error) {
	client := http.Client{Timeout: d}
	resp, err := client.Get(url)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	return string(b), nil
}

func startLogged(bin, dir, logPath string, env []string) (*exec.Cmd, error) {
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(bin)
	cmd.Dir = dir
	cmd.Stdout = f
	cmd.Stderr = f
	if env != nil {
		cmd.Env = env
	}
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, err
	}
	go func() {
		_ = cmd.Wait()
		_ = f.Close()
	}()
	return cmd, nil
}

func waitExit(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if cmd.ProcessState != nil && cmd.ProcessState.Exited() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func kill(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
}

func waitPort(addr string, d time.Duration) error {
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		last = err
		time.Sleep(100 * time.Millisecond)
	}
	return last
}

func waitFileContains(path, sub string, d time.Duration) error {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(path)
		if err == nil && strings.Contains(string(b), sub) {
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s 里没有 %q", path, sub)
}

func freePort() (int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port, nil
}

func workingSet(pid int) uint64 {
	out, err := exec.Command("tasklist", "/FI", "PID eq "+strconv.Itoa(pid), "/FO", "CSV", "/NH").Output()
	if err != nil {
		return 0
	}
	parts := strings.Split(strings.TrimSpace(string(out)), ",")
	if len(parts) < 5 {
		return 0
	}
	text := string(out)
	idx := strings.LastIndex(text, "\"")
	if idx <= 0 {
		return 0
	}
	field := strings.TrimSpace(text[:idx])
	if cut := strings.LastIndex(field, "\""); cut >= 0 {
		field = field[cut+1:]
	}
	field = strings.ReplaceAll(field, ",", "")
	field = strings.TrimSuffix(strings.TrimSpace(field), "K")
	field = strings.TrimSpace(field)
	n, _ := strconv.ParseUint(field, 10, 64)
	return n * 1024
}

func clip(s string) string {
	if len(s) > 2500 {
		return s[:2500]
	}
	return s
}

func tail(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return err.Error()
	}
	s := string(b)
	if len(s) > 2000 {
		s = s[len(s)-2000:]
	}
	return s
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
