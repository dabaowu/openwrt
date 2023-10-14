package capwap

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"log/slog"

	"google.golang.org/protobuf/proto"
)

// 启动任意go程, 让server接管的go程
type AnyFunc func(ctx context.Context, cancel context.CancelFunc, a ...interface{})

// 字节流解析成snmp报文
type Packet struct {
	Msg        *Message
	RemoteAddr *net.UDPAddr
	Ctx        context.Context // 期望暴露给go程，用于创建子go程
}

// 定义ctx key类型
type ctx_key string

const __key_ctx_response_time__ ctx_key = "__key_ctx_response_time__"

func (p *Packet) setResponseTime(ms int64) {
	if p.Ctx == nil {
		p.Ctx = context.WithValue(context.Background(), __key_ctx_response_time__, ms)
		return
	}
	p.Ctx = context.WithValue(p.Ctx, __key_ctx_response_time__, ms)
}

// server发送get-request请求，接收到响应时，会把响应时长设置在ctx中，通过ctx获取
func (p *Packet) GetResponseTime() (ms int64) {
	if p.Ctx == nil {
		return 0
	}
	m := p.Ctx.Value(__key_ctx_response_time__)
	if m == nil {
		return 0
	}
	s, yes := m.(int64)
	if !yes {
		return 0
	}
	ms = s
	return
}

// capwap UDP server
type Server struct {
	sync.RWMutex // 当有报文处理的go程执行时，应加锁
	sync.WaitGroup
	ctx        context.Context
	cancel     context.CancelFunc
	running    int // 0:stop, 1:running, 2:closing
	retryCount int // number of retry attempts
	handler    func(*Packet)
	tc         todoCache
	conn       *net.UDPConn
	requestid  uint32
}

// 每个server统一维护自己的get requestID，new返回下一个可用reqid
func (s *Server) NewRequestId() uint32 {
	return atomic.AddUint32(&s.requestid, 1)
}

// server处于阻塞监听接收网络报文处理服务
// 接收到snmp的reponse消息后，会将消息穿入到chan中【接收者】
func (s *Server) Serve(addr ...string) error {
	if s.running > 0 {
		return fmt.Errorf("capwap udp server is running")
	}
	var err error
	ip := ""
	port := 5007
	if len(addr) > 0 {
		ss := strings.Split(addr[0], ":")
		if len(ss) == 2 {
			ip = ss[0]
			port, _ = strconv.Atoi(ss[1])
			if port < 1 || port > 65534 {
				port = 5007
			}
		}
	}
	s.conn, err = net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.ParseIP(ip),
		Port: port,
	})
	if err != nil {
		return err
	}
	if ip == "" { // 日志输出显示
		ip = "[::]"
	}
	slog.Debug("capwap server listening on  %s:%d", ip, port)
	// 服务启动时创建上下文，控制服务退出
	s.ctx, s.cancel = context.WithCancel(context.Background())
	s.running = 1

	// 接收报文处理
	buf := make([]byte, 8192)
	for {
		select {
		case <-s.ctx.Done():
			s.Wait()
			return nil
		default:
			s.conn.SetReadDeadline(time.Now().Add(time.Second * 2)) // 避免长时间读阻塞
			n, raddr, err := s.conn.ReadFromUDP(buf)
			if err != nil {
				continue
			}
			if err == nil && n > 0 {
				msg := &Message{}
				err := proto.Unmarshal(buf[:n], msg)
				if err == nil {
					// 若是响应消息，则发送到管道中
					if msg.Type == MsgType_Response {
						s.tc.send(msg)
						continue
					}
					// 不是响应，则是trap or inform请求，启动一个新的go成处理
					pkt := Packet{}
					pkt.Msg = msg
					pkt.RemoteAddr = raddr
					s.Add(1)
					go s.h(&pkt)
				} else {
					slog.Error("recv data parse to Message err:", err)
				}
			}
		}
	}
}

// 停止server的服务状态
func (s *Server) Stop() error {
	if s.running == 0 {
		return fmt.Errorf("udp server is not running")
	}
	if s.running == 2 {
		return fmt.Errorf("udp server is closing")
	}
	slog.Debug("user manually stopped")
	s.cancel()
	s.running = 2 // 服务状态，关闭中
	s.Wait()
	s.running = 0 // 服务状态，未启动
	return nil
}

// serve状态
func (s *Server) IsRunning() bool {
	return s.running != 0
}

// 调用注入的处理函数,设置执行超时
func (s *Server) h(pkt *Packet) {
	defer s.Done()
	if s.handler == nil {
		slog.Error("snmp trap server have not assign handler")
		return
	}

	// 为启动新的任务，添加上下文
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(time.Second*10))
	defer cancel()
	pkt.Ctx = ctx

	// 任务执行期间，禁止服务退出
	s.RLock()
	defer s.RUnlock()

	// 异步执行任务
	c := make(chan struct{}, 1)
	go func() {
		s.handler(pkt)
		c <- struct{}{}
	}()
	// 等待任务退出，或者超时退出
	for {
		select {
		case <-ctx.Done():
			raddr := ""
			if pkt.RemoteAddr != nil {
				raddr = pkt.RemoteAddr.String()
			}
			slog.Warn("trap server handle msg-pkt timeout, msg.type=%v,raddr=%v", pkt.Msg.Type, raddr)
			return
		case <-c:
			return
		}
	}
}

// 发送数据，返回响应数据[set\get\getnet的 response]
// 如果server配置了retryCnt，接收超时[1秒]会自动重发。
func (s *Server) Send(pkt *Packet, timeout ...time.Duration) (res *Packet, err error) {
	if pkt.Msg == nil || pkt.RemoteAddr == nil {
		return nil, fmt.Errorf("don't be send, msg and raddr don't be nil")
	}
	if pkt.Msg == nil {
		return nil, fmt.Errorf("don't be send,msg is nil")
	}

	switch pkt.Msg.Type {
	case MsgType_SetRequest, MsgType_GetRequest, MsgType_Inform: // 发送的是请求消息，需要接收响应
		{
			var bs []byte
			bs, err = proto.Marshal(pkt.Msg)
			if err != nil {
				return nil, err
			}
			retry := 0
		labRetry:
			err = s.conn.SetWriteDeadline(time.Now().Add(time.Second))
			if err != nil {
				return nil, err
			}
			_, err = s.conn.WriteToUDP(bs, pkt.RemoteAddr)
			if err != nil {
				return nil, err
			}
			// 发送后需要接收响应
			if pkt.Msg.Header == nil {
				return nil, fmt.Errorf("send successfuly, but no header")
			}
			rid := pkt.Msg.Header.Id
			msg, ms := s.tc.recv(rid, timeout...)
			if msg == nil { // 接收超时（1秒），重发
				if retry < s.retryCount {
					retry++
					goto labRetry
				}
				return nil, fmt.Errorf("received timeout")
			}
			res = &Packet{}
			res.Ctx = context.Background()
			res.Msg = msg
			res.RemoteAddr = pkt.RemoteAddr
			pkt.setResponseTime(ms)
			return
		}
	default: // response,trap
		// 不需要接收响应
		var bs []byte
		bs, err = proto.Marshal(pkt.Msg)
		if err != nil {
			return nil, err
		}
		err = s.conn.SetWriteDeadline(time.Now().Add(time.Second))
		if err != nil {
			return nil, err
		}
		_, err = s.conn.WriteToUDP(bs, pkt.RemoteAddr)
		return nil, err
	}
}

// 返回一个新的，初始化的serverr
func NewServer(h func(pkt *Packet)) *Server {
	s := Server{}
	s.tc = todoCache{}
	s.tc.m = make(map[uint32]*todo)
	s.handler = h
	return &s
}

// 设置get-set请求超时后重发次数，0-3之间
// 0:发送采集超时不重发采集
// 2:发送采集超时后会在会送两次重新采集超时，间隔1秒，累积采集3次
func (s *Server) SetRetryTimes(c int) {
	if c < 0 || c > 3 {
		c = 0
	}
	s.retryCount = c
}
