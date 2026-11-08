package main

// In its own network namespace (userns.go), all traffic of the node goes
// through pasta. Registry downloads now and then stall there: containerd
// waits in a read, and no more bytes arrive. Image pulls do not need the
// node network, so they skip pasta:
//
//   - The agent outside, still in the container's network namespace, dials
//     on request and hands the connected socket over a Unix socket (fd 4).
//   - The agent inside runs an HTTP CONNECT proxy on 127.0.0.1 and gives it
//     to containerd as HTTPS_PROXY. Each tunnel uses a socket from outside,
//     so the bytes go straight from the kernel to containerd.
//
// When the dial outside fails, the proxy dials inside, through pasta.

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"sync"
	"syscall"
	"time"
)

const dialFD = 4

const dialTimeout = 30 * time.Second

// newOutsideDialer returns the socket for the agent inside and answers its
// dial requests until the socket closes.
func newOutsideDialer() (*os.File, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	outside := os.NewFile(uintptr(fds[0]), "dial")
	inside := os.NewFile(uintptr(fds[1]), "dial")
	c, err := net.FileConn(outside)
	outside.Close()
	if err != nil {
		inside.Close()
		return nil, err
	}
	go serveDials(c.(*net.UnixConn))
	return inside, nil
}

// serveDials reads requests: the address as data, and one socket to answer on.
func serveDials(ctrl *net.UnixConn) {
	defer ctrl.Close()
	buf := make([]byte, 512)
	oob := make([]byte, syscall.CmsgSpace(4))
	for {
		n, oobn, _, _, err := ctrl.ReadMsgUnix(buf, oob)
		if err != nil {
			return
		}
		reply, err := receiveFile(oob[:oobn])
		if err != nil {
			continue
		}
		go answerDial(string(buf[:n]), reply)
	}
}

func answerDial(addr string, reply *os.File) {
	rc, err := net.FileConn(reply)
	reply.Close()
	if err != nil {
		return
	}
	defer rc.Close()
	c, err := net.DialTimeout("tcp", addr, dialTimeout)
	if err != nil {
		_, _ = rc.Write([]byte(err.Error()))
		return
	}
	defer c.Close()
	f, err := c.(*net.TCPConn).File()
	if err != nil {
		_, _ = rc.Write([]byte(err.Error()))
		return
	}
	defer f.Close()
	_, _, _ = rc.(*net.UnixConn).WriteMsgUnix([]byte("ok"), syscall.UnixRights(int(f.Fd())), nil)
}

// dialOutside asks the agent outside for a TCP connection to addr.
func dialOutside(ctrl *net.UnixConn, addr string) (net.Conn, error) {
	fds, err := syscall.Socketpair(syscall.AF_UNIX, syscall.SOCK_SEQPACKET|syscall.SOCK_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	local := os.NewFile(uintptr(fds[0]), "dial-reply")
	remote := os.NewFile(uintptr(fds[1]), "dial-reply")
	_, _, err = ctrl.WriteMsgUnix([]byte(addr), syscall.UnixRights(int(remote.Fd())), nil)
	remote.Close()
	if err != nil {
		local.Close()
		return nil, err
	}
	lc, err := net.FileConn(local)
	local.Close()
	if err != nil {
		return nil, err
	}
	defer lc.Close()
	_ = lc.SetReadDeadline(time.Now().Add(dialTimeout + 5*time.Second))
	buf := make([]byte, 512)
	oob := make([]byte, syscall.CmsgSpace(4))
	n, oobn, _, _, err := lc.(*net.UnixConn).ReadMsgUnix(buf, oob)
	if err != nil {
		return nil, err
	}
	if string(buf[:n]) != "ok" {
		return nil, errors.New(string(buf[:n]))
	}
	f, err := receiveFile(oob[:oobn])
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return net.FileConn(f)
}

func receiveFile(oob []byte) (*os.File, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	for _, m := range msgs {
		fds, err := syscall.ParseUnixRights(&m)
		if err != nil {
			continue
		}
		for _, fd := range fds[1:] {
			syscall.Close(fd)
		}
		if len(fds) > 0 {
			syscall.CloseOnExec(fds[0])
			return os.NewFile(uintptr(fds[0]), "socket"), nil
		}
	}
	return nil, errors.New("no file in message")
}

// startPullProxy listens on 127.0.0.1 and returns the proxy URL for
// containerd. Without the socket from outside it returns "".
func startPullProxy(ctx context.Context) string {
	f := os.NewFile(dialFD, "dial")
	c, err := net.FileConn(f)
	f.Close()
	if err != nil {
		log.Printf("pull proxy: %v", err)
		return ""
	}
	ctrl, ok := c.(*net.UnixConn)
	if !ok {
		c.Close()
		return ""
	}
	return servePullProxy(ctx, ctrl)
}

func servePullProxy(ctx context.Context, ctrl *net.UnixConn) string {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		log.Printf("pull proxy: %v", err)
		ctrl.Close()
		return ""
	}
	context.AfterFunc(ctx, func() { ln.Close(); ctrl.Close() })
	var warn sync.Once
	dial := func(addr string) (net.Conn, error) {
		c, err := dialOutside(ctrl, addr)
		if err == nil {
			return c, nil
		}
		warn.Do(func() { log.Printf("pull proxy: dial outside: %v, using the node network", err) })
		return net.DialTimeout("tcp", addr, dialTimeout)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go proxyConnect(c, dial)
		}
	}()
	return "http://" + ln.Addr().String()
}

// proxyConnect serves one CONNECT request and copies the bytes both ways.
func proxyConnect(c net.Conn, dial func(string) (net.Conn, error)) {
	defer c.Close()
	_ = c.SetReadDeadline(time.Now().Add(dialTimeout))
	br := bufio.NewReader(c)
	req, err := http.ReadRequest(br)
	if err != nil {
		return
	}
	_ = c.SetReadDeadline(time.Time{})
	if req.Method != http.MethodConnect {
		fmt.Fprint(c, "HTTP/1.1 405 Method Not Allowed\r\nConnection: close\r\n\r\n")
		return
	}
	up, err := dial(req.Host)
	if err != nil {
		fmt.Fprintf(c, "HTTP/1.1 502 Bad Gateway\r\nConnection: close\r\n\r\n%v\n", err)
		return
	}
	defer up.Close()
	if _, err := fmt.Fprint(c, "HTTP/1.1 200 Connection established\r\n\r\n"); err != nil {
		return
	}
	done := make(chan struct{})
	go func() {
		_, _ = io.Copy(up, br) // br holds what the client sent after the request
		if tc, ok := up.(*net.TCPConn); ok {
			_ = tc.CloseWrite()
		}
		close(done)
	}()
	_, _ = io.Copy(c, up)
	if tc, ok := c.(*net.TCPConn); ok {
		_ = tc.CloseWrite()
	}
	<-done
}
