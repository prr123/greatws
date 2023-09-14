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

//go:build linux
// +build linux

package bigws

import (
	"time"

	"golang.org/x/sys/unix"
)

type apiState struct {
	epfd   int
	events []unix.EpollEvent
}

// 创建
func (e *EventLoop) apiCreate() (err error) {
	var state apiState

	state.epfd, err = unix.EpollCreate1(0)
	if err != nil {
		return err
	}

	state.events = make([]unix.EpollEvent, 1024)
	e.apidata = &state
	return nil
}

// 调速大小
func (e *EventLoop) apiResize(setSize int) {
	oldEvents := e.apidata.events
	newEvents := make([]unix.EpollEvent, setSize)
	copy(newEvents, oldEvents)
	e.apidata.events = newEvents
}

// 释放
func (e *EventLoop) apiFree() {
	unix.Close(e.apidata.epfd)
}

// 新加读事件
func (e *EventLoop) addRead(fd int) error {
	state := e.apidata

	return unix.EpollCtl(state.epfd, unix.EPOLL_CTL_ADD, fd, &unix.EpollEvent{Fd: int32(fd), Events: unix.EPOLLERR | unix.EPOLLHUP | unix.EPOLLRDHUP | unix.EPOLLPRI | unix.EPOLLIN})
}

// 新加写事件
func (e *EventLoop) addWrite(fd int) error {
	state := e.apidata
	return unix.EpollCtl(state.epfd, unix.EPOLL_CTL_MOD, fd, &unix.EpollEvent{
		Fd:     int32(fd),
		Events: unix.EPOLLERR | unix.EPOLLHUP | unix.EPOLLRDHUP | unix.EPOLLPRI | unix.EPOLLIN | unix.EPOLLOUT,
	},
	)
}

// 删除事件
func (e *EventLoop) del(fd int) error {
	state := e.apidata
	return unix.EpollCtl(state.epfd, unix.EPOLL_CTL_DEL, fd, &unix.EpollEvent{Fd: int32(fd)})
}

// 事件循环
func (e *EventLoop) apiPoll(tv time.Duration) (retVal int, err error) {
	state := e.apidata

	msec := -1
	if tv > 0 {
		msec = int(tv) / int(time.Millisecond)
	}

	retVal, err = unix.EpollWait(state.epfd, state.events, msec)
	if err != nil {
		return 0, err
	}
	numEvents := 0
	if retVal > 0 {
		numEvents = retVal
		for i := 0; i < numEvents; i++ {
			ev := &state.events[i]
			conn := e.parent.getConn(int(ev.Fd))
			if conn == nil {
				unix.Close(int(ev.Fd))
				continue
			}
			if ev.Events&(unix.EPOLLIN|unix.EPOLLPRI) > 0 {
				// 读取数据，这里要发行下websocket的解析变成流式解析
				conn.processWebsocketFrame()
			}
			if ev.Events&unix.EPOLLOUT > 0 {
				// 刷新下直接写入失败的数据
				conn.flushOrClose()
			}
			if ev.Events&(unix.EPOLLERR|unix.EPOLLHUP|unix.EPOLLRDHUP) > 0 {
				// TODO 完善下细节
				conn.Close()
			}
		}

	}

	return numEvents, nil
}

func apiName() string {
	return "epoll"
}
