package plugins

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"time"

	log "github.com/Sirupsen/logrus"

	"github.com/docker/docker/pkg/ioutils"
)

const pluginApiVersion = "v1"

func connect(addr string) (*httputil.ClientConn, error) {
	c, err := net.DialTimeout("unix", addr, 30*time.Second)
	if err != nil {
		return nil, err
	}
	return httputil.NewClientConn(c, nil), nil
}

func marshallBody(data interface{}) (io.Reader, error) {
	if data == nil {
		return nil, nil
	}

	body, err := json.Marshal(data)
	if err != nil {
		return nil, err
	}

	return bytes.NewBuffer(body), nil
}

func call(addr, method, path string, data interface{}) (io.ReadCloser, error) {
	client, err := connect(addr)
	if err != nil {
		return nil, err
	}

	reqBody, err := marshallBody(data)

	b = bytes.NewBuffer(reqBody)
	log.Debugf("sending request for extension:\n%s", string(b))
	if err != nil {
		return nil, err
	}

	path = "/" + pluginApiVersion + "/" + path

	req, err := http.NewRequest(method, path, reqBody)
	if err != nil {
		client.Close()
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	resp, err := client.Do(req)
	if err != nil {
		client.Close()
		return nil, err
	}

	// FIXME: this should be better defined
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("got bad status: %s", resp.Status)
	}

	return ioutils.NewReadCloserWrapper(resp.Body, func() error {
		if err := resp.Body.Close(); err != nil {
			return err
		}
		return client.Close()
	}), nil
}
