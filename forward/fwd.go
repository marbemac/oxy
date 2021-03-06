// package forwarder implements http handler that forwards requests to remote server
// and serves back the response
package forward

import (
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"time"

	"github.com/mailgun/oxy/utils"
)

type ReqObserver interface {
	OnRequest(r *http.Request)
	OnResponse(r *http.Request, resp *http.Response, d time.Duration)
}

func Observer(r ReqObserver) optSetter {
	return func(f *Forwarder) error {
		f.observer = r
		return nil
	}
}

// ReqRewriter can alter request headers and body
type ReqRewriter interface {
	Rewrite(r *http.Request)
}

type optSetter func(f *Forwarder) error

func RoundTripper(r http.RoundTripper) optSetter {
	return func(f *Forwarder) error {
		f.roundTripper = r
		return nil
	}
}

func Rewriter(r ReqRewriter) optSetter {
	return func(f *Forwarder) error {
		f.rewriter = r
		return nil
	}
}

// ErrorHandler is a functional argument that sets error handler of the server
func ErrorHandler(h utils.ErrorHandler) optSetter {
	return func(f *Forwarder) error {
		f.errHandler = h
		return nil
	}
}

func Logger(l utils.Logger) optSetter {
	return func(f *Forwarder) error {
		f.log = l
		return nil
	}
}

type Forwarder struct {
	errHandler   utils.ErrorHandler
	roundTripper http.RoundTripper
	rewriter     ReqRewriter
	log          utils.Logger
	observer     ReqObserver
}

func New(setters ...optSetter) (*Forwarder, error) {
	f := &Forwarder{}
	for _, s := range setters {
		if err := s(f); err != nil {
			return nil, err
		}
	}
	if f.roundTripper == nil {
		f.roundTripper = http.DefaultTransport
	}
	if f.rewriter == nil {
		h, err := os.Hostname()
		if err != nil {
			h = "localhost"
		}
		f.rewriter = &HeaderRewriter{TrustForwardHeader: true, Hostname: h}
	}
	if f.log == nil {
		f.log = utils.NullLogger
	}
	if f.errHandler == nil {
		f.errHandler = utils.DefaultHandler
	}
	return f, nil
}

func (f *Forwarder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if f.observer != nil {
		f.observer.OnRequest(req)
	}

	start := time.Now().UTC()
	response, err := f.roundTripper.RoundTrip(f.copyRequest(req, req.URL))
	duration := time.Now().UTC().Sub(start)
	if err != nil {
		f.log.Errorf("Error forwarding to %v, err: %v, resp: %v", req.URL, err, response)
		if f.observer != nil {
			f.observer.OnResponse(req, response, duration)
		}
		f.errHandler.ServeHTTP(w, req, err)
		return
	}
	if req.TLS != nil {
		f.log.Infof("Round trip: %v, code: %v, duration: %v tls:version: %x, tls:resume:%t, tls:csuite:%x, tls:server:%v",
			req.URL, response.StatusCode, time.Now().UTC().Sub(start),
			req.TLS.Version,
			req.TLS.DidResume,
			req.TLS.CipherSuite,
			req.TLS.ServerName)
	} else {
		f.log.Infof("Round trip: %v, code: %v, duration: %v",
			req.URL, response.StatusCode, time.Now().UTC().Sub(start))
	}

	if f.observer != nil {
		f.observer.OnResponse(req, response, duration)
	}

	utils.CopyHeaders(w.Header(), response.Header)
	w.WriteHeader(response.StatusCode)
	written, _ := io.Copy(w, response.Body)
	if written != 0 {
		w.Header().Set(ContentLength, strconv.FormatInt(written, 10))
	}
	response.Body.Close()
}

func (f *Forwarder) copyRequest(req *http.Request, u *url.URL) *http.Request {
	outReq := new(http.Request)
	*outReq = *req // includes shallow copies of maps, but we handle this below

	outReq.URL = utils.CopyURL(req.URL)
	outReq.URL.Scheme = u.Scheme
	outReq.URL.Host = u.Host
	outReq.URL.Opaque = u.Opaque
	outReq.URL.Path = u.Path
	outReq.URL.RawQuery = u.RawQuery
	outReq.URL.Fragment = u.Fragment

	outReq.Proto = "HTTP/1.1"
	outReq.ProtoMajor = 1
	outReq.ProtoMinor = 1

	// Overwrite close flag so we can keep persistent connection for the backend servers
	outReq.Close = false

	outReq.Header = make(http.Header)
	utils.CopyHeaders(outReq.Header, req.Header)

	if f.rewriter != nil {
		f.rewriter.Rewrite(outReq)
	}
	return outReq
}
