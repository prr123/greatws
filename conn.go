// Copyright 2021-2023 antlabs. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//	http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package bigws

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"sync"
	"sync/atomic"
	"time"

	"github.com/antlabs/wsutil/errs"
	"github.com/antlabs/wsutil/fixedwriter"
	"github.com/antlabs/wsutil/frame"
	"github.com/antlabs/wsutil/opcode"
	"golang.org/x/sys/unix"
)

const (
	maxControlFrameSize = 125
)

type FrameHeader struct {
	PayloadLen int64
	Opcode     opcode.Opcode
	MaskKey    uint32
	Mask       bool
	head       byte
}

type frameState int

const (
	frameStateHeaderStart frameState = iota
	frameStateHeaderPayloadAndMask
	frameStatePayload
)

type conn struct {
	fd       int    // 文件描述符fd
	rbuf     []byte // 读缓冲区
	ri       int    // 读索引
	wbuf     []byte // 写缓冲区, 当直接Write失败时，会将数据写入缓冲区
	curState frameState
	haveSize int
	rh       FrameHeader

	fragmentFramePayload []byte // 存放分片帧的缓冲区
	fragmentFrameHeader  *frame.FrameHeader
}

type Conn struct {
	conn

	mu      sync.Mutex
	client  bool  // 客户端为true，服务端为false
	*Config       // 配置
	closed  int32 // 是否关闭
}

func (c *conn) Write(b []byte) (n int, err error) {
	// 如果缓冲区有数据，将数据写入缓冲区
	curN := len(b)
	if len(c.wbuf) > 0 {
		c.wbuf = append(c.wbuf, b...)
		b = c.wbuf
	}

	// 直接写入数据
	n, err = unix.Write(c.fd, b)
	if err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			newBuf := make([]byte, len(b)-n)
			copy(newBuf, b[n:])
			c.wbuf = newBuf
			return curN, nil
		}
	}
	// 出错
	return n, err
}

func (c *Conn) getFd() int {
	return c.fd
}

// 基于状态机解析frame
func (c *Conn) readHeader() (err error) {
	state := c.curState
	// 开始解析frame
	if state == frameStateHeaderStart {
		// fin rsv1 rsv2 rsv3 opcode
		if len(c.rbuf)-c.ri < 2 {
			return
		}
		c.rh.head = c.rbuf[c.ri]

		// h.Fin = head[0]&(1<<7) > 0
		// h.Rsv1 = head[0]&(1<<6) > 0
		// h.Rsv2 = head[0]&(1<<5) > 0
		// h.Rsv3 = head[0]&(1<<4) > 0
		c.rh.Opcode = opcode.Opcode(c.rh.head & 0xF)

		maskAndPayloadLen := c.rbuf[c.ri+1]
		have := 0
		c.rh.Mask = maskAndPayloadLen&(1<<7) > 0

		if c.rh.Mask {
			have += 4
			// size += 4
		}

		c.rh.PayloadLen = int64(maskAndPayloadLen & 0x7F)
		switch {
		// 长度
		case c.rh.PayloadLen >= 0 && c.rh.PayloadLen <= 125:
			if c.rh.PayloadLen == 0 && !c.rh.Mask {
				return
			}
		case c.rh.PayloadLen == 126:
			// 2字节长度
			have += 2
			// size += 2
		case c.rh.PayloadLen == 127:
			// 8字节长度
			have += 8
			// size += 8
		default:
			// 预期之外的, 直接报错
			return errs.ErrFramePayloadLength
		}
		c.curState, state = frameStateHeaderPayloadAndMask, frameStateHeaderPayloadAndMask
		c.haveSize = have
		c.ri += 2
	}

	if state == frameStateHeaderPayloadAndMask {
		if len(c.rbuf)-c.ri < c.haveSize {
			return
		}
		have := c.haveSize
		head := c.rbuf[c.ri : c.ri+have]
		switch c.rh.PayloadLen {
		case 126:
			c.rh.PayloadLen = int64(binary.BigEndian.Uint16(head[:2]))
			head = head[2:]
		case 127:
			c.rh.PayloadLen = int64(binary.BigEndian.Uint64(head[:8]))
			head = head[8:]
		}

		if c.rh.Mask {
			c.rh.MaskKey = binary.LittleEndian.Uint32(head[:4])
		}
		c.curState = frameStatePayload
	}

	return
}

func (c *Conn) failRsv1(op opcode.Opcode) bool {
	// 解压缩没有开启
	if !c.decompression {
		return true
	}

	// 不是text和binary
	if op != opcode.Text && op != opcode.Binary {
		return true
	}

	return false
}

func decode(payload []byte) ([]byte, error) {
	r := bytes.NewReader(payload)
	r2 := decompressNoContextTakeover(r)
	var o bytes.Buffer
	if _, err := io.Copy(&o, r2); err != nil {
		return nil, err
	}
	r2.Close()
	return o.Bytes(), nil
}

func (c *Conn) processCallback(f frame.Frame) (err error) {
	op := f.Opcode
	if c.fragmentFrameHeader != nil {
		op = c.fragmentFrameHeader.Opcode
	}

	rsv1 := f.GetRsv1()
	// 检查Rsv1 rsv2 Rfd, errsv3
	if rsv1 && c.failRsv1(op) || f.GetRsv2() || f.GetRsv3() {
		err = fmt.Errorf("%w:Rsv1(%t) Rsv2(%t) rsv2(%t) compression:%t", ErrRsv123, rsv1, f.GetRsv2(), f.GetRsv3(), c.compression)
		return c.writeErrAndOnClose(ProtocolError, err)
	}

	fin := f.GetFin()
	if c.fragmentFrameHeader != nil && !f.Opcode.IsControl() {
		if f.Opcode == 0 {
			c.fragmentFramePayload = append(c.fragmentFramePayload, f.Payload...)

			// 分段的在这返回
			if fin {
				// 解压缩
				if c.fragmentFrameHeader.GetRsv1() && c.decompression {
					tempBuf, err := decode(c.fragmentFramePayload)
					if err != nil {
						return err
					}
					c.fragmentFramePayload = tempBuf
				}
				// 这里的check按道理应该放到f.Fin前面， 会更符合rfc的标准, 前提是c.utf8Check修改成流式解析
				// TODO c.utf8Check 修改成流式解析
				if c.fragmentFrameHeader.Opcode == opcode.Text && !c.utf8Check(c.fragmentFramePayload) {
					c.Callback.OnClose(c, ErrTextNotUTF8)
					return ErrTextNotUTF8
				}

				c.Callback.OnMessage(c, c.fragmentFrameHeader.Opcode, c.fragmentFramePayload)
				c.fragmentFramePayload = c.fragmentFramePayload[0:0]
				c.fragmentFrameHeader = nil
			}
			return nil
		}

		c.writeErrAndOnClose(ProtocolError, ErrFrameOpcode)
		return ErrFrameOpcode
	}

	if f.Opcode == opcode.Text || f.Opcode == opcode.Binary {
		if !fin {
			prevFrame := f.FrameHeader
			// 第一次分段
			if len(c.fragmentFramePayload) == 0 {
				c.fragmentFramePayload = append(c.fragmentFramePayload, f.Payload...)
				f.Payload = nil
			}

			// 让fragmentFrame的Payload指向readBuf, readBuf 原引用直接丢弃
			c.fragmentFrameHeader = &prevFrame
			return
		}

		if rsv1 && c.decompression {
			// 不分段的解压缩
			f.Payload, err = decode(f.Payload)
			if err != nil {
				return err
			}
		}

		if f.Opcode == opcode.Text {
			if !c.utf8Check(f.Payload) {
				c.Close()
				c.Callback.OnClose(c, ErrTextNotUTF8)
				return ErrTextNotUTF8
			}
		}

		c.Callback.OnMessage(c, f.Opcode, f.Payload)
		return
	}

	if f.Opcode == Close || f.Opcode == Ping || f.Opcode == Pong {
		//  对方发的控制消息太大
		if f.PayloadLen > maxControlFrameSize {
			c.writeErrAndOnClose(ProtocolError, ErrMaxControlFrameSize)
			return ErrMaxControlFrameSize
		}
		// Close, Ping, Pong 不能分片
		if !fin {
			c.writeErrAndOnClose(ProtocolError, ErrNOTBeFragmented)
			return ErrNOTBeFragmented
		}

		if f.Opcode == Close {
			if len(f.Payload) == 0 {
				return c.writeErrAndOnClose(NormalClosure, ErrClosePayloadTooSmall)
			}

			if len(f.Payload) < 2 {
				return c.writeErrAndOnClose(ProtocolError, ErrClosePayloadTooSmall)
			}

			if !c.utf8Check(f.Payload[2:]) {
				return c.writeErrAndOnClose(ProtocolError, ErrTextNotUTF8)
			}

			code := binary.BigEndian.Uint16(f.Payload)
			if !validCode(code) {
				return c.writeErrAndOnClose(ProtocolError, ErrCloseValue)
			}

			// 回敬一个close包
			if err := c.WriteTimeout(Close, f.Payload, 2*time.Second); err != nil {
				return err
			}

			err = bytesToCloseErrMsg(f.Payload)
			c.Callback.OnClose(c, err)
			return err
		}

		if f.Opcode == Ping {
			// 回一个pong包
			if c.replyPing {
				if err := c.WriteTimeout(Pong, f.Payload, 2*time.Second); err != nil {
					c.Callback.OnClose(c, err)
					return err
				}
				c.Callback.OnMessage(c, f.Opcode, f.Payload)
				return
			}
		}

		if f.Opcode == Pong && c.ignorePong {
			return
		}

		c.Callback.OnMessage(c, f.Opcode, nil)
		return
	}
	// 检查Opcode
	c.writeErrAndOnClose(ProtocolError, ErrOpcode)
	return ErrOpcode
}

func (c *Conn) writeErrAndOnClose(code StatusCode, userErr error) error {
	defer c.Callback.OnClose(c, userErr)
	if err := c.WriteTimeout(opcode.Close, statusCodeToBytes(code), 2*time.Second); err != nil {
		return err
	}

	return userErr
}

func (c *Conn) WriteTimeout(op Opcode, data []byte, t time.Duration) (err error) {
	// TODO 超时时间
	return c.WriteMessage(op, data)
}

func (c *Conn) readPayloadAndCallback() {
	if true {
		var f frame.Frame
		c.processCallback(f)
	}
}

func (c *Conn) processWebsocketFrame() {
	// 1. 处理frame header
	c.readHeader()

	// 2. 处理frame payload
	// TODO 这个函数要放到协程里面运行
	c.readPayloadAndCallback()
}

// 该函数有3个动作
// 写成功
// EAGAIN，等待可写再写
// 报错，直接关闭这个fd
func (c *Conn) flushOrClose() {
	c.mu.Lock()
	defer c.mu.Unlock()

	n, err := unix.Write(c.fd, c.wbuf)
	if err != nil {
		if errors.Is(err, unix.EAGAIN) || errors.Is(err, unix.EINTR) {
			wbuf := c.wbuf
			copy(wbuf, wbuf[n:])
			c.wbuf = wbuf[:len(wbuf)-n]
			return
		}
		unix.Close(c.fd)
		atomic.StoreInt32(&c.closed, 1)
	}
}

type wrapBuffer struct {
	bytes.Buffer
}

func (w *wrapBuffer) Close() error {
	return nil
}

func (c *Conn) WriteMessage(op Opcode, writeBuf []byte) (err error) {
	if atomic.LoadInt32(&c.closed) == 1 {
		return ErrClosed
	}

	if op == opcode.Text {
		if !c.utf8Check(writeBuf) {
			return ErrTextNotUTF8
		}
	}

	rsv1 := c.compression && (op == opcode.Text || op == opcode.Binary)
	if rsv1 {
		var out wrapBuffer
		w := compressNoContextTakeover(&out, defaultCompressionLevel)
		if _, err = io.Copy(w, bytes.NewReader(writeBuf)); err != nil {
			return
		}

		if err = w.Close(); err != nil {
			return
		}
		writeBuf = out.Bytes()
	}

	// f.Opcode = op
	// f.PayloadLen = int64(len(writeBuf))
	maskValue := uint32(0)
	if c.client {
		maskValue = rand.Uint32()
	}

	var fw fixedwriter.FixedWriter
	c.mu.Lock()
	err = frame.WriteFrame(&fw, &c.conn, writeBuf, true, rsv1, c.client, op, maskValue)
	c.mu.Unlock()
	return err
}

func (c *Conn) Close() {
}