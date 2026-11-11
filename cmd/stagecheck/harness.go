package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// ---------- 结果 ----------

type report struct {
	mu     sync.Mutex
	failed int
	passed int
	lines  []string
}

var rep = &report{}

func (r *report) check(id, what string, err error) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err != nil {
		r.failed++
		line := fmt.Sprintf("FAIL %-8s %s: %v", id, what, err)
		r.lines = append(r.lines, line)
		fmt.Println(line)
		return false
	}
	r.passed++
	fmt.Printf("PASS %-8s %s\n", id, what)
	return true
}

func note(format string, args ...any) {
	fmt.Printf("     "+format+"\n", args...)
}

// ---------- 进程 ----------

type proc struct {
	name   string
	dir    string
	log    string
	env    []string
	cmd    *exec.Cmd
	exited atomic.Bool
	killed atomic.Bool
}

func (p *proc) start() error {
	f, err := os.OpenFile(p.log, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	self, err := os.Executable()
	if err != nil {
		return err
	}
	cmd := exec.Command(self)
	cmd.Dir = p.dir
	cmd.Stdout = f
	cmd.Stderr = f
	// 竞态报告写到子进程自己的目录。相对路径，避开 Windows 盘符里的冒号。
	cmd.Env = append(append(os.Environ(), p.env...), "GORACE=log_path=race halt_on_error=0")
	if err := cmd.Start(); err != nil {
		f.Close()
		return err
	}
	p.cmd = cmd
	p.exited.Store(false)
	p.killed.Store(false)
	go func() {
		_ = cmd.Wait()
		p.exited.Store(true)
		_ = f.Close()
	}()
	return nil
}

func (p *proc) kill() {
	if p == nil || p.cmd == nil || p.cmd.Process == nil {
		return
	}
	p.killed.Store(true)
	_ = p.cmd.Process.Kill()
	deadline := time.Now().Add(5 * time.Second)
	for !p.exited.Load() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
}

func (p *proc) alive() bool {
	return p != nil && p.cmd != nil && !p.exited.Load()
}

type mgr struct {
	name string
	grpc int
	tcp  int
	p    *proc
	// 非空时 Gateway 经这两个代理连 Manager，用来模拟网络故障。测试工具自己的查询仍然直连。
	viaTCP  *netLink
	viaGRPC *netLink
}

func (m *mgr) gwTCP() int {
	if m.viaTCP != nil {
		return m.viaTCP.port
	}
	return m.tcp
}

func (m *mgr) gwGRPC() int {
	if m.viaGRPC != nil {
		return m.viaGRPC.port
	}
	return m.grpc
}

type gw struct {
	id   int
	http string
	p    *proc
}

type cluster struct {
	root     string
	managers []*mgr
	gateways []*gw
	groups   map[string]int
	// 每台 Manager 在 Gateway 配置里的 publish 开关。缺省为真。
	publish map[string]bool
	// 每台 Manager 在 Gateway 配置里的 register 开关。缺省为真。
	register map[string]bool
	// 不写进 Gateway 配置的 Manager。
	hidden map[string]bool
	// 追加到 Gateway 配置末尾之前的顶层键，例如上限。
	extraTop string
	// 追加到 Gateway 配置最后的表，例如 [memory]。
	extraTables string
	// 之后新建的 Manager 进程额外带的环境变量。
	managerEnv []string
	links      []*netLink
}

func newCluster(tag string) (*cluster, error) {
	root, err := os.MkdirTemp("", "ft-stage-"+tag+"-")
	if err != nil {
		return nil, err
	}
	return &cluster{root: root, groups: map[string]int{}, publish: map[string]bool{}, register: map[string]bool{}, hidden: map[string]bool{}}, nil
}

func freePort() int {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		panic(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func waitPort(addr string, d time.Duration) error {
	deadline := time.Now().Add(d)
	var last error
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 300*time.Millisecond)
		if err == nil {
			c.Close()
			return nil
		}
		last = err
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s 没听上: %v", addr, last)
}

func (c *cluster) addManager(name string) (*mgr, error) {
	m := &mgr{name: name, grpc: freePort(), tcp: freePort()}
	dir := filepath.Join(c.root, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	m.p = &proc{name: name, dir: dir, log: filepath.Join(dir, "out.log"),
		env: append([]string{"SC_ROLE=manager", "SC_GRPC=" + strconv.Itoa(m.grpc), "SC_TCP=" + strconv.Itoa(m.tcp)}, c.managerEnv...)}
	c.managers = append(c.managers, m)
	return m, c.startManager(m)
}

func (c *cluster) startManager(m *mgr) error {
	if err := m.p.start(); err != nil {
		return err
	}
	if err := waitPort(fmt.Sprintf("127.0.0.1:%d", m.tcp), 15*time.Second); err != nil {
		return err
	}
	return waitPort(fmt.Sprintf("127.0.0.1:%d", m.grpc), 15*time.Second)
}

func (c *cluster) gatewayToml() string {
	var b strings.Builder
	if !strings.Contains(c.extraTop, "chanLens") {
		b.WriteString("chanLens = 256\n")
	}
	if !strings.Contains(c.extraTop, "heartbeat") {
		b.WriteString("heartbeat = 30\n")
	}
	b.WriteString(c.extraTop)
	visible := 0
	for _, m := range c.managers {
		if !c.hidden[m.name] {
			visible++
		}
	}
	if visible == 1 && len(c.publish) == 0 && len(c.register) == 0 {
		// 单 Manager 用旧的两个键，顺带验证旧配置仍然可用。
		for _, m := range c.managers {
			if c.hidden[m.name] {
				continue
			}
			fmt.Fprintf(&b, "topicServiceAddr = \"127.0.0.1:%d\"\n\n[grpc]\naddress = \"127.0.0.1:%d\"\n", m.gwTCP(), m.gwGRPC())
		}
	}
	b.WriteString("\n[bucket]\nNum = 4\nCentralChanCount = 8192\nBuffChanCount = 1024\nConsumerNum = 4\n")
	if !(visible == 1 && len(c.publish) == 0 && len(c.register) == 0) {
		for _, m := range c.managers {
			if c.hidden[m.name] {
				continue
			}
			pub, ok := c.publish[m.name]
			if !ok {
				pub = true
			}
			reg, ok := c.register[m.name]
			if !ok {
				reg = true
			}
			fmt.Fprintf(&b, "\n[[manager]]\nname = %q\ntcp = \"127.0.0.1:%d\"\ngrpc = \"127.0.0.1:%d\"\nregister = %t\npublish = %t\n", m.name, m.gwTCP(), m.gwGRPC(), reg, pub)
		}
	}
	b.WriteString(c.extraTables)
	for topic, n := range c.groups {
		fmt.Fprintf(&b, "\n[[group]]\ntopic = %q\nshards = %d\n", topic, n)
	}
	return b.String()
}

func (c *cluster) addGateway(id int) (*gw, error) {
	g := &gw{id: id, http: fmt.Sprintf("127.0.0.1:%d", freePort())}
	dir := filepath.Join(c.root, fmt.Sprintf("gw%d", id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	g.p = &proc{name: fmt.Sprintf("gw%d", id), dir: dir, log: filepath.Join(dir, "out.log"),
		env: []string{"SC_ROLE=gateway", "SC_CLUSTER=" + strconv.Itoa(id), "SC_HTTP=" + g.http}}
	c.gateways = append(c.gateways, g)
	return g, c.startGateway(g)
}

// startGateway 写入当前配置并拉起。等到它连上配置里每一台活着的 Manager。
func (c *cluster) startGateway(g *gw) error {
	if err := os.WriteFile(filepath.Join(g.p.dir, "fireTower.toml"), []byte(c.gatewayToml()), 0o644); err != nil {
		return err
	}
	before := countInFile(g.p.log, "[manager client] connected:")
	if err := g.p.start(); err != nil {
		return err
	}
	if err := waitPort(g.http, 15*time.Second); err != nil {
		return err
	}
	want := 0
	for _, m := range c.managers {
		if !c.hidden[m.name] && m.p.alive() {
			want++
		}
	}
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if countInFile(g.p.log, "[manager client] connected:")-before >= want {
			// 握手确认最多等 500ms，之后登记才用路由键。
			time.Sleep(700 * time.Millisecond)
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("%s 没有连上全部 %d 台 Manager", g.p.name, want)
}

func countInFile(path, sub string) int {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0
	}
	return bytes.Count(b, []byte(sub))
}

func (c *cluster) procs() []*proc {
	var out []*proc
	for _, m := range c.managers {
		out = append(out, m.p)
	}
	for _, g := range c.gateways {
		out = append(out, g.p)
	}
	return out
}

func (c *cluster) stop() {
	for _, p := range c.procs() {
		p.kill()
	}
	for _, l := range c.links {
		l.close()
	}
}

var crashRe = regexp.MustCompile(`(?m)^(panic: |fatal error: |unexpected fault address|\[signal )`)

// health 检查没被我们杀掉的进程都还活着，日志里没有未恢复的 panic，也没有竞态报告。
func (c *cluster) health() error {
	var bad []string
	recovered := 0
	for _, p := range c.procs() {
		if !p.killed.Load() && !p.alive() {
			bad = append(bad, p.name+" 进程已退出")
		}
		b, _ := os.ReadFile(p.log)
		if loc := crashRe.FindIndex(b); loc != nil {
			end := loc[0] + 300
			if end > len(b) {
				end = len(b)
			}
			bad = append(bad, fmt.Sprintf("%s 日志里有未恢复的崩溃: %q", p.name, string(b[loc[0]:end])))
		}
		if bytes.Contains(b, []byte("DATA RACE")) {
			bad = append(bad, p.name+" 日志里有 DATA RACE")
		}
		recovered += bytes.Count(b, []byte("PANIC"))
		files, _ := filepath.Glob(filepath.Join(p.dir, "race.*"))
		if len(files) > 0 {
			bad = append(bad, fmt.Sprintf("%s 有 %d 个竞态报告文件: %s", p.name, len(files), files[0]))
		}
	}
	note("已恢复的 PANIC 日志条数: %d（只应出现在故意注入的用例里）", recovered)
	if len(bad) > 0 {
		return fmt.Errorf("%s", strings.Join(bad, "; "))
	}
	return nil
}

// ---------- HTTP 查询 ----------

func httpGet(addr, path string) (string, error) {
	cl := http.Client{Timeout: 5 * time.Second}
	resp, err := cl.Get("http://" + addr + path)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return strings.TrimSpace(string(b)), err
}

func num(g *gw, topic string) (int, error) {
	s, err := httpGet(g.http, "/num?topic="+url.QueryEscape(topic))
	if err != nil {
		return -1, err
	}
	return strconv.Atoi(s)
}

// numIs 等到这台 Gateway 读到的人数等于 want。等不到就报最后读到的值。
func numIs(g *gw, topic string, want int, d time.Duration) error {
	deadline := time.Now().Add(d)
	last := -1
	var lastErr error
	for {
		n, err := num(g, topic)
		last, lastErr = n, err
		if err == nil && n == want {
			return nil
		}
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	if lastErr != nil {
		return fmt.Errorf("gw%d 读 %q 人数失败: %v", g.id, topic, lastErr)
	}
	return fmt.Errorf("gw%d 读到 %q 人数 %d，应为 %d", g.id, topic, last, want)
}

func numEverywhere(gs []*gw, topic string, want int, d time.Duration) error {
	for _, g := range gs {
		if !g.p.alive() {
			continue
		}
		if err := numIs(g, topic, want, d); err != nil {
			return err
		}
	}
	return nil
}

func goroutines(g *gw) int {
	s, err := httpGet(g.http, "/goroutines")
	if err != nil {
		return -1
	}
	n, _ := strconv.Atoi(s)
	return n
}

// ---------- 客户端 ----------

var tokRe = regexp.MustCompile(`tk[0-9]+x`)
var tokSeq atomic.Int64

func newTok() string { return fmt.Sprintf("tk%dx", tokSeq.Add(1)) }

type client struct {
	g        *gw
	uid      string
	query    string
	mu       sync.Mutex
	c        *websocket.Conn
	got      map[string]int
	total    int
	gaps     int // 收到的缺失提示帧数
	lastSize int // 最后一帧的字节数
	subs     []string
	dead     atomic.Bool
	// 置位后不再从连接里读，模拟卡住的客户端。
	stall atomic.Bool
	// 大于 0 时每读一帧就睡这么久，模拟读得慢但一直在线的客户端。
	readDelay atomic.Int64
}

func dial(g *gw, uid string) (*client, error) {
	cl := &client{g: g, uid: uid, got: map[string]int{}}
	return cl, cl.connect()
}

func dialQuery(g *gw, uid, query string) (*client, error) {
	cl := &client{g: g, uid: uid, query: query, got: map[string]int{}}
	return cl, cl.connect()
}

func (c *client) connect() error {
	d := websocket.Dialer{HandshakeTimeout: 5 * time.Second}
	u := "ws://" + c.g.http + "/ws?uid=" + url.QueryEscape(c.uid)
	if c.query != "" {
		u += "&" + c.query
	}
	conn, _, err := d.Dial(u, nil)
	if err != nil {
		return err
	}
	c.mu.Lock()
	old := c.c
	c.c = conn
	c.mu.Unlock()
	if old != nil {
		_ = old.Close()
	}
	c.dead.Store(false)
	// 卡死的客户端连 Pong 也不回。默认的处理会自动回 Pong，那样它一直在续命，模拟不出死连接。
	conn.SetPingHandler(func(data string) error {
		if c.stall.Load() {
			return nil
		}
		err := conn.WriteControl(websocket.PongMessage, []byte(data), time.Now().Add(time.Second))
		if err == websocket.ErrCloseSent {
			return nil
		}
		return err
	})
	go c.read(conn)
	return nil
}

func (c *client) read(conn *websocket.Conn) {
	for {
		for c.stall.Load() {
			time.Sleep(50 * time.Millisecond)
		}
		if d := c.readDelay.Load(); d > 0 {
			time.Sleep(time.Duration(d))
		}
		_ = conn.SetReadDeadline(time.Now().Add(10 * time.Minute))
		_, msg, err := conn.ReadMessage()
		if err != nil {
			c.mu.Lock()
			if c.c == conn {
				c.dead.Store(true)
			}
			c.mu.Unlock()
			return
		}
		c.mu.Lock()
		c.total++
		c.lastSize = len(msg)
		if bytes.Contains(msg, []byte(`"type":"gap"`)) {
			c.gaps++
		}
		for _, tok := range tokRe.FindAll(msg, -1) {
			c.got[string(tok)]++
		}
		c.mu.Unlock()
	}
}

func (c *client) write(v any) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.c == nil {
		return fmt.Errorf("not connected")
	}
	_ = c.c.SetWriteDeadline(time.Now().Add(5 * time.Second))
	return c.c.WriteJSON(v)
}

func (c *client) subscribe(topics ...string) error {
	c.mu.Lock()
	c.subs = append(c.subs, topics...)
	c.mu.Unlock()
	return c.write(map[string]any{"type": "subscribe", "topic": strings.Join(topics, ","), "data": map[string]any{}})
}

func (c *client) unsubscribe(topics ...string) error {
	c.mu.Lock()
	var keep []string
	for _, s := range c.subs {
		drop := false
		for _, t := range topics {
			if s == t {
				drop = true
			}
		}
		if !drop {
			keep = append(keep, s)
		}
	}
	c.subs = keep
	c.mu.Unlock()
	return c.write(map[string]any{"type": "unSubscribe", "topic": strings.Join(topics, ","), "data": map[string]any{}})
}

// reconnect 重新拨号并把原来的订阅重新发一遍，等同于真实客户端断线重连。
func (c *client) reconnect() error {
	if err := c.connect(); err != nil {
		return err
	}
	c.mu.Lock()
	subs := append([]string(nil), c.subs...)
	c.mu.Unlock()
	if len(subs) == 0 {
		return nil
	}
	return c.write(map[string]any{"type": "subscribe", "topic": strings.Join(subs, ","), "data": map[string]any{}})
}

func (c *client) publish(topic, tok string) error {
	raw, _ := json.Marshal(map[string]string{"tok": tok})
	return c.write(map[string]any{"type": "publish", "topic": topic, "data": json.RawMessage(raw)})
}

// publishBig 带一段填充，用来把不读的连接的缓冲区写满。
func (c *client) publishBig(topic, tok string, pad int) error {
	raw, _ := json.Marshal(map[string]string{"tok": tok, "pad": strings.Repeat("p", pad)})
	return c.write(map[string]any{"type": "publish", "topic": topic, "data": json.RawMessage(raw)})
}

func (c *client) count(tok string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.got[tok]
}

func (c *client) close() {
	c.mu.Lock()
	conn := c.c
	c.mu.Unlock()
	if conn != nil {
		_ = conn.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""), time.Now().Add(time.Second))
		_ = conn.Close()
	}
}

func (c *client) name() string { return fmt.Sprintf("%s@gw%d", c.uid, c.g.id) }

// deliver 发一条带唯一标记的消息。want 里每个连接必须恰好收到一次，deny 里的一次都不能收到。
func deliver(pub *client, topic string, want, deny []*client, wait time.Duration) error {
	tok := newTok()
	if err := pub.publish(topic, tok); err != nil {
		return fmt.Errorf("%s 发布 %q 失败: %v", pub.name(), topic, err)
	}
	deadline := time.Now().Add(wait)
	for time.Now().Before(deadline) {
		all := true
		for _, c := range want {
			if c.count(tok) < 1 {
				all = false
				break
			}
		}
		if all {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	// 多等一会儿，重复投递和串到别人那里的消息才看得见。
	time.Sleep(250 * time.Millisecond)
	var missing, dup, leaked []string
	for _, c := range want {
		switch n := c.count(tok); {
		case n == 0:
			missing = append(missing, c.name())
		case n > 1:
			dup = append(dup, fmt.Sprintf("%s×%d", c.name(), n))
		}
	}
	for _, c := range deny {
		if n := c.count(tok); n > 0 {
			leaked = append(leaked, fmt.Sprintf("%s×%d", c.name(), n))
		}
	}
	if len(missing)+len(dup)+len(leaked) == 0 {
		return nil
	}
	var parts []string
	if len(missing) > 0 {
		parts = append(parts, fmt.Sprintf("没收到 %d/%d: %s", len(missing), len(want), clipList(missing)))
	}
	if len(dup) > 0 {
		parts = append(parts, "重复: "+clipList(dup))
	}
	if len(leaked) > 0 {
		parts = append(parts, "没订阅却收到: "+clipList(leaked))
	}
	return fmt.Errorf("topic %q 由 %s 发布: %s", topic, pub.name(), strings.Join(parts, "; "))
}

func clipList(s []string) string {
	if len(s) > 6 {
		return strings.Join(s[:6], ",") + fmt.Sprintf(" 等 %d 个", len(s))
	}
	return strings.Join(s, ",")
}

// deliverUntil 反复发布直到一次完整送达，返回花了多久。用在故障恢复之后。
func deliverUntil(pub *client, topic string, want []*client, total time.Duration) (time.Duration, int, error) {
	start := time.Now()
	var last error
	tries := 0
	for time.Since(start) < total {
		tries++
		if last = deliver(pub, topic, want, nil, 800*time.Millisecond); last == nil {
			return time.Since(start), tries, nil
		}
		// 重复投递不是「还没恢复」，立刻报出来。
		if strings.Contains(last.Error(), "重复") {
			return time.Since(start), tries, last
		}
	}
	return time.Since(start), tries, last
}

func filter(cs []*client, keep func(*client) bool) []*client {
	var out []*client
	for _, c := range cs {
		if keep(c) {
			out = append(out, c)
		}
	}
	return out
}

func onGW(cs []*client, g *gw) []*client {
	return filter(cs, func(c *client) bool { return c.g == g })
}

func notOnGW(cs []*client, g *gw) []*client {
	return filter(cs, func(c *client) bool { return c.g != g })
}

// addGatewayAsync 只拉起进程，不等它连上 Manager。大批量启动时用。
func (c *cluster) addGatewayAsync(id int) (*gw, error) {
	g := &gw{id: id, http: fmt.Sprintf("127.0.0.1:%d", freePort())}
	dir := filepath.Join(c.root, fmt.Sprintf("gw%d", id))
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	g.p = &proc{name: fmt.Sprintf("gw%d", id), dir: dir, log: filepath.Join(dir, "out.log"),
		env: []string{"SC_ROLE=gateway", "SC_CLUSTER=" + strconv.Itoa(id), "SC_HTTP=" + g.http}}
	c.gateways = append(c.gateways, g)
	if err := os.WriteFile(filepath.Join(dir, "fireTower.toml"), []byte(c.gatewayToml()), 0o644); err != nil {
		return nil, err
	}
	return g, g.p.start()
}

func (c *cluster) waitGateway(g *gw, want int) error {
	if err := waitPort(g.http, 60*time.Second); err != nil {
		return err
	}
	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if countInFile(g.p.log, "[manager client] connected:") >= want {
			time.Sleep(700 * time.Millisecond)
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("%s 没有连上 Manager", g.p.name)
}
