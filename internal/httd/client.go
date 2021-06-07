package httd

import (
	"bytes"
	"compress/gzip"
	"context"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"strconv"
	"time"

	"github.com/Vedza/disgord/json"
)

// defaults and string format's for Discord interaction
const (
	BaseURL = "https://discord.com/api"

	RegexpSnowflakes     = `([0-9]+)`
	RegexpURLSnowflakes  = `\/` + RegexpSnowflakes + `\/?`
	RegexpEmoji          = `([.^/]+)\s?`
	RegexpReactionPrefix = `\/channels\/([0-9]+)\/messages\/\{id\}\/reactions\/`

	// Header
	AuthorizationFormat = "%s"
	UserAgentFormat     = "DiscordBot (%s, %s) %s"

	ContentEncoding = "Content-Encoding"
	ContentType     = "Content-Type"
	ContentTypeJSON = "application/json"
	GZIPCompression = "gzip"
)

// Requester holds all the sub-request interface for Discord interaction
type Requester interface {
	Do(ctx context.Context, req *Request) (resp *http.Response, body []byte, err error)
}

// TODO: should RESTBucket and RESTBucketManager be merged?

// RESTBucket is a REST bucket for one endpoint or several endpoints. This includes the global bucket.
type RESTBucket interface {
	// Transaction allows a selective atomic transaction. For distributed systems, the buckets can be
	// eventual consistent until a rate limit is hit then they must be strongly consistent. The global bucket
	// must always be strongly consistent. Tip: it might be easier/best to keep everything strongly consistent,
	// and only care about eventual consistency to get better performance as a "bug"/"accident".
	Transaction(context.Context, func() (*http.Response, []byte, error)) (*http.Response, []byte, error)
}

// RESTBucketManager manages the buckets and the global bucket.
type RESTBucketManager interface {
	// Bucket returns the bucket for a given local hash. Note that a local hash simply means
	// a hashed endpoint. This is because Discord does not specify bucket hashed ahead of time.
	// Note you should map localHashes to Discord bucket hashes once that insight have been gained.
	// Discord Bucket hashes are found in the response header, field name `X-RateLimit-Bucket`.
	Bucket(localHash string, cb func(bucket RESTBucket))

	// BucketGrouping shows which hashed endpoints falls under which bucket hash
	// here a bucket hash is defined by discord, otherwise the bucket hash
	// is the same as the hashed endpoint.
	//
	// Hashed endpoints are generated by the Request struct.
	BucketGrouping() (group map[string][]string)
}

type ErrREST struct {
	Code           int      `json:"code"`
	Msg            string   `json:"message"`
	Suggestion     string   `json:"-"`
	HTTPCode       int      `json:"-"`
	Bucket         []string `json:"-"`
	HashedEndpoint string   `json:"-"`
}

var _ error = (*ErrREST)(nil)

func (e *ErrREST) Error() string {
	return fmt.Sprintf("%s\n%s\n%s => %+v", e.Msg, e.Suggestion, e.HashedEndpoint, e.Bucket)
}

type HttpClientDoer interface {
	Do(req *http.Request) (*http.Response, error)
}

// Client for handling Discord REST requests
type Client struct {
	url                          string // base url with API version
	reqHeader                    http.Header
	httpClient                   HttpClientDoer
	cancelRequestWhenRateLimited bool
	buckets                      RESTBucketManager
}

func (c *Client) BucketGrouping() (group map[string][]string) {
	return c.buckets.BucketGrouping()
}

// SupportsDiscordAPIVersion check if a given discord api version is supported by this package.
func SupportsDiscordAPIVersion(version int) bool {
	supports := []int{
		8,
	}

	var supported bool
	for _, supportedVersion := range supports {
		if supportedVersion == version {
			supported = true
			break
		}
	}

	return supported
}

func copyHeader(h http.Header) http.Header {
	cp := make(http.Header, len(h))
	for k, vs := range h {
		for i := range vs {
			cp.Add(k, vs[i])
		}
	}
	return cp
}

// NewClient ...
func NewClient(conf *Config) (*Client, error) {
	if !SupportsDiscordAPIVersion(conf.APIVersion) {
		return nil, errors.New(fmt.Sprintf("Discord API version %d is not supported", conf.APIVersion))
	}

	if conf.BotToken == "" {
		return nil, errors.New("no Discord Bot Token was provided")
	}

	if conf.HttpClient == nil {
		return nil, errors.New("missing http client")
	}

	if conf.RESTBucketManager == nil {
		conf.RESTBucketManager = NewManager(nil)
	}

	// Clients using the HTTP API must provide a valid User Agent which specifies
	// information about the client library and version in the following format:
	//	User-Agent: DiscordBot ($url, $versionNumber)
	if conf.UserAgentSourceURL == "" || conf.UserAgentVersion == "" {
		return nil, errors.New("both a source(url) and a version must be present for sending requests to the Discord REST API")
	}

	// setup the required http request header fields
	authorization := fmt.Sprintf(AuthorizationFormat, conf.BotToken)
	userAgent := fmt.Sprintf(UserAgentFormat, conf.UserAgentSourceURL, conf.UserAgentVersion, conf.UserAgentExtra)
	header := map[string][]string{
		"Authorization":   {authorization},
		"User-Agent":      {userAgent},
		"Accept-Encoding": {"gzip"},
	}

	return &Client{
		url:        BaseURL + "/v" + strconv.Itoa(conf.APIVersion),
		reqHeader:  header,
		httpClient: conf.HttpClient,
		buckets:    conf.RESTBucketManager,
	}, nil
}

// Config is the configuration options for the httd.Client structure. Essentially the behaviour of all requests
// sent to Discord.
type Config struct {
	APIVersion int
	BotToken   string

	HttpClient HttpClientDoer

	CancelRequestWhenRateLimited bool

	// RESTBucketManager stores all rate limit buckets and dictates the behaviour of how rate limiting is respected
	RESTBucketManager RESTBucketManager

	// Header field: `User-Agent: DiscordBot ({Source}, {Version}) {Extra}`
	UserAgentVersion   string
	UserAgentSourceURL string
	UserAgentExtra     string
}

// Details ...
type Details struct {
	Ratelimiter     string
	Endpoint        string // always as a suffix to Ratelimiter(!)
	ResponseStruct  interface{}
	SuccessHTTPCode int
}

func (c *Client) decodeResponseBody(resp *http.Response) (body []byte, err error) {
	buffer, err := ioutil.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}

	switch resp.Header.Get(ContentEncoding) {
	case GZIPCompression:
		b := bytes.NewBuffer(buffer)
		r, err := gzip.NewReader(b)
		if err != nil {
			return nil, err
		}
		defer r.Close()

		var resB bytes.Buffer
		_, err = resB.ReadFrom(r)
		if err != nil {
			return nil, err
		}

		body = resB.Bytes()
	default:
		body = buffer
	}

	return body, nil
}

func (c *Client) Do(ctx context.Context, r *Request) (resp *http.Response, body []byte, err error) {
	r.PopulateMissing()
	if r.Body != nil && r.bodyReader == nil {
		switch b := r.Body.(type) { // Determine the type of the passed body so we can treat it differently
		case io.Reader:
			r.bodyReader = b
		default:
			// If the type is unknown, possibly Marshal it as JSON
			if r.ContentType != ContentTypeJSON {
				return nil, nil, errors.New("unknown request body types and only be used in conjunction with httd.ContentTypeJSON")
			}

			if r.bodyReader, err = convertStructToIOReader(json.Marshal, r.Body); err != nil {
				return nil, nil, err
			}
		}
	}

	// create http request
	req, err := http.NewRequestWithContext(ctx, r.Method.String(), c.url+r.Endpoint, r.bodyReader)
	if err != nil {
		return nil, nil, err
	}

	header := copyHeader(c.reqHeader)
	header.Set(ContentType, r.ContentType)
	if r.Reason != "" {
		header.Add(XAuditLogReason, r.Reason)
	} else {
		// the header is a map, so it's a shared memory resource
		req.Header.Del(XAuditLogReason)
	}
	req.Header = header

	// queue & send request
	c.buckets.Bucket(r.hashedEndpoint, func(bucket RESTBucket) {
		resp, body, err = bucket.Transaction(ctx, func() (*http.Response, []byte, error) {
			resp, err := c.httpClient.Do(req)
			if err != nil {
				return nil, nil, err
			}

			// store the current timestamp
			epochMs := time.Now().UnixNano() / int64(time.Millisecond)
			resp.Header.Set(XDisgordNow, strconv.FormatInt(epochMs, 10))

			// decode body
			body, err := c.decodeResponseBody(resp)
			_ = resp.Body.Close()
			if err != nil {
				return nil, nil, err
			}

			// normalize Discord header fields
			resp.Header, err = NormalizeDiscordHeader(resp.StatusCode, resp.Header, body)
			return resp, body, err
		})
	})
	if err != nil {
		return nil, nil, err
	}

	// check if request was successful
	noDiff := resp.StatusCode == http.StatusNotModified
	withinSuccessScope := 200 <= resp.StatusCode && resp.StatusCode < 300
	if !(noDiff || withinSuccessScope) {
		// not within successful http range
		msg := "response was not within the successful http code range [200, 300). code: "
		msg += strconv.Itoa(resp.StatusCode)

		err = &ErrREST{
			Msg:            msg,
			Suggestion:     string(body),
			HTTPCode:       resp.StatusCode,
			Bucket:         c.buckets.BucketGrouping()[r.hashedEndpoint],
			HashedEndpoint: r.hashedEndpoint,
		}

		// store the Discord error if it exists
		if len(body) > 0 {
			_ = json.Unmarshal(body, err)
		}
		return nil, nil, err
	}

	return resp, body, nil
}

// helper functions
func convertStructToIOReader(marshal func(v interface{}) ([]byte, error), v interface{}) (io.Reader, error) {
	jsonParamsBytes, err := marshal(v)
	if err != nil {
		return nil, err
	}
	jsonParamsReader := bytes.NewReader(jsonParamsBytes)

	return jsonParamsReader, nil
}
