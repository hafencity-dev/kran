package execenv

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"testing/iotest"
	"time"

	"github.com/hafencity-dev/kran/internal/proto"
)

func TestSpoolReaderOffsetsAndFollow(t *testing.T) {
	path := filepath.Join(t.TempDir(), "out")
	w, err := openSpool(path)
	if err != nil {
		t.Fatal(err)
	}
	var ended atomic.Bool
	w.write(proto.FrameStdout, []byte("hello "))
	w.write(proto.FrameStderr, []byte("err1 "))
	big := strings.Repeat("z", spoolChunk+100) // split into two frames
	w.write(proto.FrameStdout, []byte(big))

	// Follow: reader started before the rest is written gets everything.
	r := newSpoolReader(path, 0, ended.Load)
	done := make(chan string)
	go func() {
		o, e, _ := collect(t, r)
		done <- o + "|" + e
	}()
	time.Sleep(30 * time.Millisecond)
	w.write(proto.FrameStdout, []byte("tail"))
	w.Close()
	ended.Store(true)
	select {
	case got := <-done:
		if got != "hello "+big+"tail|err1 " {
			t.Fatalf("follow got %d bytes", len(got))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reader did not end")
	}

	// Offsets count payload bytes across streams: 6 (hello) + 5 (err1).
	for _, c := range []struct {
		off  int64
		want string
	}{
		{0, "hello err1 " + big + "tail"},
		{3, "lo err1 " + big + "tail"},
		{6, "err1 " + big + "tail"},
		{11 + int64(len(big)) - 2, "zztail"},
		{11 + int64(len(big)) + 4, ""},
		{1 << 40, ""},
	} {
		r := newSpoolReader(path, c.off, ended.Load)
		o, e, n := collect(t, r)
		if got := mergeOrder(path, c.off); got != c.want || int64(len(o)+len(e)) != n {
			t.Errorf("offset %d: got %q…", c.off, trunc(got))
		}
	}

	// Tiny reads still decode (stream semantics).
	r = newSpoolReader(path, 0, ended.Load)
	o, _, _ := collect(t, iotest.OneByteReader(r))
	if o != "hello "+big+"tail" {
		t.Fatal("one-byte reads")
	}

	// Closing unblocks a waiting reader.
	var never atomic.Bool
	r = newSpoolReader(filepath.Join(t.TempDir(), "missing"), 0, never.Load)
	errc := make(chan error)
	go func() {
		_, err := r.Read(make([]byte, 100))
		errc <- err
	}()
	time.Sleep(30 * time.Millisecond)
	r.Close()
	if err := <-errc; !errors.Is(err, errReaderClosed) {
		t.Fatalf("closed reader: %v", err)
	}
}

// mergeOrder reads the spool from off and returns payloads in order.
func mergeOrder(path string, off int64) string {
	r := newSpoolReader(path, off, func() bool { return true })
	var sb strings.Builder
	buf := make([]byte, proto.MaxFramePayload)
	for {
		_, p, err := proto.ReadFrame(r, buf)
		if err != nil {
			if err != io.EOF {
				sb.WriteString("ERR:" + err.Error())
			}
			return sb.String()
		}
		sb.Write(p)
	}
}

func trunc(s string) string {
	if len(s) > 40 {
		return s[:40]
	}
	return s
}
