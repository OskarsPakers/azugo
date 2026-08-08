package azugo

import (
	"bytes"
	"errors"
	"mime/multipart"
	"testing"

	"github.com/go-quicktest/qt"
	"github.com/valyala/fasthttp"
)

func multipartFile(t *testing.T, field, name string, size int) ([]byte, string) {
	t.Helper()

	var body bytes.Buffer

	w := multipart.NewWriter(&body)

	fw, err := w.CreateFormFile(field, name)
	qt.Assert(t, qt.IsNil(err))

	_, err = fw.Write(bytes.Repeat([]byte("A"), size))
	qt.Assert(t, qt.IsNil(err))
	qt.Assert(t, qt.IsNil(w.Close()))

	return body.Bytes(), w.FormDataContentType()
}

func TestUploadLargerThanDefaultBodyLimit(t *testing.T) {
	a := NewTestApp()
	a.Start(t)
	defer a.Stop()

	const fileSize = 6 << 20 // 6 MiB > fasthttp.DefaultMaxRequestBodySize (4 MiB)

	a.Post("/upload", func(ctx *Context) {
		fh, err := ctx.Form.File("file")
		qt.Check(t, qt.IsNil(err))
		if fh == nil {
			ctx.StatusCode(fasthttp.StatusInternalServerError)

			return
		}

		qt.Check(t, qt.Equals(fh.Size, int64(fileSize)))
		ctx.StatusCode(fasthttp.StatusOK)
	})

	body, ct := multipartFile(t, "file", "big.bin", fileSize)

	c := a.TestClient()
	resp, err := c.Post("/upload", body, c.WithHeader("Content-Type", ct))
	defer fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.IsNil(err))

	qt.Check(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
}

func TestUploadParseErrorSurfaced(t *testing.T) {
	a := NewTestApp()
	a.Start(t)
	defer a.Stop()

	t.Setenv("TMPDIR", "/nonexistent-azugo-upload-tmp")

	const fileSize = 1 << 20 // above the in-memory threshold, so it must spill to disk

	a.Post("/upload", func(ctx *Context) {
		_, err := ctx.Form.File("file")

		var parseErr FormParseError
		qt.Check(t, qt.IsTrue(errors.As(err, &parseErr)), qt.Commentf("expected FormParseError, got %v", err))
		qt.Check(t, qt.IsNotNil(ctx.Form.Parse()))

		ctx.Error(err)
	})

	body, ct := multipartFile(t, "file", "big.bin", fileSize)

	c := a.TestClient()
	resp, err := c.Post("/upload", body, c.WithHeader("Content-Type", ct))
	defer fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.IsNil(err))

	qt.Check(t, qt.Equals(resp.StatusCode(), fasthttp.StatusBadRequest))
}

func TestUploadExceedsConfiguredLimit(t *testing.T) {
	a := NewTestApp()
	a.ServerOptions.MaxMultipartFormSize = 1 << 20
	a.Start(t)
	defer a.Stop()

	a.Post("/upload", func(ctx *Context) {
		_, err := ctx.Form.File("file")

		var parseErr FormParseError
		qt.Check(t, qt.IsTrue(errors.As(err, &parseErr)), qt.Commentf("expected FormParseError, got %v", err))
		qt.Check(t, qt.IsTrue(errors.Is(err, fasthttp.ErrBodyTooLarge)), qt.Commentf("expected ErrBodyTooLarge, got %v", err))

		ctx.Error(err)
	})

	body, ct := multipartFile(t, "file", "big.bin", 2<<20)

	c := a.TestClient()
	resp, err := c.Post("/upload", body, c.WithHeader("Content-Type", ct))
	defer fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.IsNil(err))

	qt.Check(t, qt.Equals(resp.StatusCode(), fasthttp.StatusBadRequest))
}

func TestUploadWithinConfiguredLimit(t *testing.T) {
	a := NewTestApp()
	a.ServerOptions.MaxMultipartFormSize = 4 << 20
	a.Start(t)
	defer a.Stop()

	const fileSize = 1 << 20

	a.Post("/upload", func(ctx *Context) {
		fh, err := ctx.Form.File("file")
		qt.Check(t, qt.IsNil(err))
		if fh != nil {
			qt.Check(t, qt.Equals(fh.Size, int64(fileSize)))
		}

		ctx.StatusCode(fasthttp.StatusOK)
	})

	body, ct := multipartFile(t, "file", "big.bin", fileSize)

	c := a.TestClient()
	resp, err := c.Post("/upload", body, c.WithHeader("Content-Type", ct))
	defer fasthttp.ReleaseResponse(resp)
	qt.Assert(t, qt.IsNil(err))

	qt.Check(t, qt.Equals(resp.StatusCode(), fasthttp.StatusOK))
}
