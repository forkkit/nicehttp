package nicehttp

import (
	"errors"
	"fmt"
	"github.com/lithdew/bytesutil"
	"github.com/valyala/fasthttp"
	"golang.org/x/sync/errgroup"
	"io"
	"os"
)

// Client wraps over fasthttp.Client a couple of nice, useful helper functions.
type Client struct {
	// Client is the underlying instance which nicehttp.Client wraps around.
	Client fasthttp.Client

	// Decide whether or not files downloaded should have their size pre-allocated.
	Preallocate bool

	// Decide whether or not URLs that accept being downloaded in parallel chunks are handled with multiple workers.
	AcceptsRanges bool

	// The number of workers that are to be spawned for downloading chunks in parallel.
	NumWorkers int

	// Size of individual byte chunks downloaded.
	RangeSize int

	// Max number of redirects to follow before a request is marked to have failed.
	MaxRedirectCount int
}

// Do sends a HTTP request prescribed in req and populates its results into res. It additionally handles redirects
// unlike the de-facto Do(req, res) method in fasthttp.
func (c *Client) Do(req *fasthttp.Request, res *fasthttp.Response) error {
	for i := 0; i <= c.MaxRedirectCount; i++ {
		if err := c.Client.Do(req, res); err != nil {
			return err
		}

		if !fasthttp.StatusCodeIsRedirect(res.StatusCode()) {
			return nil
		}

		location := res.Header.Peek("Location")
		if len(location) == 0 {
			return errors.New("missing 'Location' header after redirect")
		}

		req.URI().UpdateBytes(location)

		res.Reset()
	}

	return errors.New("redirected too many times")
}

// DownloadFile downloads of url, and writes its contents to a newly-created file titled filename.
func (c *Client) DownloadFile(filename, url string) error {
	f, err := os.Create(filename)
	if err != nil {
		return fmt.Errorf("failed to open dest file: %w", err)
	}

	headers := acquireResponseHeaders()
	defer releaseResponseHeaders(headers)

	if err := c.QueryHeaders(headers, url); err == nil {
		acceptsRanges := bytesutil.String(headers.Peek("Accept-Ranges")) == "bytes"

		length := headers.ContentLength()
		if (acceptsRanges && length <= 0) || length < 0 {
			return fmt.Errorf("content length is %d - see doc for (*fasthttp.ResponseHeader).ContentLength()", length)
		}

		if c.Preallocate {
			if err := f.Truncate(int64(length)); err != nil {
				return fmt.Errorf("failed to truncate file to %d byte(s): %w", length, err)
			}
		}

		if c.AcceptsRanges && acceptsRanges {
			return c.DownloadInChunks(f, url, length, c.NumWorkers, c.RangeSize)
		}
	}

	return c.Download(f, url)
}

// QueryHeaders queries headers from url via a HTTP HEAD request, and populates dst with its contents.
func (c *Client) QueryHeaders(dst *fasthttp.ResponseHeader, url string) error {
	if dst == nil {
		return errors.New("dst must not be nil")
	}

	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(res)

	req.Header.SetMethod(fasthttp.MethodHead)
	req.SetRequestURI(url)

	if err := c.Do(req, res); err != nil {
		return fmt.Errorf("failed to call HEAD on url %q: %w", url, err)
	}

	res.Header.CopyTo(dst)

	return nil
}

// Download contents of url and write it to w.
func (c *Client) Download(w io.Writer, url string) error {
	req := fasthttp.AcquireRequest()
	defer fasthttp.ReleaseRequest(req)

	res := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseResponse(res)

	req.SetRequestURI(url)

	if err := c.Do(req, res); err != nil {
		return fmt.Errorf("failed to download %q: %w", url, err)
	}

	return res.BodyWriteTo(w)
}

// DownloadInChunks downloads file at url comprised of length bytes in cs byte-sized chunks using w goroutines, and
// store it in file f.
func (c *Client) DownloadInChunks(f io.WriterAt, url string, length, w, cs int) error {
	var g errgroup.Group

	// ByteRange represents a byte range.
	type ByteRange struct{ Start, End int }

	ch := make(chan ByteRange, w)

	// Spawn w workers that will dispatch and execute byte range-inclusive HTTP requests.

	for i := 0; i < w; i++ {
		i := i

		g.Go(func() error {
			req := fasthttp.AcquireRequest()
			defer fasthttp.ReleaseRequest(req)

			res := fasthttp.AcquireResponse()
			defer fasthttp.ReleaseResponse(res)

			req.SetRequestURI(url)

			for r := range ch {
				req.Header.SetByteRange(r.Start, r.End)

				if err := fasthttp.Do(req, res); err != nil {
					return fmt.Errorf("worker %d failed to get bytes range (start: %d, end: %d): %w", i, r.Start, r.End, err)
				}

				if err := res.BodyWriteTo(&WriterAtOffset{Src: f, Offset: int64(r.Start)}); err != nil {
					return fmt.Errorf("worker %d failed to write to file at offset %d: %w", i, r.Start, err)
				}
			}

			return nil
		})
	}

	// Fill up ch with byte ranges to be download from url.

	var r ByteRange

	for r.End < length {
		r.End += cs
		if r.End > length {
			r.End = length
		}

		ch <- r

		r.Start += cs
		if r.Start > length {
			r.Start = length
		}
	}

	close(ch)

	// Wait until all byte ranges have been downloaded, or return early if an error was encountered downloading
	// a chunk.

	if err := g.Wait(); err != nil {
		return fmt.Errorf("failed to download %q in chunks: %w", url, err)
	}

	return nil
}