package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/hafencity-dev/kran/internal/proto"
	"github.com/hafencity-dev/kran/internal/transport"
)

const uploadWorkers = 4

// uploadBlobs sends the blobs the target is missing over parallel streams,
// zstd-compressed, reporting progress as phase frames and measuring
// throughput for the scheduler. It returns the raw bytes sent.
func (d *Daemon) uploadBlobs(ctx context.Context, sess transport.Session, e *jobEntry, node string, m *proto.Manifest, plan proto.SyncPlan) (int64, error) {
	if len(plan.Missing) == 0 {
		e.phase(proto.Phase{State: proto.JobSyncing,
			Message: fmt.Sprintf("%s is up to date (%d changed, %d deleted)", node, plan.Changed, plan.Deleted), Progress: 1})
		return 0, nil
	}
	sizes := make(map[string]int64, len(m.Entries))
	for _, en := range m.Entries {
		if en.Hash != "" {
			sizes[en.Hash] = en.Size
		}
	}
	for _, h := range plan.Missing {
		// The scanner can open blobs of every repo; send only this job's.
		if _, ok := sizes[h]; !ok {
			return 0, fmt.Errorf("%s asked for a file that is not in this workspace", node)
		}
	}
	total := plan.Bytes
	if total <= 0 {
		for _, h := range plan.Missing {
			total += sizes[h]
		}
	}
	msg := fmt.Sprintf("sending %d files, %s", len(plan.Missing), humanBytes(total))
	e.phase(proto.Phase{State: proto.JobSyncing, Message: msg})

	parent := ctx
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var sent atomic.Int64
	start := time.Now()

	// Progress at most 4 times a second.
	progDone := make(chan struct{})
	go func() {
		defer close(progDone)
		tk := time.NewTicker(250 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tk.C:
				if total > 0 {
					e.phase(proto.Phase{State: proto.JobSyncing, Message: msg,
						Progress: float64(sent.Load()) / float64(total)})
				}
			}
		}
	}()

	work := make(chan string)
	var firstErr error
	var errOnce sync.Once
	setErr := func(err error) {
		errOnce.Do(func() {
			firstErr = err
			cancel()
		})
	}
	workers := uploadWorkers
	if len(plan.Missing) < workers {
		workers = len(plan.Missing)
	}
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := d.uploadWorker(ctx, sess, e.id, work, sizes, &sent); err != nil {
				setErr(err)
			}
		}()
	}
feed:
	for _, h := range plan.Missing {
		select {
		case work <- h:
		case <-ctx.Done():
			break feed
		}
	}
	close(work)
	wg.Wait()
	cancel()
	<-progDone
	if firstErr == nil && parent.Err() != nil {
		firstErr = parent.Err()
	}
	if firstErr != nil {
		return sent.Load(), firstErr
	}
	n := sent.Load()
	if el := time.Since(start).Seconds(); n >= 1<<20 && el > 0 {
		d.peers.recordBandwidth(node, float64(n)/el)
	}
	return n, nil
}

func (d *Daemon) uploadWorker(ctx context.Context, sess transport.Session, id string, work <-chan string, sizes map[string]int64, sent *atomic.Int64) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	pc, err := peerCall(cctx, sess, proto.PeerBlobPut, id, nil, nil)
	cancel()
	if err != nil {
		return err
	}
	defer pc.Close()
	stop := context.AfterFunc(ctx, func() { pc.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	// Acks arrive in order; the reader checks each one.
	var pending atomic.Int64
	ackErr := make(chan error, 1)
	acksDone := make(chan struct{})
	expect := make(chan string, 1024)
	go func() {
		defer close(acksDone)
		for h := range expect {
			t, p, err := pc.readFrame()
			if err != nil {
				ackErr <- fmt.Errorf("read blob ack: %w", err)
				return
			}
			if t != pfAck {
				ackErr <- fmt.Errorf("unexpected frame %d instead of a blob ack", t)
				return
			}
			var a blobAck
			if err := json.Unmarshal(p, &a); err != nil {
				ackErr <- err
				return
			}
			if a.Hash != h {
				ackErr <- fmt.Errorf("blob ack for %s, expected %s", a.Hash, h)
				return
			}
			if a.Error != "" {
				ackErr <- fmt.Errorf("store blob %.12s: %s", h, a.Error)
				return
			}
			pending.Add(-1)
		}
	}()

	enc, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1))
	if err != nil {
		close(expect)
		return err
	}
	defer enc.Close()
	sendErr := func() error {
		for h := range work {
			select {
			case err := <-ackErr:
				return err
			default:
			}
			if err := d.sendBlob(pc, enc, h, sizes[h], sent); err != nil {
				return ctxErr(ctx, err)
			}
			pending.Add(1)
			expect <- h
		}
		return pc.writeFrame(pfDone, nil)
	}()
	close(expect)
	if sendErr != nil {
		return sendErr
	}
	select {
	case <-acksDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case err := <-ackErr:
		return err
	default:
	}
	if pending.Load() != 0 {
		return errors.New("blob upload ended before every blob was acknowledged")
	}
	return nil
}

func (d *Daemon) sendBlob(pc *peerConn, enc *zstd.Encoder, hash string, size int64, sent *atomic.Int64) error {
	r, n, err := d.deps.Scanner.Open(hash)
	if err != nil {
		return fmt.Errorf("open blob %.12s: %w", hash, err)
	}
	defer r.Close()
	if size == 0 {
		size = n
	}
	if err := pc.writeJSONFrame(pfBlob, blobHeader{Hash: hash, Size: size}); err != nil {
		return err
	}
	enc.Reset(frameWriter{pc})
	if _, err := io.Copy(enc, &countingReader{r: r, n: sent}); err != nil {
		return err
	}
	if err := enc.Close(); err != nil {
		return err
	}
	return pc.writeFrame(pfEnd, nil)
}

type countingReader struct {
	r io.Reader
	n *atomic.Int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n.Add(int64(n))
	return n, err
}
