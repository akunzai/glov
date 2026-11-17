// Package fetch downloads the files base images are made from.
package fetch

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"
)

// Get returns up to limit bytes of url's body; any status but 200 OK is an
// error.
func Get(ctx context.Context, c *http.Client, url string, limit int64) ([]byte, error) {
	resp, err := request(ctx, c, url)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, limit))
}

// IdleTimeout ends a request that receives nothing for that long: a
// connection the server stops sending on otherwise waits forever.
var IdleTimeout = time.Minute

func request(ctx context.Context, c *http.Client, url string) (*http.Response, error) {
	ctx, cancel := context.WithCancel(ctx)
	stalled := new(atomic.Bool)
	timer := time.AfterFunc(IdleTimeout, func() { stalled.Store(true); cancel() })
	stop := func() { timer.Stop(); cancel() }
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		stop()
		return nil, err
	}
	resp, err := c.Do(req)
	if err != nil {
		stop()
		if stalled.Load() {
			err = fmt.Errorf("GET %s: no answer for %s", url, IdleTimeout)
		}
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		stop()
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	resp.Body = &idleBody{ReadCloser: resp.Body, timer: timer, stop: stop, stalled: stalled}
	return resp, nil
}

// idleBody restarts the request's idle timer whenever data arrives.
type idleBody struct {
	io.ReadCloser
	timer   *time.Timer
	stop    func()
	stalled *atomic.Bool
}

func (b *idleBody) Read(p []byte) (int, error) {
	n, err := b.ReadCloser.Read(p)
	if n > 0 {
		b.timer.Reset(IdleTimeout)
	}
	if err != nil && err != io.EOF && b.stalled.Load() {
		err = fmt.Errorf("no data for %s: %w", IdleTimeout, err)
	}
	return n, err
}

func (b *idleBody) Close() error {
	b.stop()
	return b.ReadCloser.Close()
}

// Download writes url's body to path, reporting every 10% of a download of
// known size to progress; path is removed on failure.
func Download(ctx context.Context, c *http.Client, url, path string, progress func(string)) error {
	resp, err := request(ctx, c, url)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	w := &progressWriter{w: bufio.NewWriterSize(f, 1<<20), total: resp.ContentLength, name: filepath.Base(url), report: progress}
	_, err = io.Copy(w, resp.Body)
	if ferr := w.w.Flush(); err == nil {
		err = ferr
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(path)
		return fmt.Errorf("download %s: %w", url, err)
	}
	progress(fmt.Sprintf("downloaded %s (%d MiB)", w.name, w.n>>20))
	return nil
}

// progressWriter reports every 10% of a download of known size.
type progressWriter struct {
	w           *bufio.Writer
	n, total    int64
	name        string
	lastPercent int64
	report      func(string)
}

func (p *progressWriter) Write(b []byte) (int, error) {
	n, err := p.w.Write(b)
	p.n += int64(n)
	if p.total > 0 {
		if pct := p.n * 100 / p.total; pct/10 > p.lastPercent/10 {
			p.lastPercent = pct
			p.report(fmt.Sprintf("downloading %s: %d%% of %d MiB", p.name, pct, p.total>>20))
		}
	}
	return n, err
}
