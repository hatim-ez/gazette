package gazette

import (
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"time"

	log "github.com/Sirupsen/logrus"
	"github.com/hashicorp/golang-lru"

	"github.com/pippio/gazette/journal"
)

const (
	// By default, all client operations are applied against the default
	// endpoint. However, the client will cache the last |kClientRouteCacheSize|
	// redirect or Location: headers received for distinct paths, and directly
	// route future requests to cached locations. This allows the client to
	// discover direct, responsible endpoints for journals it uses.
	kClientRouteCacheSize = 1024

	// Upper-bound time we'll wait for response headers. This should be very
	// large, as we never want a thundering herd where clients time out an
	// operational but slow broker, but we also need to guard against brokers
	// who die (and don't send a RST). Note that Gazette reads (including
	// blocking reads) immediately return response headers, so this timeout
	// is largely directed at writes (which return headers only on completion
	// or error).
	kClientResponseHeaderTimeout = time.Minute * 5
)

type httpClient interface {
	Do(*http.Request) (*http.Response, error)
	Get(url string) (*http.Response, error)
}

var kContentRangeRegexp = regexp.MustCompile("bytes\\s+(\\d+)-\\d+/\\d+")

type Client struct {
	// Endpoint which is queried by default.
	defaultEndpoint *url.URL

	// Maps request.URL.Path to previously-received "Location:" headers,,
	// stripped of URL query arguments. Future requests of the same URL path are
	// first attempted against the cached endpoint.
	locationCache *lru.Cache

	httpClient httpClient
}

func NewClient(endpoint string) (*Client, error) {
	ep, err := url.Parse(endpoint)
	if err != nil {
		return nil, err
	}
	cache, err := lru.New(kClientRouteCacheSize)
	if err != nil {
		return nil, err
	}

	httpTransport := &http.Transport{
		Proxy: http.ProxyFromEnvironment,
		ResponseHeaderTimeout: kClientResponseHeaderTimeout,
	}
	// When testing, fragment locations are "persisted" to the local filesystem,
	// and file:// URL's are returned by Gazette servers. Register a protocol
	// handler so they may be opened by the client.
	httpTransport.RegisterProtocol("file", http.NewFileTransport(http.Dir("/")))

	return &Client{
		defaultEndpoint: ep,
		locationCache:   cache,
		httpClient:      &http.Client{Transport: httpTransport},
	}, nil
}

func (c *Client) Head(args journal.ReadArgs) (journal.ReadResult, *url.URL) {
	request, err := http.NewRequest("HEAD", c.buildReadURL(args).String(), nil)
	if err != nil {
		return journal.ReadResult{Error: err}, nil
	}
	response, err := c.Do(request)
	if err != nil {
		return journal.ReadResult{Error: err}, nil
	}

	result, fragmentLocation := c.parseReadResult(args, response)
	response.Body.Close()
	return result, fragmentLocation
}

func (c *Client) GetDirect(args journal.ReadArgs) (journal.ReadResult, io.ReadCloser) {
	request, err := http.NewRequest("GET", c.buildReadURL(args).String(), nil)
	if err != nil {
		return journal.ReadResult{Error: err}, nil
	}
	response, err := c.Do(request)
	if err != nil {
		return journal.ReadResult{Error: err}, nil
	}

	result, _ := c.parseReadResult(args, response)
	if result.Error != nil {
		response.Body.Close()
		return result, nil
	}
	return result, response.Body
}

func (c *Client) Get(args journal.ReadArgs) (journal.ReadResult, io.ReadCloser) {
	// Perform a non-blocking HEAD first, to check for an available persisted fragment.
	headArgs := args
	headArgs.Blocking = false
	result, fragmentLocation := c.Head(headArgs)

	if result.Error == journal.ErrNotYetAvailable {
		// Fall-through, re-attempting request as a GET.
	} else if result.Error != nil {
		return result, nil
	} else if fragmentLocation != nil {
		body, err := c.openFragment(fragmentLocation, result)
		result.Error = err
		return result, body
	}
	// No persisted fragment is available. We must repeat the request as a GET.
	// Data will be streamed directly from the server.
	return c.GetDirect(args)
}

// Returns a reader by reading directly from a fragment. |location| is a
// potentially signed or authorized URL to fragment storage. The fragment is
// opened, seek'd to the desired |result.Offset|, and returned. Note we don't
// use a range request here, as the fragment is usually gzip'd (and implicitly
// decompressed while being read).
func (c *Client) openFragment(location *url.URL,
	result journal.ReadResult) (io.ReadCloser, error) {

	response, err := c.httpClient.Get(location.String())
	if err != nil {
		return nil, err
	} else if response.StatusCode != http.StatusOK {
		response.Body.Close()
		return nil, fmt.Errorf("fetching fragment: %s", response.Status)
	}
	// Attempt to seek to |result.Offset| within the fragment.
	delta := result.Offset - result.Fragment.Begin
	if _, err := io.CopyN(ioutil.Discard, response.Body, delta); err != nil {
		response.Body.Close()
		return nil, fmt.Errorf("seeking fragment: %s", err)
	}
	return response.Body, nil // Success.
}

// Performs a Gazette PUT operation, which appends content to the named journal.
func (c *Client) Put(args journal.AppendArgs) error {
	request, err := http.NewRequest("PUT", "/"+args.Journal.String(), args.Content)
	if err != nil {
		return err
	}
	response, err := c.Do(request)
	if err != nil {
		return err
	}
	err = c.parseAppendResponse(response)
	response.Body.Close()
	return err
}

func (c *Client) buildReadURL(args journal.ReadArgs) *url.URL {
	return &url.URL{
		Path: "/" + string(args.Journal),
		RawQuery: url.Values{
			"offset": {strconv.FormatInt(args.Offset, 10)},
			"block":  {strconv.FormatBool(args.Blocking)},
		}.Encode(),
	}
}

func (c *Client) parseReadResult(args journal.ReadArgs,
	response *http.Response) (result journal.ReadResult, fragmentLocation *url.URL) {

	// Attempt to parse Content-Range offset.
	contentRangeStr := response.Header.Get("Content-Range")
	if contentRangeStr != "" {
		if m := kContentRangeRegexp.FindStringSubmatch(contentRangeStr); len(m) == 0 {
			result.Error = fmt.Errorf("invalid Content-Range: %s", contentRangeStr)
			return
		} else if offset, err := strconv.ParseInt(m[1], 10, 64); err != nil {
			// Regular expression match asserts this should parse.
			log.WithFields(log.Fields{"err": err, "match": m[1]}).Panic("failed to convert")
		} else {
			result.Offset = offset
		}
	}
	// Attempt to parse write head.
	writeHeadStr := response.Header.Get(WriteHeadHeader)
	if writeHeadStr != "" {
		if head, err := strconv.ParseInt(writeHeadStr, 10, 64); err != nil {
			result.Error = fmt.Errorf("parsing %s: %s", WriteHeadHeader, err)
			return
		} else {
			result.WriteHead = head
		}
	}
	// Attempt to parse fragment.
	fragmentNameStr := response.Header.Get(FragmentNameHeader)
	if fragmentNameStr != "" {
		if fragment, err := journal.ParseFragment(args.Journal, fragmentNameStr); err != nil {
			result.Error = fmt.Errorf("parsing %s: %s", FragmentNameHeader, err)
			return
		} else {
			result.Fragment = fragment
		}
	}
	fragmentLocationStr := response.Header.Get(FragmentLocationHeader)

	if response.StatusCode == http.StatusRequestedRangeNotSatisfiable {
		result.Error = journal.ErrNotYetAvailable
		return
	} else if response.StatusCode != http.StatusPartialContent {
		// Read and return response body as result.Error. Note that if this is a
		// HEAD, the response body will be empty.
		body, _ := ioutil.ReadAll(response.Body)
		result.Error = fmt.Errorf("%s (%s)", response.Status, string(body))
		return
	}
	// We have a "success" status from the server. Expect required headers are present.
	if contentRangeStr == "" {
		result.Error = fmt.Errorf("expected Content-Range header")
		return
	} else if writeHeadStr == "" {
		result.Error = fmt.Errorf("expected %s header", WriteHeadHeader)
		return
	}
	// Fragment name is optional (it won't be available on blocked requests).

	// Fragment location is optional, but expect that it parses if present.
	if fragmentLocationStr != "" {
		if l, err := url.Parse(fragmentLocationStr); err != nil {
			result.Error = fmt.Errorf("parsing %s: %s", FragmentLocationHeader, err)
			return
		} else {
			fragmentLocation = l
		}
	}
	return
}

func (c *Client) parseAppendResponse(response *http.Response) error {
	if response.StatusCode != http.StatusNoContent {
		if body, err := ioutil.ReadAll(response.Body); err != nil {
			return err
		} else {
			return fmt.Errorf("%s (%s)", response.Status, string(body))
		}
	}
	return nil
}

// Thin layer upon http.Do(), which manages re-writes from and update to the
// Client.locationCache. Specifically, request.Path is mapped into a previously-
// stored Location re-write. If none is available, the request is re-written to
// reference the default endpoint. Cache entries are updated on successful
// redirect or response with a Location: header. On error, cache entries are
// expunged (eg, future requests are performed against the default endpoint).
func (c *Client) Do(request *http.Request) (*http.Response, error) {
	// Apply a cached re-write for this request path if found.
	if cached, ok := c.locationCache.Get(request.URL.Path); ok {
		location := cached.(*url.URL)
		request.URL.Scheme = location.Scheme
		request.URL.User = location.User
		request.URL.Host = location.Host
		request.URL.Path = location.Path
		// Note that RawQuery is not re-written.
	} else {
		// Otherwise, re-write to use the default endpoint.
		request.URL.Scheme = c.defaultEndpoint.Scheme
		request.URL.User = c.defaultEndpoint.User
		request.URL.Host = c.defaultEndpoint.Host
		// Note that Path & RawQuery are not re-written.
	}

	response, err := c.httpClient.Do(request)
	if err != nil {
		c.locationCache.Remove(request.URL.Path)
		return response, err
	}

	if location, err := response.Location(); err == nil {
		// The response included a Location header. Cache it for future use.
		// It probably also indicates request failure as well (30X or 404 response).
		c.locationCache.Add(request.URL.Path, location)
	} else if err != http.ErrNoLocation {
		log.WithField("err", err).Warn("parsing Gazette Location header")
	} else if response.Request.URL.String() != request.URL.String() {
		// We successfully followed a redirect. Cache it for future use.
		c.locationCache.Add(request.URL.Path, response.Request.URL)
	}
	return response, err
}
