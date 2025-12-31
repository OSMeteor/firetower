package socket

import (
	"bufio"
	"bytes"
	"encoding/binary"

	"github.com/pkg/errors"
)

// 协议中用到的常量
const (
	ConstHeader       = "FireHeader"
	ConstHeaderLength = 10
	ConstIntLength    = 4 // int转byte长度为4
	ConstSplitSpace   = " "
	ConstNewLine      = '\n'
)

// firetower protocol
// header+messageLength+[pushType]+ConstSplitSpace+[topic]+ConstNewLine+[content]
// |      header       |           type           |       params       |  body  |

// Enpack 封包
func Enpack(pushType, messageId, source, topic string, content []byte) ([]byte, error) {
	if pushType == "" {
		return nil, errors.New("type is empty")
	}
	if topic == "" {
		return nil, errors.New("topic is empty")
	}
	if content == nil {
		return nil, errors.New("content is empty")
	}
	// ConstHeaderConstIntLengthData
	// data = pushType+ConstSplitSpace+topic+ConstSplitSpace+content
	res := []byte(pushType)
	res = append(res, []byte(ConstSplitSpace)...)
	res = append(res, []byte(messageId)...)
	res = append(res, []byte(ConstSplitSpace)...)
	res = append(res, []byte(source)...)
	res = append(res, []byte(ConstSplitSpace)...)
	res = append(res, []byte(topic)...)
	res = append(res, ConstNewLine)
	res = append(res, content...)
	return append(append([]byte(ConstHeader), IntToBytes(len(res))...), res...), nil
}

// Depack 解包
func Depack(buffer []byte, readerChannel chan *SendMessage) ([]byte, error) {
	length := len(buffer)
	var (
		i   int
		err error
	)
	for i = 0; i < length; {
		// 首先判断是否是一个完整的包
		// 最小长度 check
		if length < i+ConstHeaderLength+ConstIntLength {
			break
		}
		// 寻找包的开头
		if string(buffer[i:i+ConstHeaderLength]) == ConstHeader {
			messageLength := BytesToInt(buffer[i+ConstHeaderLength : i+ConstHeaderLength+ConstIntLength])
			if length < i+ConstHeaderLength+ConstIntLength+messageLength {
				// 长度不够，说明是半包，跳出循环等待下一次数据拼接
				break
			}
			
			// 提取数据段
			data := buffer[i+ConstHeaderLength+ConstIntLength : i+ConstHeaderLength+ConstIntLength+messageLength]
			reader := bufio.NewReader(bytes.NewReader(data))
			
			var (
				params  [][]byte
				param   []byte
				content = make([]byte, messageLength)
				n       int
			)

			param, _, err = reader.ReadLine()
			if err != nil {
				// 读取出错，可能是包体有问题，稍微跳过一个字节继续寻找？
				// 或者直接认为这个包损坏，跳过整个长度？
				// 为了稳妥，如果我们确认了头部和长度，应该跳过这个包
				i += ConstHeaderLength + ConstIntLength + messageLength
				continue
			}

			params = bytes.Split(param, []byte(ConstSplitSpace))
			n, err = reader.Read(content)
			if err != nil && err.Error() != "EOF" { // Read might return EOF if content is exactly what's left
				i += ConstHeaderLength + ConstIntLength + messageLength
				continue
			}
			// content should take n bytes. 
			// Wait, reader.Read(content) reads UP TO len(content). 
			// Since we created reader from fixed size data, it should read it all.
			
			// Re-assemble params
			if len(params) > 0 {
				params = append(params, content[:n])
			}

			if len(params) < 3 {
				// 包解析发生错误
				err = errors.New("包解析出错")
				// 仅仅记录错误，不返回，继续解析后续
				i += ConstHeaderLength + ConstIntLength + messageLength
				continue 
			}
			
			sendMessage := GetSendMessage(string(params[1]), string(params[2]))
			if len(params) > 0 {
				sendMessage.Type = string(params[0])
			}
			if len(params) > 3 {
				sendMessage.Topic = string(params[3])
			}
			// The last one is data
			if len(params) > 4 {
				sendMessage.Data = params[4]
			} else if len(params) == 4 {
				// maybe data is empty? params[3] is topic, params[4] is content.
				// user code: params = append(params, content[:n])
				// split produced [type, id, source, topic] (4 elements)
				// plus content -> 5 elements.
				// If split produced fewer...
			}
			// Using original logic strict check:
			// Original: 
			// params = bytes.Split(param, ...)
			// params = append(params, content[:n])
			// if len(params) < 3 ...
			// sendMessage.Type = string(params[0])
			// sendMessage.Topic = string(params[3]) -> panic if len < 4
			// sendMessage.Data = params[4] -> panic if len < 5
			
			// Defensive coding:
			if len(params) >= 5 {
				sendMessage.Type = string(params[0])
				sendMessage.Topic = string(params[3])
				sendMessage.Data = params[4]
				readerChannel <- sendMessage
			}
			
			// IMPORTANT: Advance index
			i += ConstHeaderLength + ConstIntLength + messageLength
		} else {
			// 如果不是 Header，说明可能是垃圾数据或者上一个包的尾部（理论上不该出现）
			// 逐字节后移
			i++
		}
	}
	
	if i >= length {
		return make([]byte, 0), nil
	}
	return buffer[i:], nil // 返回剩馀部分
}

// IntToBytes 整形转换成字节
func IntToBytes(n int) []byte {
	x := int32(n)

	bytesBuffer := bytes.NewBuffer([]byte{})
	binary.Write(bytesBuffer, binary.BigEndian, x)
	return bytesBuffer.Bytes()
}

// BytesToInt 字节转换成整形
func BytesToInt(b []byte) int {
	bytesBuffer := bytes.NewBuffer(b)

	var x int32
	binary.Read(bytesBuffer, binary.BigEndian, &x)

	return int(x)
}
