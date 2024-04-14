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

	grpc "google.golang.org/grpc"
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
	ctx                              context.Context
	cancel                           context.CancelFunc
	running                          int // 0:stop, 1:running, 2:closing
	retryCount                       int // number of retry attempts
	udpHandler                       func(*Packet)
	tc                               todoCache
	conn                             *net.UDPConn
	requestid                        uint32
	UnimplementedFileTransportServer // capwap tcp传输服务
	tcphandlerGet                    func(ctx context.Context, in *Message) (*MsgFile, error)
	tcphandlerPut                    func(ctx context.Context, in *MsgFile) (*Message, error)
}

// tcp链接获取文件
func (s *Server) GetFile(ctx context.Context, msg *Message) (*MsgFile, error) {
	return s.tcphandlerGet(ctx, msg)
}

// tcp链接获取文件
func (s *Server) PetFile(ctx context.Context, file *MsgFile) (*Message, error) {
	return s.tcphandlerPut(ctx, file)
}

// 每个server统一维护自己的get requestID，new返回下一个可用reqid
func (s *Server) NewRequestId() uint32 {
	return atomic.AddUint32(&s.requestid, 1)
}

// 启动capwap服务,后台运行;
// 接收到snmp的reponse消息后，会将消息穿入到chan中【接收者】
func (s *Server) Start(addr ...string) error {
	if s.running > 0 {
		return fmt.Errorf("capwap udp server is running")
	}
	localAddr := ":5008"
	if len(addr) > 0 {
		localAddr = addr[0]
	}
	go s.servUdp(localAddr)
	go s.servTcp(localAddr)
	return nil
}
func (s *Server) servUdp(addr string) error {
	var err error
	ip := "0.0.0.0"
	port := 5008
	ss := strings.Split(addr, ":")
	if len(ss) > 2 {
		if len(ss[0]) > 7 {
			ip = ss[0]
		}
		if p, err := strconv.Atoi(ss[1]); err == nil {
			port = p
		}
	}

	s.conn, err = net.ListenUDP("udp", &net.UDPAddr{
		IP:   net.ParseIP(ip),
		Port: port,
	})
	if err != nil {
		return err
	}
	logger.Info("capwap server listening on  %s:%d", ip, port)
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
			if n > 0 {
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
					logger.Error("recv data parse to Message err:", err)
				}
			}
		}
	}
}
func (s *Server) servTcp(addr string) error {
	lis, err := net.Listen("tcp", addr)
	if err != nil {
		logger.Error("failed to listen: %v", err)
		return err
	}
	maxSize := 100 * 1024 * 1024 // 设置最大传输数据，默认是4MB，需要修改
	rpc := grpc.NewServer(
		grpc.MaxRecvMsgSize(maxSize),
		grpc.MaxSendMsgSize(maxSize),
	)
	// s := grpc.NewServer()
	// 注册 User 模块
	RegisterFileTransportServer(rpc, s)

	logger.Info("capwap tcp server listening on %v", lis.Addr())
	if err := rpc.Serve(lis); err != nil {
		logger.Error("failed to serve[capwap.tcp]: %v", err)
		return err
	}
	return nil
}

// 停止server的服务状态
func (s *Server) Stop() error {
	if s.running == 0 {
		return fmt.Errorf("udp server is not running")
	}
	if s.running == 2 {
		return fmt.Errorf("udp server is closing")
	}
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
	if s.udpHandler == nil {
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
		s.udpHandler(pkt)
		c <- struct{}{}
	}()
	// 等待任务退出，或者超时退出
	for {
		select {
		case <-ctx.Done():
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
func NewServer(udpHandler func(pkt *Packet),
	tcpGetHandler func(ctx context.Context, in *Message) (*MsgFile, error),
	tcpPutHandler func(ctx context.Context, in *MsgFile) (*Message, error)) *Server {
	s := Server{}
	s.tc = todoCache{}
	s.tc.m = make(map[uint32]*todo)
	s.udpHandler = udpHandler
	s.tcphandlerGet = tcpGetHandler
	s.tcphandlerPut = tcpPutHandler
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
