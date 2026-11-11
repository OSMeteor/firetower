package socket

import (
	"os"
	"path/filepath"
	"testing"
)

func writeFile(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestGuard_CgroupMemoryLimit(t *testing.T) {
	cases := []struct {
		name  string
		self  string
		files map[string]string
		want  int64
	}{
		{"v2 with limit", "0::/app\n", map[string]string{"app/memory.max": "2147483648\n"}, 2 << 30},
		{"v2 unlimited", "0::/app\n", map[string]string{"app/memory.max": "max\n"}, 0},
		{"v2 root fallback", "0::/\n", map[string]string{"memory.max": "1073741824\n"}, 1 << 30},
		{"v1 with path", "4:memory:/docker/abc\n0::/\n", map[string]string{"memory/docker/abc/memory.limit_in_bytes": "536870912\n"}, 512 << 20},
		{"v1 unlimited", "4:cpuacct,memory:/x\n", map[string]string{"memory/x/memory.limit_in_bytes": "9223372036854771712\n"}, 0},
		{"v1 root fallback", "4:memory:/gone\n", map[string]string{"memory/memory.limit_in_bytes": "268435456\n"}, 256 << 20},
		{"garbage", "nonsense\n", map[string]string{"memory.max": "abc"}, 0},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			dir := t.TempDir()
			self := filepath.Join(dir, "self-cgroup")
			writeFile(t, self, c.self)
			root := filepath.Join(dir, "cg")
			for p, d := range c.files {
				writeFile(t, filepath.Join(root, p), d)
			}
			if got := cgroupMemoryLimit(self, root); got != c.want {
				t.Fatalf("got %d, want %d", got, c.want)
			}
		})
	}
}

func TestGuard_MeminfoTotal(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "meminfo")
	writeFile(t, p, "MemFree: 1 kB\nMemTotal:        8214980 kB\n")
	if got := meminfoTotal(p); got != 8214980*1024 {
		t.Fatalf("got %d", got)
	}
	if got := meminfoTotal(filepath.Join(dir, "missing")); got != 0 {
		t.Fatalf("missing file gave %d", got)
	}
}
