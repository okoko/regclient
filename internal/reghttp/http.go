// Package reghttp is used for HTTP requests to a registry
package reghttp

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	// crypto libraries included for go-digest
	_ "crypto/sha256"
	_ "crypto/sha512"

	"github.com/sirupsen/logrus"

	"github.com/regclient/regclient/config"
	"github.com/regclient/regclient/internal/auth"
	"github.com/regclient/regclient/internal/reqmeta"
	"github.com/regclient/regclient/types/errs"
	"github.com/regclient/regclient/types/warning"
)

var defaultDelayInit, _ = time.ParseDuration("1s")
var defaultDelayMax, _ = time.ParseDuration("30s")
var warnRegexp = regexp.MustCompile(`^299\s+-\s+"([^"]+)"`)

const (
	DefaultRetryLimit = 3
)

// Client is an HTTP client wrapper.
// It handles features like authentication, retries, backoff delays, TLS settings.
type Client struct {
	getConfigHost func(string) *config.Host
	host          map[string]*clientHost
	httpClient    *http.Client
	rootCAPool    [][]byte
	rootCADirs    []string
	retryLimit    int
	delayInit     time.Duration
	delayMax      time.Duration
	log           *logrus.Logger
	userAgent     string
	mu            sync.Mutex
}

type clientHost struct {
	initialized  bool
	backoffCur   int
	backoffUntil time.Time
	config       *config.Host
	httpClient   *http.Client
	auth         map[string]auth.Auth
	newAuth      func() auth.Auth
	muAuth       sync.Mutex
	reqFreq      time.Duration
	reqNext      time.Time
	muNext       sync.Mutex
}

// Req is a request to send to a registry.
type Req struct {
	MetaKind    reqmeta.Kind                  // kind of request for the priority queue
	Host        string                        // registry name, hostname and mirrors will be looked up from host configuration
	Method      string                        // http method to call
	DirectURL   *url.URL                      // url to query, overrides repository, path, and query
	Repository  string                        // repository to scope the request
	Path        string                        // path of the request within a repository
	Query       url.Values                    // url query parameters
	BodyLen     int64                         // length of body to send
	BodyBytes   []byte                        // bytes of the body, overridden by BodyFunc
	BodyFunc    func() (io.ReadCloser, error) // function to return a new body
	Headers     http.Header                   // headers to send in the request
	NoPrefix    bool                          // do not include the repository prefix
	NoMirrors   bool                          // do not send request to a mirror
	ExpectLen   int64                         // expected size of the returned body
	TransactLen int64                         // size of an overall transaction for the priority queue
	IgnoreErr   bool                          // ignore http errors and do not trigger backoffs
}

// Resp is used to handle the result of a request.
type Resp struct {
	ctx              context.Context
	client           *Client
	req              *Req
	resp             *http.Response
	mirror           string
	done             bool
	reader           io.Reader
	readCur, readMax int64
	throttleDone     func()
}

// Opts is used to configure client options.
type Opts func(*Client)

// NewClient returns a client for handling requests.
func NewClient(opts ...Opts) *Client {
	c := Client{
		httpClient: &http.Client{},
		host:       map[string]*clientHost{},
		retryLimit: DefaultRetryLimit,
		delayInit:  defaultDelayInit,
		delayMax:   defaultDelayMax,
		log:        &logrus.Logger{Out: io.Discard},
		rootCAPool: [][]byte{},
		rootCADirs: []string{},
	}
	for _, opt := range opts {
		opt(&c)
	}
	return &c
}

// WithCerts adds certificates.
func WithCerts(certs [][]byte) Opts {
	return func(c *Client) {
		c.rootCAPool = append(c.rootCAPool, certs...)
	}
}

// WithCertDirs adds directories to check for host specific certs.
func WithCertDirs(dirs []string) Opts {
	return func(c *Client) {
		c.rootCADirs = append(c.rootCADirs, dirs...)
	}
}

// WithCertFiles adds certificates by filename.
func WithCertFiles(files []string) Opts {
	return func(c *Client) {
		for _, f := range files {
			//#nosec G304 command is run by a user accessing their own files
			cert, err := os.ReadFile(f)
			if err != nil {
				c.log.WithFields(logrus.Fields{
					"err":  err,
					"file": f,
				}).Warn("Failed to read certificate")
			} else {
				c.rootCAPool = append(c.rootCAPool, cert)
			}
		}
	}
}

// WithConfigHost adds the callback to request a [config.Host] struct.
func WithConfigHost(gch func(string) *config.Host) Opts {
	return func(c *Client) {
		c.getConfigHost = gch
	}
}

// WithDelay initial time to wait between retries (increased with exponential backoff).
func WithDelay(delayInit time.Duration, delayMax time.Duration) Opts {
	return func(c *Client) {
		if delayInit > 0 {
			c.delayInit = delayInit
		}
		// delayMax must be at least delayInit, if 0 initialize to 30x delayInit
		if delayMax > c.delayInit {
			c.delayMax = delayMax
		} else if delayMax > 0 {
			c.delayMax = c.delayInit
		} else {
			c.delayMax = c.delayInit * 30
		}
	}
}

// WithHTTPClient uses a specific http client with retryable requests.
func WithHTTPClient(hc *http.Client) Opts {
	return func(c *Client) {
		c.httpClient = hc
	}
}

// WithRetryLimit restricts the number of retries (defaults to 5).
func WithRetryLimit(rl int) Opts {
	return func(c *Client) {
		if rl > 0 {
			c.retryLimit = rl
		}
	}
}

// WithLog injects a logrus Logger configuration.
func WithLog(log *logrus.Logger) Opts {
	return func(c *Client) {
		c.log = log
	}
}

// WithTransport uses a specific http transport with retryable requests.
func WithTransport(t *http.Transport) Opts {
	return func(c *Client) {
		c.httpClient = &http.Client{Transport: t}
	}
}

// WithUserAgent sets a user agent header.
func WithUserAgent(ua string) Opts {
	return func(c *Client) {
		c.userAgent = ua
	}
}

// Do runs a request, returning the response result.
func (c *Client) Do(ctx context.Context, req *Req) (*Resp, error) {
	resp := &Resp{
		ctx:     ctx,
		client:  c,
		req:     req,
		readCur: 0,
		readMax: req.ExpectLen,
	}
	err := resp.next()
	return resp, err
}

// next sends requests until a mirror responds or all requests fail.
func (resp *Resp) next() error {
	var err error
	c := resp.client
	req := resp.req
	// lookup reqHost entry
	reqHost := c.getHost(req.Host)
	// create sorted list of mirrors, based on backoffs, upstream, and priority
	hosts := make([]*clientHost, 0, 1+len(reqHost.config.Mirrors))
	if !req.NoMirrors {
		for _, m := range reqHost.config.Mirrors {
			hosts = append(hosts, c.getHost(m))
		}
	}
	hosts = append(hosts, reqHost)
	sort.Slice(hosts, sortHostsCmp(hosts, reqHost.config.Name))
	// loop over requests to mirrors and retries
	curHost := 0
	for {
		backoff := false
		dropHost := false
		retryHost := false
		if len(hosts) == 0 {
			if err != nil {
				return err
			}
			return errs.ErrAllRequestsFailed
		}
		if curHost >= len(hosts) {
			curHost = 0
		}
		h := hosts[curHost]
		resp.mirror = h.config.Name

		// check that context isn't canceled/done
		ctxErr := resp.ctx.Err()
		if ctxErr != nil {
			return ctxErr
		}
		// wait for other concurrent requests to this host
		throttleDone, throttleErr := h.config.Throttle().Acquire(resp.ctx, reqmeta.Data{
			Kind: req.MetaKind,
			Size: req.BodyLen + req.ExpectLen + req.TransactLen,
		})
		if throttleErr != nil {
			return throttleErr
		}

		// try each host in a closure to handle all the backoff/dropHost from one place
		loopErr := func() error {
			var err error
			if req.Method == "HEAD" && h.config.APIOpts != nil {
				var disableHead bool
				disableHead, err = strconv.ParseBool(h.config.APIOpts["disableHead"])
				if err == nil && disableHead {
					dropHost = true
					return fmt.Errorf("head requests disabled for host \"%s\": %w", h.config.Name, errs.ErrUnsupportedAPI)
				}
			}

			// build the url
			var u url.URL
			if req.DirectURL != nil {
				u = *req.DirectURL
			} else {
				u = url.URL{
					Host:   h.config.Hostname,
					Scheme: "https",
				}
				path := strings.Builder{}
				path.WriteString("/v2")
				if h.config.PathPrefix != "" && !req.NoPrefix {
					path.WriteString("/" + h.config.PathPrefix)
				}
				if req.Repository != "" {
					path.WriteString("/" + req.Repository)
				}
				path.WriteString("/" + req.Path)
				u.Path = path.String()
				if h.config.TLS == config.TLSDisabled {
					u.Scheme = "http"
				}
				if req.Query != nil {
					u.RawQuery = req.Query.Encode()
				}
			}
			// close previous response
			if resp.resp != nil && resp.resp.Body != nil {
				_ = resp.resp.Body.Close()
			}
			// delay for backoff if needed
			bu := resp.backoffUntil()
			if !bu.IsZero() && bu.After(time.Now()) {
				sleepTime := time.Until(bu)
				c.log.WithFields(logrus.Fields{
					"Host":    h.config.Name,
					"Seconds": sleepTime.Seconds(),
				}).Warn("Sleeping for backoff")
				select {
				case <-resp.ctx.Done():
					return errs.ErrCanceled
				case <-time.After(sleepTime):
				}
			}
			var httpReq *http.Request
			httpReq, err = http.NewRequestWithContext(resp.ctx, req.Method, u.String(), nil)
			if err != nil {
				dropHost = true
				return err
			}
			if req.BodyFunc != nil {
				body, err := req.BodyFunc()
				if err != nil {
					dropHost = true
					return err
				}
				httpReq.Body = body
				httpReq.GetBody = req.BodyFunc
				httpReq.ContentLength = req.BodyLen
			} else if len(req.BodyBytes) > 0 {
				body := io.NopCloser(bytes.NewReader(req.BodyBytes))
				httpReq.Body = body
				httpReq.GetBody = func() (io.ReadCloser, error) { return body, nil }
				httpReq.ContentLength = req.BodyLen
			}
			if len(req.Headers) > 0 {
				httpReq.Header = req.Headers.Clone()
			}
			if c.userAgent != "" && httpReq.Header.Get("User-Agent") == "" {
				httpReq.Header.Add("User-Agent", c.userAgent)
			}
			if resp.readCur > 0 && resp.readMax > 0 {
				if req.Headers.Get("Range") == "" {
					httpReq.Header.Add("Range", fmt.Sprintf("bytes=%d-%d", resp.readCur, resp.readMax))
				} else {
					// TODO: support Seek within a range request
					dropHost = true
					return fmt.Errorf("unable to resume a connection within a range request")
				}
			}

			hAuth := h.getAuth(req.Repository)
			if hAuth != nil {
				// include docker generated scope to emulate docker clients
				if req.Repository != "" {
					scope := "repository:" + req.Repository + ":pull"
					if req.Method != "HEAD" && req.Method != "GET" {
						scope = scope + ",push"
					}
					_ = hAuth.AddScope(h.config.Hostname, scope)
				}
				// add auth headers
				err = hAuth.UpdateRequest(httpReq)
				if err != nil {
					if errors.Is(err, errs.ErrHTTPUnauthorized) {
						dropHost = true
					} else {
						backoff = true
					}
					return err
				}
			}

			// delay for the rate limit
			if h.reqFreq > 0 {
				h.muNext.Lock()
				if time.Now().Before(h.reqNext) {
					time.Sleep(time.Until(h.reqNext))
					h.reqNext = h.reqNext.Add(h.reqFreq)
				} else {
					h.reqNext = time.Now().Add(h.reqFreq)
				}
				h.muNext.Unlock()
			}

			// update http client for insecure requests and root certs
			httpClient := *h.httpClient

			// send request
			resp.client.log.WithFields(logrus.Fields{
				"url":      httpReq.URL.String(),
				"method":   httpReq.Method,
				"withAuth": (len(httpReq.Header.Values("Authorization")) > 0),
			}).Debug("http req")
			resp.resp, err = httpClient.Do(httpReq)

			if err != nil {
				c.log.WithFields(logrus.Fields{
					"URL": u.String(),
					"err": err,
				}).Debug("Request failed")
				backoff = true
				return err
			}
			// extract any warnings
			// TODO: move warning handler into RoundTripper to get warnings from each round trip
			for _, wh := range resp.resp.Header.Values("Warning") {
				if match := warnRegexp.FindStringSubmatch(wh); len(match) == 2 {
					// TODO: pass other fields (registry hostname) with structured logging
					warning.Handle(resp.ctx, resp.client.log, match[1])
				}
			}
			statusCode := resp.resp.StatusCode
			if statusCode < 200 || statusCode >= 300 {
				switch statusCode {
				case http.StatusUnauthorized:
					// if auth can be done, retry same host without delay, otherwise drop/backoff
					if hAuth != nil {
						err = hAuth.HandleResponse(resp.resp)
					} else {
						err = fmt.Errorf("authentication handler unavailable")
					}
					if err != nil {
						if errors.Is(err, errs.ErrEmptyChallenge) || errors.Is(err, errs.ErrNoNewChallenge) || errors.Is(err, errs.ErrHTTPUnauthorized) {
							c.log.WithFields(logrus.Fields{
								"URL": u.String(),
								"Err": err,
							}).Debug("Failed to handle auth request")
						} else {
							c.log.WithFields(logrus.Fields{
								"URL": u.String(),
								"Err": err,
							}).Warn("Failed to handle auth request")
						}
						dropHost = true
					} else {
						err = fmt.Errorf("authentication required")
						retryHost = true
					}
					return err
				case http.StatusNotFound:
					// if not found, drop mirror for this req, but other requests don't need backoff
					dropHost = true
				case http.StatusRequestedRangeNotSatisfiable:
					// if range request error (blob push), drop mirror for this req, but other requests don't need backoff
					dropHost = true
				case http.StatusTooManyRequests, http.StatusRequestTimeout, http.StatusGatewayTimeout, http.StatusInternalServerError:
					// server is likely overloaded, backoff but still retry
					backoff = true
				default:
					// all other errors indicate a bigger issue, don't retry and set backoff
					backoff = true
					dropHost = true
				}
				c.log.WithFields(logrus.Fields{
					"URL":    u.String(),
					"Status": http.StatusText(statusCode),
				}).Debug("Request failed")
				errHTTP := HTTPError(resp.resp.StatusCode)
				errBody, _ := io.ReadAll(resp.resp.Body)
				_ = resp.resp.Body.Close()
				return fmt.Errorf("request failed: %w: %s", errHTTP, errBody)
			}

			resp.reader = resp.resp.Body
			resp.done = false
			// set variables from headers if found
			clHeader := resp.resp.Header.Get("Content-Length")
			if resp.readCur == 0 && clHeader != "" {
				cl, parseErr := strconv.ParseInt(clHeader, 10, 64)
				if parseErr != nil {
					c.log.WithFields(logrus.Fields{
						"err":    err,
						"header": clHeader,
					}).Debug("failed to parse content-length header")
				} else if resp.readMax > 0 {
					if resp.readMax != cl {
						return fmt.Errorf("unexpected content-length, expected %d, received %d", resp.readMax, cl)
					}
				} else {
					resp.readMax = cl
				}
			}
			// verify Content-Range header when range request used, fail if missing
			if httpReq.Header.Get("Range") != "" && resp.resp.Header.Get("Content-Range") == "" {
				dropHost = true
				_ = resp.resp.Body.Close()
				return fmt.Errorf("range request not supported by server")
			}
			return nil
		}()
		// return on success
		if loopErr == nil {
			resp.throttleDone = throttleDone
			return nil
		}
		// backoff, dropHost, and/or go to next host in the list
		throttleDone()
		if backoff {
			if req.IgnoreErr {
				// don't set a backoff, immediately drop the host when errors ignored
				dropHost = true
			} else {
				boErr := resp.backoffSet()
				if boErr != nil {
					// reached backoff limit
					dropHost = true
				}
			}
		}
		// when error does not allow retries, abort with the last known err value
		if err != nil && errors.Is(loopErr, errs.ErrNotRetryable) {
			return err
		}
		err = loopErr
		if dropHost {
			hosts = append(hosts[:curHost], hosts[curHost+1:]...)
		} else if !retryHost {
			curHost++
		}
	}
}

// HTTPResponse returns the [http.Response] from the last request.
func (resp *Resp) HTTPResponse() *http.Response {
	return resp.resp
}

// Read provides a retryable read from the body of the response.
func (resp *Resp) Read(b []byte) (int, error) {
	if resp.done {
		return 0, io.EOF
	}
	if resp.resp == nil {
		return 0, errs.ErrNotFound
	}
	// perform the read
	i, err := resp.reader.Read(b)
	resp.readCur += int64(i)
	if err == io.EOF || err == io.ErrUnexpectedEOF {
		if resp.resp.Request.Method == "HEAD" || resp.readCur >= resp.readMax {
			resp.backoffClear()
			resp.done = true
		} else {
			// short read, retry?
			resp.client.log.WithFields(logrus.Fields{
				"curRead":    resp.readCur,
				"contentLen": resp.readMax,
			}).Debug("EOF before reading all content, retrying")
			// retry
			respErr := resp.backoffSet()
			if respErr == nil {
				respErr = resp.next()
			}
			// unrecoverable EOF
			if respErr != nil {
				resp.client.log.WithFields(logrus.Fields{
					"err": respErr,
				}).Warn("Failed to recover from short read")
				resp.done = true
				return i, err
			}
			// retry successful, no EOF
			return i, nil
		}
	}

	if err == nil {
		return i, nil
	}
	return i, err
}

// Close frees up resources from the request.
func (resp *Resp) Close() error {
	if resp.throttleDone != nil {
		resp.throttleDone()
		resp.throttleDone = nil
	}
	if resp.resp == nil {
		return errs.ErrNotFound
	}
	if !resp.done {
		resp.backoffClear()
	}
	resp.done = true
	return resp.resp.Body.Close()
}

// Seek provides a limited ability seek within the request response.
func (resp *Resp) Seek(offset int64, whence int) (int64, error) {
	newOffset := resp.readCur
	switch whence {
	case io.SeekStart:
		newOffset = offset
	case io.SeekCurrent:
		newOffset += offset
	case io.SeekEnd:
		if resp.readMax <= 0 {
			return resp.readCur, fmt.Errorf("seek from end is not supported")
		} else if resp.readMax+offset < 0 {
			return resp.readCur, fmt.Errorf("seek past beginning of the file is not supported")
		}
		newOffset = resp.readMax + offset
	default:
		return resp.readCur, fmt.Errorf("unknown value of whence: %d", whence)
	}
	if newOffset != resp.readCur {
		resp.readCur = newOffset
		// rerun the request to restart
		err := resp.next()
		if err != nil {
			return resp.readCur, err
		}
	}
	return resp.readCur, nil
}

func (resp *Resp) backoffClear() {
	c := resp.client
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.host[resp.mirror]
	if ch.backoffCur > c.retryLimit {
		ch.backoffCur = c.retryLimit
	}
	if ch.backoffCur > 0 {
		ch.backoffCur--
		if ch.backoffCur == 0 {
			ch.backoffUntil = time.Time{}
		}
	}
}

func (resp *Resp) backoffSet() error {
	c := resp.client
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.host[resp.mirror]
	ch.backoffCur++
	// sleep for backoff time
	sleepTime := c.delayInit << ch.backoffCur
	// limit to max delay
	if sleepTime > c.delayMax {
		sleepTime = c.delayMax
	}
	// check rate limit header
	if resp.resp != nil && resp.resp.Header.Get("Retry-After") != "" {
		ras := resp.resp.Header.Get("Retry-After")
		ra, _ := time.ParseDuration(ras + "s")
		if ra > c.delayMax {
			sleepTime = c.delayMax
		} else if ra > sleepTime {
			sleepTime = ra
		}
	}

	ch.backoffUntil = time.Now().Add(sleepTime)

	if ch.backoffCur >= c.retryLimit {
		return fmt.Errorf("%w: backoffs %d", errs.ErrBackoffLimit, ch.backoffCur)
	}

	return nil
}

func (resp *Resp) backoffUntil() time.Time {
	c := resp.client
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := c.host[resp.mirror]
	return ch.backoffUntil
}

func (c *Client) getHost(host string) *clientHost {
	c.mu.Lock()
	defer c.mu.Unlock()
	h, ok := c.host[host]
	if ok && h.initialized {
		return h
	}
	if !ok {
		h = &clientHost{}
	}
	if h.config == nil {
		if c.getConfigHost != nil {
			h.config = c.getConfigHost(host)
		} else {
			h.config = config.HostNewName(host)
		}
		// check for normalized hostname
		if h.config.Name != host {
			host = h.config.Name
			hNormal, ok := c.host[host]
			if ok && hNormal.initialized {
				return hNormal
			}
		}
	}
	if h.auth == nil {
		h.auth = map[string]auth.Auth{}
	}
	if h.config.ReqPerSec > 0 && h.reqFreq == 0 {
		h.reqFreq = time.Duration(float64(time.Second) / h.config.ReqPerSec)
	}

	if h.httpClient == nil {
		h.httpClient = c.httpClient
		// update http client for insecure requests and root certs
		if h.config.TLS == config.TLSInsecure || len(c.rootCAPool) > 0 || len(c.rootCADirs) > 0 || h.config.RegCert != "" || (h.config.ClientCert != "" && h.config.ClientKey != "") {
			// create a new client and modify the transport
			httpClient := *c.httpClient
			if httpClient.Transport == nil {
				httpClient.Transport = http.DefaultTransport.(*http.Transport).Clone()
			}
			t, ok := httpClient.Transport.(*http.Transport)
			if ok {
				var tlsc *tls.Config
				if t.TLSClientConfig != nil {
					tlsc = t.TLSClientConfig.Clone()
				} else {
					//#nosec G402 the default TLS 1.2 minimum version is allowed to support older registries
					tlsc = &tls.Config{}
				}
				if h.config.TLS == config.TLSInsecure {
					tlsc.InsecureSkipVerify = true
				} else {
					rootPool, err := makeRootPool(c.rootCAPool, c.rootCADirs, h.config.Hostname, h.config.RegCert)
					if err != nil {
						c.log.WithFields(logrus.Fields{
							"err": err,
						}).Warn("failed to setup CA pool")
					} else {
						tlsc.RootCAs = rootPool
					}
				}
				if h.config.ClientCert != "" && h.config.ClientKey != "" {
					cert, err := tls.X509KeyPair([]byte(h.config.ClientCert), []byte(h.config.ClientKey))
					if err != nil {
						c.log.WithFields(logrus.Fields{
							"err": err,
						}).Warn("failed to configure client certs")
					} else {
						tlsc.Certificates = []tls.Certificate{cert}
					}
				}
				t.TLSClientConfig = tlsc
				httpClient.Transport = t
			}
			h.httpClient = &httpClient
		}
	}

	if h.newAuth == nil {
		h.newAuth = func() auth.Auth {
			return auth.NewAuth(
				auth.WithLog(c.log),
				auth.WithHTTPClient(h.httpClient),
				auth.WithCreds(h.AuthCreds()),
				auth.WithClientID(c.userAgent),
			)
		}
	}

	h.initialized = true
	c.host[host] = h
	return h
}

// getAuth returns an auth, which may be repository specific.
func (ch *clientHost) getAuth(repo string) auth.Auth {
	ch.muAuth.Lock()
	defer ch.muAuth.Unlock()
	if !ch.config.RepoAuth {
		repo = "" // without RepoAuth, unset the provided repo
	}
	if _, ok := ch.auth[repo]; !ok {
		ch.auth[repo] = ch.newAuth()
	}
	return ch.auth[repo]
}

func (ch *clientHost) AuthCreds() func(h string) auth.Cred {
	if ch == nil || ch.config == nil {
		return auth.DefaultCredsFn
	}
	return func(h string) auth.Cred {
		hCred := ch.config.GetCred()
		return auth.Cred{User: hCred.User, Password: hCred.Password, Token: hCred.Token}
	}
}

// HTTPError returns an error based on the status code.
func HTTPError(statusCode int) error {
	switch statusCode {
	case 401:
		return fmt.Errorf("%w [http %d]", errs.ErrHTTPUnauthorized, statusCode)
	case 403:
		return fmt.Errorf("%w [http %d]", errs.ErrHTTPUnauthorized, statusCode)
	case 404:
		return fmt.Errorf("%w [http %d]", errs.ErrNotFound, statusCode)
	case 429:
		return fmt.Errorf("%w [http %d]", errs.ErrHTTPRateLimit, statusCode)
	default:
		return fmt.Errorf("%w: %s [http %d]", errs.ErrHTTPStatus, http.StatusText(statusCode), statusCode)
	}
}

func makeRootPool(rootCAPool [][]byte, rootCADirs []string, hostname string, hostcert string) (*x509.CertPool, error) {
	pool, err := x509.SystemCertPool()
	if err != nil {
		return nil, err
	}
	for _, ca := range rootCAPool {
		if ok := pool.AppendCertsFromPEM(ca); !ok {
			return nil, fmt.Errorf("failed to load ca: %s", ca)
		}
	}
	for _, dir := range rootCADirs {
		hostDir := filepath.Join(dir, hostname)
		files, err := os.ReadDir(hostDir)
		if err != nil {
			if !os.IsNotExist(err) {
				return nil, fmt.Errorf("failed to read directory %s: %w", hostDir, err)
			}
			continue
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			if strings.HasSuffix(f.Name(), ".crt") {
				f := filepath.Join(hostDir, f.Name())
				//#nosec G304 file from a known directory and extension read by the user running the command on their own host
				cert, err := os.ReadFile(f)
				if err != nil {
					return nil, fmt.Errorf("failed to read %s: %w", f, err)
				}
				if ok := pool.AppendCertsFromPEM(cert); !ok {
					return nil, fmt.Errorf("failed to import cert from %s", f)
				}
			}
		}
	}
	if hostcert != "" {
		if ok := pool.AppendCertsFromPEM([]byte(hostcert)); !ok {
			// try to parse the certificate and generate a useful error
			block, _ := pem.Decode([]byte(hostcert))
			if block == nil {
				err = fmt.Errorf("pem.Decode is nil")
			} else {
				_, err = x509.ParseCertificate(block.Bytes)
			}
			return nil, fmt.Errorf("failed to load host specific ca (registry: %s): %w: %s", hostname, err, hostcert)
		}
	}
	return pool, nil
}

// sortHostCmp to sort host list of mirrors.
func sortHostsCmp(hosts []*clientHost, upstream string) func(i, j int) bool {
	now := time.Now()
	// sort by backoff first, then priority decending, then upstream name last
	return func(i, j int) bool {
		if now.Before(hosts[i].backoffUntil) || now.Before(hosts[j].backoffUntil) {
			return hosts[i].backoffUntil.Before(hosts[j].backoffUntil)
		}
		if hosts[i].config.Priority != hosts[j].config.Priority {
			return hosts[i].config.Priority < hosts[j].config.Priority
		}
		return hosts[i].config.Name != upstream
	}
}
