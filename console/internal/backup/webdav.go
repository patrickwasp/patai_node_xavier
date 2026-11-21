package backup

import (
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// WebDAV is a folder on a WebDAV server, such as Nextcloud's, a NAS's, or rclone's, with basic auth.
type WebDAV struct {
	URL      string // the folder's URL
	User     string
	Password string
	Client   *http.Client // nil is HTTPClient()
}

const propfind = `<?xml version="1.0" encoding="utf-8"?>
<D:propfind xmlns:D="DAV:"><D:prop><D:getcontentlength/><D:getlastmodified/><D:resourcetype/></D:prop></D:propfind>`

// client doesn't follow redirects: one would turn a WebDAV method into a GET, and could drop the password.
func (w *WebDAV) client() *http.Client {
	c := HTTPClient()
	if w.Client != nil {
		copied := *w.Client
		c = &copied
	}
	c.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return c
}

func (w *WebDAV) folder() (*url.URL, error) {
	u, err := url.Parse(w.URL)
	if err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return nil, errors.New("that isn't a folder's address")
	}
	if !strings.HasSuffix(u.Path, "/") {
		u.Path += "/"
		if u.RawPath != "" {
			u.RawPath += "/"
		}
	}
	return u, nil
}

func (w *WebDAV) file(name string) (*url.URL, error) {
	u, err := w.folder()
	if err != nil {
		return nil, err
	}
	return u.JoinPath(name), nil
}

func (w *WebDAV) do(ctx context.Context, method string, u *url.URL, body io.Reader, header map[string]string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return nil, err
	}
	if body != nil && req.ContentLength == 0 {
		req.ContentLength = -1 // sent in chunks, however long it turns out
	}
	for k, v := range header {
		req.Header.Set(k, v)
	}
	if w.User != "" || w.Password != "" {
		req.SetBasicAuth(w.User, w.Password)
	}
	resp, err := w.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("the server didn't answer: %w", unwrapURL(err))
	}
	return resp, nil
}

// davError is the error for an answer that wasn't what was asked for.
func davError(resp *http.Response) error {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return errors.New("the user name or password was refused")
	case resp.StatusCode/100 == 3 && resp.Header.Get("Location") != "":
		return fmt.Errorf("the folder has moved to %s", resp.Header.Get("Location"))
	}
	return fmt.Errorf("the server answered %s", resp.Status)
}

func drain(resp *http.Response) {
	io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
	resp.Body.Close()
}

func (w *WebDAV) List(ctx context.Context) ([]Object, error) {
	folder, err := w.folder()
	if err != nil {
		return nil, err
	}
	resp, err := w.do(ctx, "PROPFIND", folder, strings.NewReader(propfind),
		map[string]string{"Depth": "1", "Content-Type": "application/xml; charset=utf-8"})
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusNotFound {
		drain(resp)
		return nil, nil
	}
	if resp.StatusCode != http.StatusMultiStatus {
		return nil, davError(resp)
	}
	var result struct {
		Responses []struct {
			Href      string `xml:"DAV: href"`
			Propstats []struct {
				Status string `xml:"DAV: status"`
				Prop   struct {
					Length   string `xml:"DAV: getcontentlength"`
					Modified string `xml:"DAV: getlastmodified"`
					Type     struct {
						Collection *struct{} `xml:"DAV: collection"`
					} `xml:"DAV: resourcetype"`
				} `xml:"DAV: prop"`
			} `xml:"DAV: propstat"`
		} `xml:"DAV: response"`
	}
	err = xml.NewDecoder(io.LimitReader(resp.Body, 64<<20)).Decode(&result)
	drain(resp)
	if err != nil {
		return nil, errors.New("the server's listing couldn't be read")
	}
	var objects []Object
	for _, r := range result.Responses {
		p := r.Href
		if u, err := url.Parse(r.Href); err == nil {
			p = u.Path
		} else if unescaped, err := url.PathUnescape(r.Href); err == nil {
			p = unescaped
		}
		p = strings.TrimSuffix(p, "/")
		if p == "" || p == strings.TrimSuffix(folder.Path, "/") {
			continue
		}
		var o Object
		found, collection := false, false
		for _, ps := range r.Propstats {
			if !strings.Contains(ps.Status, " 200") {
				continue
			}
			found = true
			collection = collection || ps.Prop.Type.Collection != nil
			if n, err := strconv.ParseInt(strings.TrimSpace(ps.Prop.Length), 10, 64); err == nil {
				o.Size = n
			}
			if at, err := http.ParseTime(strings.TrimSpace(ps.Prop.Modified)); err == nil {
				o.Modified = at
			}
		}
		if found && !collection {
			o.Name = path.Base(p)
			objects = append(objects, o)
		}
	}
	return objects, nil
}

// mkcol makes the folder at u, and any above it that aren't there.
func (w *WebDAV) mkcol(ctx context.Context, u *url.URL, depth int) error {
	resp, err := w.do(ctx, "MKCOL", u, nil, nil)
	if err != nil {
		return err
	}
	switch resp.StatusCode {
	case http.StatusCreated, http.StatusMethodNotAllowed: // 405 is a folder that's there already
		drain(resp)
		return nil
	case http.StatusConflict: // the folder above isn't there
		parent := strings.TrimSuffix(u.Path, "/")
		if depth > 32 || !strings.Contains(parent, "/") || parent == "" {
			return davError(resp)
		}
		drain(resp)
		above := *u
		above.Path, above.RawPath = strings.TrimSuffix(path.Dir(parent), "/")+"/", ""
		if err := w.mkcol(ctx, &above, depth+1); err != nil {
			return err
		}
		resp, err := w.do(ctx, "MKCOL", u, nil, nil)
		if err != nil {
			return err
		}
		if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusMethodNotAllowed {
			drain(resp)
			return nil
		}
		return davError(resp)
	}
	return davError(resp)
}

// Put writes a small file as an upload does, so a cut-off write never leaves half of one.
func (w *WebDAV) Put(ctx context.Context, name string, data []byte) error {
	_, _, err := w.Upload(ctx, name, bytes.NewReader(data), func() error { return nil }, nil)
	return err
}

func (w *WebDAV) Get(ctx context.Context, name string) (io.ReadCloser, int64, error) {
	u, err := w.file(name)
	if err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithCancel(ctx)
	// identity, or Go's client would unpack a compressed answer and not know its length.
	resp, err := w.do(ctx, http.MethodGet, u, nil, map[string]string{"Accept-Encoding": "identity"})
	if err != nil {
		cancel()
		return nil, 0, err
	}
	if resp.StatusCode != http.StatusOK {
		cancel()
		if resp.StatusCode == http.StatusNotFound {
			drain(resp)
			return nil, 0, errMissing
		}
		return nil, 0, davError(resp)
	}
	return GuardStall(resp.Body, cancel), resp.ContentLength, nil
}

func (w *WebDAV) Delete(ctx context.Context, name string) error {
	u, err := w.file(name)
	if err != nil {
		return err
	}
	resp, err := w.do(ctx, http.MethodDelete, u, nil, nil)
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 && resp.StatusCode != http.StatusNotFound {
		return davError(resp)
	}
	drain(resp)
	return nil
}

func (w *WebDAV) Upload(ctx context.Context, name string, r io.Reader, done func() error, progress func(int64)) (int64, string, error) {
	folder, err := w.folder()
	if err != nil {
		return 0, "", err
	}
	if err := w.mkcol(ctx, folder, 0); err != nil {
		return 0, "", err
	}
	part, final := folder.JoinPath(PartName(name)), folder.JoinPath(name)
	sendCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	in := newSending(sendCtx, r, progress, cancel)
	resp, err := w.do(sendCtx, http.MethodPut, part, in, map[string]string{"Content-Type": "application/octet-stream"})
	err = in.end(sendCtx, err)
	if err == nil && resp.StatusCode/100 != 2 {
		err = davError(resp)
	} else if err == nil {
		drain(resp)
		if !in.whole() {
			err = errors.New("the server answered before it had the whole file")
		}
	}
	if err == nil {
		err = done()
	}
	if err == nil {
		resp, err = w.do(ctx, "MOVE", part, nil, map[string]string{"Destination": final.String(), "Overwrite": "T"})
		if err == nil && resp.StatusCode/100 != 2 {
			err = davError(resp)
		} else if err == nil {
			drain(resp)
		}
	}
	if err != nil {
		w.discard(part)
		return 0, "", err
	}
	return in.result()
}

// discard removes an upload that won't be finished.
func (w *WebDAV) discard(part *url.URL) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	if resp, err := w.do(ctx, http.MethodDelete, part, nil, nil); err == nil {
		drain(resp)
	}
}
