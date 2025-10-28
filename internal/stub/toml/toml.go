package toml

import (
	"bufio"
	"os"
	"strconv"
	"strings"
)

// Tree is a tiny representation of a TOML document that supports the subset of
// features used by the project: sections and scalar key/value pairs.
type Tree struct {
	data map[string]interface{}
}

// LoadFile reads a TOML file and parses simple assignments into an in-memory
// map. Nested keys are stored using the dotted notation employed throughout the
// codebase (e.g. "bucket.Num").
func LoadFile(path string) (*Tree, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	tree := &Tree{data: make(map[string]interface{})}
	scanner := bufio.NewScanner(f)
	section := ""
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		parts := strings.SplitN(line, "=", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		value := strings.TrimSpace(parts[1])
		fullKey := key
		if section != "" {
			fullKey = section + "." + key
		}

		switch {
		case strings.HasPrefix(value, "\"") && strings.HasSuffix(value, "\""):
			tree.data[fullKey] = strings.Trim(value, "\"")
		case strings.EqualFold(value, "true") || strings.EqualFold(value, "false"):
			tree.data[fullKey] = strings.EqualFold(value, "true")
		default:
			if i, err := strconv.ParseInt(value, 10, 64); err == nil {
				tree.data[fullKey] = i
			} else {
				tree.data[fullKey] = value
			}
		}
	}
	return tree, scanner.Err()
}

// Get returns the parsed value stored under the provided path.
func (t *Tree) Get(path string) interface{} {
	if t == nil {
		return nil
	}
	return t.data[path]
}
