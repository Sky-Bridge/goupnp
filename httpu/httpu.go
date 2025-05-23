package httpu

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

// ClientInterface is the general interface provided to perform HTTP-over-UDP
// requests.
type ClientInterface interface {
	// Do performs a request. The timeout is how long to wait for before returning
	// the responses that were received. An error is only returned for failing to
	// send the request. Failures in receipt simply do not add to the resulting
	// responses.
	Do(
		req *http.Request,
		timeout time.Duration,
		numSends int,
	) ([]*http.Response, error)
}

// ClientInterfaceCtx is the equivalent of ClientInterface, except with methods
// taking a context.Context parameter.
type ClientInterfaceCtx interface {
	// DoWithContext performs a request. If the input request has a
	// deadline, then that value will be used as the timeout for how long
	// to wait before returning the responses that were received. If the
	// request's context is canceled, this method will return immediately.
	//
	// If the request's context is never canceled, and does not have a
	// deadline, then this function WILL NEVER RETURN. You MUST set an
	// appropriate deadline on the context, or otherwise cancel it when you
	// want to finish an operation.
	//
	// An error is only returned for failing to send the request. Failures
	// in receipt simply do not add to the resulting responses.
	DoWithContext(
		req *http.Request,
		numSends int,
	) ([]*http.Response, error)
}

// HTTPUClient is a client for dealing with HTTPU (HTTP over UDP). Its typical
// function is for HTTPMU, and particularly SSDP.
type HTTPUClient struct {
	connLock sync.Mutex // Protects use of conn.
	conn     net.PacketConn
}

var _ ClientInterface = &HTTPUClient{}
var _ ClientInterfaceCtx = &HTTPUClient{}

// NewHTTPUClient creates a new HTTPUClient, opening up a new UDP socket for the
// purpose.
func NewHTTPUClient() (*HTTPUClient, error) {
	conn, err := net.ListenPacket("udp", ":0")
	if err != nil {
		return nil, err
	}
	return &HTTPUClient{conn: conn}, nil
}

// NewHTTPUClientAddr creates a new HTTPUClient which will broadcast packets
// from the specified address, opening up a new UDP socket for the purpose on a random port
func NewHTTPUClientAddr(addr string) (*HTTPUClient, error) {
	ip := net.ParseIP(addr)
	if ip == nil {
		return nil, errors.New("Invalid listening address")
	}

	return NewHTTPUClientAddrWithPort(ip.String() + ":0")
}

// NewHTTPUClientAddrWithPort creates a new HTTPUClient which will broadcast packets
// from the specified address, opening up a new UDP socket for the purpose on a specific port
func NewHTTPUClientAddrWithPort(addr string) (*HTTPUClient, error) {
	conn, err := net.ListenPacket("udp", addr)
	if err != nil {
		return nil, err
	}
	return &HTTPUClient{conn: conn}, nil
}

// Close shuts down the client. The client will no longer be useful following
// this.
func (httpu *HTTPUClient) Close() error {
	httpu.connLock.Lock()
	defer httpu.connLock.Unlock()
	return httpu.conn.Close()
}

// Do implements ClientInterface.Do.
//
// Note that at present only one concurrent connection will happen per
// HTTPUClient.
func (httpu *HTTPUClient) Do(
	req *http.Request,
	timeout time.Duration,
	numSends int,
) ([]*http.Response, error) {
	ctx := req.Context()
	if timeout > 0 {
		var cancel func()
		ctx, cancel = context.WithTimeout(ctx, timeout)
		defer cancel()
		req = req.WithContext(ctx)
	}

	return httpu.DoWithContext(req, numSends)
}

// DoWithContext implements ClientInterfaceCtx.DoWithContext.
//
// Make sure to read the documentation on the ClientInterfaceCtx interface
// regarding cancellation!
func (httpu *HTTPUClient) DoWithContext(
	req *http.Request,
	numSends int,
) ([]*http.Response, error) {
	httpu.connLock.Lock()
	defer httpu.connLock.Unlock()

	// Create the request. This is a subset of what http.Request.Write does
	// deliberately to avoid creating extra fields which may confuse some
	// devices.
	var requestBuf bytes.Buffer
	method := req.Method
	if method == "" {
		method = "GET"
	}
	if _, err := fmt.Fprintf(&requestBuf, "%s %s HTTP/1.1\r\n", method, req.URL.RequestURI()); err != nil {
		return nil, err
	}
	if err := req.Header.Write(&requestBuf); err != nil {
		return nil, err
	}
	if _, err := requestBuf.Write([]byte{'\r', '\n'}); err != nil {
		return nil, err
	}

	destAddr, err := net.ResolveUDPAddr("udp", req.Host)
	if err != nil {
		return nil, err
	}

	// Handle context deadline/timeout
	ctx := req.Context()
	deadline, ok := ctx.Deadline()
	if ok {
		if err = httpu.conn.SetDeadline(deadline); err != nil {
			return nil, err
		}
	}

	// Handle context cancelation
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			// if context is cancelled, stop any connections by setting time in the past.
			httpu.conn.SetDeadline(time.Now().Add(-time.Second))
		case <-done:
		}
	}()

	// Send request.
	for i := 0; i < numSends; i++ {
		if n, err := httpu.conn.WriteTo(requestBuf.Bytes(), destAddr); err != nil {
			return nil, err
		} else if n < len(requestBuf.Bytes()) {
			return nil, fmt.Errorf("httpu: wrote %d bytes rather than full %d in request",
				n, len(requestBuf.Bytes()))
		}
		time.Sleep(5 * time.Millisecond)
	}

	// Await responses until timeout.
	var responses []*http.Response
	responseBytes := make([]byte, 2048)
	for {
		// 2048 bytes should be sufficient for most networks.
		n, _, err := httpu.conn.ReadFrom(responseBytes)
		if err != nil {
			if err, ok := err.(net.Error); ok {
				if err.Timeout() {
					break
				}
				if err.Temporary() {
					// Sleep in case this is a persistent error to avoid pegging CPU until deadline.
					time.Sleep(10 * time.Millisecond)
					continue
				}
			}
			return nil, err
		}

		// Parse response.
		response, err := http.ReadResponse(bufio.NewReader(bytes.NewBuffer(responseBytes[:n])), req)
		if err != nil {
			log.Printf("httpu: error while parsing response: %v", err)
			continue
		}

		// Set the related local address used to discover the device.
		if a, ok := httpu.conn.LocalAddr().(*net.UDPAddr); ok {
			response.Header.Add(LocalAddressHeader, a.IP.String())
		}

		responses = append(responses, response)
	}

	// Timeout reached - return discovered responses.
	return responses, nil
}

const LocalAddressHeader = "goupnp-local-address"
