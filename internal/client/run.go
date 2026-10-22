package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"sync"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

// Run-session timing. The daemon sends a heartbeat every 2 s; silence for
// RecvTimeout means it is wedged or gone.
var (
	HeartbeatInterval = 2 * time.Second
	RecvTimeout       = 10 * time.Second
)

// UpgradeProtocol is the HTTP Upgrade token of run sessions.
const UpgradeProtocol = "kran-run/1"

// Run opens a run session. The session sends heartbeats by itself; Recv
// never returns FrameHeartbeat and fails with ErrSessionTimeout when the
// daemon has been silent for RecvTimeout.
func (c *httpClient) Run(ctx context.Context) (RunSession, error) {
	conn, err := dialUnix(ctx, c.socket)
	if err != nil {
		return nil, err
	}
	if dl, ok := ctx.Deadline(); ok {
		conn.SetDeadline(dl)
	} else {
		conn.SetDeadline(time.Now().Add(10 * time.Second))
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	req, _ := http.NewRequest(http.MethodPost, baseURL+"/api/v1/run", nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", UpgradeProtocol)
	req.Header.Set("X-Kran-Client-Pid", fmt.Sprint(os.Getpid()))
	if err := req.Write(conn); err != nil {
		conn.Close()
		return nil, fmt.Errorf("open run session: %w", err)
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		conn.Close()
		return nil, fmt.Errorf("open run session: %w", err)
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		defer conn.Close()
		return nil, readAPIError(resp)
	}
	if !stop() {
		conn.Close()
		return nil, ctx.Err()
	}
	conn.SetDeadline(time.Time{})
	return newRunSession(conn, br), nil
}

type runSession struct {
	conn net.Conn
	br   *bufio.Reader
	buf  []byte

	// Captured at creation so tests can tune the package vars safely.
	hbEvery     time.Duration
	recvTimeout time.Duration

	wmu    sync.Mutex
	closed chan struct{}
	once   sync.Once
}

func newRunSession(conn net.Conn, br *bufio.Reader) *runSession {
	s := &runSession{conn: conn, br: br, closed: make(chan struct{}),
		hbEvery: HeartbeatInterval, recvTimeout: RecvTimeout}
	go s.heartbeat()
	return s
}

func (s *runSession) heartbeat() {
	t := time.NewTicker(s.hbEvery)
	defer t.Stop()
	for {
		select {
		case <-s.closed:
			return
		case <-t.C:
			if err := s.Send(proto.FrameHeartbeat, nil); err != nil {
				return
			}
		}
	}
}

func (s *runSession) Send(t proto.FrameType, payload []byte) error {
	s.wmu.Lock()
	defer s.wmu.Unlock()
	select {
	case <-s.closed:
		return net.ErrClosed
	default:
	}
	// A daemon that stops reading must not block the CLI forever.
	s.conn.SetWriteDeadline(time.Now().Add(s.recvTimeout))
	return proto.WriteFrame(s.conn, t, payload)
}

func (s *runSession) SendJSON(t proto.FrameType, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	return s.Send(t, b)
}

func (s *runSession) Recv() (proto.FrameType, []byte, error) {
	for {
		s.conn.SetReadDeadline(time.Now().Add(s.recvTimeout))
		t, p, err := proto.ReadFrame(s.br, s.buf)
		if err != nil {
			var ne net.Error
			if errors.As(err, &ne) && ne.Timeout() {
				return 0, nil, ErrSessionTimeout
			}
			return 0, nil, err
		}
		if cap(p) > cap(s.buf) {
			s.buf = p[:0]
		}
		if t == proto.FrameHeartbeat {
			continue
		}
		return t, p, nil
	}
}

func (s *runSession) Close() error {
	var err error
	s.once.Do(func() {
		close(s.closed)
		err = s.conn.Close()
	})
	return err
}
