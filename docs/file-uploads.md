# File upload routes — guidelines

This document explains how multipart/file uploads work in Azugo, why a writable
temporary directory is required, and how to build robust upload routes. It exists
because uploads can silently appear "cut off" (empty form, missing files) when the
temporary directory is unavailable.

## TL;DR

- Uploads are **not** limited to 4 MB. Request bodies are streamed, and file parts
  larger than the in-memory threshold are written to **temporary files in the OS temp
  directory** (`$TMPDIR`, default `/tmp`). That directory **must exist and be writable**
  by the process, or multipart parsing fails.
- A failed parse (e.g. unwritable temp dir, malformed body, exceeded size limit) now
  surfaces as a `FormParseError` from the `ctx.Form` accessors and from
  `ctx.Form.Parse()`. This is the usual cause of "the upload got cut off".
- Uploads are unbounded by default. Set `ServerOptions.MaxMultipartFormSize` to cap
  them; larger uploads then fail with a `FormParseError` wrapping
  `fasthttp.ErrBodyTooLarge`.
- Always validate size, content type, and filename, and never use an uploaded file
  after the handler returns (its temp file is removed on request cleanup).

## How uploads work in Azugo

The HTTP server is configured (in `app.go`) with:

```go
server := &fasthttp.Server{
    StreamRequestBody:            true,  // body is streamed, large parts spill to disk
    DisablePreParseMultipartForm: true,  // multipart is parsed lazily, not during read
}
```

Because the body is streamed, the server-wide body limit does **not** reject large
uploads — an oversized body is turned into a stream and read on demand. That is why
uploads larger than fasthttp's 4 MB default work.

The form is parsed **lazily**, the first time a handler touches `ctx.Form`. Parsing
reads the multipart stream and writes any file part larger than the in-memory threshold
to a **temporary file** created in the OS temp directory (`os.CreateTemp("", ...)` →
`$TMPDIR`, default `/tmp`). Routes that never read the form do no parsing and create no
temp files.

Fields and files are accessed through the `ctx.Form` API:

```go
ctx.Form.String("title")          // text field
ctx.Form.File("document")         // single *multipart.FileHeader (required)
ctx.Form.FileOptional("avatar")   // single *multipart.FileHeader or nil
ctx.Form.Files("attachments")     // []*multipart.FileHeader
ctx.Form.Parse()                  // force parsing; returns the parse error, if any
```

Temporary files are removed automatically when the request context is released. **Do
not** keep a `*multipart.FileHeader` or an open file past the end of the handler.

## Why "cut off" happens: the temp directory

If `$TMPDIR` (or `/tmp`) does not exist, is read-only, or the process lacks write
permission, fasthttp cannot spill the file part to disk and multipart parsing fails.
The handler observes this as a `FormParseError`:

```go
fh, err := ctx.Form.File("document")
// err is a FormParseError when the temp dir is unwritable or the body is malformed,
// and a ParamRequiredError when the field is simply absent.
```

This is extremely common in hardened container images:

- distroless / `scratch` images that have no `/tmp` directory at all
- Kubernetes pods with `securityContext.readOnlyRootFilesystem: true`
- containers where `/tmp` is mounted but not writable by the run-as user

> Note: before the lazy-parse change, this error was swallowed and the handler saw an
> empty form with no indication of the cause. It is now returned from the `ctx.Form`
> accessors.

## Required deployment setup

### Provide a writable temp directory

Pick one of the following:

1. **Mount a writable tmpfs at `/tmp`** (recommended for read-only root filesystems):

   ```yaml
   # Kubernetes pod spec
   securityContext:
     readOnlyRootFilesystem: true
   volumeMounts:
     - name: tmp
       mountPath: /tmp
   volumes:
     - name: tmp
       emptyDir:
         medium: Memory      # tmpfs; omit for disk-backed
         sizeLimit: 512Mi    # size to your largest concurrent uploads
   ```

2. **Point `TMPDIR` at a writable location** and ensure it exists:

   ```dockerfile
   ENV TMPDIR=/var/tmp/uploads
   RUN mkdir -p /var/tmp/uploads && chown app:app /var/tmp/uploads
   ```

   Go's `os.TempDir()` honours `$TMPDIR`, so this redirects the spill files.

3. For `scratch`/distroless images, explicitly create the temp directory and make sure
   the running UID can write to it.

### Size the temp storage

- Temp files hold the uploaded payload until the handler finishes. Size `/tmp`
  (or your `TMPDIR`) for **peak concurrent upload bytes**, not a single request.
- A tmpfs counts against pod memory — account for it in memory limits/requests.

### Verify at startup

Fail fast instead of discovering the problem on the first upload. Check that the temp
directory is writable when the app boots:

```go
func assertTempWritable() error {
    f, err := os.CreateTemp("", "upload-check-*")
    if err != nil {
        return fmt.Errorf("temp dir %q is not writable: %w", os.TempDir(), err)
    }
    name := f.Name()
    _ = f.Close()
    _ = os.Remove(name)
    return nil
}
```

## Bounding upload size

Uploads are unbounded by default (limited only by available temp storage). To cap them,
set the limit once when constructing the app:

```go
app.ServerOptions.MaxMultipartFormSize = 32 << 20 // 32 MiB
```

An upload exceeding the limit fails when the form is accessed with a `FormParseError`
that wraps `fasthttp.ErrBodyTooLarge` (and maps to HTTP 400 via `ctx.Error`).

## Writing an upload route

```go
func uploadHandler(ctx *azugo.Context) {
    // Surface a parse failure (unwritable temp dir, oversized upload, malformed body)
    // before treating a missing file as a client error.
    if err := ctx.Form.Parse(); err != nil {
        ctx.Error(err) // FormParseError -> 400
        return
    }

    title, err := ctx.Form.String("title")
    if err != nil {
        ctx.Error(err)
        return
    }

    fh, err := ctx.Form.File("document")
    if err != nil {
        ctx.Error(err)
        return
    }

    // Validate before trusting anything from the client.
    const maxSize = 10 << 20 // 10 MiB
    if fh.Size > maxSize {
        ctx.StatusCode(fasthttp.StatusRequestEntityTooLarge)
        return
    }

    src, err := fh.Open()
    if err != nil {
        ctx.Error(err)
        return
    }
    defer src.Close()

    // Sanitise the filename — never use fh.Filename directly in a path.
    safeName := filepath.Base(filepath.Clean("/" + fh.Filename))

    // Persist within the handler; the temp file is removed when the request completes.
    dst, err := os.Create(filepath.Join(storageDir, safeName))
    if err != nil {
        ctx.Error(err)
        return
    }
    defer dst.Close()

    if _, err := io.Copy(dst, src); err != nil {
        ctx.Error(err)
        return
    }

    ctx.StatusCode(fasthttp.StatusCreated)
}
```

### Multiple files

```go
for _, fh := range ctx.Form.Files("attachments") {
    src, err := fh.Open()
    if err != nil {
        ctx.Error(err)
        return
    }
    // ... process, then close inside the loop (don't defer in a loop)
    src.Close()
}
```

## Security checklist

- **Bound upload size** via `ServerOptions.MaxMultipartFormSize`, and validate per-file
  sizes in the handler.
- **Do not trust the client `Content-Type`** — sniff the content if the type matters.
- **Sanitise filenames**: use `filepath.Base`, reject path separators and `..`; prefer
  generating your own storage name.
- **Store uploads outside any served static root** so they can't be requested back as
  served content.
- **Cap concurrency** for upload endpoints to bound temp-disk and memory usage.
- Treat the temp directory as sensitive: other processes on the host should not be able
  to read in-flight upload spill files.

## Reference

- `ServerOptions.MaxMultipartFormSize` — maximum multipart body size in bytes; `0` (the
  default) means no limit.
- `ctx.Form.Parse() error` — forces parsing and returns the parse error, if any.
- `FormParseError` — returned by the `ctx.Form` accessors when parsing fails; maps to
  HTTP 400 and wraps the underlying cause (e.g. `fasthttp.ErrBodyTooLarge`).
- The temporary directory follows the Go runtime (`$TMPDIR` / `os.TempDir()`); control
  it via the environment as described above.
