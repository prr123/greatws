//go:build linux || darwin || netbsd || freebsd || openbsd || dragonfly
// +build linux darwin netbsd freebsd openbsd dragonfly

package greatws

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"unsafe"

	"github.com/antlabs/wsutil/bytespool"
	"github.com/antlabs/wsutil/enum"
	"github.com/antlabs/wsutil/fixedwriter"
	"github.com/antlabs/wsutil/frame"
	"github.com/antlabs/wsutil/mask"
	"github.com/antlabs/wsutil/opcode"
	"golang.org/x/sys/unix"
)

type ioUringOpState uint32

const (
	connInvalid ioUringOpState = 1 << iota
	opRead
	opWrite
	opClose
)

func (s ioUringOpState) String() string {
	switch s {
	case opRead:
		return "read"
	case opWrite:
		return "write"
	case opClose:
		return "close"
	default:
		return "invalid"
	}
}

type ioUringWrite struct {
	free     func()
	writeBuf []byte
}

// 只存放io-uring相关的控制信息
type onlyIoUringState struct {
	wSeq uint32
	m    sync.Map
}

type Conn struct {
	conn

	// 存在io-uring相关的控制信息
	onlyIoUringState

	wbuf             []byte // 写缓冲区, 当直接Write失败时，会将数据写入缓冲区
	mu               sync.Mutex
	client           bool  // 客户端为true，服务端为false
	*Config                // 配置
	closed           int32 // 是否关闭
	waitOnMessageRun sync.WaitGroup
	closeOnce        sync.Once
	parent           *EventLoop
}

func (c *Conn) setParent(el *EventLoop) {
	atomic.StorePointer((*unsafe.Pointer)((unsafe.Pointer)(&c.parent)), unsafe.Pointer(el))
}

func (c *Conn) getParent() *EventLoop {
	return (*EventLoop)(atomic.LoadPointer((*unsafe.Pointer)((unsafe.Pointer)(&c.parent))))
}

func newConn(fd int64, client bool, conf *Config) *Conn {
	rbuf := bytespool.GetBytes(conf.initPayloadSize())
	c := &Conn{
		conn: conn{
			fd:   fd,
			rbuf: rbuf,
		},
		// 初始化不分配内存，只有在需要的时候才分配
		// wbuf:   make([]byte, 0, 1024),
		Config: conf,
		client: client,
	}

	return c
}

func duplicateSocket(socketFD int) (int, error) {
	return unix.Dup(socketFD)
}

func (c *Conn) closeInner(wait bool, err error) {
	c.getLogger().Debug("close conn", slog.Int64("fd", c.fd))
	if true {
		c.waitOnMessageRun.Wait()
	}

	c.multiEventLoop.del(c)
	atomic.StoreInt64(&c.fd, -1)
	c.closeOnce.Do(func() {
		c.OnClose(c, nil)
		atomic.StorePointer((*unsafe.Pointer)((unsafe.Pointer)(&c.parent)), nil)
	})
	atomic.StoreInt32(&c.closed, 1)
}

func (c *Conn) closeAndWaitOnMessage(wait bool, err error) {
	if atomic.LoadInt32(&c.closed) == 1 {
		return
	}
	if wait {
		c.waitOnMessageRun.Wait()
	}

	c.mu.Lock()
	c.closeInner(false, err)
	c.mu.Unlock()
}

func (c *Conn) Close() {
	c.closeAndWaitOnMessage(false, nil)
}

func (c *Conn) Write(b []byte) (n int, err error) {
	// 如果缓冲区有数据，合并数据
	curN := len(b)

	if len(c.wbuf) > 0 {
		c.wbuf = append(c.wbuf, b...)
		b = c.wbuf
	}
	_, err = c.writeOrAddPoll(b)
	if err != nil {
		return 0, err
	}
	// 出错
	return curN, err
}

func (c *Conn) writeOrAddPoll(b []byte) (n int, err error) {
	total := 0
	// i 的目的是debug的时候使用
	for i := 0; len(b) > 0; i++ {

		// 直接写入数据
		n, err = unix.Write(int(c.fd), b)
		// fmt.Printf("1.write %d:%v: %d\n", n, err, len(b))

		if err != nil {
			// 如果是EAGAIN或EINTR错误，说明是写缓冲区满了，或者被信号中断，将数据写入缓冲区
			if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
				if n < 0 {
					n = 0
				}
				if len(b) > 0 {
					// TODO sync.Pool
					newBuf := make([]byte, len(b)-n)
					copy(newBuf, b[n:])

					c.wbuf = newBuf
				}

				if err = c.multiEventLoop.addWrite(c, 0); err != nil {
					return 0, err
				}
				return total, nil
			}
			c.getLogger().Error("writeOrAddPoll", "err", err.Error(), slog.Int64("fd", c.fd), slog.Int("b.len", len(b)))
			go c.closeInner(true, err)
			return
		}
		if n > 0 {
			b = b[n:]
			total += n
		}
	}

	if len(c.wbuf) == total {
		c.wbuf = nil
	}
	return total, nil
}

// 该函数有3个动作
// 写成功
// EAGAIN，等待可写再写
// 报错，直接关闭这个fd
func (c *Conn) flushOrClose() (err error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	_, err = c.writeOrAddPoll(c.wbuf)
	return err
}

// kqueu/epoll模式下，读取数据
// 该函数从缓冲区读取数据，并且解析出websocket frame
// 有几种情况需要处理下
// 1. 缓冲区空间不句够，需要扩容
// 2. 缓冲区数据不够，并且一次性读取了多个frame
func (c *Conn) processWebsocketFrame() (n int, err error) {
	// 1. 处理frame header
	if !c.useIoUring() {
		// 不使用io_uring的直接调用read获取buffer数据
		for i := 0; ; i++ {
			fd := atomic.LoadInt64(&c.fd)
			n, err = unix.Read(int(fd), (*c.rbuf)[c.rw:])
			// fmt.Printf("i = %d, n = %d, fd = %d, rbuf = %d, rw:%d, err = %v, %v, payload:%d\n", i, n, c.fd, len((*c.rbuf)[c.rw:]), c.rw+n, err, time.Now(), c.rh.PayloadLen)
			if err != nil {
				// 信号中断，继续读
				if errors.Is(err, unix.EINTR) {
					continue
				}
				// 出错返回
				if !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EWOULDBLOCK) {
					return 0, err
				}
				// 缓冲区没有数据，等待可读
				err = nil
				break
			}

			// 读到eof，直接关闭
			if n == 0 && len((*c.rbuf)[c.rw:]) > 0 {
				go c.closeAndWaitOnMessage(true, io.EOF)
				c.OnClose(c, io.EOF)
				return
			}

			if n > 0 {
				c.rw += n
			}

			if len((*c.rbuf)[c.rw:]) == 0 {
				// 说明缓存区已经满了。需要扩容
				// 并且如果使用epoll ET mode，需要继续读取，直到返回EAGAIN, 不然会丢失数据
				// 结合以上两种，缓存区满了就直接处理frame，解析出payload的长度，得到一个刚刚好的缓存区
				// for i := 0; len((*c.rbuf)[c.rw:]) == 0 && i < 3; i++ {
				if _, err := c.readHeader(); err != nil {
					return 0, fmt.Errorf("read header err: %w", err)
				}
				if _, err := c.readPayloadAndCallback(); err != nil {
					return 0, fmt.Errorf("read header err: %w", err)
				}

				// TODO
				if len((*c.rbuf)[c.rw:]) == 0 {
					//
					//panic(fmt.Sprintf("需要扩容:rw(%d):rr(%d):currState(%v)", c.rw, c.rr, c.curState.String()))
				}
				continue
			}
		}
	}

	for i := 0; ; i++ {
		sucess, err := c.readHeader()
		if err != nil {
			return 0, fmt.Errorf("read header err: %w", err)
		}

		if !sucess {
			return 0, nil
		}
		sucess, err = c.readPayloadAndCallback()
		if err != nil {
			return 0, fmt.Errorf("read header err: %w", err)
		}

		if !sucess {
			return 0, nil
		}
	}
}

func closeFd(fd int) {
	unix.Close(int(fd))
}

func (c *Conn) WriteFrameOnlyIoUring(fw *fixedwriter.FixedWriter, payload []byte, fin bool, rsv1 bool, isMask bool, code opcode.Opcode, maskValue uint32) (err error) {
	buf := bytespool.GetBytes(len(payload) + enum.MaxFrameHeaderSize)

	var wIndex int
	fw.Reset(*buf)

	if wIndex, err = frame.WriteHeader(*buf, fin, rsv1, false, false, code, len(payload), isMask, maskValue); err != nil {
		goto free
	}

	fw.SetW(wIndex)
	_, err = fw.Write(payload)
	if err != nil {
		goto free
	}
	if isMask {
		mask.Mask(fw.Bytes()[wIndex:], maskValue)
	}

	for i := 0; i < 3; i++ {

		c.mu.Lock()
		c.wSeq++
		if c.wSeq == math.MaxUint16 {
			c.wSeq = 0
		}
		newSeq := c.wSeq
		if _, ok := c.m.Load(newSeq); ok {
			c.mu.Unlock()
			continue
		}

		fb := &ioUringWrite{
			writeBuf: fw.Bytes(),
			free: func() {
				bytespool.PutBytes(buf)
			},
		}
		c.onlyIoUringState.m.Store(newSeq, fb)
		fw.Free()
		c.getLogger().Debug("store seq", slog.Int("seq", int(newSeq)), slog.Int64("fd", c.fd))
		err = c.parent.addWrite(c, uint16(newSeq))
		c.mu.Unlock()
		return
	}

	err = fmt.Errorf("store seq failed")

free:
	fw.Free()
	bytespool.PutBytes(buf)
	return
}
