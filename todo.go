package capwap

import (
	"sync"
	"time"
)

// 消息缓存，trap ser发送get、set消息后，将详细id缓存，trap ser接收到消息是response消息时，读取id，并在cache中查找id发送消息
type todoCache struct {
	sync.RWMutex
	m map[uint32]*todo
}

// 消息事件
type todo struct {
	rid  uint32        // snmp请求ID
	when int64         // 时间戳、单位ms
	c    chan *Message // 消息
	du   int64         // 发送报文，接收报文耗时，ms
}

// 返回值是 消息和接收消息耗时
func (o *todoCache) recv(rid uint32, timeout ...time.Duration) (msg *Message, ms int64) {
	o.Lock()
	td, ok := o.m[rid]
	if !ok {
		td = &todo{}
		td.rid = rid
		td.when = time.Now().UnixMilli()
		td.c = make(chan *Message, 5) // 避免有报文重发时c管道阻塞
		o.m[rid] = td
	}
	o.Unlock()

	defer func() {
		o.Lock()
		delete(o.m, rid)
		o.Unlock()
	}()

	out := time.Second
	if len(timeout) > 0 {
		out = timeout[0]
	}
	t := time.After(out) // 响应超时时间为1秒
	select {
	case msg = <-td.c:
		ms = td.du
		return
	case <-t:
		return nil, 0
	}
}

func (o *todoCache) send(msg *Message) {
	if msg.Header == nil {
		return
	}
	rid := msg.Header.GetId()
	o.RLock()
	defer o.RUnlock()
	td, ok := o.m[rid]
	if ok {
		td.du = time.Now().UnixMilli() - td.when
		td.c <- msg
	}
}
