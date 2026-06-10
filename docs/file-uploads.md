# File upload routes — guidelines

This document explains how multipart/file uploads work in Azugo, why a writable
temporary directory is required, and how to build robust upload routes. It exists
because uploads can silently appear "cut off" (empty form, missing files) when the
temp directory is unavailable or the body exceeds the default size limit.

## TL;DR

- Uploads larger than the in-memory threshold are streamed to **temporary files in
  the OS temp directory** (`$TMPDIR`, default `/tmp`). That directory **must exist
  and be writable** by the process, or multipart parsing fails.
- When parsing fails, Azugo currently **swallows the error** and the handler sees an
  **empty form** — not an error. This is the usual cause of "the upload got cut off".
- The fasthttp default request body limit is **4 MB**. Anything larger is rejected at
  the transport layer. Azugo does not yet expose a knob to raise it (see
  [Known limitations](#known-limitations)).
- Always validate size, content type, and filename, and never use an uploaded file
  after the handler returns (its temp file is removed on request cleanup).

## How uploads work in Azugo

The HTTP server is configured (in `app.go`) with:

```go
server := &fasthttp.Server{
    StreamRequestBody:            true,  // body is streamed, large parts spill to disk
    DisablePreParseMultipartForm: true,  // multipart is parsed lazily, not during read
    // MaxRequestBodySize is NOT set -> fasthttp default of 4 MB applies
}
```

For every `POST`/`PUT`/`PATCH` with `Content-Type: multipart/form-data`, Azugo eagerly
parses the form when it builds the request context (`request.go`):

```go
} else if bytes.HasPrefix(c.Request.Header.ContentType(), contentTypeMultipartFormData) {
    if form, err := c.Request.MultipartForm(); err == nil { // NOTE: error is ignored
        ctx.Form.form = &multiPartArgs{args: form}
    }
}
```

`MultipartForm()` reads the multipart stream and writes any file part larger than the
in-memory threshold to a **temporary file created in the OS temp directory**
(`os.CreateTemp("", ...)` → `$TMPDIR`, default `/tmp`). Because `StreamRequestBody` is
enabled, fasthttp also uses the temp directory to buffer the streamed body itself once
it grows beyond the read buffer.

Parsed fields and files are then accessed through the `ctx.Form` API:

```go
ctx.Form.String("title")          // text field
ctx.Form.File("document")         // single *multipart.FileHeader (required)
ctx.Form.FileOptional("avatar")   // single *multipart.FileHeader or nil
ctx.Form.Files("attachments")     // []*multipart.FileHeader
```

Temporary files are removed automatically when the request context is released
(`form.go` calls `Request.RemoveMultipartFormFiles()` on reset). **Do not** keep a
`*multipart.FileHeader` or an open file past the end of the handler.

## Why "cut off" happens

There are two distinct failure modes, both of which look like a truncated/empty upload
to the client:

### 1. Temp directory not writable

If `$TMPDIR` (or `/tmp`) does not exist, is read-only, or the process lacks write
permission, fasthttp cannot spill the file part to disk. `MultipartForm()` returns an
error, and because Azugo ignores that error, `ctx.Form` is left empty. The handler then
sees:

- `ctx.Form.File("document")` → `ParamRequiredError` ("document is required")
- `ctx.Form.Files(...)` → empty slice

…even though the client sent a valid file. This is extremely common in hardened
container images:

- distroless / `scratch` images that have no `/tmp` directory at all
- Kubernetes pods with `securityContext.readOnlyRootFilesystem: true`
- containers where `/tmp` is mounted but not writable by the run-as user

### 2. Body exceeds the size limit

fasthttp's default `MaxRequestBodySize` is **4 MB**. A larger request body is rejected
or truncated during read, so the multipart parse never completes and you again get an
empty/partial form.

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

## Writing an upload route

```go
func uploadHandler(ctx *azugo.Context) {
    // Required text field alongside the file.
    title, err := ctx.Form.String("title")
    if err != nil {
        ctx.Error(err) // ParamRequiredError -> 400
        return
    }

    fh, err := ctx.Form.File("document")
    if err != nil {
        // NOTE: this also fires when the temp dir was unwritable, because the
        // form ends up empty. See "Distinguishing a real error" below.
        ctx.Error(err)
        return
    }

    // Validate BEFORE trusting anything from the client.
    const maxSize = 10 << 20 // 10 MiB
    if fh.Size > maxSize {
        ctx.StatusCode(fasthttp.StatusRequestEntityTooLarge)
        return
    }

    // Open the part (this may be an in-memory part or a temp file on disk).
    src, err := fh.Open()
    if err != nil {
        ctx.Error(err)
        return
    }
    defer src.Close()

    // Sanitise the filename — never use fh.Filename directly in a path.
    safeName := filepath.Base(filepath.Clean("/" + fh.Filename))

    // Persist to your storage. Do this within the handler; the temp file is
    // removed when the request completes.
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

### Distinguishing a real error from an empty form

Because Azugo discards the multipart parse error, a missing file and an unwritable temp
dir look identical at the `ctx.Form` layer. If you need to tell them apart (e.g. to
return `500` instead of `400`, or to log the real cause), re-trigger the parse via the
underlying fasthttp request, which returns the error:

```go
if _, err := ctx.Request().MultipartForm(); err != nil {
    ctx.Log().With(zap.Error(err)).Error("multipart parse failed (check temp dir / body size)")
    ctx.StatusCode(fasthttp.StatusInternalServerError)
    return
}
```

## Security checklist

- **Validate size** per file and in aggregate; reject early.
- **Do not trust the client `Content-Type`** — sniff the content if the type matters.
- **Sanitise filenames**: use `filepath.Base`, reject path separators and `..`; prefer
  generating your own storage name.
- **Store uploads outside any served static root** so they can't be requested back as
  executable/served content.
- **Cap concurrency** for upload endpoints to bound temp-disk and memory usage.
- Treat the temp directory as sensitive: other processes on the host should not be able
  to read in-flight upload spill files.

## Known limitations

These are framework-level gaps to be aware of (and good candidates for improvement):

1. **Multipart parse errors are swallowed** (`request.go`): a failed parse yields an
   empty form rather than surfacing the error. Until this is changed, use the
   "Distinguishing a real error" pattern above for diagnostics.
2. **`MaxRequestBodySize` is not configurable** via `ServerOptions`: the fasthttp 4 MB
   default applies to all requests, so large uploads are rejected. Raising it currently
   requires a framework change to set `fasthttp.Server.MaxRequestBodySize`.
3. **The temp directory is not configurable through Azugo**: it follows the Go runtime
   (`$TMPDIR`/`os.TempDir()`). Control it via the environment as described above.

If your application needs large uploads, raise these as framework enhancements so that
`MaxRequestBodySize` and the temp directory can be set through `ServerOptions`, and so
that parse failures are returned rather than silently producing an empty form.
