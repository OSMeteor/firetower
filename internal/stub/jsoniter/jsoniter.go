package jsoniter

import "encoding/json"

// RawMessage is an alias to encoding/json.RawMessage to remain compatible with
// the json-iterator/go API used in the project.
type RawMessage = json.RawMessage

// Marshal proxies to the standard library JSON encoder.
func Marshal(v interface{}) ([]byte, error) {
	return json.Marshal(v)
}

// Unmarshal proxies to the standard library JSON decoder.
func Unmarshal(data []byte, v interface{}) error {
	return json.Unmarshal(data, v)
}
