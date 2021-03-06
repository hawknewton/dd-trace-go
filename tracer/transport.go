package tracer

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"time"
)

const (
	defaultHostname    = "localhost"
	defaultPort        = "7777"
	defaultEncoder     = MSGPACK_ENCODER // defines the default encoder used when the Transport is initialized
	legacyEncoder      = JSON_ENCODER    // defines the legacy encoder used with earlier agent versions
	defaultHTTPTimeout = time.Second     // defines the current timeout before giving up with the send process
	encoderPoolSize    = 5               // how many encoders are available
)

// Transport is an interface for span submission to the agent.
type Transport interface {
	Send(spans [][]*Span) (*http.Response, error)
	SetHeader(key, value string)
}

// NewTransport returns a new Transport implementation that sends traces to a
// trace agent running on the given hostname and port. If the zero values for
// hostname and port are provided, the default values will be used ("localhost"
// for hostname, and "7777" for port).
//
// In general, using this method is only necessary if you have a trace agent
// running on a non-default port or if it's located on another machine.
func NewTransport(hostname, port string) Transport {
	if hostname == "" {
		hostname = defaultHostname
	}
	if port == "" {
		port = defaultPort
	}
	return newHTTPTransport(hostname, port)
}

// newDefaultTransport return a default transport for this tracing client
func newDefaultTransport() Transport {
	return newHTTPTransport(defaultHostname, defaultPort)
}

type httpTransport struct {
	url               string            // the delivery URL
	legacyURL         string            // legacy delivery URL
	pool              *encoderPool      // encoding allocates lot of buffers (which might then be resized) so we use a pool so they can be re-used
	client            *http.Client      // the HTTP client used in the POST
	headers           map[string]string // the Transport headers
	compatibilityMode bool              // the Agent targets a legacy API for compatibility reasons
}

// newHTTPTransport returns an httpTransport for the given endpoint
func newHTTPTransport(hostname, port string) *httpTransport {
	// initialize the default EncoderPool with Encoder headers
	pool, contentType := newEncoderPool(defaultEncoder, encoderPoolSize)
	defaultHeaders := make(map[string]string)
	defaultHeaders["Content-Type"] = contentType

	return &httpTransport{
		url:       fmt.Sprintf("http://%s:%s/v0.3/traces", hostname, port),
		legacyURL: fmt.Sprintf("http://%s:%s/v0.2/traces", hostname, port),
		pool:      pool,
		client: &http.Client{
			Timeout: defaultHTTPTimeout,
		},
		headers:           defaultHeaders,
		compatibilityMode: false,
	}
}

func (t *httpTransport) Send(traces [][]*Span) (*http.Response, error) {
	if t.url == "" {
		return nil, errors.New("provided an empty URL, giving up")
	}

	// borrow an encoder
	encoder := t.pool.Borrow()
	defer t.pool.Return(encoder)

	// encode the spans and return the error if any
	err := encoder.Encode(traces)
	if err != nil {
		return nil, err
	}

	// prepare the client and send the payload
	req, _ := http.NewRequest("POST", t.url, encoder)
	for header, value := range t.headers {
		req.Header.Set(header, value)
	}
	response, err := t.client.Do(req)

	// if we have an error, return an empty Response to protect against nil pointer dereference
	if err != nil {
		return &http.Response{StatusCode: 0}, err
	}

	// if we got a 404 we should downgrade the API to a stable version (at most once)
	if (response.StatusCode == 404 || response.StatusCode == 415) && !t.compatibilityMode {
		log.Printf("calling the endpoint '%s' but received %d; downgrading the API\n", t.url, response.StatusCode)
		t.apiDowngrade()
		return t.Send(traces)
	}

	response.Body.Close()
	return response, err
}

// SetHeader sets the internal header for the httpTransport
func (t *httpTransport) SetHeader(key, value string) {
	t.headers[key] = value
}

// changeEncoder switches the internal encoders pool so that a different API with different
// format can be targeted, preventing failures because of outdated agents
func (t *httpTransport) changeEncoder(encoderType int) {
	pool, contentType := newEncoderPool(encoderType, encoderPoolSize)
	t.pool = pool
	t.headers["Content-Type"] = contentType
}

// apiDowngrade downgrades the used encoder and API level. This method must fallback to a safe
// encoder and API, so that it will success despite users' configurations. This action
// ensures that the compatibility mode is activated so that the downgrade will be
// executed only once.
func (t *httpTransport) apiDowngrade() {
	t.compatibilityMode = true
	t.url = t.legacyURL
	t.changeEncoder(legacyEncoder)
}
